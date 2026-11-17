package runtime

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// reconcileOwner names the lease a Reconcile call holds on its operation.
const reconcileOwner = "operation.reconcile"

// Reconcile asks the provider what became of an outcome_unknown operation's
// unresolved attempt now, instead of at the time the runtime scheduled, and
// returns the operation as Get shows it afterwards.
//
// The client's principal needs model.ActionOperationRead and
// model.ActionOperationReconcile on the operation; both are checked before
// anything is revealed or called. A settled operation is returned as it is,
// with no provider call. An operation in any other state than
// outcome_unknown, or whose latest attempt is resolved, is model.Conflict:
// the runtime acts on it by itself. A worker, or an earlier Reconcile still
// running, holding the operation is model.Conflict too.
//
// The result is the adapter's answer and nothing else. The effect found
// applied settles the operation (provider_accepted or delivered) and one
// reported failed for good settles it failed or rejected. The provider
// holding no record of the effect, after the adapter's settle window, shows
// it was not applied: the operation becomes retry_pending and no send
// happens here; the runtime sends it on its schedule. An answer that proves
// nothing, an adapter without reconcile, and a failed reconciliation leave
// the operation, its evidence and its schedule untouched: the first returns
// it unchanged, the others return their error (model.Unsupported for an
// adapter that cannot reconcile). Nothing is ever assumed.
func (rt *Runtime) Reconcile(ctx context.Context, c *engine.Client, req model.Lookup[model.OperationRef]) (model.Operation, error) {
	ops := operations.New(c)
	rec, view, err := ops.Reconcilable(ctx, req)
	if err != nil {
		return model.Operation{}, err
	}
	if settled(rec.Operation.State) {
		return view, nil
	}
	if rec.Operation.State != model.Unknown {
		return model.Operation{}, notReconcilable()
	}
	done, err := rt.e.Begin()
	if err != nil {
		return model.Operation{}, err
	}
	defer done()
	x, release, err := rt.holdOperation(ctx, req.Ref)
	if err != nil {
		return model.Operation{}, err
	}
	defer release()
	if err := x.reconcile(ctx); err != nil {
		return model.Operation{}, err
	}
	return ops.Get(ctx, req)
}

func notReconcilable() error {
	return &model.Error{Code: model.Conflict, SafeMessage: "operation is not awaiting reconciliation", Retry: model.NoRetry}
}

// holdOperation leases ref against workers and other reconciliations, then
// loads its record and evidence under the lease. release drops the lease.
func (rt *Runtime) holdOperation(ctx context.Context, ref model.OperationRef) (*execution, func(), error) {
	e := rt.e
	var lease store.Lease
	err := operations.Transact(ctx, e, ref.Tenant, func(tx store.Tx) error {
		var err error
		lease, err = tx.AcquireLease(ctx, store.LeaseRequest{
			Resource: ref.Ref(), Owner: reconcileOwner, Until: e.Clock().Now().Add(defaultLease),
		})
		return err
	})
	if model.IsCode(err, model.Conflict) {
		return nil, nil, &model.Error{Code: model.Conflict, SafeMessage: "operation is being processed", Retry: model.RetrySameOperation}
	}
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		_ = operations.Transact(context.WithoutCancel(ctx), e, ref.Tenant, func(tx store.Tx) error { return tx.ReleaseLease(ctx, lease) })
	}
	x := &execution{
		rt: rt, e: e, adhoc: true,
		work: store.Work{Kind: operations.WorkKindOperation, Resource: ref.Ref(), Fence: lease.Fence, Until: lease.Until},
	}
	err = e.Store().View(ctx, ref.Tenant, func(r store.Reader) error {
		var err error
		if x.rec, err = r.Operation(ctx, ref); err != nil {
			return err
		}
		x.rec = operations.Effective(x.rec)
		x.hist, err = operations.LoadHistory(ctx, r, ref)
		return err
	})
	if err != nil {
		release()
		return nil, nil, err
	}
	return x, release, nil
}

