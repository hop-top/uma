package extensions

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"hop.top/uma/model"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

func goldenIntent() intent {
	return intent{
		Target:   model.Ref{Tenant: "tenant-a", Kind: model.KindConversation, Inbox: "00000004-0000-7000-8000-000000000003", ID: "00000004-0000-7000-8000-000000000004"},
		Audience: model.Audience{Visibility: model.Internal, PolicyRef: "staff-only"},
		Action:   model.Extension{Name: "com.example.chat.messages.pin", Version: 1, Data: json.RawMessage(`{"message":"m1","x_future":true}`)},
	}
}

// The stored intent is durable: its encoding is pinned by a golden file and
// decodes back to the same value.
func TestIntentGolden(t *testing.T) {
	stored, err := goldenIntent().encode()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "uma.intent.extension" || stored.Version != 1 {
		t.Fatalf("intent schema = %s v%d", stored.Name, stored.Version)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, stored.Data, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("testdata", "intent.json")
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) // #nosec G304 -- fixed test path
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pretty.Bytes(), want) {
		t.Fatalf("intent encoding changed:\n got %s\nwant %s", pretty.Bytes(), want)
	}
	back, err := decodeIntent(model.Extension{Name: stored.Name, Version: stored.Version, Data: want})
	if err != nil {
		t.Fatal(err)
	}
	g := goldenIntent()
	if back.Target != g.Target || back.Audience != g.Audience || back.Action.Name != g.Action.Name ||
		back.Action.Version != g.Action.Version || !bytes.Equal(compact(t, back.Action.Data), g.Action.Data) {
		t.Fatalf("decoded %+v, want %+v", back, g)
	}
}

func compact(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, b); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// Every intent field has an explicit snake_case json tag.
func TestIntentTagsComplete(t *testing.T) {
	snake := regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	typ := reflect.TypeFor[intent]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		tag, _, _ := bytes.Cut([]byte(f.Tag.Get("json")), []byte(","))
		if !snake.Match(tag) {
			t.Errorf("intent.%s: json tag %q is not snake_case", f.Name, f.Tag.Get("json"))
		}
	}
}

// Other stored intents are not extension intents.
func TestDecodeIntentRejectsOtherSchemas(t *testing.T) {
	for _, e := range []model.Extension{
		{Name: "uma.intent.reply", Version: 1, Data: json.RawMessage(`{}`)},
		{Name: intentName, Version: 2, Data: json.RawMessage(`{}`)},
	} {
		if _, err := decodeIntent(e); !model.IsCode(err, model.SchemaMismatch) {
			t.Errorf("%s v%d: error = %v, want schema_mismatch", e.Name, e.Version, err)
		}
	}
	if _, err := decodeIntent(model.Extension{Name: intentName, Version: 1, Data: json.RawMessage(`[`)}); !model.IsCode(err, model.Permanent) {
		t.Errorf("unreadable intent: error = %v, want permanent", err)
	}
}
