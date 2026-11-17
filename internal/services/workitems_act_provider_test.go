package services_test

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// newActSvc is newSvc over an adapter declaring work_item_act with the
// issue transitions (close: completed | not_planned, reopen: reopened).
func newActSvc(t *testing.T) *svc {
	t.Helper()
	p := &rulePolicy{}
	r := newRig(t, func(c *engine.Config, f *outFactory) {
		c.Authorizer = p
		f.WorkItemActs = true
	})
	return &svc{rig: r, pol: p, seq: portstest.NewSequenceIDs(0x5e1)}
}

// providerState is w's provider state with status and raw status set.
func providerState(w model.WorkItem, status, raw string) *model.WorkItemState {
	return &model.WorkItemState{
		Subject: w.Subject, URL: w.URL, Status: status, RawStatus: raw, Requester: w.Requester,
		Assignees: slices.Clone(w.Assignees), LocalFields: slices.Clone(w.LocalFields),
	}
}

// appliedAct answers an act as applied, reporting w's state moved to the
// requested status and reason under source revision rev at at.
func appliedAct(w model.WorkItem, rev string, at time.Time) enginetest.ActFunc {
	return func(_ context.Context, r adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
		raw := r.Status
		if r.Reason != "" {
			raw += ":" + r.Reason
		}
		return adapter.WorkItemActOutcome{
			Outcome: adapter.Outcome{State: model.Delivered}, State: providerState(w, r.Status, raw),
			SourceRevision: rev, OccurredAt: at,
		}, nil
	}
}

func rejectedAct(context.Context, adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	return adapter.WorkItemActOutcome{Outcome: adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Validation, "provider refused")}}, nil
}

func closeUpdate(reason string) model.WorkItemUpdate {
	u := model.WorkItemUpdate{Status: ptr(model.WorkItemClosed)}
	if reason != "" {
		u.StatusReason = ptr(reason)
	}
	return u
}

func (s *svc) workItemEvents() []model.Event {
	var out []model.Event
	for _, ev := range s.Events() {
		if ev.Kind == model.EventWorkItemUpdated {
			out = append(out, ev)
		}
	}
	return out
}

func (s *svc) conversationStatus(ref model.ConversationRef) string {
	s.T.Helper()
	c, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: ref})
	if err != nil {
		s.T.Fatal(err)
	}
	return c.Status
}

// A provider-owned status is queued as an outbound operation: authorized
// as work_item.act, its result the work item, nothing changed locally and
// nothing sent until a worker runs it.
func TestActProviderStatusQueuesOperation(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	events := len(s.Events())
	a, err := s.act(w, "op-close", w.Revision, closeUpdate("not_planned"))
	if err != nil || a.Operation.State != model.Queued || a.Replayed || a.Operation.Result == nil || *a.Operation.Result != w.Ref.Ref() {
		t.Fatalf("Act = %+v, %v", a, err)
	}
	if got := s.stored(w.Ref); got.Revision != w.Revision || got.Status != model.WorkItemOpen {
		t.Fatalf("work item changed at claim: %+v", got)
	}
	if len(s.Events()) != events {
		t.Fatal("events emitted at claim")
	}
	seen := s.pol.seen()
	if last := seen[len(seen)-1]; last.Action != model.ActionWorkItemAct || last.Resource != w.Ref.Ref() || last.Revision != w.Revision {
		t.Fatalf("policy saw %+v, want work_item.act on the work item at its revision", last)
	}
	if len(s.F.Acts()) != 0 {
		t.Fatal("the provider was called at submit")
	}
}

