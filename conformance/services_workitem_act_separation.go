package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// separation is the lifecycle independence case of WorkItems.Act with the
// conversation in status: a provider close or reopen, by command or by the
// provider's own webhook, never changes the status of the work item's
// conversation, whatever it is, and a conversation transition never reaches
// the provider.
func separation(status string) func(*testing.T, *tracker) {
	return func(t *testing.T, tr *tracker) { workItemActSeparation(t, tr, status) }
}

// observe delivers the provider's webhook of the issue moving to status,
// one minute after the latest change, and mirrors it in the provider.
func (tr *tracker) observe(event, status string) {
	tr.t.Helper()
	tr.a.mu.Lock()
	tr.a.at = tr.a.at.Add(time.Minute)
	at := tr.a.at
	tr.a.state.Status, tr.a.state.RawStatus = status, status+":external"
	s := tr.a.state
	tr.a.mu.Unlock()
	tr.accept(model.Envelope{
		EventID: event, Destination: deskDest, Kind: model.EnvelopeWorkItem, SystemActor: true,
		References: []model.ExternalRef{tr.issue("7")}, OccurredAt: at, SourceRevision: event, WorkItem: &s,
	})
}

// workItemActSeparation moves the issue closed, open, closed and open again,
// alternately by command and by webhook, with the conversation in status
// from the start: it stays there, and the issue follows each change.
func workItemActSeparation(t *testing.T, tr *tracker, status string) {
	if status != model.ConversationOpen {
		conv, err := tr.client.Conversations().Get(tr.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "get"}, Ref: tr.item.Ref.Conversation})
		if err != nil {
			t.Fatal(err)
		}
		_, err = tr.client.Conversations().Transition(tr.ctx, model.Transition{Command: tr.cmd(conv.Revision), Conversation: conv.Ref, State: status})
		if err != nil {
			t.Fatalf("Transition to %s: %v", status, err)
		}
		if n := len(tr.a.requests()); n != 0 {
			t.Fatalf("a conversation transition reached the provider: %d acts", n)
		}
		if got := tr.stored(); got.Status != model.WorkItemOpen || got.Revision != tr.item.Revision {
			t.Fatalf("a conversation transition changed the work item: %+v", got)
		}
	}
	want := func(step, item string) {
		t.Helper()
		if got := tr.conversationStatus(); got != status {
			t.Fatalf("after %s the conversation is %s, want it %s", step, got, status)
		}
		if got := tr.stored().Status; got != item {
			t.Fatalf("after %s the work item is %s, want %s", step, got, item)
		}
	}

	a, err := tr.act("op-close", tr.stored().Revision, closeWith("not_planned"))
	if err != nil {
		t.Fatal(err)
	}
	tr.settle(a.Operation.Ref)
	want("a close by command", model.WorkItemClosed)

	tr.observe("e-reopened", model.WorkItemOpen)
	want("a reopen by webhook", model.WorkItemOpen)

	tr.observe("e-closed", model.WorkItemClosed)
	want("a close by webhook", model.WorkItemClosed)

	a, err = tr.act("op-reopen", tr.stored().Revision, model.WorkItemUpdate{Status: ptrTo(model.WorkItemOpen), StatusReason: ptrTo("reopened")})
	if err != nil {
		t.Fatal(err)
	}
	tr.settle(a.Operation.Ref)
	want("a reopen by command", model.WorkItemOpen)
}
