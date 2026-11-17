package conformance

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// workItemStatuses are the normalized work item statuses.
var workItemStatuses = []string{model.WorkItemOpen, model.WorkItemPending, model.WorkItemResolved, model.WorkItemClosed}

// undeclaredReason is a well-formed status reason no adapter declares.
const undeclaredReason = "conformance_undeclared"

// workItemSeedAge is how long before the engine's clock a seeded work item
// was last observed, so the provider's answer, stamped at the clock or
// later, is newer than the stored copy.
const workItemSeedAge = time.Hour

// workItemInbox seeds channel A with a per-work-item inbox, once.
func (e *adapterEnv) workItemInbox(t *testing.T) {
	t.Helper()
	if e.items == 0 {
		e.seed(t, adapterChanA, adapterInboxA, model.Selection{Mode: model.ModePerWorkItem}, "support")
	}
}

// workItem stores an open conversation bound to a work item on route that
// the engine last observed in status.
func (e *adapterEnv) workItem(t *testing.T, route model.ExternalRef, status string) model.WorkItem {
	t.Helper()
	e.workItemInbox(t)
	e.items++
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: adapterInboxA, ID: model.ID(fmt.Sprintf("00000ad0-0000-7000-8000-0000000c%04x", e.items))},
		Mode: model.ModePerWorkItem, Status: model.ConversationOpen, ReplyRoute: route, CreatedAt: e.clock.Now(), Revision: "cv1",
	}
	w := model.WorkItem{
		Ref:      model.WorkItemRef{Conversation: conv.Ref, ID: model.ID(fmt.Sprintf("00000ad0-0000-7000-8000-0000000d%04x", e.items))},
		External: route, Subject: "conformance work item", URL: "https://conformance.example/work/" + route.ID,
		Status: status, RawStatus: status, Binding: model.WorkItemBindingActive,
		Source:   model.WorkItemSource{Revision: "seed", OccurredAt: e.clock.Now().Add(-workItemSeedAge)},
		Revision: "wi1",
	}
	err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error {
		if err := tx.PutConversation(e.ctx, conv, ""); err != nil {
			return err
		}
		return tx.PutWorkItem(e.ctx, w, "")
	})
	if err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	return w
}

// seededWorkItem creates the provider's work item for peer in status and
// stores the engine's copy, observed in localStatus. It skips the case when
// the provider has no work items or cannot hold the status.
func (e *adapterEnv) seededWorkItem(t *testing.T, peer, status, localStatus string) (model.WorkItem, WorkItemProvider) {
	t.Helper()
	wp, ok := e.provider.(WorkItemProvider)
	if !ok {
		t.Skip("provider does not implement WorkItemProvider")
	}
	ref, err := wp.SeedWorkItem(adapterChanA, peer, status)
	if errors.Is(err, ErrNotExpressible) {
		t.Skipf("provider cannot hold a work item in status %s", status)
	}
	if err != nil {
		t.Fatalf("SeedWorkItem(%s, %s): %v", peer, status, err)
	}
	return e.workItem(t, ref, localStatus), wp
}

// actUpdate is the work item update asking for status with reason, empty
// for the adapter's default.
func actUpdate(status, reason string) model.WorkItemUpdate {
	u := model.WorkItemUpdate{Status: &status}
	if reason != "" {
		u.StatusReason = &reason
	}
	return u
}

// act submits update u on w under operation op, at w's revision.
func (e *adapterEnv) act(t *testing.T, w model.WorkItem, op model.OperationID, u model.WorkItemUpdate) (model.Acceptance, error) {
	t.Helper()
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	return e.client.WorkItems().Act(e.ctx, model.WorkItemAction{
		Command: model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(op))}, Operation: op, ExpectedRevision: w.Revision},
		Target:  w.Ref, Action: model.Extension{Name: model.SchemaWorkItemUpdate, Version: 1, Data: data},
	})
}

