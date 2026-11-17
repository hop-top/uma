package postgres

import (
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"hop.top/uma/model"
)

func wantCode(t *testing.T, err error, code model.ErrorCode) {
	t.Helper()
	if got := model.CodeOf(err); got != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
}

// TestSchemaVersionIsNewestMigration keeps SchemaVersion in step with the
// embedded migrations.
func TestSchemaVersionIsNewestMigration(t *testing.T) {
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	var newest int64
	for _, e := range entries {
		v, err := strconv.ParseInt(strings.SplitN(e.Name(), "_", 2)[0], 10, 64)
		if err != nil {
			t.Fatalf("migration %s: version prefix: %v", e.Name(), err)
		}
		newest = max(newest, v)
	}
	if newest != SchemaVersion {
		t.Fatalf("newest migration is %d, SchemaVersion is %d", newest, SchemaVersion)
	}
	if got := (&Store{}).Properties().SchemaVersion; got != SchemaVersion {
		t.Fatalf("Properties().SchemaVersion = %d, want %d", got, SchemaVersion)
	}
}

func TestOpenRequiresMigrate(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	_, err := Open(ctx, pool, Options{})
	wantCode(t, err, model.SchemaMismatch)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if _, err := Open(ctx, pool, Options{}); err != nil {
		t.Fatalf("Open after Migrate: %v", err)
	}
	// Migrate and Open close only their database/sql wrapper.
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pool closed by Migrate or Open: %v", err)
	}
}

// TestOpenRejectsNewerSchema applies a migration from a later release, then
// opens with this package: Open and Migrate both refuse the newer schema.
func TestOpenRejectsNewerSchema(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	if err := migrate(ctx, pool, newerMigrations(t)); err != nil {
		t.Fatalf("apply newer migration: %v", err)
	}
	_, err := Open(ctx, pool, Options{})
	wantCode(t, err, model.SchemaMismatch)
	if !strings.Contains(err.Error(), "newer") {
		t.Fatalf("Open error = %v, want a newer-schema message", err)
	}
	wantCode(t, Migrate(ctx, pool), model.SchemaMismatch)
}

// newerMigrations is the embedded migrations plus testdata/newer.
func newerMigrations(t *testing.T) fs.FS {
	t.Helper()
	out := fstest.MapFS{}
	add := func(fsys fs.FS, name string) {
		b, err := fs.ReadFile(fsys, name)
		if err != nil {
			t.Fatal(err)
		}
		out[path.Base(name)] = &fstest.MapFile{Data: b}
	}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		add(migrations(), e.Name())
	}
	add(os.DirFS("testdata/newer"), "99999_newer_schema.sql")
	return out
}

