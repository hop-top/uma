package postgres

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var update = flag.Bool("update", false, "rewrite testdata/bodies golden files")

// bodyTypes is every record type stored in a body column. Model records are
// pinned by the model package's golden files; store records by TestBodyGolden.
var bodyTypes = []any{
	model.Inbox{},
	model.Contact{},
	model.Identity{},
	model.IdentityLink{},
	model.Conversation{},
	model.Label{},
	model.Message{},
	model.WorkItem{},
	model.Task{},
	model.Stream{},
	model.Event{},
	store.AttachmentRecord{},
	store.SubscriptionRecord{},
	store.IngressRecord{},
	store.OperationRecord{},
	store.AttemptEvidence{},
}

var (
	snakeKey     = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)
	timeType     = reflect.TypeFor[time.Time]()
	durationType = reflect.TypeFor[time.Duration]()
	rawType      = reflect.TypeFor[json.RawMessage]()
	goldenAt     = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
)

// TestBodyTagsComplete walks every struct type reachable from a stored body
// and requires an explicit snake_case json key on each exported field, so a
// Go field rename can never change the stored shape.
func TestBodyTagsComplete(t *testing.T) {
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
			walk(typ.Elem())
			return
		case reflect.Struct:
		default:
			return
		}
		if seen[typ] || typ == timeType {
			return
		}
		seen[typ] = true
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if !f.Anonymous {
				key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if !snakeKey.MatchString(key) {
					t.Errorf("%s.%s: json key %q, want an explicit snake_case key", typ, f.Name, key)
				}
			}
			walk(f.Type)
		}
	}
	for _, v := range bodyTypes {
		walk(reflect.TypeOf(v))
	}
}

// TestBodyGolden encodes a fully populated store record the way a body is
// written and compares it byte for byte with testdata/bodies/<Type>.json,
// then decodes it and requires the original record back. Run with -update
// to rewrite the golden files.
func TestBodyGolden(t *testing.T) {
	testdata := openTestdata(t)
	t.Run("AttachmentRecord", bodyGolden[store.AttachmentRecord](testdata))
	t.Run("SubscriptionRecord", bodyGolden[store.SubscriptionRecord](testdata))
	t.Run("IngressRecord", bodyGolden[store.IngressRecord](testdata))
	t.Run("OperationRecord", bodyGolden[store.OperationRecord](testdata))
	t.Run("AttemptEvidence", bodyGolden[store.AttemptEvidence](testdata))
	t.Run("ExportRecord", bodyGolden[store.ExportRecord](testdata))
}

// openTestdata opens the testdata directory as a root, so golden reads and
// writes cannot leave it.
func openTestdata(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot("testdata")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func bodyGolden[T any](testdata *os.Root) func(*testing.T) {
	return func(t *testing.T) { checkBodyGolden[T](t, testdata) }
}

func checkBodyGolden[T any](t *testing.T, testdata *os.Root) {
	var rec T
	name := reflect.TypeFor[T]().Name()
	populate(t, reflect.ValueOf(&rec).Elem(), name)
	compact, err := encode(&rec)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	if err := json.Indent(&got, compact, "", "  "); err != nil {
		t.Fatal(err)
	}
	got.WriteByte('\n')
	path := filepath.Join("testdata", "bodies", name+".json")
	file := filepath.Join("bodies", name+".json")
	if *update {
		if err := testdata.MkdirAll("bodies", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := testdata.WriteFile(file, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := testdata.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run go test -update to create it)", err)
	}
	if !bytes.Equal(got.Bytes(), golden) {
		t.Errorf("body of %s differs from %s:\n got: %s\nwant: %s", name, path, got.Bytes(), golden)
	}
	back, err := decode[T](compact)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, rec) {
		t.Errorf("%s does not round-trip:\n got: %#v\nwant: %#v", name, back, rec)
	}
}

// populate sets every exported field reachable from v to a non-zero value:
// strings to their field name, numbers to 1, durations to 1500ms, bools to
// true, times to a fixed UTC instant, pointers to a populated value and slices and maps to one
// populated element. Unsupported kinds fail the test.
func populate(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	switch {
	case v.Type() == timeType:
		v.Set(reflect.ValueOf(goldenAt))
		return
	case v.Type() == durationType:
		v.SetInt(int64(1500 * time.Millisecond))
		return
	case v.Type() == rawType:
		v.SetBytes([]byte(`{"field":"` + name + `"}`))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		populate(t, v.Elem(), name)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		populate(t, v.Index(0), name)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		populate(t, key, name+"Key")
		elem := reflect.New(v.Type().Elem()).Elem()
		populate(t, elem, name)
		v.SetMapIndex(key, elem)
	case reflect.Struct:
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if f.IsExported() {
				populate(t, v.Field(i), f.Name)
			}
		}
	default:
		t.Fatalf("populate: %s has unsupported kind %s", name, v.Kind())
	}
}
