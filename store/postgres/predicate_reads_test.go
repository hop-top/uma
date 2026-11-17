package postgres

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The predicate locks the reads of an ingress or start unit leave. At
// SERIALIZABLE a read leaves a lock on what it scanned, and a concurrent
// unit writing there then forms a read/write cycle with it: a read that
// scans a range of the tenant's rows where one row would do conflicts with
// every unit inserting a row into that range. The tests read as the engine
// does, from tables of some size, and bound the locks each read leaves to
// the rows and index pages it needs.
//
// A planner without statistics, as on a table that has not been analyzed
// yet or a tenant it has not seen, takes any index whose leading column the
// query fixes as good as the one that fixes every column, and the
// tenant's whole range is then what the query reads. The tests run on such
// tables; where a read also has to hold once statistics exist, a second
// case analyzes them first.

// lockedRelations returns the relations the unit holds predicate locks on,
// each with its lock granularity ("relation", "page" or "tuple") and count.
func lockedRelations(ctx context.Context, t *testing.T, tx store.Tx) []string {
	t.Helper()
	rows, err := tx.(txn).h.tx.Query(ctx, `
SELECT l.relation::regclass::text || ' ' || l.locktype, count(*)
FROM pg_locks l
WHERE l.pid = pg_backend_pid() AND l.mode = 'SIReadLock' AND l.relation::regclass::text LIKE 'uma\_%'
GROUP BY 1 ORDER BY 1`)
	if err != nil {
		t.Fatalf("predicate locks: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s x%d", k, n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// requireNoLocks fails when the unit holds a predicate lock of any
// granularity on one of the relations in forbidden.
func requireNoLocks(ctx context.Context, t *testing.T, what string, tx store.Tx, forbidden ...string) {
	t.Helper()
	locks := lockedRelations(ctx, t, tx)
	for _, l := range locks {
		name, _, _ := strings.Cut(l, " ")
		if slices.Contains(forbidden, name) {
			t.Errorf("%s left predicate locks on %s; all locks: %v", what, name, locks)
		}
	}
}

// grow adds n conversations to the rig's inbox, each claimed in a group of
// its own and holding a message with a provider reference, as an inbox that
// has received from n senders does. It returns the last group and the
// reference of the last message.
func (r *insertRig) grow(t *testing.T, n int) (store.SelectionKey, model.ExternalRef) {
	t.Helper()
	ctx := t.Context()
	var group store.SelectionKey
	var ref model.ExternalRef
	if err := r.st.Transact(ctx, r.tenant, func(tx store.Tx) error {
		for i := range n {
			c := r.conversationClaim(fmt.Sprintf("grown-%d", i))
			got, _, err := tx.ClaimConversation(ctx, c)
			if err != nil {
				return err
			}
			m := r.message(0, true)
			m.Ref.Conversation = got.Ref
			if err := tx.PutMessage(ctx, m, ""); err != nil {
				return err
			}
			group, ref = c.Selection, m.External[0]
		}
		return nil
	}); err != nil {
		t.Fatalf("grow: %v", err)
	}
	return group, ref
}

// analyze gives the planner statistics on the rig's tables.
func (r *insertRig) analyze(t *testing.T) {
	t.Helper()
	if _, err := r.st.pool.Exec(t.Context(), "ANALYZE"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

// TestMessagesByReferenceReadsReferencesFirst: looking a provider message
// ID up scans the reference index and fetches the messages it names by
// key. It does not scan the inbox's messages, which every new message of
// the inbox is written into, whether the ID is held or not.
func TestMessagesByReferenceReadsReferencesFirst(t *testing.T) {
	for _, tc := range []struct {
		name    string
		analyze bool
		held    bool
	}{
		{"unanalyzed tables, ID not held", false, false},
		{"analyzed tables, ID not held", true, false},
		{"analyzed tables, ID held", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInsertRig(t)
			_, held := r.grow(t, 60)
			if tc.analyze {
				r.analyze(t)
			}
			ref, want := model.ProviderMessageRef(r.ch, "never-received"), 0
			if tc.held {
				ref, want = held, 1
			}
			ctx := t.Context()
			if err := r.st.Transact(ctx, r.tenant, func(tx store.Tx) error {
				msgs, err := tx.MessagesByReference(ctx, r.in, ref, 2)
				if err != nil {
					return err
				}
				if len(msgs) != want {
					t.Errorf("%d messages, want %d", len(msgs), want)
				}
				requireNoLocks(ctx, t, "MessagesByReference", tx, "uma_messages_conversation", "uma_messages_pos")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestCandidatesReadClaimsFirst: reading a group's candidates scans the
// group's claims and fetches the conversations they name by key. It does
// not scan the inbox's conversations, which every new conversation of the
// inbox is written into, whether the group holds a conversation or not.
func TestCandidatesReadClaimsFirst(t *testing.T) {
	r := newInsertRig(t)
	last, _ := r.grow(t, 60)
	ctx := t.Context()
	if err := r.st.Transact(ctx, r.tenant, func(tx store.Tx) error {
		held, err := tx.Candidates(ctx, last, 8)
		if err != nil || len(held) != 1 {
			t.Errorf("held group: %d candidates, %v; want 1", len(held), err)
		}
		absent := store.SelectionKey{Inbox: r.in, Mode: last.Mode, Key: roomSelection + "never-seen"}
		if none, err := tx.Candidates(ctx, absent, 8); err != nil || len(none) != 0 {
			t.Errorf("absent group: %d candidates, %v; want none", len(none), err)
		}
		requireNoLocks(ctx, t, "Candidates", tx, "uma_conversations_pos", "uma_conversations_tenant_id_inbox_id_id_key")
		for _, l := range lockedRelations(ctx, t, tx) {
			if strings.HasPrefix(l, "uma_conversations relation") {
				t.Errorf("Candidates locked every conversation: %v", l)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWritesCheckConversationByKey: a unit that stores a message in a
// conversation, or claims a new one, checks the conversation it names by
// key: the existence check of the store and the foreign key checks of the
// message and of the claim, run at commit. Neither scans the tenant's
// conversations, which every new conversation of the tenant is written
// into.
func TestWritesCheckConversationByKey(t *testing.T) {
	r := newInsertRig(t)
	r.grow(t, 60)
	ctx := t.Context()
	if err := r.st.Transact(ctx, r.tenant, func(tx store.Tx) error {
		m := r.message(0, false)
		m.Ref.Conversation = r.convs[1]
		if err := tx.PutMessage(ctx, m, ""); err != nil {
			return err
		}
		if _, _, err := tx.ClaimConversation(ctx, r.conversationClaim("checked")); err != nil {
			return err
		}
		// The deferred foreign key checks run now, as they do at commit.
		if _, err := tx.(txn).h.tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
			return err
		}
		requireNoLocks(ctx, t, "storing a message and claiming a conversation", tx, "uma_conversations_pos")
		for _, l := range lockedRelations(ctx, t, tx) {
			if strings.HasPrefix(l, "uma_conversations relation") {
				t.Errorf("a conversation check locked every conversation: %v", l)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
