package postgres_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

const tenant model.TenantID = "postgres-test-tenant"

// TestDatabaseErrorPoisonsUnit: once a statement fails in the database, the
// PostgreSQL transaction is aborted. Later calls report the same error and
// the unit commits nothing, even when the callback swallows the error.
func TestDatabaseErrorPoisonsUnit(t *testing.T) {
	ctx := t.Context()
	st, err := postgres.Open(ctx, postgres.NewTestDatabase(t, true), postgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ids := portstest.NewSequenceIDs(7)
	id := func() model.ID {
		v, err := ids.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	ch := model.ChannelRef{Tenant: tenant, ID: id()}
	in := model.InboxRef{Tenant: tenant, ID: id()}
	c1 := model.ConversationRef{Inbox: in, ID: id()}
	c2 := model.ConversationRef{Inbox: in, ID: id()}
	msg := model.MessageRef{Conversation: c1, ID: id()}
	at := time.Unix(1_700_000_000, 0).UTC()
	conv := func(ref model.ConversationRef) model.Conversation {
		return model.Conversation{Ref: ref, Mode: model.ModePerContact, Status: model.ConversationOpen, CreatedAt: at, Revision: "r1"}
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		return firstErr(
			tx.PutChannel(ctx, model.Channel{Ref: ch, Type: "test.chat", Revision: "r1"}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: in, Channel: ch, Revision: "r1"}, ""),
			tx.PutConversation(ctx, conv(c1), ""),
			tx.PutConversation(ctx, conv(c2), ""),
			tx.PutMessage(ctx, model.Message{Ref: msg, RecordedAt: at, Revision: "r1"}, ""),
		)
	}); err != nil {
		t.Fatal(err)
	}

	extra := model.ChannelRef{Tenant: tenant, ID: id()}
	var first error
	err = st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{Ref: extra, Type: "test.chat", Revision: "r1"}, ""); err != nil {
			return err
		}
		// The same message ID under another conversation: the store's own
		// checks pass, the primary key rejects the insert.
		first = tx.PutMessage(ctx, model.Message{Ref: model.MessageRef{Conversation: c2, ID: msg.ID}, RecordedAt: at, Revision: "r1"}, "")
		if model.CodeOf(first) != model.Conflict {
			t.Errorf("colliding insert = %v, want conflict", first)
		}
		if _, err := tx.GetChannel(ctx, ch); model.CodeOf(err) != model.Conflict {
			t.Errorf("call after a database error = %v, want the unit's conflict", err)
		}
		return nil
	})
	if model.CodeOf(err) != model.Conflict {
		t.Fatalf("Transact = %v, want the unit's conflict", err)
	}
	if err := st.View(ctx, tenant, func(r store.Reader) error {
		_, err := r.GetChannel(ctx, extra)
		return err
	}); model.CodeOf(err) != model.NotFound {
		t.Fatalf("write before the failure = %v, want not_found: the unit must not commit", err)
	}
}

