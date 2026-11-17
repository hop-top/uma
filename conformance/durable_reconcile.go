package conformance

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

// Operations.Reconcile cases run the engine over the store under test with
// the fault provider. The provider answers a reconciliation from what it
// applied; the store carries the operation, its evidence and its schedule
// across the process the case crashes before calling Reconcile.
var reconcileCases = []durableCase{
	{"ReconcileSettlesLostAnswer", reconcileSettlesLostAnswer},
	{"ReconcileNotAppliedRetriesOnSchedule", reconcileNotAppliedRetries},
	{"ReconcileInsideSettleWindowProvesNothing", reconcileInsideSettleWindow},
	{"ReconcileWithoutReconcilerIsUnsupported", reconcileWithoutReconciler},
	{"ReconcileIsAuthorized", reconcileIsAuthorized},
}

// lostUnapplied loses the answer of a send the provider never applied.
func lostUnapplied(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}, nil, false
}

func reconcileReq(ref model.OperationRef) model.Lookup[model.OperationRef] {
	return model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "conformance-reconcile"}, Ref: ref}
}

func (p *faultProc) reconcile(ref model.OperationRef) (model.Operation, error) {
	return p.client.Operations().Reconcile(p.ctx, reconcileReq(ref))
}

// lostSend runs a reply whose answer fp scripts until it is outcome_unknown
// with its next reconciliation scheduled, then restarts the process: the
// worker is gone, so only Reconcile acts on the operation.
func lostSend(t *testing.T, f *faultEnv, answer faultSend, cross func(*testing.T)) (*faultProc, model.OperationRef) {
	t.Helper()
	f.p.respond(answer)
	p := f.start(t)
	op := p.reply(t, "op-1")
	p.work()
	p.idle(t)
	got := p.op(t, op)
	wantOpState(t, got, model.Unknown, model.OutcomeUnknown)
	if got.NextAttemptAt == nil || got.NextAttemptAt.Before(f.fx.Now()) {
		t.Fatalf("NextAttemptAt = %v, want the scheduled reconciliation (now %v)", got.NextAttemptAt, f.fx.Now())
	}
	return f.restart(t, cross), op
}

// The provider applied a send and its answer was lost. Reconcile settles
// the operation from the provider's record without a second send, repeating
// it changes nothing, and the reconciliation the runtime had scheduled finds
// the operation settled.
func reconcileSettlesLostAnswer(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p, op := lostSend(t, f, applyLost, cross)
	lost := p.op(t, op).UpdatedAt
	e.fx.Advance(time.Second)
	got, err := p.reconcile(op)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	wantOpState(t, got, model.ProviderAccepted, "")
	if want := micro(e.fx.Now()); lost.IsZero() || !got.UpdatedAt.After(lost) || !got.UpdatedAt.Equal(want) {
		t.Fatalf("Reconcile UpdatedAt = %v, want the settling write at %v, after the earlier %v", got.UpdatedAt, want, lost)
	}
	if len(got.ProviderReferences) != 1 || got.NextAttemptAt != nil {
		t.Fatalf("operation = %+v, want the provider's reference and nothing scheduled", got)
	}
	if want := p.op(t, op); want.Revision != got.Revision || want.State != got.State || !want.UpdatedAt.Equal(got.UpdatedAt) {
		t.Fatalf("Get = %+v, want what Reconcile returned (%+v)", want, got)
	}
	again, err := p.reconcile(op)
	if err != nil || again.Revision != got.Revision {
		t.Fatalf("second Reconcile = %+v, %v; want the same operation", again, err)
	}
	if calls := f.p.log(); !slices.Equal(calls, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls = %v, want the one send, then the one reconciliation", calls)
	}
	p.work()
	p.idle(t)
	p.later(t, time.Hour)
	if calls := f.p.log(); !slices.Equal(calls, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls after the scheduled work ran = %v, want no more", calls)
	}
	if evs := f.deliveries(t, op); len(evs) != 2 {
		t.Fatalf("delivery events = %d, want outcome_unknown and provider_accepted", len(evs))
	}
}

// The provider never applied the send. Reconcile shows that and sends
// nothing: the operation waits for its scheduled retry, which sends once.
func reconcileNotAppliedRetries(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p, op := lostSend(t, f, lostUnapplied, cross)
	got, err := p.reconcile(op)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got.State != model.RetryPending || got.Failure == nil || got.Failure.Retry != model.RetrySameOperation || got.NextAttemptAt == nil {
		t.Fatalf("operation = %+v, want retry_pending with the retry scheduled", got)
	}
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d after Reconcile, want the original only", n)
	}
	p.work()
	p.idle(t)
	p.later(t, time.Hour)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	if calls := f.p.log(); !slices.Equal(calls, []string{"send", "reconcile", "send"}) {
		t.Fatalf("provider calls = %v, want send, reconcile, then the one retry", calls)
	}
}

