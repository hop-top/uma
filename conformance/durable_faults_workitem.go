package conformance

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// workItemFault is a faultEnv with a work item acting channel: an issue the
// engine last saw open an hour ago, in a conversation of its own.
type workItemFault struct {
	*faultEnv
	item model.WorkItem
}

func newWorkItemFault(t *testing.T, e *env) *workItemFault {
	t.Helper()
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	f.p.workItemActs, f.p.now = true, f.fx.Now
	route := model.ExternalRef{Channel: f.ch, Namespace: faultNS, Kind: model.RefKindWorkItem, ID: "7", ParentID: "desk"}
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: f.inbox, ID: e.id(t)},
		Mode: model.ModePerWorkItem, Status: model.ConversationOpen, ReplyRoute: route, CreatedAt: f.fx.Now(), Revision: "cv1",
	}
	item := model.WorkItem{
		Ref:      model.WorkItemRef{Conversation: conv.Ref, ID: e.id(t)},
		External: route, Subject: "Build fails", URL: "https://faults.example/issues/7", Status: model.WorkItemOpen, RawStatus: "open",
		Binding: model.WorkItemBindingActive, Source: model.WorkItemSource{Revision: "seed", OccurredAt: f.fx.Now().Add(-time.Hour)},
		Revision: "wi1",
	}
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.PutConversation(e.ctx, conv, ""); err != nil {
			return err
		}
		return tx.PutWorkItem(e.ctx, item, "")
	})
	return &workItemFault{faultEnv: f, item: item}
}

// closeIssue queues closing the issue under operation opID.
func (p *faultProc) closeIssue(t *testing.T, w *workItemFault, opID model.OperationID) model.OperationRef {
	t.Helper()
	status, reason := model.WorkItemClosed, "not_planned"
	data, err := json.Marshal(model.WorkItemUpdate{Status: &status, StatusReason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	a, err := p.client.WorkItems().Act(p.ctx, model.WorkItemAction{
		Command: model.Command{Call: model.Call{Correlation: "c-" + model.CorrelationID(opID)}, Operation: opID, ExpectedRevision: w.item.Revision},
		Target:  w.item.Ref, Action: model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: data},
	})
	if err != nil {
		t.Fatalf("WorkItems.Act: %v", err)
	}
	return a.Operation.Ref
}

func (p *faultProc) workItem(t *testing.T, w *workItemFault) model.WorkItem {
	t.Helper()
	item, err := p.client.WorkItems().Get(p.ctx, model.Lookup[model.WorkItemRef]{Call: model.Call{Correlation: "get"}, Ref: w.item.Ref})
	if err != nil {
		t.Fatalf("WorkItems.Get: %v", err)
	}
	return item
}

// commandUpdates counts the command-triggered work_item.updated events of
// the work item, naming the operations they carry.
func (f *workItemFault) commandUpdates(t *testing.T) []model.OperationRef {
	t.Helper()
	var out []model.OperationRef
	for _, ev := range f.allEvents(t, model.Filter{Kinds: []string{model.EventWorkItemUpdated}}, 50) {
		var pl model.WorkItemUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
			t.Fatal(err)
		}
		if pl.Trigger == model.TriggerCommand && pl.Operation != nil {
			out = append(out, *pl.Operation)
		}
	}
	return out
}

