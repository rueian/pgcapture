package eventing

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/jackc/pgtype"
)

type Model interface {
	Name() (namespace, table string)
}

type ModelHandlerFunc func(model interface{}, deleted bool) error
type ModelHandlers map[Model]ModelHandlerFunc

func reflectModel(model Model) (ref reflection, err error) {
	typ := reflect.TypeOf(model)
	if typ.Kind() != reflect.Ptr || typ.Elem().Kind() != reflect.Struct {
		return ref, errors.New("the field Model of SwitchHandler should be a pointer of struct")
	}
	typ = typ.Elem()
	ref = reflection{idx: make(map[string]int, typ.NumField()), typ: typ}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !reflect.PtrTo(f.Type).Implements(decoderType) {
			return ref, fmt.Errorf("the field %s of %s should be a pgtype.BinaryDecoder", f.Name, typ.Elem())
		}
		tag, ok := f.Tag.Lookup("pg")
		if !ok {
			return ref, fmt.Errorf("the field %s of %s should should have a pg tag", f.Name, typ.Elem())
		}
		if n := strings.Split(tag, ","); len(n) > 0 && n[0] != "" {
			ref.idx[n[0]] = i
		}
	}
	return ref, nil
}

func ModelName(namespace, table string) string {
	if namespace == "" {
		return "public." + table
	}
	return namespace + "." + table
}

type reflection struct {
	idx map[string]int
	typ reflect.Type
	hdl ModelHandlerFunc
}

var decoderType = reflect.TypeOf((*pgtype.BinaryDecoder)(nil)).Elem()