// A worker sends the act along the work item's external reference; the
// provider's answer applies its state to the work item with a
// command-triggered work_item.updated naming the operation, and the
// operation's delivery.updated.
func TestActAppliedUpdatesStatusAndEmits(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	at := s.Clock.Now().UTC().Add(time.Minute)
	s.F.RespondActs(appliedAct(w, "src-2", at))
	a, err := s.act(w, "op-close", w.Revision, closeUpdate("not_planned"))
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Step("w"); n != 1 {
		t.Fatalf("stepped %d items", n)
	}
	acts := s.F.Acts()
	if len(acts) != 1 || acts[0].Route != w.External || acts[0].Target != nil || acts[0].Status != model.WorkItemClosed ||
		acts[0].Reason != "not_planned" || acts[0].Operation != a.Operation.Ref {
		t.Fatalf("provider acts = %+v", acts)
	}
	op := s.Op(a.Operation.Ref)
	enginetest.RequireState(t, op, model.Delivered, "")
	got := s.stored(w.Ref)
	if got.Status != model.WorkItemClosed || got.RawStatus != "closed:not_planned" || got.Source.Revision != "src-2" ||
		!got.Source.OccurredAt.Equal(at) || got.Revision == w.Revision {
		t.Fatalf("work item = %+v", got)
	}
	evs := s.workItemEvents()
	if len(evs) != 1 {
		t.Fatalf("work_item.updated events = %d, want 1", len(evs))
	}
	var p model.WorkItemUpdatedPayload
	if err := json.Unmarshal(evs[0].Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if evs[0].Actor != actor || evs[0].Revision != got.Revision || p.Trigger != model.TriggerCommand || p.Operation == nil ||
		*p.Operation != a.Operation.Ref || p.SourceRevision != "src-2" || p.PreviousRevision != w.Revision ||
		!slices.Equal(p.Changes, []string{model.WorkItemFieldStatus}) || p.Status == nil ||
		*p.Status != (model.WorkItemStatusChange{From: model.WorkItemOpen, To: model.WorkItemClosed, RawFrom: w.RawStatus, RawTo: "closed:not_planned"}) {
		t.Fatalf("event %+v payload %+v", evs[0], p)
	}
	if !slices.ContainsFunc(s.Events(), func(ev model.Event) bool {
		return ev.Kind == model.EventDeliveryUpdated && ev.Resource == a.Operation.Ref.Ref()
	}) {
		t.Fatal("no delivery.updated for the operation")
	}
}

// A refused act settles the operation and leaves the work item as it was.
func TestActRejectedLeavesStatus(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	s.F.RespondActs(rejectedAct)
	a, err := s.act(w, "op-close", w.Revision, closeUpdate(""))
	if err != nil {
		t.Fatal(err)
	}
	s.Step("w")
	enginetest.RequireState(t, s.Op(a.Operation.Ref), model.Rejected, model.Validation)
	if got := s.stored(w.Ref); got.Revision != w.Revision || got.Status != model.WorkItemOpen || got.RawStatus != w.RawStatus {
		t.Fatalf("work item = %+v, want it untouched", got)
	}
	if n := len(s.workItemEvents()); n != 0 {
		t.Fatalf("work_item.updated events = %d", n)
	}
}

// An applied answer without the provider's state, or with a state older
// than the one applied since, changes nothing: inbound observations stay
// the source of order.
func TestActAppliedStateInSourceOrder(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	a, err := s.act(w, "op-bare", w.Revision, closeUpdate(""))
	if err != nil {
		t.Fatal(err)
	}
	s.Step("w")
	enginetest.RequireState(t, s.Op(a.Operation.Ref), model.Delivered, "")
	if got := s.stored(w.Ref); got.Revision != w.Revision {
		t.Fatalf("applied without state changed the work item: %+v", got)
	}

	newer := s.Clock.Now().UTC().Add(time.Hour)
	w.Source, w.Revision = model.WorkItemSource{Revision: "src-9", OccurredAt: newer}, "w2"
	s.put(func(tx store.Tx) error { return tx.PutWorkItem(s.Ctx, w, "w1") })
	s.F.RespondActs(appliedAct(w, "src-3", newer.Add(-time.Minute)))
	if _, err := s.act(w, "op-stale", w.Revision, closeUpdate("completed")); err != nil {
		t.Fatal(err)
	}
	s.Step("w")
	if got := s.stored(w.Ref); got.Revision != w.Revision || got.Status != model.WorkItemOpen {
		t.Fatalf("a stale provider answer overwrote a newer observation: %+v", got)
	}
}

// A provider success that returns no state leaves the work item to the
// provider's own webhook: even on a channel declaring refetch, settling
// queues no refetch and reads nothing back, since no principal exists at
// settle time to run one.
func TestActAppliedWithoutStateQueuesNoRefetch(t *testing.T) {
	p := &rulePolicy{}
	r := newRig(t, func(c *engine.Config, f *outFactory) {
		c.Authorizer = p
		f.WorkItemActs, f.Refetches = true, true
	})
	s := &svc{rig: r, pol: p, seq: portstest.NewSequenceIDs(0x5e1)}
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	a, err := s.act(w, "op-bare", w.Revision, closeUpdate(""))
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Step("w"); n != 1 {
		t.Fatalf("stepped %d items", n)
	}
	enginetest.RequireState(t, s.Op(a.Operation.Ref), model.Delivered, "")
	if n := s.Step("w"); n != 0 {
		t.Fatalf("settling queued %d more work items, want none", n)
	}
	if slices.Contains(s.F.Calls(), "refetch") {
		t.Fatalf("provider calls %v include a refetch", s.F.Calls())
	}
	if got := s.stored(w.Ref); got.Revision != w.Revision || got.Status != model.WorkItemOpen {
		t.Fatalf("work item changed without the provider's webhook: %+v", got)
	}
}

// No path of a work item act changes its conversation's status: not the
// claim, not the settled close, not a failed one.
func TestActNeverTransitionsConversation(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	conv := w.Ref.Conversation
	s.F.RespondActs(appliedAct(w, "src-2", s.Clock.Now().UTC().Add(time.Minute)))
	if _, err := s.act(w, "op-close", w.Revision, closeUpdate("completed")); err != nil {
		t.Fatal(err)
	}
	if st := s.conversationStatus(conv); st != model.ConversationOpen {
		t.Fatalf("conversation %s after claim", st)
	}
	s.Step("w")
	if got := s.stored(w.Ref); got.Status != model.WorkItemClosed {
		t.Fatalf("work item not closed: %+v", got)
	}
	if st := s.conversationStatus(conv); st != model.ConversationOpen {
		t.Fatalf("closing the issue made the conversation %s", st)
	}
	for _, ev := range s.Events() {
		if ev.Kind == model.EventConversationChanged || ev.Resource == conv.Ref() {
			t.Fatalf("conversation event %s", ev.Kind)
		}
	}
}

// The same operation ID and payload returns the stored operation, also
// once the work item moved on; another payload conflicts.
func TestActReplaySamePayload(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	s.F.RespondActs(appliedAct(w, "src-2", s.Clock.Now().UTC().Add(time.Minute)))
	a, err := s.act(w, "op-close", w.Revision, closeUpdate("not_planned"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.act(w, "op-close", w.Revision, closeUpdate("not_planned"))
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay before execution = %+v, %v", again, err)
	}
	s.Step("w")
	if got := s.stored(w.Ref); got.Status != model.WorkItemClosed {
		t.Fatalf("work item = %+v", got)
	}
	again, err = s.act(w, "op-close", w.Revision, closeUpdate("not_planned"))
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref || again.Operation.State != model.Delivered {
		t.Fatalf("replay after the work item moved on = %+v, %v", again, err)
	}
	_, err = s.act(w, "op-close", w.Revision, closeUpdate("completed"))
	requireCode(t, err, model.Conflict, "different payload")
	if n := len(s.F.Acts()); n != 1 {
		t.Fatalf("provider acts = %d, want 1", n)
	}
}

// A closed provider status reopens where the adapter declares it.
func TestActReopenFromClosedWhenDeclared(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemClosed)
	s.F.RespondActs(appliedAct(w, "src-2", s.Clock.Now().UTC().Add(time.Minute)))
	a, err := s.act(w, "op-reopen", w.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemOpen), StatusReason: ptr("reopened")})
	if err != nil || a.Operation.State != model.Queued {
		t.Fatalf("reopen = %+v, %v", a, err)
	}
	s.Step("w")
	if got := s.stored(w.Ref); got.Status != model.WorkItemOpen || got.RawStatus != "open:reopened" {
		t.Fatalf("work item = %+v", got)
	}
}

