package sink

import (
	"context"
	"io/ioutil"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v4"
	"github.com/rueian/pgcapture/pkg/decode"
	"github.com/rueian/pgcapture/pkg/pb"
	"github.com/rueian/pgcapture/pkg/source"
)

func newPGXSink() *PGXSink {
	return &PGXSink{
		ConnStr:  "postgres://postgres@127.0.0.1/postgres?sslmode=disable",
		SourceID: "repl_test",
	}
}

func TestPGXSink(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	conn.Exec(ctx, "DROP EXTENSION IF EXISTS pgcapture")

	sink := newPGXSink()

	cp, err := sink.Setup()
	if err != nil {
		t.Fatal(err)
	}

	// test empty checkpoint
	if cp.LSN != 0 || !cp.Time.IsZero() {
		t.Fatalf("checkpoint of empty topic should be zero")
	}

	changes := make(chan source.Change)
	committed := sink.Apply(changes)

	lsn := uint64(0)
	now := time.Now()

	// ignore duplicated start lsn
	changes <- source.Change{
		Checkpoint: source.Checkpoint{LSN: lsn},
		Message:    &pb.Message{Type: &pb.Message_Begin{Begin: &pb.Begin{}}},
	}

	doTx := func(chs []*pb.Change) {
		now = now.Add(time.Second)
		ts := now.Unix()*1000000 + int64(now.Nanosecond())/1000 - microsecFromUnixEpochToY2K
		lsn++
		changes <- source.Change{
			Checkpoint: source.Checkpoint{LSN: lsn},
			Message:    &pb.Message{Type: &pb.Message_Begin{Begin: &pb.Begin{}}},
		}
		for _, change := range chs {
			lsn++
			changes <- source.Change{
				Checkpoint: source.Checkpoint{LSN: lsn},
				Message:    &pb.Message{Type: &pb.Message_Change{Change: change}},
			}
		}
		lsn++
		changes <- source.Change{
			Checkpoint: source.Checkpoint{LSN: lsn},
			Message:    &pb.Message{Type: &pb.Message_Commit{Commit: &pb.Commit{CommitTime: uint64(ts)}}},
		}
		if cp := <-committed; cp.LSN != lsn || cp.Time.Equal(now) {
			t.Fatalf("unexpected %v %v %v", cp, lsn, now)
		}
		if err = sink.Error(); err != nil {
			t.Fatalf("unexpected %v", err)
		}
	}

	doTx([]*pb.Change{{
		Op:        pb.Change_INSERT,
		Namespace: decode.ExtensionNamespace,
		Table:     decode.ExtensionDDLLogs,
		NewTuple:  []*pb.Field{{Name: "query", Datum: []byte(`create table t3 (f1 int, f2 int, f3 text, primary key(f1, f2))`)}},
	}})

	doTx([]*pb.Change{{
		Op:        pb.Change_INSERT,
		Namespace: "public",
		Table:     "t3",
		NewTuple: []*pb.Field{
			{Name: "f1", Oid: 23, Datum: []byte{0, 0, 0, 1}},
			{Name: "f2", Oid: 23, Datum: []byte{0, 0, 0, 1}},
			{Name: "f3", Oid: 25, Datum: []byte{'A'}},
		},
	}})

	doTx([]*pb.Change{{
		Op:        pb.Change_UPDATE,
		Namespace: "public",
		Table:     "t3",
		NewTuple: []*pb.Field{
			{Name: "f1", Oid: 23, Datum: []byte{0, 0, 0, 1}},
			{Name: "f2", Oid: 23, Datum: []byte{0, 0, 0, 1}},
			{Name: "f3", Oid: 25, Datum: []byte{'B'}},
		},
	}})

	// update with key changes
	doTx([]*pb.Change{{
		Op:        pb.Change_UPDATE,
		Namespace: "public",
		Table:     "t3",
		NewTuple: []*pb.Field{
			{Name: "f1", Oid: 23, Datum: []byte{0, 0, 0, 2}},
			{Name: "f2", Oid: 23, Datum: []byte{0, 0, 0, 3}},
			{Name: "f3", Oid: 25, Datum: []byte{'B'}},
		},
		OldTuple: []*pb.Field{
			{Name: "f1", Oid: 23, Datum: []byte{0, 0, 0, 1}},
			{Name: "f2", Oid: 23, Datum: []byte{0, 0, 0, 1}},
		},
	}})

	// handle select create case
	doTx([]*pb.Change{{
		Op:        pb.Change_INSERT,
		Namespace: decode.ExtensionNamespace,
		Table:     decode.ExtensionDDLLogs,
		NewTuple:  []*pb.Field{{Name: "query", Datum: []byte(`select * into t4 from t3`)}, {Name: "tags", Datum: []byte(`{SELECT}`)}},
	}, { // the data change after select create should be ignored
		Op:        pb.Change_INSERT,
		Namespace: "public",
		Table:     "t3",
		NewTuple: []*pb.Field{
			{Name: "f1", Oid: 23, Datum: []byte{0, 0, 0, 2}},
			{Name: "f2", Oid: 23, Datum: []byte{0, 0, 0, 3}},
			{Name: "f3", Oid: 25, Datum: []byte{'B'}},
		},
	}})

	doTx([]*pb.Change{{
		Op:        pb.Change_DELETE,
		Namespace: "public",
		Table:     "t3",
		OldTuple: []*pb.Field{
			{Name: "f1", Oid: 23, Datum: []byte{0, 0, 0, 2}},
			{Name: "f2", Oid: 23, Datum: []byte{0, 0, 0, 3}},
		},
	}})

	sink.Stop()

	// test restart checkpoint
	sink = newPGXSink()

	cp, err = sink.Setup()
	if err != nil {
		t.Fatal(err)
	}
	if cp.LSN != lsn || !cp.Time.Truncate(time.Millisecond).Equal(now.Truncate(time.Millisecond)) {
		t.Fatalf("unexpected %v %v %v", cp, lsn, now)
	}
	sink.Stop()
}

