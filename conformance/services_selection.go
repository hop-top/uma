package conformance

import (
	"fmt"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// selectionHistory is the number of closed conversations one sender
// accumulates before the checks: more than the engine's candidate limit, so
// a store that counted closed history toward it would fail ingress.
const selectionHistory = 300

// serviceSelectionAfterClosedHistory runs per-contact ingress for one sender
// whose every conversation is closed after its first message: each message
// creates a successor, and closed history never stops selection. After the
// history, a new message creates one more conversation and the next one
// joins it.
func serviceSelectionAfterClosedHistory(t *testing.T, factory StoreFactory) {
	lock := true
	d := newDeskWith(t, factory, model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1})
	msg := func(n int) model.Envelope {
		return model.Envelope{
			EventID: fmt.Sprintf("e-%d", n), MessageID: fmt.Sprintf("m-%d", n), Destination: deskDest,
			Kind: model.EnvelopeMessage, Sender: d.user("u-1"), Assurance: "verified",
			Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
			Audience: model.Audience{Visibility: model.Public}, OccurredAt: d.clock.Now(),
		}
	}
	open := func() []model.Conversation {
		return deskRead(d, store.Reader.Conversations, model.Filter{
			Inboxes: []model.InboxRef{d.inbox}, Statuses: []string{model.ConversationOpen},
		})
	}
	seen := map[model.ID]bool{}
	for n := range selectionHistory {
		d.clock.Advance(time.Minute)
		d.accept(msg(n))
		cur := open()
		if len(cur) != 1 || seen[cur[0].Ref.ID] {
			t.Fatalf("message %d: open conversations %d, want one new successor", n, len(cur))
		}
		seen[cur[0].Ref.ID] = true
		if _, err := d.client.Conversations().Transition(d.ctx, model.Transition{
			Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(fmt.Sprintf("op-close-%d", n)), ExpectedRevision: cur[0].Revision},
			Conversation: cur[0].Ref, State: model.ConversationClosed,
		}); err != nil {
			t.Fatalf("close conversation %d: %v", n, err)
		}
	}
	d.clock.Advance(time.Minute)
	d.accept(msg(selectionHistory))
	cur := open()
	if len(cur) != 1 || seen[cur[0].Ref.ID] {
		t.Fatalf("after %d closed: open conversations %d, want one new successor", selectionHistory, len(cur))
	}
	d.clock.Advance(time.Minute)
	d.accept(msg(selectionHistory + 1))
	in := model.Filter{Inboxes: []model.InboxRef{d.inbox}}
	if n := len(deskRead(d, store.Reader.Conversations, in)); n != selectionHistory+1 {
		t.Fatalf("conversations = %d, want %d: the last message must join the open one", n, selectionHistory+1)
	}
	if n := len(deskRead(d, store.Reader.Messages, model.Filter{Conversation: &cur[0].Ref})); n != 2 {
		t.Fatalf("messages in the open conversation = %d, want 2", n)
	}
}
