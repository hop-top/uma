package conformance

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// inputPrefix renders the input a table assertion is checking as the start of
// its failure message, so a failure names the input instead of the one
// file:line every input of the table shares. No input renders nothing.
func inputPrefix(input []any) string {
	if len(input) == 0 {
		return ""
	}
	return fmt.Sprint(input...) + ": "
}

// brief renders v for a failure message without the dump %+v makes of it:
// zero fields are left out, and a reference type, which is named by its ID,
// shows only that ID.
func brief(v any) string {
	var b strings.Builder
	briefValue(&b, reflect.ValueOf(v))
	return b.String()
}

func briefValue(b *strings.Builder, v reflect.Value) {
	if !v.IsValid() {
		b.WriteString("nil")
		return
	}
	if v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			b.WriteString("nil")
			return
		}
		briefValue(b, v.Elem())
		return
	}
	if v.Type().Implements(reflect.TypeFor[fmt.Stringer]()) && v.CanInterface() {
		fmt.Fprint(b, v.Interface())
		return
	}
	switch v.Kind() {
	case reflect.Struct:
		if id := v.FieldByName("ID"); id.IsValid() && isIdentity(v.Type()) {
			fmt.Fprintf(b, "%s(%v)", strings.TrimSuffix(v.Type().Name(), "Ref"), id)
			return
		}
		b.WriteString("{")
		sep := ""
		for i := range v.NumField() {
			f := v.Field(i)
			if f.IsZero() || !v.Type().Field(i).IsExported() {
				continue
			}
			b.WriteString(sep + v.Type().Field(i).Name + ":")
			briefValue(b, f)
			sep = " "
		}
		b.WriteString("}")
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			fmt.Fprintf(b, "%d bytes", v.Len())
			return
		}
		b.WriteString("[")
		for i := range v.Len() {
			if i > 0 {
				b.WriteString(" ")
			}
			briefValue(b, v.Index(i))
		}
		b.WriteString("]")
	case reflect.Map:
		fmt.Fprintf(b, "map(%d)", v.Len())
	default:
		if v.CanInterface() {
			fmt.Fprintf(b, "%v", v.Interface())
			return
		}
		fmt.Fprintf(b, "%v", v)
	}
}

// isIdentity reports whether typ is a reference an ID names on its own: a
// Ref type whose other fields are the tenant and parent references that ID
// is unique within.
func isIdentity(typ reflect.Type) bool {
	if !strings.HasSuffix(typ.Name(), "Ref") {
		return false
	}
	for i := range typ.NumField() {
		f := typ.Field(i)
		switch {
		case f.Name == "ID", f.Name == "Tenant":
		case f.Type.Kind() == reflect.Struct && strings.HasSuffix(f.Type.Name(), "Ref"):
		default:
			return false
		}
	}
	return true
}

// wantEqual fails unless got equals want, naming what was compared and each
// top-level field that differs instead of dumping both whole records.
func wantEqual(t *testing.T, what string, got, want any) {
	t.Helper()
	if reflect.DeepEqual(got, want) {
		return
	}
	if diffFields(t, what, got, want) == 0 {
		t.Errorf("%s = %s, want %s", what, brief(got), brief(want))
	}
}

// diffFields reports each top-level field of got that differs from want,
// by name, so a failure says which field a store rewrote. It returns how
// many it reported.
func diffFields(t *testing.T, what string, got, want any) int {
	t.Helper()
	g, w := reflect.ValueOf(got), reflect.ValueOf(want)
	if g.Kind() != reflect.Struct || g.Type() != w.Type() {
		return 0
	}
	n := 0
	for i := range g.NumField() {
		name := g.Type().Field(i).Name
		if !g.Type().Field(i).IsExported() {
			continue
		}
		if !reflect.DeepEqual(g.Field(i).Interface(), w.Field(i).Interface()) {
			n++
			t.Errorf("%s.%s = %s, want %s", what, name, brief(g.Field(i).Interface()), brief(w.Field(i).Interface()))
		}
	}
	return n
}