// mustAct submits an act that must be queued, with the work item untouched.
func (e *adapterEnv) mustAct(t *testing.T, w model.WorkItem, op model.OperationID, status, reason string) model.OperationRef {
	t.Helper()
	a, err := e.act(t, w, op, actUpdate(status, reason))
	if err != nil {
		t.Fatalf("WorkItems.Act(%s, reason %q): %v", status, reason, err)
	}
	if a.Operation.State != model.Queued {
		t.Fatalf("Act state = %q, want queued", a.Operation.State)
	}
	if got := e.storedWorkItem(t, w.Ref); got.Revision != w.Revision {
		t.Fatalf("work item changed at submission: %+v", got)
	}
	return a.Operation.Ref
}

func (e *adapterEnv) storedWorkItem(t *testing.T, ref model.WorkItemRef) model.WorkItem {
	t.Helper()
	w, err := e.client.WorkItems().Get(e.ctx, model.Lookup[model.WorkItemRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: ref})
	if err != nil {
		t.Fatalf("WorkItems.Get: %v", err)
	}
	return w
}

// actCalls returns the work item acts that reached channel A's provider on
// route.
func (e *adapterEnv) actCalls(route model.ExternalRef) []ProviderCall {
	var out []ProviderCall
	for _, c := range e.calls(adapterChanA, model.CapabilityWorkItemAct) {
		if c.Route == route {
			out = append(out, c)
		}
	}
	return out
}

// wantActApplied requires op to be settled as applied by the provider.
func wantActApplied(t *testing.T, op model.Operation) {
	t.Helper()
	if (op.State != model.Delivered && op.State != model.ProviderAccepted) || op.Failure != nil {
		t.Fatalf("operation = %s (failure %v), want it applied", op.State, op.Failure)
	}
}

// wantProviderStatus requires the provider to hold status for work.
func wantProviderStatus(t *testing.T, wp WorkItemProvider, work model.ExternalRef, status string) {
	t.Helper()
	got, err := wp.WorkItemStatus(adapterChanA, work)
	if err != nil {
		t.Fatalf("WorkItemStatus: %v", err)
	}
	if got != status {
		t.Fatalf("provider holds status %q, want %q", got, status)
	}
}

// wantUntouched requires the engine's copy of w and its conversation to be
// as stored.
func (e *adapterEnv) wantUntouched(t *testing.T, w model.WorkItem) {
	t.Helper()
	if got := e.storedWorkItem(t, w.Ref); got.Revision != w.Revision || got.Status != w.Status {
		t.Fatalf("work item = %+v, want it untouched", got)
	}
	e.wantConversationOpen(t, w)
}

// wantConversationOpen requires w's conversation to be open: a work item
// act never changes it.
func (e *adapterEnv) wantConversationOpen(t *testing.T, w model.WorkItem) {
	t.Helper()
	c, err := e.client.Conversations().Get(e.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: w.Ref.Conversation})
	if err != nil {
		t.Fatalf("Conversations.Get: %v", err)
	}
	if c.Status != model.ConversationOpen {
		t.Fatalf("conversation status = %q after a work item act, want open", c.Status)
	}
}

// declares reports whether d declares moving a work item from status from
// to status to with reason, empty for the default.
func declares(d adapter.Descriptor, from, to, reason string) bool {
	return slices.ContainsFunc(d.WorkItemActs, func(tr adapter.WorkItemTransition) bool {
		return tr.To == to && slices.Contains(tr.From, from) && (reason == "" || slices.Contains(tr.Reasons, reason))
	})
}

// declaredAct returns a declared transition and a source status of it the
// provider can hold a work item in, or skips the case.
func (e *adapterEnv) declaredAct(t *testing.T) (adapter.WorkItemTransition, string, WorkItemProvider) {
	t.Helper()
	wp, ok := e.provider.(WorkItemProvider)
	if !ok {
		t.Skip("provider does not implement WorkItemProvider")
	}
	for _, tr := range e.desc.WorkItemActs {
		for _, from := range tr.From {
			if _, err := wp.SeedWorkItem(adapterChanA, "probe", from); err == nil {
				return tr, from, wp
			}
		}
	}
	t.Skip("provider cannot hold a work item at any declared source status")
	return adapter.WorkItemTransition{}, "", nil
}