// The provider's search shows the applied send a minute late; the adapter
// declares a two minute settle window. Reconcile inside the window proves
// nothing: the operation, its evidence and its schedule stay as they were
// and nothing is sent. After the lag the same call settles it.
func reconcileInsideSettleWindow(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	f.p.settle, f.p.lag, f.p.now = 2*time.Minute, time.Minute, f.fx.Now
	p, op := lostSend(t, f, applyLost, cross)
	before, evidence := p.op(t, op), len(f.evidence(t, op))
	f.fx.Advance(30 * time.Second)
	got, err := p.reconcile(op)
	if err != nil {
		t.Fatalf("Reconcile inside the window: %v", err)
	}
	wantOpState(t, got, model.Unknown, model.OutcomeUnknown)
	if got.Revision != before.Revision || got.NextAttemptAt == nil || !got.NextAttemptAt.Equal(*before.NextAttemptAt) {
		t.Fatalf("operation = %+v, want it unchanged from %+v", got, before)
	}
	if n := len(f.evidence(t, op)); n != evidence {
		t.Fatalf("evidence entries = %d, was %d: an answer that proves nothing is not recorded", n, evidence)
	}
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d, want the original only", n)
	}
	f.fx.Advance(time.Minute)
	got, err = p.reconcile(op)
	if err != nil {
		t.Fatalf("Reconcile after the lag: %v", err)
	}
	wantOpState(t, got, model.ProviderAccepted, "")
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}

// An adapter that does not declare reconcile cannot be asked: Reconcile is
// unsupported and the operation stays outcome_unknown.
func reconcileWithoutReconciler(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, false)
	needsAdvance(t, e)
	f.p.respond(applyLost)
	p := f.start(t)
	op := p.reply(t, "op-1")
	p.work()
	p.idle(t)
	before := p.op(t, op)
	wantOpState(t, before, model.Unknown, model.OutcomeUnknown)
	p = f.restart(t, cross)
	_, err := p.reconcile(op)
	wantCode(t, err, model.Unsupported)
	if got := p.op(t, op); got.Revision != before.Revision {
		t.Fatalf("operation = %+v, want it unchanged", got)
	}
	if calls := f.p.log(); !slices.Equal(calls, []string{"send"}) {
		t.Fatalf("provider calls = %v, want only the send", calls)
	}
}

// reconcileAuthorizer allows every action except the denied ones.
type reconcileAuthorizer struct {
	mu     sync.Mutex
	denied []string
}

func (a *reconcileAuthorizer) Authorize(_ context.Context, r policy.Authorization) (policy.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return policy.Decision{Allow: !slices.Contains(a.denied, r.Action)}, nil
}

func (a *reconcileAuthorizer) deny(actions ...string) {
	a.mu.Lock()
	a.denied = actions
	a.mu.Unlock()
}

// clientOf returns a client of a second engine over the env's store and
// provider whose policy is auth.
func (f *faultEnv) clientOf(t *testing.T, auth policy.Authorizer, p model.Principal) uma.Client {
	t.Helper()
	f.gen++
	eng, err := uma.New(uma.Config{
		Store: f.s, Authorizer: auth, Adapters: []adapter.Factory{f.p},
		Clock: nowFunc(f.fx.Now), IDs: portstest.NewSequenceIDs(0xfa00 + f.gen),
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	c, err := eng.For(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Reconcile needs operation.read and operation.reconcile; a caller without
// either is refused before the provider is called, and another tenant's
// caller does not find the operation. The authorized caller then settles
// it.
func reconcileIsAuthorized(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p, op := lostSend(t, f, applyLost, cross)
	auth := &reconcileAuthorizer{}
	caller := model.Principal{Tenant: tenantA, Actor: "conformance-agent"}
	c := f.clientOf(t, auth, caller)
	for _, action := range []string{model.ActionOperationReconcile, model.ActionOperationRead} {
		auth.deny(action)
		_, err := c.Operations().Reconcile(p.ctx, reconcileReq(op))
		wantCode(t, err, model.Unauthorized)
		if calls := f.p.log(); !slices.Equal(calls, []string{"send"}) {
			t.Fatalf("denied %s: provider calls = %v, want none for the refused call", action, calls)
		}
		wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)
	}
	auth.deny()
	foreign := f.clientOf(t, auth, model.Principal{Tenant: tenantB, Actor: "conformance-agent"})
	_, err := foreign.Operations().Reconcile(p.ctx, reconcileReq(op))
	wantCode(t, err, model.NotFound)
	got, err := c.Operations().Reconcile(p.ctx, reconcileReq(op))
	if err != nil {
		t.Fatalf("authorized Reconcile: %v", err)
	}
	wantOpState(t, got, model.ProviderAccepted, "")
	auth.deny(model.ActionOperationReconcile)
	_, err = c.Operations().Reconcile(p.ctx, reconcileReq(op))
	wantCode(t, err, model.Unauthorized)
}
