package sink

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgtype"
	"github.com/jackc/pgx/v4"
	"github.com/rueian/pgcapture/pkg/decode"
	"github.com/rueian/pgcapture/pkg/pb"
	"github.com/rueian/pgcapture/pkg/source"
	"github.com/rueian/pgcapture/pkg/sql"
	"github.com/sirupsen/logrus"
)

type PGXSink struct {
	BaseSink

	ConnStr   string
	SourceID  string
	LogReader io.Reader

	conn    *pgx.Conn
	raw     *pgconn.PgConn
	schema  *decode.PGXSchemaLoader
	log     *logrus.Entry
	lsn     uint64
	inTX    bool
	skip    bool
	refresh bool
}

func (p *PGXSink) Setup() (cp source.Checkpoint, err error) {
	ctx := context.Background()
	p.conn, err = pgx.Connect(ctx, p.ConnStr)
	if err != nil {
		return cp, err
	}
	p.raw = p.conn.PgConn()

	var locked bool
	if err := p.conn.QueryRow(ctx, "select pg_try_advisory_lock(('x' || md5(current_database()))::bit(64)::bigint)").Scan(&locked); err != nil {
		return cp, err
	}
	if !locked {
		return cp, errors.New("pg_try_advisory_lock failed, another process is occupying")
	}

	if _, err = p.conn.Exec(ctx, sql.InstallExtension); err != nil {
		return cp, err
	}

	p.schema = decode.NewPGXSchemaLoader(p.conn)
	if err = p.schema.RefreshKeys(); err != nil {
		return cp, err
	}

	p.BaseSink.CleanFn = func() {
		p.conn.Close(context.Background())
	}

	p.log = logrus.WithFields(logrus.Fields{
		"From":     "PGXSink",
		"SourceID": p.SourceID,
	})

	return p.findCheckpoint(ctx)
}

func (p *PGXSink) findCheckpoint(ctx context.Context) (cp source.Checkpoint, err error) {
	var str, ts string
	var pts pgtype.Timestamptz
	err = p.conn.QueryRow(ctx, "SELECT commit, commit_ts FROM pgcapture.sources WHERE id = $1 AND status IS NULL", p.SourceID).Scan(&str, &pts)
	if err == pgx.ErrNoRows {
		err = nil
		if p.LogReader != nil {
			p.log.Info("try to find last checkpoint from log reader")
			str, ts, err = ScanCheckpointFromLog(p.LogReader)
		}
	}
	if str != "" {
		var l pglogrepl.LSN
		l, err = pglogrepl.ParseLSN(str)
		cp.LSN = uint64(l)
	}
	if ts != "" {
		cp.Time, err = time.Parse("2006-01-02 15:04:05.999999999Z07", ts)
	} else {
		cp.Time = pts.Time
	}
	if err != nil {
		return cp, err
	}
	p.log.WithFields(logrus.Fields{
		"RequiredLSN": cp.LSN,
		"SeekTs":      cp.Time,
	}).Info("last checkpoint found")

	if cp.LSN != 0 {
		p.lsn = cp.LSN - 1 // the next begin message will use the same WALStart, therefore -1 here
	}
	return cp, nil
}

func (p *PGXSink) Apply(changes chan source.Change) chan source.Checkpoint {
	var first bool
	return p.BaseSink.apply(changes, func(change source.Change, committed chan source.Checkpoint) (err error) {
		if !first {
			p.log.WithFields(logrus.Fields{
				"MessageLSN":  change.Checkpoint.LSN,
				"SinkLastLSN": p.lsn,
				"Message":     change.Message.String(),
			}).Info("applying the first message from source")
			first = true
		}
		if p.lsn >= change.Checkpoint.LSN {
			p.log.WithFields(logrus.Fields{
				"MessageLSN":  change.Checkpoint.LSN,
				"SinkLastLSN": p.lsn,
				"Message":     change.Message.String(),
			}).Warn("message dropped due to its lsn smaller than the last lsn of sink")
			return nil
		}
		switch msg := change.Message.Type.(type) {
		case *pb.Message_Begin:
			if p.inTX {
				err = ErrIncompleteTx
				break
			}
			err = p.handleBegin(msg.Begin)
			p.inTX = true
		case *pb.Message_Change:
			if !p.inTX {
				err = ErrIncompleteTx
				break
			}
			if p.skip {
				return nil
			}
			if decode.IsDDL(msg.Change) {
				p.refresh = true
				err = p.handleDDL(msg.Change)
			} else {
				err = p.handleChange(msg.Change)
			}
		case *pb.Message_Commit:
			if !p.inTX {
				err = ErrIncompleteTx
				break
			}
			if err = p.handleCommit(change.Checkpoint, msg.Commit); err != nil {
				break
			}
			committed <- change.Checkpoint
			p.inTX = false
			p.skip = false
			if p.refresh {
				err = p.schema.RefreshKeys()
				p.refresh = false
			}
		}
		if err != nil {
			p.log.WithFields(logrus.Fields{
				"MessageLSN": change.Checkpoint.LSN,
				"Message":    change.Message.String(),
			}).Errorf("fail to apply message: %v", err)
		}
		return err
	})
}

func (p *PGXSink) handleBegin(m *pb.Begin) (err error) {
	_, err = p.conn.Exec(context.Background(), "begin")
	return
}

func (p *PGXSink) handleChange(m *pb.Change) (err error) {
	ctx := context.Background()
	switch m.Op {
	case pb.Change_INSERT:
		return p.handleInsert(ctx, m)
	case pb.Change_UPDATE:
		return p.handleUpdate(ctx, m)
	case pb.Change_DELETE:
		return p.handleDelete(ctx, m)
	}
	return nil
}