// firstAct returns a declared transition with a seeded work item at its
// source status.
func (e *adapterEnv) firstAct(t *testing.T, peer string) (adapter.WorkItemTransition, model.WorkItem, WorkItemProvider) {
	t.Helper()
	tr, from, _ := e.declaredAct(t)
	w, wp := e.seededWorkItem(t, peer, from, from)
	return tr, w, wp
}

var adapterWorkItemActCases = []adapterCase{
	{name: "DeclaredTransitionsHonoured", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		wp, ok := e.provider.(WorkItemProvider)
		if !ok {
			t.Skip("provider does not implement WorkItemProvider")
		}
		type queued struct {
			name   string
			tr     adapter.WorkItemTransition
			reason string
			w      model.WorkItem
			op     model.OperationRef
		}
		var acts []queued
		for _, tr := range e.desc.WorkItemActs {
			for _, from := range tr.From {
				for _, reason := range slices.Concat([]string{""}, tr.Reasons) {
					n := len(acts) + 1
					ref, err := wp.SeedWorkItem(adapterChanA, fmt.Sprintf("act-%d", n), from)
					if errors.Is(err, ErrNotExpressible) {
						continue
					}
					if err != nil {
						t.Fatalf("SeedWorkItem: %v", err)
					}
					w := e.workItem(t, ref, from)
					op := e.mustAct(t, w, model.OperationID(fmt.Sprintf("op-act-%d", n)), tr.To, reason)
					acts = append(acts, queued{fmt.Sprintf("%s to %s reason %q", from, tr.To, reason), tr, reason, w, op})
				}
			}
		}
		if len(acts) == 0 {
			t.Skip("provider cannot hold a work item at any declared source status")
		}
		e.start(t)
		e.stop(t)
		for _, a := range acts {
			t.Run(a.name, func(t *testing.T) {
				wantActApplied(t, e.op(t, a.op))
				calls := e.actCalls(a.w.External)
				if len(calls) != 1 || !calls[0].Applied || calls[0].Status != a.tr.To {
					t.Fatalf("provider acts = %+v, want one applied act to %s", calls, a.tr.To)
				}
				if got := calls[0].Reason; (a.reason != "" && got != a.reason) || (a.reason == "" && got != "" && !slices.Contains(a.tr.Reasons, got)) {
					t.Fatalf("act carried reason %q, want %q or a declared default", got, a.reason)
				}
				wantProviderStatus(t, wp, a.w.External, a.tr.To)
				if got := e.storedWorkItem(t, a.w.Ref); got.Revision != a.w.Revision && got.Status != a.tr.To {
					t.Fatalf("work item status = %q, want %q: it changes only to the provider's state", got.Status, a.tr.To)
				}
				e.wantConversationOpen(t, a.w)
			})
		}
	}},
	{name: "UndeclaredIsUnsupportedBeforeAnyCall", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		var tried int
		for i, from := range workItemStatuses {
			w := e.workItem(t, e.route(adapterChanA, fmt.Sprintf("undeclared-%d", i)), from)
			for _, to := range workItemStatuses {
				for _, reason := range []string{"", undeclaredReason} {
					if declares(e.desc, from, to, reason) {
						continue
					}
					tried++
					op := model.OperationID(fmt.Sprintf("op-undeclared-%s-%s-%s", from, to, reason))
					if _, err := e.act(t, w, op, actUpdate(to, reason)); model.CodeOf(err) != model.Unsupported {
						t.Fatalf("Act %s to %s reason %q = %v, want unsupported", from, to, reason, err)
					}
				}
			}
			e.wantUntouched(t, w)
		}
		if tried == 0 {
			t.Fatal("every status, source and reason was declared")
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
	{name: "ClosedReopensOnlyWhereDeclared", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		if !declares(e.desc, model.WorkItemClosed, model.WorkItemOpen, "") {
			w := e.workItem(t, e.route(adapterChanA, "closed-1"), model.WorkItemClosed)
			if _, err := e.act(t, w, "op-reopen", actUpdate(model.WorkItemOpen, "")); model.CodeOf(err) != model.Unsupported {
				t.Fatalf("reopening a closed work item the adapter declares no transition for = %v, want unsupported", err)
			}
			if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
				t.Fatalf("provider calls = %+v, want none", calls)
			}
			return
		}
		w, wp := e.seededWorkItem(t, "closed-1", model.WorkItemClosed, model.WorkItemClosed)
		ref := e.mustAct(t, w, "op-reopen", model.WorkItemOpen, "")
		e.start(t)
		e.stop(t)
		wantActApplied(t, e.op(t, ref))
		wantProviderStatus(t, wp, w.External, model.WorkItemOpen)
		e.wantConversationOpen(t, w)
	}},
	{name: "StaleLocalStatusIsStillSent", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		tr, from, _ := e.declaredAct(t)
		// The provider already holds the target; the engine's copy is behind.
		w, wp := e.seededWorkItem(t, "stale-1", tr.To, from)
		ref := e.mustAct(t, w, "op-stale", tr.To, "")
		e.start(t)
		e.stop(t)
		wantActApplied(t, e.op(t, ref))
		if calls := e.actCalls(w.External); len(calls) != 1 || !calls[0].Applied {
			t.Fatalf("provider acts = %+v, want the one act sent although the provider showed the state", calls)
		}
		wantProviderStatus(t, wp, w.External, tr.To)
	}},
	{name: "RejectedActIsFinal", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		tr, w, wp := e.firstAct(t, "rejected-1")
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRejected})
		ref := e.mustAct(t, w, "op-rejected", tr.To, "")
		e.start(t)
		e.later(t, time.Hour)
		e.stop(t)
		op := e.op(t, ref)
		if op.State != model.Rejected || op.Failure == nil || op.Failure.Retry != model.NoRetry {
			t.Fatalf("operation = %+v, want rejected without retry", op)
		}
		if n := len(e.actCalls(w.External)); n != 1 {
			t.Fatalf("provider acts = %d, want 1", n)
		}
		wantProviderStatus(t, wp, w.External, w.Status)
		e.wantUntouched(t, w)
	}},
	{name: "TransientActIsRetried", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		tr, w, wp := e.firstAct(t, "transient-1")
		e.respond(t, adapterChanA, Response{Outcome: OutcomeTransient}, Response{Outcome: OutcomeAccepted})
		ref := e.mustAct(t, w, "op-transient", tr.To, "")
		e.start(t)
		wantOp(t, e.op(t, ref), model.RetryPending, model.Transient)
		e.wantUntouched(t, w)
		e.later(t, time.Hour)
		e.stop(t)
		wantActApplied(t, e.op(t, ref))
		calls := e.actCalls(w.External)
		if len(calls) != 2 || calls[0].Applied || !calls[1].Applied {
			t.Fatalf("provider acts = %+v, want an unapplied then an applied act", calls)
		}
		wantProviderStatus(t, wp, w.External, tr.To)
	}},
	{name: "RetryAfterHonouredForAct", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		tr, w, _ := e.firstAct(t, "rate-limited-1")
		const wait = 10 * time.Minute
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRateLimited, RetryAfter: wait}, Response{Outcome: OutcomeAccepted})
		ref := e.mustAct(t, w, "op-rate-limited", tr.To, "")
		e.start(t)
		op := e.op(t, ref)
		wantOp(t, op, model.RetryPending, model.RateLimited)
		if op.Failure.RetryAfter != wait {
			t.Fatalf("Retry-After = %v, want the provider's %v", op.Failure.RetryAfter, wait)
		}
		e.later(t, wait-time.Minute)
		if n := len(e.actCalls(w.External)); n != 1 {
			t.Fatalf("provider acts before Retry-After elapsed = %d, want 1", n)
		}
		e.later(t, 2*time.Minute)
		e.stop(t)
		wantActApplied(t, e.op(t, ref))
		if n := len(e.actCalls(w.External)); n != 2 {
			t.Fatalf("provider acts = %d, want 2", n)
		}
	}},
	{name: "LostActAnswerIsNeverResent", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		tr, w, wp := e.firstAct(t, "answer-lost-1")
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAnswerLost})
		ref := e.mustAct(t, w, "op-answer-lost", tr.To, "")
		e.start(t)
		op := e.op(t, ref)
		wantOp(t, op, model.Unknown, model.OutcomeUnknown)
		if op.Failure.Retry != model.ReconcileFirst {
			t.Fatalf("lost answer retry advice = %q, want reconcile_first", op.Failure.Retry)
		}
		e.later(t, time.Hour)
		e.stop(t)
		if n := len(e.actCalls(w.External)); n != 1 {
			t.Fatalf("provider acts = %d, want 1: a lost answer is never sent again", n)
		}
		wantProviderStatus(t, wp, w.External, tr.To)
		if !e.reconciles() {
			wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
			return
		}
		wantActApplied(t, e.op(t, ref))
		if n := len(e.calls(adapterChanA, model.CapabilityReconcile)); n == 0 {
			t.Fatal("no reconcile call; the applied act was adopted without reconciling")
		}
		e.wantConversationOpen(t, w)
	}},
	{name: "LostActRequestReconcilesBeforeRetry", needs: []model.Capability{model.CapabilityWorkItemAct, model.CapabilityReconcile}, run: func(t *testing.T, e *adapterEnv) {
		tr, w, wp := e.firstAct(t, "request-lost-1")
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRequestLost}, Response{Outcome: OutcomeAccepted})
		ref := e.mustAct(t, w, "op-request-lost", tr.To, "")
		e.start(t)
		wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
		e.later(t, time.Hour)
		e.stop(t)
		wantActApplied(t, e.op(t, ref))
		var seq []model.Capability
		for _, c := range e.provider.Calls(adapterChanA) {
			seq = append(seq, c.Capability)
		}
		want := []model.Capability{model.CapabilityWorkItemAct, model.CapabilityReconcile, model.CapabilityWorkItemAct}
		if !slices.Equal(seq, want) {
			t.Fatalf("provider calls = %v, want %v", seq, want)
		}
		wantProviderStatus(t, wp, w.External, tr.To)
	}},
	{name: "RevokedCredentialMakesNoActCall", needs: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		tr, w, _ := e.firstAct(t, "revoked-1")
		e.creds.revoke()
		ref := e.mustAct(t, w, "op-revoked-credential", tr.To, "")
		e.start(t)
		e.later(t, time.Hour)
		e.stop(t)
		op := e.op(t, ref)
		wantOp(t, op, model.Failed, model.Unauthorized)
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
		e.noSecret(t, ref)
		e.wantUntouched(t, w)
	}},
}

var adapterWorkItemActUnsupportedCases = []adapterCase{
	{name: "ActWithoutWorkItemAct", absent: []model.Capability{model.CapabilityWorkItemAct}, run: func(t *testing.T, e *adapterEnv) {
		w := e.workItem(t, e.route(adapterChanA, "issue-1"), model.WorkItemOpen)
		if _, err := e.act(t, w, "op-unsupported", actUpdate(model.WorkItemClosed, "")); model.CodeOf(err) != model.Unsupported {
			t.Fatalf("WorkItems.Act on a channel without work_item_act = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
}
