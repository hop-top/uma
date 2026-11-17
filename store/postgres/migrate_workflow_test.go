package postgres

import (
	"testing"

	"github.com/pressly/goose/v3"
)

// TestWorkflowMigrationBackfill migrates rows written at schema 19 to 20:
// a conversation body holding an assignee and a snooze fills its columns
// to the microsecond, as do the messages of that conversation; another
// conversation and its messages keep NULL. Sub-microsecond digits round,
// as PostgreSQL parses timestamps. Migrating down drops the
// columns and up again restores them.
func TestWorkflowMigrationBackfill(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	down := func(to int64) {
		t.Helper()
		if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
			_, err := p.DownTo(ctx, to)
			return err
		}); err != nil {
			t.Fatalf("down to %d: %v", to, err)
		}
	}
	down(19)
	const (
		channel = "00000000-0000-7000-8000-0000000000c1"
		inbox   = "00000000-0000-7000-8000-0000000000b1"
		snoozed = "00000000-0000-7000-8000-0000000000a1"
		plain   = "00000000-0000-7000-8000-0000000000a2"
		m1      = "00000000-0000-7000-8000-0000000000d1"
		m2      = "00000000-0000-7000-8000-0000000000d2"
	)
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('wf', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('wf', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('wf', '` + inbox + `', '` + snoozed + `', 'per_contact', 'open',
				'{"assignee":"desk-1","snooze":{"until":"2026-01-02T03:04:05.123456789Z","at":"2026-01-01T00:00:00.5Z","by":"desk-1"}}', 'r1', 0, 0)`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('wf', '` + inbox + `', '` + plain + `', 'per_contact', 'open', '{"labels":null}', 'r1', 0, 0)`,
		`INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision, occurred_unix, occurred_nanos)
			VALUES ('wf', '` + inbox + `', '` + snoozed + `', '` + m1 + `', '{"recorded_at":"2026-01-01T00:00:01.000002Z"}', 'r1', 0, 0)`,
		`INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision, occurred_unix, occurred_nanos)
			VALUES ('wf', '` + inbox + `', '` + plain + `', '` + m2 + `', '{"recorded_at":"2026-01-01T00:00:01Z"}', 'r1', 0, 0)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.UpTo(ctx, 20)
		return err
	}); err != nil {
		t.Fatalf("up to 20: %v", err)
	}
	var assignee *string
	var untilU, atU *int64
	var untilN, atN *int32
	row := pool.QueryRow(ctx, `SELECT assignee, snooze_until_unix, snooze_until_nanos, snooze_at_unix, snooze_at_nanos
		FROM uma_conversations WHERE id = $1`, snoozed)
	if err := row.Scan(&assignee, &untilU, &untilN, &atU, &atN); err != nil {
		t.Fatal(err)
	}
	if assignee == nil || *assignee != "desk-1" || untilU == nil || *untilU != 1767323045 || *untilN != 123457000 ||
		atU == nil || *atU != 1767225600 || *atN != 500000000 {
		t.Fatalf("snoozed conversation backfilled as assignee %v until %v.%v at %v.%v", show(assignee), show(untilU), show(untilN), show(atU), show(atN))
	}
	row = pool.QueryRow(ctx, `SELECT assignee, snooze_until_unix, snooze_at_unix FROM uma_conversations WHERE id = $1`, plain)
	if err := row.Scan(&assignee, &untilU, &atU); err != nil {
		t.Fatal(err)
	}
	if assignee != nil || untilU != nil || atU != nil {
		t.Fatalf("plain conversation backfilled as %v %v %v", show(assignee), show(untilU), show(atU))
	}
	var recU *int64
	var recN *int32
	if err := pool.QueryRow(ctx, `SELECT recorded_unix, recorded_nanos FROM uma_messages WHERE id = $1`, m1).Scan(&recU, &recN); err != nil {
		t.Fatal(err)
	}
	if recU == nil || *recU != 1767225601 || *recN != 2000 {
		t.Fatalf("message of the snoozed conversation backfilled as %v.%v", show(recU), show(recN))
	}
	if err := pool.QueryRow(ctx, `SELECT recorded_unix, recorded_nanos FROM uma_messages WHERE id = $1`, m2).Scan(&recU, &recN); err != nil {
		t.Fatal(err)
	}
	if recU != nil || recN != nil {
		t.Fatalf("message of a conversation without snooze backfilled as %v.%v", show(recU), show(recN))
	}
	down(19)
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_name IN ('uma_conversations', 'uma_messages')
		  AND column_name IN ('assignee', 'snooze_until_unix', 'snooze_until_nanos', 'snooze_at_unix', 'snooze_at_nanos', 'recorded_unix', 'recorded_nanos')`).Scan(&cols); err != nil {
		t.Fatal(err)
	}
	if cols != 0 {
		t.Fatalf("%d workflow columns after migrating down, want 0", cols)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
}

func show[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestLinksReadMarkersMigration migrates rows written at schema 20 to 21:
// link_count fills from the body's links, every message lacking recorded_*
// fills from its body, and the read marker table exists; down and up again
// round-trip.
func TestLinksReadMarkersMigration(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	down := func(to int64) {
		t.Helper()
		if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
			_, err := p.DownTo(ctx, to)
			return err
		}); err != nil {
			t.Fatalf("down to %d: %v", to, err)
		}
	}
	down(20)
	const (
		channel = "00000000-0000-7000-8000-0000000000c1"
		inbox   = "00000000-0000-7000-8000-0000000000b1"
		linked  = "00000000-0000-7000-8000-0000000000a1"
		plain   = "00000000-0000-7000-8000-0000000000a2"
		msg     = "00000000-0000-7000-8000-0000000000d1"
	)
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('wf', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('wf', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('wf', '` + inbox + `', '` + linked + `', 'per_contact', 'open',
				'{"links":[{"conversation":{"inbox":{"tenant":"wf","id":"` + inbox + `"},"id":"` + plain + `"}}]}', 'r1', 0, 0)`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('wf', '` + inbox + `', '` + plain + `', 'per_contact', 'open', '{"labels":null}', 'r1', 0, 0)`,
		`INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision, occurred_unix, occurred_nanos)
			VALUES ('wf', '` + inbox + `', '` + plain + `', '` + msg + `', '{"recorded_at":"2026-01-01T00:00:01.000002Z"}', 'r1', 0, 0)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for id, want := range map[string]int32{linked: 1, plain: 0} {
		var n int32
		if err := pool.QueryRow(ctx, `SELECT link_count FROM uma_conversations WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Fatalf("conversation %s link_count = %d, want %d", id, n, want)
		}
	}
	var recU *int64
	var recN *int32
	if err := pool.QueryRow(ctx, `SELECT recorded_unix, recorded_nanos FROM uma_messages WHERE id = $1`, msg).Scan(&recU, &recN); err != nil {
		t.Fatal(err)
	}
	if recU == nil || *recU != 1767225601 || *recN != 2000 {
		t.Fatalf("message backfilled as %v.%v", show(recU), show(recN))
	}
	var tables int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename = 'uma_read_markers'`).Scan(&tables); err != nil || tables != 1 {
		t.Fatalf("read marker tables = %d, %v", tables, err)
	}
	down(20)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename = 'uma_read_markers'`).Scan(&tables); err != nil || tables != 0 {
		t.Fatalf("read marker tables after down = %d, %v", tables, err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
}
