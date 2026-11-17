package runtime_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// Whether a lost answer is reconciled follows the channel's reported
// availability of reconcile, not only the descriptor: a channel that
// cannot reconcile takes the same path as an adapter that never declared
// it.

// A channel reporting reconcile unavailable for good (disabled by its
// configuration, unsupported by it) settles a lost answer outcome_unknown
// at once: no reconcile pass is scheduled, none runs, nothing is resent,
// and an explicit Operations.Reconcile still answers unsupported.
func TestChannelWithoutReconcileSettlesUnknownWithoutAPass(t *testing.T) {
	for _, reason := range []model.ErrorCode{model.Disabled, model.Unsupported} {
		t.Run(string(reason), func(t *testing.T) {
			r := newRig(t, withReconciler)
			r.F.Withdraw(model.CapabilityReconcile, reason)
			r.F.Respond(lostAnswer)
			a := r.Reply("op-1", "hello")
			r.Step("w")
			op := r.Op(a.Operation.Ref)
			requireState(t, op, model.Unknown, model.OutcomeUnknown)
			if op.NextAttemptAt != nil {
				t.Fatalf("next attempt at %v, want none: nothing is scheduled", op.NextAttemptAt)
			}
			r.Clock.Advance(time.Hour)
			if n := r.Step("w"); n != 0 {
				t.Fatalf("claimed %d items after outcome_unknown, want no reconcile pass", n)
			}
			_, err := reconcile(t, r, a.Operation.Ref)
			requireCode(t, err, model.Unsupported, "")
			requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
			if got := r.F.Calls(); !slices.Equal(got, []string{"send"}) {
				t.Fatalf("calls = %v, want the single lost send", got)
			}
		})
	}
}

// Reconcile temporarily unavailable when the answer is lost, or when the
// pass runs, defers the pass instead of settling: the attempt stays
// unresolved and is reconciled once the channel can.
func TestTemporarilyUnavailableReconcileIsDeferred(t *testing.T) {
	r := newRig(t, withReconciler)
	r.F.Withdraw(model.CapabilityReconcile, model.Unavailable)
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Unknown, model.OutcomeUnknown)
	if op.NextAttemptAt == nil {
		t.Fatal("no reconcile pass scheduled for a temporary outage")
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("claimed %d items, want the reconcile pass", n)
	}
	if op = r.Op(a.Operation.Ref); op.State != model.Unknown || op.NextAttemptAt == nil {
		t.Fatalf("operation %s, next attempt %v, want outcome_unknown with the pass deferred", op.State, op.NextAttemptAt)
	}
	r.F.Restore(model.CapabilityReconcile)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile"}) {
		t.Fatalf("calls = %v, want one send then one reconcile", got)
	}
}

// Deferring a pass for a temporary outage is bounded by the retry budget.
func TestTemporarilyUnavailableReconcileIsBounded(t *testing.T) {
	r := newRig(t, withReconciler, withRetry(engine.RetryPolicy{MaxAttempts: 2}))
	r.F.Withdraw(model.CapabilityReconcile, model.Unavailable)
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	for range 5 {
		r.Step("w")
	}
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Unknown, model.OutcomeUnknown)
	if op.NextAttemptAt != nil {
		t.Fatalf("next attempt at %v after the budget was spent, want none", op.NextAttemptAt)
	}
	if n := r.Step("w"); n != 0 {
		t.Fatalf("claimed %d items after the budget was spent", n)
	}
	if got := r.F.Calls(); !slices.Equal(got, []string{"send"}) {
		t.Fatalf("calls = %v, want the single lost send", got)
	}
}

// Availability is read when each decision is made: reconcile withdrawn
// after the pass was scheduled settles the operation at the pass without
// a call, and reconcile restored after it settled is open to an explicit
// Operations.Reconcile.
func TestReconcileAvailabilityIsReadPerDecision(t *testing.T) {
	r := newRig(t, withReconciler)
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	if op := r.Op(a.Operation.Ref); op.NextAttemptAt == nil {
		t.Fatal("no reconcile pass scheduled while reconcile is available")
	}
	r.F.Withdraw(model.CapabilityReconcile, model.Disabled)
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Unknown, model.OutcomeUnknown)
	if op.NextAttemptAt != nil {
		t.Fatalf("next attempt at %v, want none once reconcile is disabled", op.NextAttemptAt)
	}
	if got := r.F.Calls(); !slices.Equal(got, []string{"send"}) {
		t.Fatalf("calls = %v, want no reconcile once disabled", got)
	}
	r.F.Restore(model.CapabilityReconcile)
	got, err := reconcile(t, r, a.Operation.Ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireState(t, got, model.ProviderAccepted, "")
}