func TestPGXSink_DuplicatedSink(t *testing.T) {
	sink1 := newPGXSink()
	if _, err := sink1.Setup(); err != nil {
		t.Fatal(err)
	}
	defer sink1.Stop()

	sink2 := newPGXSink()
	if _, err := sink2.Setup(); err == nil || !strings.Contains(err.Error(), "occupying") {
		t.Fatal("duplicated sink")
	}
}

func TestPGXSink_ScanCheckpointFromLog(t *testing.T) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, "postgres://postgres@127.0.0.1/postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public")
	conn.Exec(ctx, "DROP EXTENSION IF EXISTS pgcapture")

	tmp, err := ioutil.TempFile("", "postgres.sql")
	if err != nil {
		t.Fatal(err)
	}
	tmp.WriteString("2021-03-01 16:25:02 UTC [2152-1] postgres@postgres FATAL:  the database system is starting up\n" +
		"2021-03-01 16:25:02 UTC [1934-5] LOG:  consistent recovery state reached at AE28/49A509D8\n" +
		"2021-03-01 16:25:02 UTC [1934-6] LOG:  invalid record length at AE28/49B13618: wanted 24, got 0\n" +
		"2021-03-01 16:25:02 UTC [1934-7] LOG:  redo done at AE28/49B135E8\n" +
		"2021-03-01 16:25:02 UTC [1934-8] LOG:  last completed transaction was at log time 2021-03-01 16:17:48.597172+00\n")
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	go func() {
		for i := 0; i < 10000; i++ {
			tmp.WriteString("2021-03-01 16:25:03 UTC [2163-1] postgres@postgres FATAL:  the database system is starting up\n")
		}
	}()
	reader, err := os.Open(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	sink := newPGXSink()
	sink.LogReader = reader

	cp, err := sink.Setup()
	if err != nil {
		t.Fatal(err)
	}
	lsn, _ := pglogrepl.ParseLSN("AE28/49B135E8")
	if cp.LSN != uint64(lsn) || cp.Time.Format(time.RFC3339Nano) != "2021-03-01T16:17:48.597172Z" {
		t.Fatalf("unexpected %v", cp)
	}
	sink.Stop()
}
