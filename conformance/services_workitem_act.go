package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// workItemActCases are the provider work item act cases of
// RunServiceSuite: WorkItems.Act on a provider-owned status through the
// operation ledger to an adapter declaring work_item_act. The work item
// changes only from the provider's answer, never at submission or from a
// refusal; the provider's webhook of the change never settles the
// operation; the conversation keeps its status whatever the issue does,
// closed, reopened, by command or by webhook.
var workItemActCases = []struct {
	name string
	run  func(*testing.T, *tracker)
}{
	{"CloseWithReason", workItemActClose},
	{"Reopen", workItemActReopen},
	{"UndeclaredUnsupported", workItemActUndeclared},
	{"MixedFieldsValidation", workItemActMixed},
	{"ProviderFailureLeavesState", workItemActRefused},
	{"WebhookNeverSettles", workItemActWebhook},
	{"Replay", workItemActReplay},
	{"ConversationIndependentWhenOpen", separation(model.ConversationOpen)},
	{"ConversationIndependentWhenResolved", separation(model.ConversationResolved)},
	{"ConversationIndependentWhenClosed", separation(model.ConversationClosed)},
}

func ptrTo[T any](v T) *T { return &v }

func closeWith(reason string) model.WorkItemUpdate {
	u := model.WorkItemUpdate{Status: ptrTo(model.WorkItemClosed)}
	if reason != "" {
		u.StatusReason = ptrTo(reason)
	}
	return u
}

// workItemActClose: a close with reason is queued with the work item
// untouched, sent on the issue reference, and settles the work item to
// the provider's closed state with one command-triggered update.
func workItemActClose(t *testing.T, tr *tracker) {
	a, err := tr.act("op-close", tr.item.Revision, closeWith("not_planned"))
	if err != nil || a.Operation.State != model.Queued || a.Operation.Result == nil || *a.Operation.Result != tr.item.Ref.Ref() {
		t.Fatalf("Act = %+v, %v", a, err)
	}
	if got := tr.stored(); got.Revision != tr.item.Revision || got.Status != model.WorkItemOpen {
		t.Fatalf("work item changed at submission: %+v", got)
	}
	op := tr.settle(a.Operation.Ref)
	if op.State != model.Delivered || op.Failure != nil {
		t.Fatalf("operation = %+v", op)
	}
	reqs := tr.a.requests()
	if len(reqs) != 1 || reqs[0].Route != tr.issue("7") || reqs[0].Status != model.WorkItemClosed || reqs[0].Reason != "not_planned" {
		t.Fatalf("provider requests = %+v", reqs)
	}
	got := tr.stored()
	if got.Status != model.WorkItemClosed || got.RawStatus != "closed:not_planned" || got.Source.Revision != "act-1" || got.Subject != "Build fails" {
		t.Fatalf("work item = %+v", got)
	}
	ups := tr.commandUpdates()
	if len(ups) != 1 || ups[0].Operation == nil || *ups[0].Operation != a.Operation.Ref || ups[0].Status == nil || ups[0].Status.To != model.WorkItemClosed {
		t.Fatalf("command updates = %+v", ups)
	}
	if st := tr.conversationStatus(); st != model.ConversationOpen {
		t.Fatalf("closing the issue made the conversation %s", st)
	}
}

// workItemActReopen: a closed issue reopens with reason reopened.
func workItemActReopen(t *testing.T, tr *tracker) {
	a, err := tr.act("op-close", tr.item.Revision, closeWith(""))
	if err != nil {
		t.Fatal(err)
	}
	tr.settle(a.Operation.Ref)
	closed := tr.stored()
	if closed.Status != model.WorkItemClosed || closed.RawStatus != "closed:completed" {
		t.Fatalf("work item = %+v", closed)
	}
	a, err = tr.act("op-reopen", closed.Revision, model.WorkItemUpdate{Status: ptrTo(model.WorkItemOpen), StatusReason: ptrTo("reopened")})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if op := tr.settle(a.Operation.Ref); op.State != model.Delivered {
		t.Fatalf("operation = %+v", op)
	}
	if got := tr.stored(); got.Status != model.WorkItemOpen || got.RawStatus != "open:reopened" {
		t.Fatalf("work item = %+v", got)
	}
	if st := tr.conversationStatus(); st != model.ConversationOpen {
		t.Fatalf("conversation %s", st)
	}
}

// workItemActUndeclared: a reason or transition the adapter does not
// declare is unsupported at submission, before anything is queued.
func workItemActUndeclared(t *testing.T, tr *tracker) {
	for _, c := range []struct {
		name string
		u    model.WorkItemUpdate
	}{
		{"duplicate", closeWith("duplicate")},
		{"resolved", model.WorkItemUpdate{Status: ptrTo(model.WorkItemResolved)}},
		{"reopen open", model.WorkItemUpdate{Status: ptrTo(model.WorkItemOpen), StatusReason: ptrTo("reopened")}},
	} {
		_, err := tr.act(model.OperationID("op-"+c.name), tr.item.Revision, c.u)
		if !model.IsCode(err, model.Unsupported) {
			t.Fatalf("%s: err = %v, want unsupported", c.name, err)
		}
	}
	if got := tr.stored(); got.Revision != tr.item.Revision || len(tr.a.requests()) != 0 {
		t.Fatalf("an unsupported act changed something: %+v, %d requests", got, len(tr.a.requests()))
	}
}

