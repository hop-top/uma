package postgres

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pressly/goose/v3"

	"hop.top/uma/store"
)

// Units writing new messages with reference, attachment and claim rows,
// or new conversations with their claim rows, interleaved statement by
// statement as in insert_conflict_test.go. Each child row references the
// row the same call inserted just before; checked at once, its foreign key
// read the parent's index page that concurrent units then inserted into.
// The keys are checked at commit (migration 24).

// TestInterleavedConversationClaimsBothCommit: units of two tenants
// claiming conversations at once both commit: the tenants share the index
// pages of a small table. Within one small tenant they still fail one
// another: the claim's key check finds the conversation through the
// tenant's range of uma_conversations_pos, the planner's choice while the
// tenant has few rows, and so reads the other unit's new conversation.
func TestInterleavedConversationClaimsBothCommit(t *testing.T) {
	a := newInsertRig(t)
	b := newInsertRigIn(t, a.st, "postgres-insert-conflicts-b")
	interleaveIn(t, a.st,
		a.tenant, []func(store.Tx) error{a.claimConversation(a.conversationClaim("1")), a.claimConversation(a.conversationClaim("2"))},
		b.tenant, []func(store.Tx) error{b.claimConversation(b.conversationClaim("1")), b.claimConversation(b.conversationClaim("2"))},
	)
}

// TestInterleavedMessageClaimsBothCommit: units claiming different
// provider message IDs for new messages at once both commit, in one
// conversation or in two.
func TestInterleavedMessageClaimsBothCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    int
	}{{"one conversation", 0}, {"two conversations", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInsertRig(t)
			interleave(t, r.st,
				[]func(store.Tx) error{r.claimMessage(r.messageClaim(0)), r.claimMessage(r.messageClaim(0))},
				[]func(store.Tx) error{r.claimMessage(r.messageClaim(tc.b)), r.claimMessage(r.messageClaim(tc.b))},
			)
		})
	}
}

// TestInterleavedIndexedMessagesBothCommit: units each writing several new
// messages with references and attachments at once both commit.
func TestInterleavedIndexedMessagesBothCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    int
	}{{"one conversation", 0}, {"two conversations", 1}} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInsertRig(t)
			interleave(t, r.st,
				[]func(store.Tx) error{r.put(r.message(0, true)), r.put(r.message(0, true)), r.put(r.message(0, true))},
				[]func(store.Tx) error{r.put(r.message(tc.b, true)), r.put(r.message(tc.b, true)), r.put(r.message(tc.b, true))},
			)
		})
	}
}

// deferredKeys are the foreign keys migration 24 checks at commit.
var deferredKeys = []string{
	"uma_message_refs_tenant_id_message_id_fkey",
	"uma_message_attachments_tenant_id_message_id_fkey",
	"uma_message_claims_tenant_id_message_id_fkey",
	"uma_conversation_claims_tenant_id_inbox_id_conversation_id_fkey",
}

// TestDeferredKeysMigration: the keys are checked at commit, still reject
// a row naming a missing parent then, and are immediate again after
// migrating down.
func TestDeferredKeysMigration(t *testing.T) {
	pool := newDatabase(t, true)
	ctx := t.Context()
	deferred := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
			WHERE contype = 'f' AND conname = ANY ($1) AND condeferrable AND condeferred`, deferredKeys).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := deferred(); n != len(deferredKeys) {
		t.Fatalf("%d of %d keys checked at commit", n, len(deferredKeys))
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO uma_message_refs (tenant_id, inbox_id, message_id, channel_tenant, channel_id, namespace, kind, ext_id, parent_id)
		VALUES ('t', $1, $1, 't', $1, 'n', 'k', 'x', '')`, string(benchID(0xa3, 1))); err != nil {
		t.Fatalf("insert of a dangling reference row = %v, want it checked at commit", err)
	}
	var pe *pgconn.PgError
	if err := tx.Commit(ctx); !errors.As(err, &pe) || pe.Code != "23503" {
		t.Fatalf("commit of a dangling reference row = %v, want a foreign key violation", err)
	}
	if err := withProvider(ctx, pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 23)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	if n := deferred(); n != 0 {
		t.Errorf("%d keys still checked at commit after migrating down", n)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE contype = 'f' AND conname = ANY ($1) AND NOT condeferrable`, deferredKeys).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(deferredKeys) {
		t.Errorf("%d of %d keys immediate after migrating down", n, len(deferredKeys))
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	if n := deferred(); n != len(deferredKeys) {
		t.Errorf("%d of %d keys checked at commit after migrating up again", n, len(deferredKeys))
	}
}