// wantClosedOnce requires the work item closed by op alone: the provider
// asked once, one command update, one delivery state, the conversation
// still open.
func (f *workItemFault) wantClosedOnce(t *testing.T, p *faultProc, op model.OperationRef) {
	t.Helper()
	wantOpState(t, p.op(t, op), model.Delivered, "")
	got := p.workItem(t, f)
	if got.Status != model.WorkItemClosed || got.RawStatus != "closed:not_planned" {
		t.Fatalf("work item = %+v, want it closed as not planned", got)
	}
	acts := f.p.acts()
	if len(acts) != 1 {
		t.Fatalf("provider acts = %d, want the one close", len(acts))
	}
	if a := acts[0]; a.Status != model.WorkItemClosed || a.Reason != "not_planned" || a.Route != f.item.External {
		t.Fatalf("provider act = %+v, want the close as not planned on the issue", a)
	}
	if ups := f.commandUpdates(t); !slices.Equal(ups, []model.OperationRef{op}) {
		t.Fatalf("command updates = %v, want one naming %v", ups, op)
	}
	if evs := f.deliveries(t, op); len(evs) != 1 {
		t.Fatalf("delivery events = %d, want one for the one settled state", len(evs))
	}
	c, err := p.client.Conversations().Get(p.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "get"}, Ref: f.item.Ref.Conversation})
	if err != nil || c.Status != model.ConversationOpen {
		t.Fatalf("conversation = %+v, %v; want it open", c, err)
	}
}

// The close is queued and the process dies before any worker ran. The next
// process runs it: the provider is asked once, and the work item takes the
// provider's state once.
func workItemActResumesAfterCrash(t *testing.T, e *env, cross func(*testing.T)) {
	f := newWorkItemFault(t, e)
	p := f.start(t)
	op := p.closeIssue(t, f, "op-1")
	wantOpState(t, p.op(t, op), model.Queued, "")
	p = f.restart(t, cross)
	if got := p.workItem(t, f); got.Revision != f.item.Revision {
		t.Fatalf("work item = %+v after a restart, want it untouched until the provider answered", got)
	}
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	f.wantClosedOnce(t, p, op)
	if calls := f.p.log(); !slices.Equal(calls, []string{"act"}) {
		t.Fatalf("provider calls = %v, want the one act", calls)
	}
}

// The attempt is recorded, then the process dies before calling the
// provider. The next owner reconciles first: the provider holds no record
// of the act, so the one act happens then.
func workItemActCrashAfterAttemptRecord(t *testing.T, e *env, cross func(*testing.T)) {
	f := newWorkItemFault(t, e)
	p := f.start(t)
	p.store.at(afterCommit, (*unitLog).begins, p.store.crash)
	op := p.closeIssue(t, f, "op-1")
	p.work()
	p.crashed(t)
	if n := len(f.p.acts()); n != 0 {
		t.Fatalf("provider acts = %d, want none before the crash", n)
	}
	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	f.wantClosedOnce(t, p, op)
	if calls := f.p.log(); !slices.Equal(calls, []string{"reconcile", "act"}) {
		t.Fatalf("provider calls = %v, want reconcile then the one act", calls)
	}
}

// The provider applies the close and the process dies before its answer is
// recorded. The next owner reconciles and settles from the provider's
// record: no second act, and the work item still shows the open state it
// last saw, since a settlement found by reconciliation carries no provider
// state: the provider's own webhook of the change brings it.
func workItemActCrashAfterProviderCall(t *testing.T, e *env, cross func(*testing.T)) {
	f := newWorkItemFault(t, e)
	p := f.start(t)
	p.store.at(beforeCommit, (*unitLog).answers, p.store.crash)
	op := p.closeIssue(t, f, "op-1")
	p.work()
	p.crashed(t)
	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	wantOpState(t, p.op(t, op), model.Delivered, "")
	if calls := f.p.log(); !slices.Equal(calls, []string{"act", "reconcile"}) {
		t.Fatalf("provider calls = %v, want the one act, then reconcile", calls)
	}
	if got := p.workItem(t, f); got.Status != model.WorkItemOpen || got.Revision != f.item.Revision {
		t.Fatalf("work item = %+v, want it unchanged: no provider state was recorded", got)
	}
	if ups := f.commandUpdates(t); len(ups) != 0 {
		t.Fatalf("command updates = %v, want none", ups)
	}
	if evs := f.deliveries(t, op); len(evs) != 1 {
		t.Fatalf("delivery events = %d, want one for the one settled state", len(evs))
	}
}
