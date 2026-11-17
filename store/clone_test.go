package store

import (
	"reflect"
	"testing"
	"time"
)

var timeType = reflect.TypeFor[time.Time]()

// fill sets every exported field reachable from v to a non-zero value, so a
// Clone that forgets a slice, map or pointer field is caught by checkDisjoint.
func fill(v reflect.Value, depth int) {
	if depth > 8 {
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Unix(1700000000, 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i), depth+1)
			}
		}
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 2, 2)
		fill(s.Index(0), depth+1)
		fill(s.Index(1), depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k := reflect.New(v.Type().Key()).Elem()
		e := reflect.New(v.Type().Elem()).Elem()
		fill(k, depth+1)
		fill(e, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		fill(p.Elem(), depth+1)
		v.Set(p)
	}
}

// checkDisjoint fails when a and b share any slice backing array, map or
// pointer target.
func checkDisjoint(t *testing.T, path string, a, b reflect.Value) {
	t.Helper()
	switch a.Kind() {
	case reflect.Struct:
		if a.Type() == timeType {
			return
		}
		for i := 0; i < a.NumField(); i++ {
			if a.Type().Field(i).IsExported() {
				checkDisjoint(t, path+"."+a.Type().Field(i).Name, a.Field(i), b.Field(i))
			}
		}
	case reflect.Slice:
		if a.Len() > 0 && a.Pointer() == b.Pointer() {
			t.Errorf("%s: slice aliases the original", path)
		}
		for i := 0; i < min(a.Len(), b.Len()); i++ {
			checkDisjoint(t, path+"[]", a.Index(i), b.Index(i))
		}
	case reflect.Map:
		if !a.IsNil() && a.Pointer() == b.Pointer() {
			t.Errorf("%s: map aliases the original", path)
		}
		for _, k := range a.MapKeys() {
			checkDisjoint(t, path+"{}", a.MapIndex(k), b.MapIndex(k))
		}
	case reflect.Pointer:
		if !a.IsNil() && a.Pointer() == b.Pointer() {
			t.Errorf("%s: pointer aliases the original", path)
		}
		if !a.IsNil() && !b.IsNil() {
			checkDisjoint(t, path+"*", a.Elem(), b.Elem())
		}
	}
}

func checkClone[T any](t *testing.T, clone func(T) T) {
	t.Helper()
	var zero T
	if got := clone(zero); !reflect.DeepEqual(got, zero) {
		t.Fatalf("Clone(zero) = %+v; nil must stay nil", got)
	}
	var v T
	fill(reflect.ValueOf(&v).Elem(), 0)
	c := clone(v)
	if !reflect.DeepEqual(v, c) {
		t.Fatalf("Clone differs from original:\n got %+v\nwant %+v", c, v)
	}
	checkDisjoint(t, reflect.TypeFor[T]().Name(), reflect.ValueOf(v), reflect.ValueOf(c))
}

func TestCloneHasNoAliases(t *testing.T) {
	cases := map[string]func(*testing.T){
		"ProtocolState":      func(t *testing.T) { checkClone(t, ProtocolState.Clone) },
		"IngressRecord":      func(t *testing.T) { checkClone(t, IngressRecord.Clone) },
		"OperationRecord":    func(t *testing.T) { checkClone(t, OperationRecord.Clone) },
		"AttemptEvidence":    func(t *testing.T) { checkClone(t, AttemptEvidence.Clone) },
		"SubscriptionRecord": func(t *testing.T) { checkClone(t, SubscriptionRecord.Clone) },
		"BatchRecord":        func(t *testing.T) { checkClone(t, BatchRecord.Clone) },
		"AttachmentRecord":   func(t *testing.T) { checkClone(t, AttachmentRecord.Clone) },
		"ExportRecord":       func(t *testing.T) { checkClone(t, ExportRecord.Clone) },
		"WorkClaim":          func(t *testing.T) { checkClone(t, WorkClaim.Clone) },
		"LinkFilter":         func(t *testing.T) { checkClone(t, LinkFilter.Clone) },
		"ConversationClaim":  func(t *testing.T) { checkClone(t, ConversationClaim.Clone) },
		"MessageClaim":       func(t *testing.T) { checkClone(t, MessageClaim.Clone) },
		"InteractionRecord":  func(t *testing.T) { checkClone(t, InteractionRecord.Clone) },
	}
	for name, run := range cases {
		t.Run(name, run)
	}
}
