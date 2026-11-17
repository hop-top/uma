package postgres

import (
	"context"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// TestOperationScopeBackfill stores operations of every shape under schema
// 18, migrates, and requires the inbox and conversation columns to equal
// what store.OperationRecord.Scope resolves from the stored body, so the
// SQL backfill and the Go resolution agree; then migrates down and up
// again.
func TestOperationScopeBackfill(t *testing.T) {
	pool := newDatabase(t, false)
	ctx := t.Context()
	before := fstest.MapFS{}
	entries, err := fs.ReadDir(migrations(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n, err := strconv.Atoi(strings.SplitN(e.Name(), "_", 2)[0]); err == nil && n < 19 {
			b, err := fs.ReadFile(migrations(), e.Name())
			if err != nil {
				t.Fatal(err)
			}
			before[e.Name()] = &fstest.MapFile{Data: b}
		}
	}
	if err := migrate(ctx, pool, before); err != nil {
		t.Fatalf("migrate to schema 18: %v", err)
	}
	const tenant model.TenantID = "backfill"
	n := 0
	id := func() model.ID {
		n++
		return model.ID("00000000-0000-7000-8000-" + strings.Repeat("0", 12-len(strconv.Itoa(n))) + strconv.Itoa(n))
	}
	b1, b2 := model.InboxRef{Tenant: tenant, ID: id()}, model.InboxRef{Tenant: tenant, ID: id()}
	c1, c2 := model.ConversationRef{Inbox: b1, ID: id()}, model.ConversationRef{Inbox: b1, ID: id()}
	c3 := model.ConversationRef{Inbox: b2, ID: id()}
	msg := func(c model.ConversationRef) *model.MessageRef { return &model.MessageRef{Conversation: c, ID: id()} }
	stream := &model.StreamRef{Conversation: c1, ID: id()}
	task := &model.TaskRef{Conversation: c2, ID: id()}
	generic := func(r model.Ref) *model.Ref { return &r }
	shapes := []struct {
		name   string
		target model.Ref
		set    func(*model.Operation)
	}{
		{"conversation target", c1.Ref(), func(*model.Operation) {}},
		{"message result", b1.Ref(), func(o *model.Operation) { o.ResultMessage = msg(c2) }},
		{"message target and result", model.MessageRef{Conversation: c1, ID: id()}.Ref(), func(o *model.Operation) { o.ResultMessage = msg(c1) }},
		{"stream result", model.StreamRef{Conversation: c1, ID: id()}.Ref(), func(o *model.Operation) { o.ResultStream = stream }},
		{"task result", model.TaskRef{Conversation: c2, ID: id()}.Ref(), func(o *model.Operation) { o.ResultTask = task }},
		{"generic conversation result", model.WorkItemRef{Conversation: c1, ID: id()}.Ref(), func(o *model.Operation) { o.Result = generic(c2.Ref()) }},
		{"generic message result", model.WorkItemRef{Conversation: c1, ID: id()}.Ref(), func(o *model.Operation) { o.Result = generic(msg(c1).Ref()) }},
		{"generic conversation result in another inbox", model.WorkItemRef{Conversation: c1, ID: id()}.Ref(), func(o *model.Operation) { o.Result = generic(c3.Ref()) }},
		{"inbox target alone", b2.Ref(), func(*model.Operation) {}},
		{"result in another inbox", b1.Ref(), func(o *model.Operation) { o.ResultMessage = msg(c3) }},
		{"first result in the inbox wins", b1.Ref(), func(o *model.Operation) {
			o.ResultMessage, o.ResultStream = msg(c3), stream
		}},
		{"tenant-scoped target", model.ContactRef{Tenant: tenant, ID: id()}.Ref(), func(*model.Operation) {}},
	}
	for _, s := range shapes {
		rec := store.OperationRecord{
			Operation: model.Operation{Ref: model.OperationRef{Tenant: tenant, ID: id()}, State: model.Queued, Revision: "r1"},
			Key: store.OperationKey{
				Tenant: tenant, Caller: "backfill-actor", Kind: "messages.reply", Target: s.target, ID: model.OperationID("backfill-" + string(id())),
			},
			FingerprintVersion: 1, Fingerprint: []byte{1},
		}
		s.set(&rec.Operation)
		body, err := encode(&rec)
		if err != nil {
			t.Fatal(err)
		}
		var targetInbox *string
		if s.target.Inbox != "" {
			v := string(s.target.Inbox)
			targetInbox = &v
		}
		if _, err := pool.Exec(ctx, `INSERT INTO uma_operations (tenant_id, id, caller, kind, target_kind, target_inbox, target_id,
				operation_key, fingerprint_version, fingerprint, state, body, revision, written_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, '\x01', 'queued', $9, 'r1', now())`,
			string(tenant), string(rec.Operation.Ref.ID), string(rec.Key.Caller), rec.Key.Kind, string(s.target.Kind), targetInbox,
			string(s.target.ID), string(rec.Key.ID), body); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	rows, err := pool.Query(ctx, `SELECT body, coalesce(inbox_id::text, ''), coalesce(conversation_id::text, '') FROM uma_operations ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var body []byte
		var inbox, conv string
		if err := rows.Scan(&body, &inbox, &conv); err != nil {
			t.Fatal(err)
		}
		rec, err := decode[store.OperationRecord](body)
		if err != nil {
			t.Fatal(err)
		}
		want := rec.Scope()
		if inbox != string(want.Inbox) || conv != string(want.Conversation) {
			t.Errorf("operation %s (%s %s) backfilled as inbox %q conversation %q, Scope resolves inbox %q conversation %q",
				rec.Operation.Ref.ID, rec.Key.Target.Kind, rec.Key.Target.ID, inbox, conv, want.Inbox, want.Conversation)
		}
		seen++
	}
	if seen != len(shapes) {
		t.Fatalf("read %d operations, want %d", seen, len(shapes))
	}
	rows.Close()
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 18)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
}

// explainer runs every query it is given under EXPLAIN as well, and keeps
// the plan.
type explainer struct {
	db.DBTX
	plan strings.Builder
}

func (x *explainer) explain(ctx context.Context, sql string, args []any) error {
	x.plan.Reset()
	rows, err := x.DBTX.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		x.plan.WriteString(line + "\n")
	}
	return rows.Err()
}

func (x *explainer) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := x.explain(ctx, sql, args); err != nil {
		return nil, err
	}
	return x.DBTX.Query(ctx, sql, args...)
}

func (x *explainer) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if err := x.explain(ctx, sql, args); err != nil {
		return errRow{err}
	}
	return x.DBTX.QueryRow(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func (x *explainer) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return x.DBTX.Exec(ctx, sql, args...)
}

// TestOperationsUseTheirIndexes holds a large ledger and requires each
// filter shape of Reader.Operations to plan as a range scan of the index
// ending in write order, never a scan of the table or a sort, and the
// indexes to go with their migration.
func TestOperationsUseTheirIndexes(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	const tenant = "operations-plan"
	uuid := func(prefix, expr string) string {
		return "('" + prefix + "' || lpad(to_hex(" + expr + "), 12, '0'))::uuid"
	}
	// 40 inboxes of 500 operations; each inbox 25 conversations of 20; a
	// rare state; one write per second.
	seed := `INSERT INTO uma_operations (tenant_id, id, caller, kind, target_kind, target_inbox, target_id, operation_key,
			fingerprint_version, fingerprint, state, body, revision, written_at, inbox_id, conversation_id)
		SELECT '` + tenant + `', ` + uuid("00000000-0000-7000-8000-", "i") + `, 'actor', 'messages.reply', 'conversation',
			` + uuid("00000000-0000-7000-8001-", "i % 40") + `, ` + uuid("00000000-0000-7000-8002-", "i % 1000") + `, 'op-' || i,
			1, '\x01', CASE WHEN i % 1000 = 0 THEN 'outcome_unknown' WHEN i % 1000 = 7 THEN 'held' ELSE 'delivered' END, '{}', 'r1',
			'2023-11-14T22:13:20Z'::timestamptz + (i || ' seconds')::interval,
			` + uuid("00000000-0000-7000-8001-", "i % 40") + `, ` + uuid("00000000-0000-7000-8002-", "i % 1000") + `
		FROM generate_series(1, 20000) AS i`
	for _, q := range []string{seed, `ANALYZE uma_operations`} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	x := &explainer{DBTX: pool}
	q := db.New(x)
	base := db.ListOperationsParams{
		TenantID: tenant, AfterAt: pastEveryOperation.at, AfterID: pastEveryOperation.id, Inboxes: []string{}, States: []string{}, Lim: 11,
	}
	inbox := "00000000-0000-7000-8001-000000000007"
	conv := "00000000-0000-7000-8002-000000000007"
	rare := "outcome_unknown"
	held := "held"
	since := time.Date(2023, 11, 15, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		set   func(*db.ListOperationsParams)
		index string
		rows  int
	}{
		{"tenant", func(*db.ListOperationsParams) {}, "uma_operations_recent", 11},
		{"inbox", func(p *db.ListOperationsParams) { p.Inbox = &inbox }, "uma_operations_inbox_recent", 11},
		{"conversation", func(p *db.ListOperationsParams) { p.ConversationInbox, p.ConversationID = &inbox, &conv }, "uma_operations_conversation_recent", 11},
		{"state", func(p *db.ListOperationsParams) { p.State = &rare }, "uma_operations_state_recent", 11},
		{"inbox and held state", func(p *db.ListOperationsParams) { p.Inbox = &inbox; p.State = &held }, "uma_operations_state_recent", 11},
		{"window", func(p *db.ListOperationsParams) { p.Before = &since }, "uma_operations_recent", 11},
		{"inbox and window", func(p *db.ListOperationsParams) { p.Inbox = &inbox; p.Since = &since }, "uma_operations_inbox_recent", 11},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.set(&p)
			rows, err := q.ListOperations(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != c.rows {
				t.Errorf("rows = %d, want %d", len(rows), c.rows)
			}
			for i := 1; i < len(rows); i++ {
				if rows[i].WrittenAt.After(rows[i-1].WrittenAt) {
					t.Fatalf("row %d is newer than row %d: not most recent first", i, i-1)
				}
			}
			plan := x.plan.String()
			if !strings.Contains(plan, c.index) || strings.Contains(plan, "Seq Scan") || strings.Contains(plan, "Sort") {
				t.Errorf("plan does not range over %s:\n%s", c.index, plan)
			}
		})
	}

	indexes := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE indexname LIKE 'uma_operations_%_recent' OR indexname = 'uma_operations_recent'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := indexes(); got != 4 {
		t.Fatalf("operation list indexes after Migrate = %d, want 4", got)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 18)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := indexes(); got != 0 {
		t.Errorf("operation list indexes after migrating down = %d, want 0", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if got := indexes(); got != 4 {
		t.Errorf("operation list indexes after migrating up again = %d, want 4", got)
	}
}