// workItemActMixed: a provider-owned status with a local field (the
// tracker declares assignees local), or a reason without a status, is a
// validation error.
func workItemActMixed(t *testing.T, tr *tracker) {
	u := closeWith("completed")
	u.Assignees = &[]model.ExternalRef{tr.user("triager")}
	_, err := tr.act("op-mixed", tr.item.Revision, u)
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("mixed: err = %v, want validation", err)
	}
	_, err = tr.act("op-reason", tr.item.Revision, model.WorkItemUpdate{StatusReason: ptrTo("completed")})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("reason alone: err = %v, want validation", err)
	}
	if got := tr.stored(); got.Revision != tr.item.Revision || len(tr.a.requests()) != 0 {
		t.Fatalf("a refused act changed something: %+v", got)
	}
}

// workItemActRefused: a provider refusal settles the operation rejected and
// leaves the work item and conversation as they were.
func workItemActRefused(t *testing.T, tr *tracker) {
	tr.a.refuse(true)
	a, err := tr.act("op-close", tr.item.Revision, closeWith("completed"))
	if err != nil {
		t.Fatal(err)
	}
	op := tr.settle(a.Operation.Ref)
	if op.State != model.Rejected || op.Failure == nil {
		t.Fatalf("operation = %+v", op)
	}
	if got := tr.stored(); got.Revision != tr.item.Revision || got.Status != model.WorkItemOpen {
		t.Fatalf("work item = %+v, want it untouched", got)
	}
	if ups := tr.commandUpdates(); len(ups) != 0 {
		t.Fatalf("command updates = %+v", ups)
	}
	if st := tr.conversationStatus(); st != model.ConversationOpen {
		t.Fatalf("conversation %s", st)
	}
}

// workItemActWebhook: the provider's webhook of the change, arriving
// before the worker, updates the work item as an inbound observation but
// leaves the operation queued; the provider's answer then settles it
// without a second change.
func workItemActWebhook(t *testing.T, tr *tracker) {
	a, err := tr.act("op-close", tr.item.Revision, closeWith("completed"))
	if err != nil {
		t.Fatal(err)
	}
	at := tr.a.at.Add(time.Minute)
	tr.accept(model.Envelope{
		EventID: "e-closed", Destination: deskDest, Kind: model.EnvelopeWorkItem, SystemActor: true,
		References: []model.ExternalRef{tr.issue("7")}, OccurredAt: at, SourceRevision: "act-1",
		WorkItem: &model.WorkItemState{
			Subject: "Build fails", URL: "https://tracker.example/repo-1/7", Status: model.WorkItemClosed,
			RawStatus: "closed:completed", Requester: tr.user("reporter"), LocalFields: []string{model.WorkItemFieldAssignees},
		},
	})
	if op := tr.op(a.Operation.Ref); op.State != model.Queued {
		t.Fatalf("the webhook settled the operation: %+v", op)
	}
	observed := tr.stored()
	if observed.Status != model.WorkItemClosed {
		t.Fatalf("webhook not applied: %+v", observed)
	}
	if op := tr.settle(a.Operation.Ref); op.State != model.Delivered {
		t.Fatalf("operation = %+v", op)
	}
	if len(tr.a.requests()) != 1 {
		t.Fatal("the provider was not asked although the webhook showed the state")
	}
	if got := tr.stored(); got.Revision != observed.Revision {
		t.Fatalf("the answer of an already observed change rewrote the work item: %+v", got)
	}
	if st := tr.conversationStatus(); st != model.ConversationOpen {
		t.Fatalf("conversation %s", st)
	}
}

// workItemActReplay: the same operation ID and payload returns the stored
// operation, before and after it settled; another payload conflicts.
func workItemActReplay(t *testing.T, tr *tracker) {
	a, err := tr.act("op-close", tr.item.Revision, closeWith("not_planned"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := tr.act("op-close", tr.item.Revision, closeWith("not_planned"))
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	tr.settle(a.Operation.Ref)
	again, err = tr.act("op-close", tr.item.Revision, closeWith("not_planned"))
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref || again.Operation.State != model.Delivered {
		t.Fatalf("replay after settling = %+v, %v", again, err)
	}
	if _, err := tr.act("op-close", tr.item.Revision, closeWith("completed")); !model.IsCode(err, model.Conflict) {
		t.Fatalf("other payload: err = %v, want conflict", err)
	}
	if n := len(tr.a.requests()); n != 1 {
		t.Fatalf("provider requests = %d, want 1", n)
	}
}
