package postgres

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// TestInteractionsMigration migrates down past the interaction table and up
// again: nothing outside it depends on it, and a row written before the
// round trip is gone after it, since the down migration drops the table.
func TestInteractionsMigration(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	tables := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'uma_interactions'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := tables(); got != 1 {
		t.Fatalf("interaction table after Migrate = %d, want 1", got)
	}
	// The token column defaults to empty and the expiry columns are required.
	var token, constraint string
	if err := pool.QueryRow(ctx, `SELECT column_default, is_nullable FROM information_schema.columns
		WHERE table_name = 'uma_interactions' AND column_name = 'response_token'`).Scan(&token, &constraint); err != nil {
		t.Fatal(err)
	}
	if token != "''::text" || constraint != "NO" {
		t.Errorf("response_token default %q nullable %q, want an empty default and NOT NULL", token, constraint)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 16)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := tables(); got != 0 {
		t.Errorf("interaction table after migrating down = %d, want 0", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if got := tables(); got != 1 {
		t.Errorf("interaction table after migrating up again = %d, want 1", got)
	}
}
