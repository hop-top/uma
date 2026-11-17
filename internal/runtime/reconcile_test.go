package runtime_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// denyActions refuses the named actions and allows every other.
type denyActions struct {
	mu   sync.Mutex
	deny map[string]bool
}

func (d *denyActions) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return policy.Decision{Allow: !d.deny[a.Action]}, nil
}

func (d *denyActions) set(actions ...string) {
	d.mu.Lock()
	d.deny = map[string]bool{}
	for _, a := range actions {
		d.deny[a] = true
	}
	d.mu.Unlock()
}

func withDenials(d *denyActions) enginetest.RigOption {
	return func(c *engine.Config, _ *enginetest.OutFactory) { c.Authorizer = d }
}

func reconcileReq(ref model.OperationRef) model.Lookup[model.OperationRef] {
	return model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "reconcile"}, Ref: ref}
}

func reconcile(t *testing.T, r *rig, ref model.OperationRef) (model.Operation, error) {
	t.Helper()
	return r.RT.Reconcile(r.Ctx, r.C.Client, reconcileReq(ref))
}

// lostNotApplied loses the answer of a send the provider never applied.
func lostNotApplied(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}, nil, false
}

// unknownRig sends one reply whose answer is lost and applied, so its
// operation is outcome_unknown with the next reconciliation scheduled.
func unknownRig(t *testing.T, opts ...enginetest.RigOption) (*rig, model.OperationRef) {
	t.Helper()
	return lostRig(t, lostAnswer, opts...)
}

// lostRig is unknownRig with the answer to the one send chosen.
func lostRig(t *testing.T, answer enginetest.SendFunc, opts ...enginetest.RigOption) (*rig, model.OperationRef) {
	t.Helper()
	r := newRig(t, append([]enginetest.RigOption{withReconciler}, opts...)...)
	r.F.Respond(answer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	return r, a.Operation.Ref
}

// Reconcile asks the provider about the operation's unresolved attempt now,
// without waiting for the scheduled reconciliation: a provider that holds
// the effect settles the operation, no second send happens, and the work
// the runtime scheduled finds a settled operation and does nothing.
func TestReconcileSettlesLostAnswer(t *testing.T) {
	r, ref := unknownRig(t)
	before := r.Op(ref)
	if before.NextAttemptAt == nil || before.NextAttemptAt.Before(r.Clock.Now()) {
		t.Fatalf("NextAttemptAt = %v, want the scheduled reconciliation (now is %v)", before.NextAttemptAt, r.Clock.Now())
	}
	op, err := reconcile(t, r, ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireState(t, op, model.ProviderAccepted, "")
	if len(op.ProviderReferences) != 1 || op.NextAttemptAt != nil {
		t.Fatalf("operation = %+v, want the provider's reference and nothing scheduled", op)
	}
	if got := r.Op(ref); got.Revision != op.Revision || got.State != op.State {
		t.Fatalf("Get = %+v, want what Reconcile returned (%+v)", got, op)
	}
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls = %v, want the one send, then the one reconcile", got)
	}
	if got := deliveryStates(t, r); !slices.Equal(got, []model.DeliveryState{model.Unknown, model.ProviderAccepted}) {
		t.Fatalf("delivery events = %v", got)
	}
	r.Clock.Advance(time.Hour)
	r.Step("w")
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls after the scheduled work ran = %v, want no more", got)
	}
}

// Reconcile is idempotent: repeating it on a settled operation returns the
// same operation without a provider call or a write.
func TestReconcileIsIdempotent(t *testing.T) {
	r, ref := unknownRig(t)
	first, err := reconcile(t, r, ref)
	if err != nil {
		t.Fatal(err)
	}
	events := len(r.Events())
	again, err := reconcile(t, r, ref)
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if again.Revision != first.Revision || again.State != first.State {
		t.Fatalf("second Reconcile = %+v, want %+v", again, first)
	}
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile"}) {
		t.Fatalf("provider calls = %v", got)
	}
	if n := len(r.Events()); n != events {
		t.Fatalf("events = %d, want %d: a repeat writes nothing", n, events)
	}
}

// A provider that holds no record of the effect proves it was not applied:
// the operation becomes retry_pending with a retry due now and no send
// happens inside Reconcile; the worker then sends once.
func TestReconcileNotAppliedSchedulesRetry(t *testing.T) {
	r, ref := lostRig(t, lostNotApplied)

	op, err := reconcile(t, r, ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if op.State != model.RetryPending || op.Failure == nil || op.Failure.Retry != model.RetrySameOperation {
		t.Fatalf("operation = %+v, want retry_pending with retry advice", op)
	}
	if op.NextAttemptAt == nil {
		t.Fatal("NextAttemptAt unset, want the scheduled retry")
	}
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d after Reconcile, want the original one only", n)
	}
	r.Step("w")
	requireState(t, r.Op(ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"send", "reconcile", "send"}) {
		t.Fatalf("provider calls = %v, want send, reconcile, then the retry", got)
	}
}