// TestConversationOrderBackfill writes conversations under schema 6, then
// migrates: the creation order columns hold the stored CreatedAt exactly,
// nanoseconds and offsets included.
func TestConversationOrderBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, _, _ := strings.Cut(e.Name(), "_"); n >= "00007" {
			continue
		}
		b, err := fs.ReadFile(migrations(), e.Name())
		if err != nil {
			t.Fatal(err)
		}
		before[e.Name()] = &fstest.MapFile{Data: b}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate to schema 6: %v", err)
	}
	const (
		tenant  = "backfill"
		channel = "00000000-0000-7000-8000-00000000000c"
		inbox   = "00000000-0000-7000-8000-00000000000b"
	)
	if _, err := pool.Exec(ctx, `
		INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
		VALUES ($1, $2, 't', 'c', 1, '', 'r1');
		`, tenant, channel); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
		VALUES ($1, $2, $3, '{}', '{}', 'r1')`, tenant, inbox, channel); err != nil {
		t.Fatal(err)
	}
	stamps := []string{
		"2023-11-14T22:13:20.123456789+02:00", // nine digits, below microsecond precision
		"2023-11-14T22:13:20.5Z",              // trimmed fraction
		"2023-11-14T22:13:20Z",                // no fraction
		"2023-11-14T22:13:20.999999999Z",      // must not round into the next second
		"0001-01-01T00:00:00Z",                // zero time
	}
	for i, s := range stamps {
		id := "00000000-0000-7000-8000-00000000010" + strconv.Itoa(i)
		if _, err := pool.Exec(ctx, `
			INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision)
			VALUES ($1, $2, $3, 'per_contact', 'closed', json_build_object('created_at', $4::text), 'r1')`,
			tenant, inbox, id, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for i, s := range stamps {
		want, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatal(err)
		}
		var unix int64
		var nanos int32
		id := "00000000-0000-7000-8000-00000000010" + strconv.Itoa(i)
		if err := pool.QueryRow(ctx, `SELECT created_unix, created_nanos FROM uma_conversations WHERE tenant_id = $1 AND id = $2`,
			tenant, id).Scan(&unix, &nanos); err != nil {
			t.Fatal(err)
		}
		if unix != want.Unix() || int(nanos) != want.Nanosecond() {
			t.Errorf("%s backfilled as (%d, %d), want (%d, %d)", s, unix, nanos, want.Unix(), want.Nanosecond())
		}
	}
}

// TestIngressMessageIDBackfill writes ingress rows under the schema before
// the message ID column, migrates, and requires the column to mirror each
// body's envelope message ID, so redaction finds records written before
// the upgrade.
func TestIngressMessageIDBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < 8 {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate before the message ID column: %v", err)
	}
	const (
		tenant  = "backfill"
		channel = "00000000-0000-7000-8000-0000000000c1"
	)
	if _, err := pool.Exec(ctx, `INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
		VALUES ($1, $2, 'x.chat', 'x.chat.config', 1, '', 'r1')`, tenant, channel); err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{
		"00000000-0000-7000-8000-0000000000e1": `{"input":{"envelope":{"message_id":"pm-1"}}}`,
		"00000000-0000-7000-8000-0000000000e2": `{"input":{"envelope":{}}}`,
	}
	for id, body := range rows {
		if _, err := pool.Exec(ctx, `INSERT INTO uma_ingress (tenant_id, id, channel_id, dedup_key, body, written_at)
			VALUES ($1, $2, $3, $4, $5, now())`, tenant, id, channel, "dedup-"+id, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	want := map[string]string{"00000000-0000-7000-8000-0000000000e1": "pm-1", "00000000-0000-7000-8000-0000000000e2": ""}
	for id, msg := range want {
		var got string
		if err := pool.QueryRow(ctx, `SELECT message_id FROM uma_ingress WHERE tenant_id = $1 AND id = $2`, tenant, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != msg {
			t.Errorf("message_id of %s = %q, want %q", id, got, msg)
		}
	}
}

// TestMessageAttachmentBackfill writes messages under the schema before
// the attachment index, migrates, and requires one row per distinct
// attachment of the message's tenant each body's parts name, so
// MessagesByAttachment finds holders written before the upgrade.
func TestMessageAttachmentBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < 10 {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate before the attachment index: %v", err)
	}
	const (
		tenant  = "backfill"
		channel = "00000000-0000-7000-8000-0000000000c1"
		inbox   = "00000000-0000-7000-8000-0000000000b1"
		conv    = "00000000-0000-7000-8000-0000000000a1"
		att     = `{"inbox":{"tenant":"backfill","id":"00000000-0000-7000-8000-0000000000b1"},"id":"00000000-0000-7000-8000-0000000000f1"}`
		foreign = `{"inbox":{"tenant":"other","id":"00000000-0000-7000-8000-0000000000b1"},"id":"00000000-0000-7000-8000-0000000000f2"}`
	)
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('backfill', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, body, revision, created_unix, created_nanos)
			VALUES ('backfill', '` + inbox + `', '` + conv + `', 'per_contact', 'open', '{}', 'r1', 0, 0)`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	bodies := map[string]string{
		"00000000-0000-7000-8000-0000000000e1": `{"content":{"parts":[{"kind":"text","text":"x"},{"kind":"attachment","attachment":` + att + `},{"kind":"attachment","attachment":` + att + `}]}}`,
		"00000000-0000-7000-8000-0000000000e2": `{"content":{"parts":[{"kind":"attachment","attachment":` + foreign + `}]}}`,
		"00000000-0000-7000-8000-0000000000e3": `{"content":{}}`,
		"00000000-0000-7000-8000-0000000000e4": `{}`,
	}
	for id, body := range bodies {
		if _, err := pool.Exec(ctx, `INSERT INTO uma_messages (tenant_id, inbox_id, conversation_id, id, body, revision)
			VALUES ($1, $2, $3, $4, $5, 'r1')`, tenant, inbox, conv, id, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT message_id::text, inbox_id, attachment_id FROM uma_message_attachments WHERE tenant_id = $1 ORDER BY message_id`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var m, in, a string
		if err := rows.Scan(&m, &in, &a); err != nil {
			t.Fatal(err)
		}
		got = append(got, m+" "+in+" "+a)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"00000000-0000-7000-8000-0000000000e1 " + inbox + " 00000000-0000-7000-8000-0000000000f1"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("backfilled rows = %q, want %q", got, want)
	}
}

// TestLinkWrittenAtMigration writes a link record under schema 11,
// migrates, and requires the record stamped with the migration time, so a
// contact orphaned before the upgrade gets a full retention horizon; then
// migrates down to schema 11 and up again.
func TestLinkWrittenAtMigration(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < 12 {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate before the link write stamp: %v", err)
	}
	const (
		channel  = "00000000-0000-7000-8000-0000000000c1"
		inbox    = "00000000-0000-7000-8000-0000000000b1"
		identity = "00000000-0000-7000-8000-0000000000d1"
		from     = "00000000-0000-7000-8000-0000000000a1"
		to       = "00000000-0000-7000-8000-0000000000a2"
		link     = "00000000-0000-7000-8000-0000000000e1"
	)
	for _, q := range []string{
		`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('backfill', '` + channel + `', 'x.chat', 'x.chat.config', 1, '', 'r1')`,
		`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + channel + `', '{}', '{}', 'r1')`,
		`INSERT INTO uma_identities (tenant_id, inbox_id, id, namespace, source_id, contact_id, body, revision)
			VALUES ('backfill', '` + inbox + `', '` + identity + `', 'x.user', 'u-1', '` + to + `', '{}', 'r2')`,
		`INSERT INTO uma_identity_links (tenant_id, id, inbox_id, identity_id, from_contact, to_contact,
			evidence_verifier, evidence_reference, body)
			VALUES ('backfill', '` + link + `', '` + inbox + `', '` + identity + `', '` + from + `', '` + to + `', 'v', '', '{}')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	var start time.Time
	if err := pool.QueryRow(ctx, `SELECT now()`).Scan(&start); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	var stamped time.Time
	if err := pool.QueryRow(ctx, `SELECT written_at FROM uma_identity_links WHERE tenant_id = 'backfill' AND id = $1`, link).
		Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped.Before(start) {
		t.Errorf("written_at = %v, want the migration time, after %v", stamped, start)
	}

	column := func() bool {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = 'uma_identity_links' AND column_name = 'written_at'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 11)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if column() {
		t.Error("written_at kept after migrating down")
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if !column() {
		t.Error("written_at missing after migrating up again")
	}
}

// TestLabelsMigration migrates down past the label tables and up again:
// nothing outside them depends on them.
func TestLabelsMigration(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	tables := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_name IN ('uma_labels', 'uma_conversation_labels')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := tables(); got != 2 {
		t.Fatalf("label tables after Migrate = %d, want 2", got)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 12)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := tables(); got != 0 {
		t.Errorf("label tables after migrating down = %d, want 0", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if got := tables(); got != 2 {
		t.Errorf("label tables after migrating up again = %d, want 2", got)
	}
}

// TestRecipientLogsMigration migrates down past the recipient log tables
// and up again: nothing outside them depends on them.
func TestRecipientLogsMigration(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	tables := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables
			WHERE table_name IN ('uma_recipient_log_heads', 'uma_recipient_log_entries')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := tables(); got != 2 {
		t.Fatalf("recipient log tables after Migrate = %d, want 2", got)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 14)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := tables(); got != 0 {
		t.Errorf("recipient log tables after migrating down = %d, want 0", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if got := tables(); got != 2 {
		t.Errorf("recipient log tables after migrating up again = %d, want 2", got)
	}
}