// reconcile runs the reconciliation under the operation's lease.
func (x *execution) reconcile(ctx context.Context) error {
	// The state may have moved since it was read: a worker settled it.
	if settled(x.rec.Operation.State) {
		return nil
	}
	if x.rec.Operation.State != model.Unknown {
		return notReconcilable()
	}
	if _, ok := x.rt.effectors[x.rec.Command.Name].(Stepper); ok {
		return &model.Error{Code: model.Unsupported, SafeMessage: "operation has no provider effect to reconcile", Retry: model.NoRetry}
	}
	job, err := x.rt.effectors.decode(x.rec.Command)
	if err != nil {
		return AsFailure(err)
	}
	x.job = job
	if done, err := x.settleFromEvidence(ctx); done {
		return err
	}
	if !x.hist.Unresolved() {
		return notReconcilable()
	}
	ch, err := job.Load(ctx, x.e, x.rec.Operation.Ref.Tenant)
	if err != nil {
		return AsFailure(err)
	}
	x.channel = ch
	inst, desc, creds, err := OpenOutbound(ctx, x.e, ch)
	if err != nil {
		return AsFailure(err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	x.inst, x.desc, x.creds = inst, desc, creds
	rc, ok := inst.(adapter.Reconciler)
	if !ok {
		return cannotReconcile()
	}
	if f := capabilityReady(ctx, desc, inst, model.CapabilityReconcile); f != nil {
		return f
	}
	attempt := x.hist.Latest
	out, err := rc.Reconcile(ctx, reconcileRequest(job, x.effect(attempt), x.hist.LatestAt))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A failed reconciliation says nothing about the attempt.
		return AsFailure(err)
	}
	a, _, early := x.classifyReconcile(ctx, out)
	if early || a.verdict == verdictUnknown {
		// Proves nothing: neither recorded nor acted on.
		return nil
	}
	if err := x.record(ctx, attempt, a); err != nil {
		return stale(err)
	}
	if a.verdict == verdictRetry {
		return x.retryPending(ctx, a)
	}
	return x.finish(ctx, a, model.TriggerSystem)
}

func cannotReconcile() *model.Error {
	return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter cannot reconcile", Retry: model.NoRetry}
}

// retryPending moves an operation whose unresolved attempt the provider
// showed was not applied to retry_pending, with that failure. A retry the
// runtime already scheduled (NextAttemptAt) keeps its time; otherwise one
// is scheduled now, so the operation never waits on work that does not
// exist.
func (x *execution) retryPending(ctx context.Context, a answer) error {
	f := a.failure
	if f == nil {
		f = &model.Error{Code: model.NotFound, SafeMessage: "provider holds no record of the effect", Retry: model.RetrySameOperation}
	}
	next := x.rec.Operation.Clone()
	next.State, next.Failure = model.RetryPending, f
	if next.NextAttemptAt != nil {
		return x.commit(ctx, next, model.TriggerSystem, nil)
	}
	id, err := operations.NewID(ctx, x.e)
	if err != nil {
		return err
	}
	now := x.e.Clock().Now()
	next.NextAttemptAt = &now
	retry := store.Work{ID: id, Kind: x.work.Kind, Resource: x.work.Resource, NotBefore: now}
	return x.commit(ctx, next, model.TriggerSystem, &retry)
}

// settleFromEvidence settles the operation from evidence already recorded,
// without a provider call: the provider applied the effect (a late receipt
// from an earlier owner included), or, for an outcome_unknown operation,
// reported a final failure of the latest attempt. The state is the
// strongest the evidence shows (operations.Outcome), not the latest, so
// reports that arrived out of order settle as the same state as in order.
// It reports whether it settled, and its result.
func (x *execution) settleFromEvidence(ctx context.Context) (bool, error) {
	o := x.hist.Outcome
	if !o.Valid() || (!operations.AppliedState(o.State) && x.rec.Operation.State != model.Unknown) {
		return false, nil
	}
	return true, x.finish(ctx, outcomeAnswer(o), model.TriggerProvider)
}

// outcomeAnswer is the answer an outcome of recorded evidence stands for.
func outcomeAnswer(o operations.Outcome) answer {
	if operations.AppliedState(o.State) {
		return answer{verdict: verdictApplied, state: o.State, references: o.References}
	}
	return answer{verdict: verdictFinal, state: o.State, references: o.References, failure: noRetry(o.Failure)}
}

// classifyReconcile reads the adapter's reconciliation of the latest
// attempt. Not found (model.Failed) inside the adapter's settle window
// (Descriptor.ReconcileSettle, from the attempt's record time) is
// inconclusive: early is set, a is outcome_unknown and settles is when the
// window passes. Not found after it proves the provider holds no applied
// effect: a is a retry, safe to attempt again.
func (x *execution) classifyReconcile(ctx context.Context, out adapter.Outcome) (a answer, settles time.Time, early bool) {
	a = classify(ctx, out, nil, false)
	if out.State != model.Failed {
		return a, time.Time{}, false
	}
	if settles = x.hist.LatestAt.Add(x.desc.ReconcileSettle); x.e.Clock().Now().Before(settles) {
		return unknownAnswer(a.references), settles, true
	}
	a.verdict = verdictRetry
	if a.failure != nil && a.failure.Retry == model.NoRetry {
		f := *a.failure
		f.Retry = model.RetrySameOperation
		a.failure = &f
	}
	return a, time.Time{}, false
}