// Undeclared transitions and reasons, and channels without work_item_act,
// are refused as unsupported before any claim or policy check; a stale
// revision conflicts before anything is queued.
func TestActWithoutCapabilityUnsupported(t *testing.T) {
	s := newActSvc(t)
	open := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	closed := s.workItem(s.Conv.Ref.Inbox, model.WorkItemClosed)
	s.pol.set(nil)
	for _, c := range []struct {
		name string
		w    model.WorkItem
		u    model.WorkItemUpdate
		want string
	}{
		{"duplicate reason", open, closeUpdate("duplicate"), "status reason"},
		{"reopened reason on close", open, closeUpdate("reopened"), "status reason"},
		{"resolved", open, model.WorkItemUpdate{Status: ptr(model.WorkItemResolved)}, "no work item transition"},
		{"pending", open, model.WorkItemUpdate{Status: ptr(model.WorkItemPending)}, "no work item transition"},
		{"close closed", closed, closeUpdate(""), "no work item transition"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.act(c.w, model.OperationID("op-"+c.name), c.w.Revision, c.u)
			requireCode(t, err, model.Unsupported, c.want)
		})
	}
	if len(s.pol.seen()) != 0 {
		t.Fatalf("policy consulted for unsupported acts: %+v", s.pol.seen())
	}
	_, err := s.act(open, "op-stale", "w0", closeUpdate(""))
	requireCode(t, err, model.Conflict, "revision changed")
	if s.Step("w") != 0 || len(s.F.Acts()) != 0 {
		t.Fatal("a refused act was queued")
	}

	plain := newSvc(t)
	w := plain.workItem(plain.Conv.Ref.Inbox, model.WorkItemOpen)
	_, err = plain.act(w, "op-close", w.Revision, closeUpdate(""))
	requireCode(t, err, model.Unsupported, "does not declare work_item_act")
}

