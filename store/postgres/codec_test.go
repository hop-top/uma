package postgres

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/model"
)

func TestCodecRawMessages(t *testing.T) {
	in := model.Message{
		Content: model.Content{Parts: []model.Part{
			{Kind: "text", Text: "hi"},
			{Kind: "card", Structured: &model.Extension{Name: "x.card", Version: 1, Data: json.RawMessage(`{"b":1,"a":[2]}`)}},
			{Kind: "empty", Structured: &model.Extension{Name: "x.empty", Version: 1, Data: json.RawMessage{}}},
		}},
		Metadata:   []model.Extension{{Name: "x.meta", Version: 2}},
		RecordedAt: time.Unix(1_700_000_000, 123456789).UTC(),
	}
	c := in.Clone()
	b, err := encode(&c)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decode[model.Message](b)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out.Content.Parts[1].Structured.Data); got != `{"b":1,"a":[2]}` {
		t.Errorf("structured data = %s, want the stored bytes", got)
	}
	if d := out.Content.Parts[2].Structured.Data; d != nil {
		t.Errorf("empty data read back as %q, want nil", d)
	}
	if d := out.Metadata[0].Data; d != nil {
		t.Errorf("absent data read back as %q, want nil", d)
	}
	if out.Content.Parts[0].Structured != nil {
		t.Error("nil extension pointer read back as non-nil")
	}
	if !out.RecordedAt.Equal(in.RecordedAt) || out.RecordedAt.Nanosecond() != 123456789 {
		t.Errorf("recorded_at = %v, want %v with nanoseconds", out.RecordedAt, in.RecordedAt)
	}
	// encode works on the caller's clone; the original is untouched.
	if in.Content.Parts[2].Structured.Data == nil {
		t.Error("encode changed the value it was not given")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, n := range []int64{0, 1, 42} {
		got, err := decodeCursor(encodeCursor(n))
		if err != nil || got != n {
			t.Fatalf("cursor %d round-trips to %d, %v", n, got, err)
		}
	}
	for _, bad := range []model.Cursor{"x", "pg.", "pg.0", "pg.-1", "pg.01", "mem.1"} {
		if _, err := decodeCursor(bad); model.CodeOf(err) != model.Validation {
			t.Errorf("decodeCursor(%q) = %v, want validation", bad, err)
		}
	}
}
