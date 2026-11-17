package postgres

import (
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

// inboundSQL is the InboundMessages query as queries/records.sql writes it.
const inboundSQL = `SELECT m.pos, m.body
FROM uma_messages m
WHERE m.tenant_id = $1 AND m.inbox_id = $2 AND m.conversation_id = $3
  AND m.pos < $4
  AND m.body ->> 'origin' IS NULL AND m.body ->> 'echo_of' IS NULL
  AND m.body ->> 'redacted' IS DISTINCT FROM 'true' AND m.body ->> 'deleted' IS DISTINCT FROM 'true'
ORDER BY m.pos DESC
LIMIT $5`

// TestInboundIndex holds a conversation of many messages, few of them live
// inbound, and requires the inbound query to select exactly those through
// the partial index, and the index to go with its migration.
func TestInboundIndex(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	const (
		tenant  = "inbound"
		channel = "00000000-0000-7000-8000-0000000000c1"
		inbox   = "00000000-0000-7000-8000-0000000000b1"
		conv    = "00000000-0000-7000-8000-0000000000a1"
	)
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('inbound', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('inbound', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('inbound', '` + inbox + `', '` + conv + `', 'per_contact', 'open', '{}', 'r1', 0, 0)`,
		// Row i: 1 is inbound, 2 has an origin, 3 an echo, 4 is redacted, 5 is
		// deleted, 6 has a JSON null origin (inbound), 7 and up are outbound.
		// Only rows 1 and 6 qualify.
		`INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision, occurred_unix, occurred_nanos)
			SELECT 'inbound', '` + inbox + `', '` + conv + `', ('00000000-0000-7000-8001-' || lpad(to_hex(i), 12, '0'))::uuid,
				CASE WHEN i = 1 THEN json_build_object('redacted', false, 'deleted', false)
				     WHEN i = 2 THEN json_build_object('origin', json_build_object('actor', 'a'), 'redacted', false, 'deleted', false)
				     WHEN i = 3 THEN json_build_object('echo_of', json_build_object('id', 'x'), 'redacted', false, 'deleted', false)
				     WHEN i = 4 THEN json_build_object('redacted', true, 'deleted', false)
				     WHEN i = 5 THEN json_build_object('redacted', false, 'deleted', true)
				     WHEN i = 6 THEN json_build_object('origin', NULL::json, 'redacted', false, 'deleted', false)
				     ELSE json_build_object('origin', json_build_object('actor', 'a'), 'redacted', false, 'deleted', false) END,
				'r1', 0, 0
			FROM generate_series(1, 20000) AS i`,
		`ANALYZE uma_messages`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	const unbounded = int64(1) << 62
	rows, err := pool.Query(ctx, inboundSQL, tenant, inbox, conv, unbounded, 10)
	if err != nil {
		t.Fatal(err)
	}
	var pos []int64
	for rows.Next() {
		var p int64
		var body []byte
		if err := rows.Scan(&p, &body); err != nil {
			t.Fatal(err)
		}
		pos = append(pos, p)
	}
	rows.Close()
	if len(pos) != 2 || pos[0] <= pos[1] {
		t.Fatalf("inbound positions = %v, want the two inbound rows, newest first", pos)
	}

	var plan strings.Builder
	rows, err = pool.Query(ctx, "EXPLAIN "+inboundSQL, tenant, inbox, conv, unbounded, 10)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(line + "\n")
	}
	rows.Close()
	if !strings.Contains(plan.String(), "uma_messages_inbound") || strings.Contains(plan.String(), "Seq Scan") {
		t.Errorf("plan does not use the partial index:\n%s", plan.String())
	}

	indexes := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname = 'uma_messages_inbound'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := indexes(); got != 1 {
		t.Fatalf("inbound index after Migrate = %d, want 1", got)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 17)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := indexes(); got != 0 {
		t.Errorf("inbound index after migrating down = %d, want 0", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if got := indexes(); got != 1 {
		t.Errorf("inbound index after migrating up again = %d, want 1", got)
	}
}
