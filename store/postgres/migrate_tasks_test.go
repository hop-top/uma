package postgres

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/pressly/goose/v3"
)

// TestTaskColumnsBackfill writes tasks and messages under schema 15,
// migrates, and requires the task reference, status order and owner
// columns and the message task and parent columns to mirror each body, so
// reads find records written before the upgrade; then migrates down and up
// again.
func TestTaskColumnsBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < 16 {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate to schema 15: %v", err)
	}
	const (
		channel = "00000000-0000-7000-8000-0000000000c1"
		inbox   = "00000000-0000-7000-8000-0000000000b1"
		owned   = "00000000-0000-7000-8000-0000000000a1"
		plain   = "00000000-0000-7000-8000-0000000000a2"
		owner   = "00000000-0000-7000-8000-0000000000d1"
		task1   = "00000000-0000-7000-8000-0000000000f1"
		task2   = "00000000-0000-7000-8000-0000000000f2"
	)
	inboxRef := `{"tenant":"backfill","id":"` + inbox + `"}`
	convRef := func(id string) string { return `{"inbox":` + inboxRef + `,"id":"` + id + `"}` }
	external := `{"channel":{"tenant":"backfill","id":"` + channel + `"},"namespace":"uma.provider","kind":"task","id":"` + task1 + `","parent_id":""}`
	zero := `{"channel":{"tenant":"","id":""},"namespace":"","kind":"","id":"","parent_id":""}`
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('backfill', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('backfill', '` + inbox + `', '` + owned + `', 'per_agent_context', 'open',
			'{"owner":{"inbox":` + inboxRef + `,"id":"` + owner + `"}}', 'r1', 0, 0)`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('backfill', '` + inbox + `', '` + plain + `', 'per_contact', 'open', '{}', 'r1', 0, 0)`,
		`INSERT INTO uma_tasks (tenant_id, inbox_id, conversation_id, id, state, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + owned + `', '` + task1 + `', 'working',
			'{"external":` + external + `,"status_at":"2023-11-14T22:13:20.123456789+02:00"}', 'r1')`,
		`INSERT INTO uma_tasks (tenant_id, inbox_id, conversation_id, id, state, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + plain + `', '` + task2 + `', 'working', '{"external":` + zero + `}', 'r1')`,
		`INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision, occurred_unix, occurred_nanos) VALUES
			('backfill', '` + inbox + `', '` + owned + `', '00000000-0000-7000-8000-0000000000e1',
				'{"task":{"conversation":` + convRef(owned) + `,"id":"` + task1 + `"}}', 'r1', 0, 0),
			('backfill', '` + inbox + `', '` + owned + `', '00000000-0000-7000-8000-0000000000e2',
				'{"in_reply_to":{"conversation":` + convRef(owned) + `,"id":"00000000-0000-7000-8000-0000000000e1"}}', 'r1', 0, 0),
			('backfill', '` + inbox + `', '` + plain + `', '00000000-0000-7000-8000-0000000000e3',
				'{"task":{"conversation":` + convRef(owned) + `,"id":"` + task1 + `"}}', 'r1', 0, 0)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	tasks := map[string]string{
		task1: channel + " uma.provider task " + task1 + " | 1699992800 123456789 | " + inbox + " " + owner,
		task2: " | -62135596800 0 | ",
	}
	for id, want := range tasks {
		var got string
		if err := pool.QueryRow(ctx, `SELECT concat_ws(' ', external_channel_id, external_namespace, external_kind, external_id, nullif(external_parent_id, ''))
			|| ' | ' || status_unix || ' ' || status_nanos || ' | ' || concat_ws(' ', owner_inbox_id, owner_id)
			FROM uma_tasks WHERE tenant_id = 'backfill' AND id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("task %s backfilled as %q, want %q", id, got, want)
		}
	}
	messages := map[string]string{
		"00000000-0000-7000-8000-0000000000e1": task1 + " ",
		"00000000-0000-7000-8000-0000000000e2": " 00000000-0000-7000-8000-0000000000e1",
		"00000000-0000-7000-8000-0000000000e3": " ", // task in another conversation: not indexed
	}
	for id, want := range messages {
		var got string
		if err := pool.QueryRow(ctx, `SELECT coalesce(task_id::text, '') || ' ' || coalesce(in_reply_to_id::text, '')
			FROM uma_messages WHERE tenant_id = 'backfill' AND id = $1`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("message %s backfilled as %q, want %q", id, got, want)
		}
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 15)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
}