// TestSchemaRejectsCrossTenantParent: composite foreign keys keep a child
// from pointing at another tenant's parent, below the store's own checks.
func TestSchemaRejectsCrossTenantParent(t *testing.T) {
	ctx := t.Context()
	pool := postgres.NewTestDatabase(t, true)
	const chID = "00000001-0000-7000-8000-000000000001"
	if _, err := pool.Exec(ctx, `INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
		VALUES ('tenant-a', $1, 't', '', 0, '', 'r1')`, chID); err != nil {
		t.Fatal(err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
		VALUES ('tenant-b', '00000001-0000-7000-8000-000000000002', $1, '{}', '{}', 'r1')`, chID)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "23503" {
		t.Fatalf("cross-tenant inbox insert = %v, want foreign key violation", err)
	}
}

// TestSchemaKeysResourceRecords: below the store's own read-then-write
// checks, the schema keeps one protocol state per channel instance and key,
// one stream, work item and attachment per tenant and ID, whatever
// conversation or inbox a second row names, one work item per
// conversation, and one channel and one inbox per tenant and key.
func TestSchemaKeysResourceRecords(t *testing.T) {
	ctx := t.Context()
	pool := postgres.NewTestDatabase(t, true)
	const (
		ch    = "00000001-0000-7000-8000-0000000000d1"
		in1   = "00000001-0000-7000-8000-0000000000d2"
		in2   = "00000001-0000-7000-8000-0000000000d3"
		conv1 = "00000001-0000-7000-8000-0000000000d4"
		conv2 = "00000001-0000-7000-8000-0000000000d5"
		conv3 = "00000001-0000-7000-8000-0000000000d7"
		id    = "00000001-0000-7000-8000-0000000000d6"
		id2   = "00000001-0000-7000-8000-0000000000d8"
		id3   = "00000001-0000-7000-8000-0000000000d9"
	)
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}
	if err := firstErr(
		exec(`INSERT INTO uma_channels (tenant_id, id, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('tenant-a', $1, 't', '', 0, '', 'r1')`, ch),
		exec(`INSERT INTO uma_inboxes (tenant_id, id, channel_id, destinations, body, revision)
			VALUES ('tenant-a', $1, $3, '{}', '{}', 'r1'), ('tenant-a', $2, $3, '{}', '{}', 'r1')`, in1, in2, ch),
		exec(`INSERT INTO uma_conversations (tenant_id, inbox_id, id, mode, status, created_unix, created_nanos, body, revision)
			VALUES ('tenant-a', $1, $2, 'm', 's', 0, 0, '{}', 'r1'), ('tenant-a', $1, $3, 'm', 's', 0, 0, '{}', 'r1'),
				('tenant-a', $1, $4, 'm', 's', 0, 0, '{}', 'r1')`, in1, conv1, conv2, conv3),
	); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, sql string
		first     []any
		second    []any
	}{
		{"protocol state", `INSERT INTO uma_protocol_states (tenant_id, channel_id, key, value_name, value_version, revision)
			VALUES ('tenant-a', $1, $2, 'v', 1, 'r1')`, []any{ch, "cursor"}, []any{ch, "cursor"}},
		{"stream", `INSERT INTO uma_streams (tenant_id, inbox_id, conversation_id, id, body, revision)
			VALUES ('tenant-a', $1, $2, $3, '{}', 'r1')`, []any{in1, conv1, id}, []any{in1, conv2, id}},
		{"work item", `INSERT INTO uma_work_items (tenant_id, inbox_id, conversation_id, id, status, body, revision)
			VALUES ('tenant-a', $1, $2, $3, 'open', '{}', 'r1')`, []any{in1, conv1, id}, []any{in1, conv2, id}},
		{"work item per conversation", `INSERT INTO uma_work_items (tenant_id, inbox_id, conversation_id, id, status, body, revision)
			VALUES ('tenant-a', $1, $2, $3, 'open', '{}', 'r1')`, []any{in1, conv3, id2}, []any{in1, conv3, id3}},
		{"attachment", `INSERT INTO uma_attachments (tenant_id, inbox_id, id, body, revision)
			VALUES ('tenant-a', $1, $2, '{}', 'r1')`, []any{in1, id}, []any{in2, id}},
		{"channel key", `INSERT INTO uma_channels (tenant_id, id, key, type_key, config_name, config_version, credential_ref, revision)
			VALUES ('tenant-a', $1, $2, 't', '', 0, '', 'r1')`, []any{"00000001-0000-7000-8000-0000000000da", "k"}, []any{"00000001-0000-7000-8000-0000000000db", "k"}},
		{"inbox key", `INSERT INTO uma_inboxes (tenant_id, id, channel_id, key, destinations, body, revision)
			VALUES ('tenant-a', $1, $2, $3, '{}', '{}', 'r1')`, []any{"00000001-0000-7000-8000-0000000000dc", ch, "k"}, []any{"00000001-0000-7000-8000-0000000000dd", ch, "k"}},
	} {
		if err := exec(c.sql, c.first...); err != nil {
			t.Fatalf("%s: first insert: %v", c.name, err)
		}
		err := exec(c.sql, c.second...)
		var pe *pgconn.PgError
		if !errors.As(err, &pe) || pe.Code != "23505" {
			t.Errorf("%s: second insert under the same key = %v, want unique violation", c.name, err)
		}
	}
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// TestConversationListsAreIndexed: work item and task lists filtered by
// conversation, the lookup ingress runs for every work item observation,
// are served by an index on the conversation's full ownership chain, not by
// a scan of the tenant's rows: the unique work item per conversation, and
// tasks in status order.
func TestConversationListsAreIndexed(t *testing.T) {
	ctx := t.Context()
	pool := postgres.NewTestDatabase(t, true)
	for table, want := range map[string][]string{
		"uma_work_items": {"tenant_id", "inbox_id", "conversation_id"},
		"uma_tasks":      {"tenant_id", "inbox_id", "conversation_id", "status_unix", "status_nanos", "id"},
		// The label selector and DeleteLabel's assignment check.
		"uma_conversation_labels": {"tenant_id", "label_id"},
	} {
		rows, err := pool.Query(ctx, `
			SELECT array_agg(a.attname::text ORDER BY k.ord)
			FROM pg_index x
			JOIN pg_class t ON t.oid = x.indrelid
			CROSS JOIN LATERAL unnest(x.indkey::int2[]) WITH ORDINALITY AS k(attnum, ord)
			JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = k.attnum
			WHERE t.relname = $1 AND pg_table_is_visible(t.oid)
			GROUP BY x.indexrelid`, table)
		if err != nil {
			t.Fatal(err)
		}
		var indexed bool
		for rows.Next() {
			var cols []string
			if err := rows.Scan(&cols); err != nil {
				t.Fatal(err)
			}
			indexed = indexed || slices.Equal(cols, want)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if !indexed {
			t.Errorf("%s: no index on %v", table, want)
		}
	}
}