// An inconclusive answer proves nothing: the operation, its evidence and
// its schedule stay as they were, and a failed reconciliation is the
// caller's error, never an outcome.
func TestReconcileNeverInventsAnOutcome(t *testing.T) {
	cases := map[string]struct {
		answer  func(adapter.ReconcileRequest) (adapter.Outcome, error)
		wantErr model.ErrorCode
	}{
		"unknown answer": {answer: func(adapter.ReconcileRequest) (adapter.Outcome, error) {
			return adapter.Outcome{State: model.Unknown}, nil
		}},
		"transient error": {answer: func(adapter.ReconcileRequest) (adapter.Outcome, error) {
			return adapter.Outcome{}, model.NewError(model.Transient, "provider unreachable")
		}, wantErr: model.Transient},
		"untyped error": {answer: func(adapter.ReconcileRequest) (adapter.Outcome, error) {
			return adapter.Outcome{}, context.DeadlineExceeded
		}, wantErr: model.Unavailable},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, ref := unknownRig(t)
			r.F.ReconcileFn = tc.answer
			before, evidence, events := r.Op(ref), r.Evidence(ref), len(r.Events())
			op, err := reconcile(t, r, ref)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if tc.wantErr != "" {
				requireCode(t, err, tc.wantErr, "")
			}
			if tc.wantErr == "" {
				requireState(t, op, model.Unknown, model.OutcomeUnknown)
			}
			after := r.Op(ref)
			if after.Revision != before.Revision || after.State != model.Unknown || !equalTimes(after.NextAttemptAt, before.NextAttemptAt) {
				t.Fatalf("operation changed: %+v, was %+v", after, before)
			}
			if got := r.Evidence(ref); len(got) != len(evidence) {
				t.Fatalf("evidence = %d entries, was %d: an inconclusive answer is not recorded", len(got), len(evidence))
			}
			if n := len(r.Events()); n != events {
				t.Fatalf("events = %d, was %d", n, events)
			}
			if n := len(r.F.ProviderSends()); n != 1 {
				t.Fatalf("provider sends = %d, want the original one", n)
			}
		})
	}
}

