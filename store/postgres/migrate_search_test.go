package postgres

import (
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// TestMessageSearchBackfill writes messages under the schema before the
// search columns, migrates, and requires every row to carry the Go
// tokenizer's tokens and its exact OccurredAt, across more rows than one
// fill batch, so search finds messages written before the upgrade. A row
// left unfilled makes Open refuse the database until Migrate runs again.
func TestMessageSearchBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < searchVersion {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate before the search columns: %v", err)
	}
	const (
		tenant  = "backfill"
		channel = "00000000-0000-7000-8000-0000000000c1"
		inbox   = "00000000-0000-7000-8000-0000000000b1"
		conv    = "00000000-0000-7000-8000-0000000000a1"
	)
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('backfill', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('backfill', '` + inbox + `', '` + conv + `', 'per_contact', 'open', '{}', 'r1', 0, 0)`,
		// More rows than one fill batch.
		`INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision)
			SELECT 'backfill', '` + inbox + `', '` + conv + `', ('00000000-0000-7000-8001-' || lpad(to_hex(i), 12, '0'))::uuid,
				json_build_object('content', json_build_object('parts', json_build_array(
					json_build_object('kind', 'text', 'text', 'Bulk row ' || i))),
					'occurred_at', '2026-01-02T03:04:05Z'),
				'r1'
			FROM generate_series(1, ` + strconv.Itoa(fillBatch+20) + `) AS i`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	type row struct{ body, occurred string }
	rows := map[string]row{
		"00000000-0000-7000-8000-0000000000e1": {`{"content":{"parts":[{"kind":"text","text":"ÉTÉ foo@bar.com"},{"kind":"text","text":"v2.1"}]},"occurred_at":"2023-11-14T22:13:20.123456789+02:00"}`, "2023-11-14T22:13:20.123456789+02:00"},
		"00000000-0000-7000-8000-0000000000e2": {`{"content":{"parts":[{"kind":"text","text":"gone"}]},"redacted":true,"occurred_at":"1969-12-31T23:59:59.5Z"}`, "1969-12-31T23:59:59.5Z"},
		"00000000-0000-7000-8000-0000000000e3": {`{"content":{}}`, "0001-01-01T00:00:00Z"},
		"00000000-0000-7000-8000-0000000000e4": {`{}`, "0001-01-01T00:00:00Z"},
	}
	for id, r := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision)
			VALUES ($1, $2, $3, $4, $5, 'r1')`, tenant, inbox, conv, id, r.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for id, r := range rows {
		m, err := decode[model.Message]([]byte(r.body))
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse(time.RFC3339Nano, r.occurred)
		if err != nil {
			t.Fatal(err)
		}
		var tokens []string
		var unix int64
		var nanos int32
		if err := pool.QueryRow(ctx, `SELECT search_tokens, occurred_unix, occurred_nanos FROM uma_messages WHERE tenant_id = $1 AND id = $2`,
			tenant, id).Scan(&tokens, &unix, &nanos); err != nil {
			t.Fatal(err)
		}
		wantTokens := []string{}
		if !m.Redacted {
			wantTokens = append(wantTokens, m.Content.SearchTokens()...)
		}
		if tokens == nil || !slices.Equal(tokens, wantTokens) {
			t.Errorf("%s: tokens %#v, want %#v", id, tokens, wantTokens)
		}
		if unix != want.Unix() || int(nanos) != want.Nanosecond() {
			t.Errorf("%s: occurred (%d, %d), want (%d, %d)", id, unix, nanos, want.Unix(), want.Nanosecond())
		}
	}
	var unfilled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM uma_messages WHERE search_tokens IS NULL`).Scan(&unfilled); err != nil {
		t.Fatal(err)
	}
	if unfilled != 0 {
		t.Fatalf("%d rows left unfilled", unfilled)
	}

	st, err := Open(ctx, pool, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	last := fmt.Sprintf("00000000-0000-7000-8001-%012x", fillBatch+20)
	if err := st.View(ctx, tenant, func(r store.Reader) error {
		p, err := r.Messages(ctx, model.ListRequest{Filter: model.Filter{Text: "bulk " + strconv.Itoa(fillBatch+20)}, Page: model.PageRequest{Limit: 10}})
		// The bulk bodies carry no ref: the row is told by its text.
		want := "Bulk row " + strconv.Itoa(fillBatch+20)
		if err == nil && (len(p.Items) != 1 || p.Items[0].Content.Parts[0].Text != want) {
			t.Errorf("search after backfill = %+v, want the message %q", p.Items, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// An interrupted fill: Open refuses until Migrate completes it.
	if _, err := pool.Exec(ctx, `UPDATE uma_messages SET search_tokens = NULL WHERE id = $1`, last); err != nil {
		t.Fatal(err)
	}
	_, err = Open(ctx, pool, Options{})
	wantCode(t, err, model.SchemaMismatch)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
	if _, err := Open(ctx, pool, Options{}); err != nil {
		t.Fatalf("Open after Migrate: %v", err)
	}
}
