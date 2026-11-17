package ingress

import (
	"context"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// stubTx answers Candidates with what it is given and counts the calls.
type stubTx struct {
	store.Tx
	reads int
	give  []model.Conversation
}

func (s *stubTx) Candidates(context.Context, store.SelectionKey, int) ([]model.Conversation, error) {
	s.reads++
	return s.give, nil
}

// candidates skips the read only for the one selection of the contact group
// of the identity the unit created, and then hands the selector what the
// read would have returned: an empty, non-nil list.
func TestCandidatesSkipsOnlyWhereEmptyByConstruction(t *testing.T) {
	inbox := model.InboxRef{Tenant: "t", ID: "in"}
	made := model.IdentityRef{Inbox: inbox, ID: "made"}
	other := model.IdentityRef{Inbox: inbox, ID: "other"}
	room := store.SelectionKey{Inbox: inbox, Mode: model.ModePerRoom, Key: "room:1"}
	stored := []model.Conversation{{Ref: model.ConversationRef{Inbox: inbox, ID: "c1"}}}

	for _, tc := range []struct {
		name      string
		fresh     *model.IdentityRef
		group     store.SelectionKey
		wantReads int
	}{
		{"identity created in the unit", &made, contactGroup(made), 0},
		{"identity found stored", nil, contactGroup(made), 1},
		{"another identity created", &other, contactGroup(made), 1},
		{"group of a room", &made, room, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := &stubTx{give: stored}
			u := &ingressUnit{ctx: t.Context(), tx: tx, fresh: tc.fresh}
			got, err := u.candidates(conversationScope{group: tc.group})
			if err != nil {
				t.Fatal(err)
			}
			if tx.reads != tc.wantReads {
				t.Fatalf("%d reads, want %d", tx.reads, tc.wantReads)
			}
			if tc.wantReads == 0 && (got == nil || len(got) != 0) {
				t.Fatalf("skipped selection got %#v, want empty and not nil", got)
			}
			if tc.wantReads == 1 && len(got) != 1 {
				t.Fatalf("read selection got %#v, want what the store returned", got)
			}
			if u.fresh != nil {
				t.Fatal("selection left the fresh identity set")
			}
		})
	}
}

// Only the first selection of a unit follows the identity's creation: a
// later one may find conversations the unit claimed since, so it reads.
func TestCandidatesSkipsOnce(t *testing.T) {
	inbox := model.InboxRef{Tenant: "t", ID: "in"}
	made := model.IdentityRef{Inbox: inbox, ID: "made"}
	tx := &stubTx{}
	u := &ingressUnit{ctx: t.Context(), tx: tx, fresh: &made}
	sc := conversationScope{group: contactGroup(made)}
	for i, want := range []int{0, 1, 2} {
		if _, err := u.candidates(sc); err != nil {
			t.Fatal(err)
		}
		if tx.reads != want {
			t.Fatalf("after selection %d: %d reads, want %d", i+1, tx.reads, want)
		}
	}
}

// A store that answers an empty group with nil still hands the selector a
// non-nil empty list, as the skipped selection does.
func TestCandidatesNeverNil(t *testing.T) {
	tx := &stubTx{}
	u := &ingressUnit{ctx: t.Context(), tx: tx}
	got, err := u.candidates(conversationScope{})
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("candidates = %#v, %v; want empty and not nil", got, err)
	}
}