func equalTimes(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

// Not found inside the adapter's settle window is inconclusive, as for the
// worker: Reconcile leaves the operation outcome_unknown instead of
// scheduling a second send.
func TestReconcileHonoursSettleWindow(t *testing.T) {
	r, ref := lostRig(t, lostNotApplied, func(_ *engine.Config, f *enginetest.OutFactory) { f.ReconcileSettle = time.Hour })
	op, err := reconcile(t, r, ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	requireState(t, op, model.Unknown, model.OutcomeUnknown)
	if n := len(r.F.ProviderSends()); n != 1 {
		t.Fatalf("provider sends = %d, want the original one", n)
	}
	r.Clock.Advance(2 * time.Hour)
	op, err = reconcile(t, r, ref)
	if err != nil {
		t.Fatalf("Reconcile after the window: %v", err)
	}
	if op.State != model.RetryPending {
		t.Fatalf("state after the window = %s, want retry_pending", op.State)
	}
}

// An adapter without reconcile cannot be asked: unsupported, and nothing
// changes.
func TestReconcileUnsupportedWithoutReconciler(t *testing.T) {
	r := newRig(t)
	r.F.Respond(lostAnswer)
	a := r.Reply("op-1", "hello")
	r.Step("w")
	before := r.Op(a.Operation.Ref)
	requireState(t, before, model.Unknown, model.OutcomeUnknown)
	_, err := reconcile(t, r, a.Operation.Ref)
	requireCode(t, err, model.Unsupported, "")
	if after := r.Op(a.Operation.Ref); after.Revision != before.Revision {
		t.Fatalf("operation changed: %+v", after)
	}
}

// Only an operation awaiting reconciliation is reconciled: a settled one
// is returned as it is, one with nothing unresolved is a conflict.
func TestReconcileByState(t *testing.T) {
	r := newRig(t, withReconciler)
	queued := r.Reply("op-queued", "one").Operation.Ref
	_, err := reconcile(t, r, queued)
	requireCode(t, err, model.Conflict, "")
	r.Step("w")
	settled, err := reconcile(t, r, queued)
	if err != nil {
		t.Fatalf("Reconcile of a settled operation: %v", err)
	}
	requireState(t, settled, model.ProviderAccepted, "")
	if calls := r.F.Calls(); slices.Contains(calls, "reconcile") {
		t.Fatalf("provider calls = %v, want no reconcile for a settled operation", calls)
	}
	_, err = r.RT.Reconcile(r.Ctx, r.C.Client, reconcileReq(model.OperationRef{Tenant: "tenant-b", ID: queued.ID}))
	requireCode(t, err, model.NotFound, "")
	_, err = r.RT.Reconcile(r.Ctx, r.C.Client, reconcileReq(model.OperationRef{Tenant: tenantA, ID: "00000007-0000-7000-8000-0000000000ff"}))
	requireCode(t, err, model.NotFound, "")
}

// Reconcile needs operation.read and, on top, operation.reconcile: either
// denial refuses before any provider call, and the decisions are asked as
// read first.
func TestReconcileIsAuthorized(t *testing.T) {
	d := &denyActions{}
	r, ref := unknownRig(t, withDenials(d))
	for _, deny := range []string{model.ActionOperationReconcile, model.ActionOperationRead} {
		d.set(deny)
		_, err := reconcile(t, r, ref)
		requireCode(t, err, model.Unauthorized, "")
		if got := r.F.Calls(); !slices.Equal(got, []string{"send"}) {
			t.Fatalf("denied %s: provider calls = %v, want no reconcile", deny, got)
		}
		d.set()
		if op := r.Op(ref); op.State != model.Unknown {
			t.Fatalf("denied %s: operation = %+v, want unchanged", deny, op)
		}
	}
	d.set()
	if op, err := reconcile(t, r, ref); err != nil || op.State != model.ProviderAccepted {
		t.Fatalf("authorized Reconcile = %+v, %v", op, err)
	}
}

// A settled operation is refused to a caller without the actions too: a
// denial never reveals state.
func TestReconcileDeniedOnSettledOperation(t *testing.T) {
	d := &denyActions{}
	r, ref := unknownRig(t, withDenials(d))
	if _, err := reconcile(t, r, ref); err != nil {
		t.Fatal(err)
	}
	d.set(model.ActionOperationReconcile)
	_, err := reconcile(t, r, ref)
	requireCode(t, err, model.Unauthorized, "")
}

// A worker holding the operation's lease owns it: Reconcile refuses to
// race it.
func TestReconcileRefusesWhileWorkerHoldsOperation(t *testing.T) {
	r, ref := unknownRig(t)
	r.Clock.Advance(time.Hour)
	works := r.Claim("worker")
	if len(works) != 1 {
		t.Fatalf("claimed %d items, want the reconciliation", len(works))
	}
	_, err := reconcile(t, r, ref)
	requireCode(t, err, model.Conflict, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"send"}) {
		t.Fatalf("provider calls = %v, want no reconcile while the worker holds the item", got)
	}
	if err := r.RT.ExecuteClaimed(r.Ctx, tenantA, works[0]); err != nil {
		t.Fatal(err)
	}
	requireState(t, r.Op(ref), model.ProviderAccepted, "")
	if op, err := reconcile(t, r, ref); err != nil || op.State != model.ProviderAccepted {
		t.Fatalf("Reconcile after the worker = %+v, %v", op, err)
	}
}

// NextAttemptAt follows the schedule through a retry and clears when the
// operation settles.
func TestNextAttemptAtFollowsTheSchedule(t *testing.T) {
	r := newRig(t)
	r.F.Respond(transientFailure)
	a := r.Reply("op-1", "hello")
	if op := r.Op(a.Operation.Ref); op.NextAttemptAt != nil {
		t.Fatalf("queued operation NextAttemptAt = %v, want none", op.NextAttemptAt)
	}
	r.Step("w")
	op := r.Op(a.Operation.Ref)
	if op.State != model.RetryPending || op.NextAttemptAt == nil || op.NextAttemptAt.Before(r.Clock.Now()) {
		t.Fatalf("operation = %+v, want retry_pending scheduled", op)
	}
	r.Clock.Advance(time.Hour)
	r.Step("w")
	op = r.Op(a.Operation.Ref)
	requireState(t, op, model.ProviderAccepted, "")
	if op.NextAttemptAt != nil {
		t.Fatalf("settled NextAttemptAt = %v, want none", op.NextAttemptAt)
	}
}

// An outcome_unknown operation the runtime stopped reconciling, its budget
// spent, has no scheduled work: Reconcile still asks the provider, and when
// that shows nothing was applied it schedules the retry itself.
func TestReconcileRevivesOperationWithoutSchedule(t *testing.T) {
	r := newRig(t, withReconciler, withRetry(engine.RetryPolicy{MaxAttempts: 2}))
	r.F.Respond(lostNotApplied)
	a := r.Reply("op-1", "hello")
	ref := a.Operation.Ref
	r.Step("w")
	r.F.FailOpen(model.NewError(model.Unavailable, "provider SDK init failed"))
	for range 4 {
		r.Step("w")
	}
	r.F.FailOpen(nil)
	spent := r.Op(ref)
	requireState(t, spent, model.Unknown, model.OutcomeUnknown)
	if spent.NextAttemptAt != nil {
		t.Fatalf("NextAttemptAt = %v, want none once the runtime stopped", spent.NextAttemptAt)
	}
	if n := r.Step("w"); n != 0 {
		t.Fatalf("claimed %d items after the budget was spent", n)
	}
	op, err := reconcile(t, r, ref)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if op.State != model.RetryPending || op.NextAttemptAt == nil {
		t.Fatalf("operation = %+v, want retry_pending with a retry scheduled", op)
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("claimed %d items, want the retry Reconcile scheduled", n)
	}
	requireState(t, r.Op(ref), model.ProviderAccepted, "")
}