func (p *PGXSink) handleDDL(m *pb.Change) (err error) {
	for _, field := range m.NewTuple {
		switch field.Name {
		case "query":
			_, err = p.conn.Exec(context.Background(), string(field.Datum))
		case "tags":
			p.skip = TableLoadDDLRegex.Match(field.Datum)
		}
	}
	return nil
}

func (p *PGXSink) handleInsert(ctx context.Context, m *pb.Change) (err error) {
	fields := len(m.NewTuple)
	vals := make([][]byte, fields)
	oids := make([]uint32, fields)
	fmts := make([]int16, fields)
	for i := 0; i < fields; i++ {
		field := m.NewTuple[i]
		vals[i] = field.Datum
		oids[i] = field.Oid
		fmts[i] = 1
	}
	return p.raw.ExecParams(ctx, sql.InsertQuery(m.Namespace, m.Table, m.NewTuple), vals, oids, fmts, fmts).Read().Err
}

func (p *PGXSink) handleDelete(ctx context.Context, m *pb.Change) (err error) {
	fields := len(m.OldTuple)
	vals := make([][]byte, fields)
	oids := make([]uint32, fields)
	fmts := make([]int16, fields)
	for i := 0; i < fields; i++ {
		field := m.OldTuple[i]
		vals[i] = field.Datum
		oids[i] = field.Oid
		fmts[i] = 1
	}
	return p.raw.ExecParams(ctx, sql.DeleteQuery(m.Namespace, m.Table, m.OldTuple), vals, oids, fmts, fmts).Read().Err
}

func (p *PGXSink) handleUpdate(ctx context.Context, m *pb.Change) (err error) {
	var keys []*pb.Field
	var sets []*pb.Field
	if m.OldTuple != nil {
		keys = m.OldTuple
		sets = m.NewTuple
	} else {
		kf, err := p.schema.GetTableKey(m.Namespace, m.Table)
		if err != nil {
			return err
		}
		keys = make([]*pb.Field, 0, len(kf))
		sets = make([]*pb.Field, 0, len(m.NewTuple)-len(kf))
	nextField:
		for _, f := range m.NewTuple {
			for _, k := range kf {
				if k == f.Name {
					keys = append(keys, f)
					continue nextField
				}
			}
			sets = append(sets, f)
		}
	}
	if len(sets) == 0 || len(keys) == 0 {
		return nil
	}

	fields := len(sets) + len(keys)
	vals := make([][]byte, fields)
	oids := make([]uint32, fields)
	fmts := make([]int16, fields)

	var j int
	for ; j < len(sets); j++ {
		field := sets[j]
		vals[j] = field.Datum
		oids[j] = field.Oid
		fmts[j] = 1
	}

	for i := 0; i < len(keys); i++ {
		j = i + j
		field := keys[i]
		vals[j] = field.Datum
		oids[j] = field.Oid
		fmts[j] = 1
	}

	return p.raw.ExecParams(ctx, sql.UpdateQuery(m.Namespace, m.Table, sets, keys), vals, oids, fmts, fmts).Read().Err
}

const (
	UpdateSourceSQL = "insert into pgcapture.sources(id,commit,commit_ts) values ($1,$2,$3) on conflict (id) do update set commit=EXCLUDED.commit,commit_ts=EXCLUDED.commit_ts,apply_ts=now()"
)

func (p *PGXSink) handleCommit(cp source.Checkpoint, commit *pb.Commit) (err error) {
	ctx := context.Background()
	if _, err = p.conn.Prepare(ctx, UpdateSourceSQL, UpdateSourceSQL); err != nil {
		return err
	}
	if _, err = p.conn.Exec(ctx, UpdateSourceSQL, pgText(p.SourceID), pgInt8(int64(cp.LSN)), pgTz(commit.CommitTime)); err != nil {
		return err
	}
	if _, err = p.conn.Exec(ctx, "commit"); err != nil {
		return err
	}
	return
}

func ScanCheckpointFromLog(f io.Reader) (lsn, ts string, err error) {
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if match := LogLSNRegex.FindStringSubmatch(line); len(match) > 1 {
			lsn = match[1]
		} else if match = LogTxTimeRegex.FindStringSubmatch(line); len(match) > 1 {
			ts = match[1]
		}
	}
	if lsn == "" || ts == "" {
		return "", "", fmt.Errorf("lsn not found in log: %w", scanner.Err())
	}
	return lsn, ts, nil
}

var (
	ErrIncompleteTx   = errors.New("receive incomplete transaction")
	LogLSNRegex       = regexp.MustCompile(`(?:consistent recovery state reached at|redo done at) ([0-9A-F]{2,8}\/[0-9A-F]{2,8})`)
	LogTxTimeRegex    = regexp.MustCompile(`last completed transaction was at log time (.*)\.?$`)
	TableLoadDDLRegex = regexp.MustCompile(`(CREATE TABLE AS|SELECT)`)
)

func pgText(t string) pgtype.Text {
	return pgtype.Text{String: t, Status: pgtype.Present}
}

func pgInt8(i int64) pgtype.Int8 {
	return pgtype.Int8{Int: i, Status: pgtype.Present}
}

func pgTz(ts uint64) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: PGTime2Time(ts), Status: pgtype.Present}
}

func PGTime2Time(ts uint64) time.Time {
	micro := microsecFromUnixEpochToY2K + int64(ts)
	return time.Unix(micro/microInSecond, (micro%microInSecond)*nsInSecond)
}

const microInSecond = int64(1e6)
const nsInSecond = int64(1e3)
const microsecFromUnixEpochToY2K = int64(946684800 * 1000000)
