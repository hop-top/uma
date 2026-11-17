package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// queuedEffect is one queued effect of capability c a receipt or membership
// case submits: submit queues it under an operation ID, untouched requires
// uma to show nothing of it yet, and applied requires it to show in uma as
// the provider applied it.
type queuedEffect struct {
	capability model.Capability
	submit     func(t *testing.T, id model.OperationID) model.OperationRef
	untouched  func(t *testing.T)
	applied    func(t *testing.T)
}

// answerLostNeverResent scripts a lost answer for fx: the operation is
// outcome_unknown with reconcile_first advice and uma untouched, the
// provider is never asked again, and an adapter declaring reconcile finds
// the applied effect.
func (e *adapterEnv) answerLostNeverResent(t *testing.T, fx queuedEffect) {
	t.Helper()
	e.respond(t, adapterChanA, Response{Outcome: OutcomeAnswerLost})
	ref := fx.submit(t, "op-lost")
	e.later(t, 0)
	op := e.op(t, ref)
	wantOp(t, op, model.Unknown, model.OutcomeUnknown)
	if op.Failure.Retry != model.ReconcileFirst {
		t.Fatalf("lost answer retry advice = %q, want reconcile_first", op.Failure.Retry)
	}
	fx.untouched(t)
	e.later(t, time.Hour)
	if n := len(e.calls(adapterChanA, fx.capability)); n != 1 {
		t.Fatalf("provider %s calls = %d, want 1: a lost answer is never resent", fx.capability, n)
	}
	if !e.reconciles() {
		wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
		fx.untouched(t)
		e.stop(t)
		return
	}
	wantActApplied(t, e.op(t, ref))
	fx.applied(t)
	if n := len(e.calls(adapterChanA, model.CapabilityReconcile)); n == 0 {
		t.Fatal("no reconcile call; the applied effect was adopted without reconciling")
	}
	e.stop(t)
}

// revokedMakesNoCall withdraws the provider permission behind fx: the
// operation fails unauthorized without a retry and the provider is never
// called.
func (e *adapterEnv) revokedMakesNoCall(t *testing.T, fx queuedEffect) {
	t.Helper()
	if err := e.provider.Revoke(adapterChanA, fx.capability); err != nil {
		t.Skipf("provider cannot revoke the %s permission: %v", fx.capability, err)
	}
	ref := fx.submit(t, "op-revoked")
	e.later(t, 0)
	e.later(t, time.Hour)
	op := e.op(t, ref)
	wantOp(t, op, model.Failed, model.Unauthorized)
	if op.Failure.Retry != model.NoRetry {
		t.Fatalf("retry advice = %q, want none", op.Failure.Retry)
	}
	if calls := e.calls(adapterChanA, fx.capability); len(calls) != 0 {
		t.Fatalf("provider %s calls = %+v, want none", fx.capability, calls)
	}
	fx.untouched(t)
	e.stop(t)
}

// rejectedIsFinal scripts a refusal for fx: the operation settles rejected
// without a retry, the provider is asked once and uma stays untouched.
func (e *adapterEnv) rejectedIsFinal(t *testing.T, fx queuedEffect) {
	t.Helper()
	e.respond(t, adapterChanA, Response{Outcome: OutcomeRejected})
	ref := fx.submit(t, "op-rejected")
	e.later(t, 0)
	e.later(t, time.Hour)
	wantRejected(t, e.op(t, ref), "")
	if n := len(e.calls(adapterChanA, fx.capability)); n != 1 {
		t.Fatalf("provider %s calls = %d, want 1: a refusal is final", fx.capability, n)
	}
	fx.untouched(t)
	e.stop(t)
}

// wantReplayed requires a's operation to be the stored ref, replayed.
func wantReplayed(t *testing.T, a model.Acceptance, err error, ref model.OperationRef) {
	t.Helper()
	if err != nil || !a.Replayed || a.Operation.Ref != ref {
		t.Fatalf("retried submission = %+v, %v; want operation %v replayed", a, err, ref)
	}
}
