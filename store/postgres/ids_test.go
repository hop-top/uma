package postgres

import (
	"testing"

	"hop.top/uma/model"
)

func TestUUIDv7(t *testing.T) {
	ids := UUIDv7()
	seen := map[model.ID]bool{}
	for range 100 {
		id, err := ids.New(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.ParseID(string(id)); err != nil {
			t.Fatalf("UUIDv7 returned %q: %v", id, err)
		}
		if id[14] != '7' {
			t.Fatalf("UUIDv7 returned %q, want version 7", id)
		}
		if seen[id] {
			t.Fatalf("UUIDv7 repeated %q", id)
		}
		seen[id] = true
	}
}