// A local field still commits locally on a channel declaring
// work_item_act, and never reaches the provider.
func TestActLocalFieldStillCommitsLocally(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen, model.WorkItemFieldStatus)
	a, err := s.act(w, "op-local", w.Revision, model.WorkItemUpdate{Status: ptr(model.WorkItemClosed)})
	if err != nil || a.Operation.State != model.Committed {
		t.Fatalf("Act = %+v, %v", a, err)
	}
	if got := s.stored(w.Ref); got.Status != model.WorkItemClosed {
		t.Fatalf("work item = %+v", got)
	}
	if s.Step("w") != 0 || len(s.F.Acts()) != 0 {
		t.Fatal("a local change reached the provider")
	}
}

// The originating actor is reauthorized before each attempt: a revoked
// permission fails the operation without a provider call.
func TestActRechecksAuthorization(t *testing.T) {
	s := newActSvc(t)
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	a, err := s.act(w, "op-close", w.Revision, closeUpdate(""))
	if err != nil {
		t.Fatal(err)
	}
	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionWorkItemAct })
	s.Step("w")
	enginetest.RequireState(t, s.Op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if len(s.F.Acts()) != 0 {
		t.Fatal("the provider was called after the permission was revoked")
	}
	if got := s.stored(w.Ref); got.Revision != w.Revision {
		t.Fatalf("work item = %+v", got)
	}
}

// A lost act answer is reconciled with the act it requested: Reconcile
// receives the attempt's status and reason, so an adapter can compare the
// provider's work item with them.
func TestActReconcileReceivesRequestedAct(t *testing.T) {
	p := &rulePolicy{}
	r := newRig(t, func(c *engine.Config, f *outFactory) {
		c.Authorizer = p
		f.WorkItemActs, f.Reconciler = true, true
	})
	s := &svc{rig: r, pol: p, seq: portstest.NewSequenceIDs(0x5e1)}
	w := s.workItem(s.Conv.Ref.Inbox, model.WorkItemOpen)
	s.F.RespondActs(func(context.Context, adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
		return adapter.WorkItemActOutcome{}, model.NewError(model.OutcomeUnknown, "answer lost")
	})
	s.F.ReconcileFn = func(adapter.ReconcileRequest) (adapter.Outcome, error) {
		return adapter.Outcome{State: model.Delivered}, nil
	}
	a, err := s.act(w, "op-close", w.Revision, closeUpdate("not_planned"))
	if err != nil {
		t.Fatal(err)
	}
	s.Step("w")
	enginetest.RequireState(t, s.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	s.Clock.Advance(time.Minute)
	s.Step("w")
	enginetest.RequireState(t, s.Op(a.Operation.Ref), model.Delivered, "")
	rc, acts := s.F.Reconciled(), s.F.Acts()
	if len(rc) != 1 || len(acts) != 1 || rc[0].Attempt != acts[0].Attempt || rc[0].Route != w.External ||
		rc[0].Status != model.WorkItemClosed || rc[0].Reason != "not_planned" {
		t.Fatalf("reconcile requests %+v, want the attempt's close with reason not_planned", rc)
	}
	if got := s.stored(w.Ref); got.Revision != w.Revision {
		t.Fatalf("reconciled without state changed the work item: %+v", got)
	}
}
