package runtime

import (
	"context"
	"errors"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// settled reports whether an operation state is final: no further attempt
// or reconciliation runs for it.
func settled(s model.DeliveryState) bool { return operations.Settled(s) }

// execution is one claimed operation work item, run to its next durable
// state by its current lease owner.
type execution struct {
	rt   *Runtime
	e    *engine.Engine
	work store.Work
	rec  store.OperationRecord
	hist operations.History

	job     Job
	channel model.Channel
	inst    adapter.Instance
	desc    adapter.Descriptor
	creds   *scopedCredentials

	// adhoc marks an execution that is not a claimed work item: a
	// Reconcile call holding the operation's lease. It completes no work
	// item, and the work item the runtime scheduled stays queued.
	adhoc bool
}

// execute runs one claimed item:
//
//  1. A settled operation only completes its work.
//  2. Evidence that the provider applied the effect, including a late
//     receipt from an earlier owner, settles the operation without a call.
//  3. An unresolved latest attempt is reconciled before anything else; a
//     channel that cannot reconcile (no reconciler, reconcile not declared,
//     or reported unavailable for the channel) leaves the operation
//     outcome_unknown and it is never retried. "Not found" inside the adapter's declared
//     settle window (Descriptor.ReconcileSettle) is inconclusive: the
//     operation stays outcome_unknown and is reconciled again after it.
//  4. Otherwise a new attempt runs if the retry budget allows: policy,
//     stored destination and capabilities are rechecked as the originating
//     actor, the attempt is recorded under the item's fence, and the
//     provider is called outside any unit of work. Its answer is recorded as
//     evidence first, then applied to the operation under the fence. An
//     unknown answer yields to evidence recorded during the call that shows
//     the effect applied.
//
// A fence that is no longer current stops the item silently: the current
// owner continues from the recorded evidence.
func (rt *Runtime) execute(ctx context.Context, tenant model.TenantID, w store.Work) error {
	e := rt.e
	opRef, err := w.Resource.AsOperation()
	if err != nil || opRef.Tenant != tenant {
		return rt.dropWork(ctx, tenant, w)
	}
	x := &execution{rt: rt, e: e, work: w}
	err = e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if x.rec, err = r.Operation(ctx, opRef); err != nil {
			return err
		}
		x.rec = operations.Effective(x.rec)
		x.hist, err = operations.LoadHistory(ctx, r, opRef)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return rt.dropWork(ctx, tenant, w)
	}
	if err != nil {
		return err
	}
	return x.run(ctx)
}

func (x *execution) run(ctx context.Context) error {
	if settled(x.rec.Operation.State) {
		return x.advance(ctx)
	}
	if st, ok := x.rt.effectors[x.rec.Command.Name].(Stepper); ok {
		return st.Step(ctx, x.e, x.work, x.rec)
	}
	job, intentErr := x.rt.effectors.decode(x.rec.Command)
	x.job = job
	if settled, err := x.settleFromEvidence(ctx); settled {
		return err
	}
	if intentErr != nil {
		return x.giveUp(ctx, intentErr)
	}
	ch, err := job.Load(ctx, x.e, x.rec.Operation.Ref.Tenant)
	if err != nil {
		return x.giveUp(ctx, err)
	}
	x.channel = ch
	if g, ok := job.(Gated); ok {
		switch {
		case g.Wait():
			return x.deferWork(ctx)
		case g.Local():
			return x.finish(ctx, answer{verdict: verdictApplied, state: model.Committed}, model.TriggerSystem)
		}
	}
	inst, desc, creds, err := OpenOutbound(ctx, x.e, x.channel)
	if err != nil {
		return x.notReady(ctx, err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	x.inst, x.desc, x.creds = inst, desc, creds
	if x.hist.Unresolved() {
		return x.reconcileFirst(ctx)
	}
	return x.attempt(ctx)
}

// advance completes the item of a settled operation. A reply that follows a
// delivery lifecycle first moves on to the strongest evidence recorded since
// it settled (operations.Advance): provider_accepted to delivered or failed,
// a delivery to read. Anything else, and evidence the operation already
// outranks, only completes the item, so a repeated or late report changes
// nothing and announces nothing.
func (x *execution) advance(ctx context.Context) error {
	tenant := x.rec.Operation.Ref.Tenant
	if !operations.TracksDelivery(x.rec.Key.Kind) {
		return x.rt.dropWork(ctx, tenant, x.work)
	}
	next, moved := operations.Advance(x.rec.Operation, x.hist.Outcome)
	if !moved {
		return x.rt.dropWork(ctx, tenant, x.work)
	}
	// The event's audience is the reply's; an intent redacted since leaves
	// the restricted default.
	if job, err := x.rt.effectors.decode(x.rec.Command); err == nil {
		x.job = job
	}
	return x.commit(ctx, next, model.TriggerProvider, nil)
}

// effect is the adapter effect of attempt: the job's route and target, its
// audience and, for a job answering an interaction, a copy of that
// interaction's context.
func (x *execution) effect(attempt model.ID) adapter.Effect {
	route, target := x.job.Route()
	eff := adapter.Effect{
		Operation: x.rec.Operation.Ref, Attempt: attempt,
		Route: route, Target: target, Audience: x.job.Audience(),
	}
	if c, ok := x.job.(InteractionCarrier); ok {
		if ic := c.Interaction(); ic != nil {
			cp := ic.Clone()
			eff.Interaction = &cp
		}
	}
	return eff
}

// reconcileReady reports whether the open instance can reconcile now: nil
// when it is an adapter.Reconciler and the channel has reconcile available
// (capabilityReady), else why not. A retryable failure, such as a status
// report that failed or a capability temporarily unavailable, is an
// outage; any other failure means this channel cannot reconcile, whatever
// the descriptor declares.
func (x *execution) reconcileReady(ctx context.Context) *model.Error {
	if _, ok := x.inst.(adapter.Reconciler); !ok {
		return cannotReconcile()
	}
	return capabilityReady(ctx, x.desc, x.inst, model.CapabilityReconcile)
}

// reconcileFirst resolves the latest attempt before any retry. A channel
// that cannot reconcile leaves the operation outcome_unknown; one whose
// reconcile is temporarily unavailable defers the pass within the retry
// budget (notReady).
func (x *execution) reconcileFirst(ctx context.Context) error {
	if f := x.reconcileReady(ctx); f != nil {
		if engine.Retryable(f) {
			return x.notReady(ctx, f)
		}
		return x.finish(ctx, unknownAnswer(nil), model.TriggerSystem)
	}
	rc := x.inst.(adapter.Reconciler)
	attempt := x.hist.Latest
	if x.hist.LastCount > x.e.Retry().MaxAttempts {
		// Reconciliation stayed inconclusive within the budget.
		return x.finish(ctx, unknownAnswer(nil), model.TriggerSystem)
	}
	out, err := rc.Reconcile(ctx, reconcileRequest(x.job, x.effect(attempt), x.hist.LatestAt))
	if err != nil {
		// A failed reconciliation says nothing about the attempt: it stays
		// unresolved and is never recorded as an answer.
		if f := AsFailure(err); ctx.Err() == nil && engine.Retryable(f) {
			return x.reschedule(ctx, unknownAnswer(nil), x.hist.Attempts)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return x.finish(ctx, unknownAnswer(nil), model.TriggerSystem)
	}
	a, settles, early := x.classifyReconcile(ctx, out)
	if early {
		// Not found inside the adapter's settle window proves nothing:
		// the provider may not show the effect yet. The attempt stays
		// unresolved and is reconciled again once the window passed.
		if err := x.record(ctx, attempt, a); err != nil {
			return err
		}
		return x.rescheduleAt(ctx, unknownAnswer(nil), x.hist.Attempts, settles)
	}
	if err := x.record(ctx, attempt, a); err != nil {
		return err
	}
	switch a.verdict {
	case verdictApplied, verdictFinal:
		return x.finish(ctx, a, model.TriggerSystem)
	case verdictRetry:
		x.hist.Last = store.AttemptEvidence{Attempt: attempt, State: a.state, Failure: a.failure}
		return x.attempt(ctx)
	}
	// Inconclusive: reconcile again later, keeping the operation
	// outcome_unknown.
	return x.reschedule(ctx, unknownAnswer(a.references), x.hist.Attempts)
}

// attempt runs one new provider attempt.
func (x *execution) attempt(ctx context.Context) error {
	if x.hist.Attempts >= x.e.Retry().MaxAttempts {
		return x.giveUp(ctx, x.lastFailure())
	}
	if err := x.recheck(ctx); err != nil {
		if x.e.IsClosed() {
			return nil
		}
		return x.notReady(ctx, err)
	}
	call, ok := x.job.Bind(x.inst)
	if !ok {
		return x.giveUp(ctx, &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter cannot perform the effect", Retry: model.NoRetry})
	}
	attempt, err := operations.NewID(ctx, x.e)
	if err != nil {
		return err
	}
	if err := x.begin(ctx, attempt); err != nil {
		return err
	}
	x.creds.beginAttempt()
	start := x.e.Clock().Now()
	out, callErr := call(ctx, x.effect(attempt))
	a := classify(ctx, out, callErr, x.creds.refusedNow())
	x.observeDispatch(ctx, start, a)
	if err := x.record(context.WithoutCancel(ctx), attempt, a); err != nil {
		return err
	}
	if ctx.Err() != nil {
		// Evidence is kept; the next owner reconciles it.
		return ctx.Err()
	}
	if a.verdict == verdictUnknown {
		// Evidence recorded while the call was in flight, such as the
		// provider's marked echo, may already show the effect applied.
		found, err := x.recordedOutcome(ctx)
		if err != nil {
			return err
		}
		if found.Valid() {
			return x.finish(ctx, outcomeAnswer(found), model.TriggerProvider)
		}
	}
	made := x.hist.Attempts + 1
	switch a.verdict {
	case verdictApplied, verdictFinal:
		return x.finish(ctx, a, model.TriggerProvider)
	case verdictRetry:
		if made >= x.e.Retry().MaxAttempts {
			a.failure = noRetry(a.failure)
			return x.finish(ctx, a, model.TriggerProvider)
		}
		a.state = model.RetryPending
		return x.reschedule(ctx, a, made)
	}
	// An unknown answer is reconciled before any retry only on a channel
	// that can reconcile, now or after an outage of the check; on any
	// other it is outcome_unknown for good, settled now rather than after
	// a pass that could only settle it.
	if f := x.reconcileReady(ctx); f == nil || engine.Retryable(f) {
		return x.reschedule(ctx, a, made)
	}
	return x.finish(ctx, a, model.TriggerProvider)
}

// recheck has the job revalidate its stored state and reauthorize the
// originating actor, then checks the job's capabilities, immediately
// before an attempt.
func (x *execution) recheck(ctx context.Context) error {
	actor := x.e.Bind(model.Principal{Tenant: x.rec.Key.Tenant, Actor: x.rec.Key.Caller})
	if err := x.job.Recheck(ctx, actor, x.rec); err != nil {
		return err
	}
	for _, c := range x.job.Capabilities() {
		if f := capabilityReady(ctx, x.desc, x.inst, c); f != nil {
			return f
		}
	}
	return nil
}

// begin records attempt under the item's fence before any provider call.
// The fenced revision bump proves this worker still owns the operation.
func (x *execution) begin(ctx context.Context, attempt model.ID) error {
	rev, err := operations.NewRevision(ctx, x.e)
	if err != nil {
		return err
	}
	next := x.rec.Operation.Clone()
	next.Revision, next.NextAttemptAt = rev, nil
	now := x.e.Clock().Now()
	err = x.rt.transactClaimed(ctx, next.Ref.Tenant, x.work, func(tx store.Tx) error {
		if err := tx.UpdateOperation(ctx, next, x.rec.Operation.Revision, x.work.Fence); err != nil {
			return err
		}
		return tx.AppendAttemptEvidence(ctx, store.AttemptEvidence{
			Operation: next.Ref, Attempt: attempt, Fence: x.work.Fence, State: model.Unknown, ObservedAt: now,
		})
	})
	if err != nil {
		return stale(err)
	}
	x.rec.Operation = next
	return nil
}

// record appends the evidence of one provider answer for attempt.
func (x *execution) record(ctx context.Context, attempt model.ID, a answer) error {
	ev := store.AttemptEvidence{
		Operation: x.rec.Operation.Ref, Attempt: attempt, Fence: x.work.Fence, State: a.state,
		ProviderReferences: slices.Clone(a.references), ObservedAt: x.e.Clock().Now(),
		Failure: a.failure,
	}
	return x.rt.transactClaimed(ctx, ev.Operation.Tenant, x.work, func(tx store.Tx) error {
		return tx.AppendAttemptEvidence(ctx, ev)
	})
}

// recordedOutcome rereads the operation's evidence and returns the
// strongest outcome it shows, if any.
func (x *execution) recordedOutcome(ctx context.Context) (operations.Outcome, error) {
	var h operations.History
	err := x.e.Store().View(ctx, x.rec.Operation.Ref.Tenant, func(r store.Reader) error {
		var err error
		h, err = operations.LoadHistory(ctx, r, x.rec.Operation.Ref)
		return err
	})
	return h.Outcome, err
}

// lastFailure is the failure that exhausted the retry budget.
func (x *execution) lastFailure() *model.Error {
	if f := x.hist.Last.Failure; f != nil {
		return noRetry(f)
	}
	if f := x.rec.Operation.Failure; f != nil {
		return noRetry(f)
	}
	return &model.Error{Code: model.Permanent, SafeMessage: "retry budget exhausted", Retry: model.NoRetry}
}

// notReady handles a failure before any provider effect of this item. A
// non-retryable one settles the operation, as outcome_unknown when an
// earlier attempt is unresolved. A retryable one, such as an unavailable
// policy or adapter, defers the item and spends the retry budget, so an
// outage cannot retry forever:
//
//   - with no unresolved attempt, the deferral is recorded as an attempt
//     abandoned before dispatch (failed, no provider references);
//   - with an unresolved attempt, it is recorded as one more inconclusive
//     answer for that attempt, which stays unresolved and keeps the
//     operation outcome_unknown, so it is never masked by a new attempt.
func (x *execution) notReady(ctx context.Context, err error) error {
	f := AsFailure(err)
	if !engine.Retryable(f) {
		return x.giveUp(ctx, f)
	}
	if x.hist.Unresolved() {
		if err := x.record(ctx, x.hist.Latest, answer{state: model.Unknown, failure: cloneError(f)}); err != nil {
			return err
		}
		if x.hist.LastCount+1 > x.e.Retry().MaxAttempts {
			return x.finish(ctx, unknownAnswer(nil), model.TriggerSystem)
		}
		return x.reschedule(ctx, unknownAnswer(nil), x.hist.Attempts)
	}
	abandoned, err := operations.NewID(ctx, x.e)
	if err != nil {
		return err
	}
	if err := x.record(ctx, abandoned, answer{state: model.Failed, failure: cloneError(f)}); err != nil {
		return err
	}
	made := x.hist.Attempts + 1
	if made >= x.e.Retry().MaxAttempts {
		return x.finish(ctx, answer{verdict: verdictFinal, state: model.Failed, failure: noRetry(f)}, model.TriggerSystem)
	}
	next := answer{verdict: verdictRetry, state: model.RetryPending, failure: cloneError(f)}
	return x.reschedule(ctx, next, made)
}

// giveUp settles the operation after a failure that no retry can fix.
func (x *execution) giveUp(ctx context.Context, err error) error {
	if x.hist.Unresolved() {
		return x.finish(ctx, unknownAnswer(nil), model.TriggerSystem)
	}
	return x.finish(ctx, answer{verdict: verdictFinal, state: model.Failed, failure: noRetry(AsFailure(err))}, model.TriggerSystem)
}

// finish settles the operation from answer a and completes the item, under
// the item's fence.
func (x *execution) finish(ctx context.Context, a answer, trigger string) error {
	next := x.rec.Operation.Clone()
	next.State = opState(a.state)
	next.ProviderReferences = mergeRefs(next.ProviderReferences, a.references)
	next.Failure = a.failure
	next.NextAttemptAt = nil
	if a.verdict == verdictApplied {
		next.Failure = nil
		if c, ok := x.job.(referenceChanger); ok {
			c.changeReferences(a.changes)
		}
	}
	return x.commit(ctx, next, trigger, nil)
}

// reschedule records the waiting state and re-enqueues the item after the
// backoff that follows attempt n, honouring a provider Retry-After.
func (x *execution) reschedule(ctx context.Context, a answer, n int) error {
	return x.rescheduleAt(ctx, a, n, time.Time{})
}

// rescheduleAt is reschedule with the item also deferred to at least
// notBefore.
func (x *execution) rescheduleAt(ctx context.Context, a answer, n int, notBefore time.Time) error {
	next := x.rec.Operation.Clone()
	next.State = a.state
	next.ProviderReferences = mergeRefs(next.ProviderReferences, a.references)
	next.Failure = a.failure
	var after time.Duration
	if a.failure != nil {
		after = a.failure.RetryAfter
	}
	delay := x.e.Retry().Delay(max(n, 1), after, x.rt.run.jitterFunc())
	id, err := operations.NewID(ctx, x.e)
	if err != nil {
		return err
	}
	at := x.e.Clock().Now().Add(delay)
	if notBefore.After(at) {
		at = notBefore
	}
	retry := store.Work{ID: id, Kind: x.work.Kind, Resource: x.work.Resource, NotBefore: at}
	next.NextAttemptAt = &at
	return x.commit(ctx, next, model.TriggerSystem, &retry)
}

// commit writes next under the item's fence with its delivery event when
// the state changed, completes the item and enqueues retry when set.
//
// A settled reply is first weighed against the evidence the unit reads: a
// status report that beat the settle, whether it arrived before the
// provider's answer or while this unit was being prepared, is not lost to
// it (operations.Advance). The units are serializable, so a report either
// is in the history read here or its own unit sees this one's settled
// state and queues the item that moves the operation on.
func (x *execution) commit(ctx context.Context, next model.Operation, trigger string, retry *store.Work) error {
	rev, err := operations.NewRevision(ctx, x.e)
	if err != nil {
		return err
	}
	prev := x.rec.Operation
	next.Revision = rev
	var eventID model.ID
	var final model.Operation
	err = x.rt.transactClaimed(ctx, next.Ref.Tenant, x.work, func(tx store.Tx) error {
		final = next
		if retry == nil && operations.TracksDelivery(x.rec.Key.Kind) {
			h, err := operations.LoadHistory(ctx, tx, next.Ref)
			if err != nil {
				return err
			}
			final, _ = operations.Advance(next, h.Outcome)
		}
		if err := tx.UpdateOperation(ctx, final, prev.Revision, x.work.Fence); err != nil {
			return err
		}
		if announced(prev, final) {
			if eventID == "" {
				if eventID, err = operations.NewID(ctx, x.e); err != nil {
					return err
				}
			}
			ev, err := x.deliveryEvent(eventID, prev, final, trigger)
			if err != nil {
				return err
			}
			if err := tx.AppendEvents(ctx, []model.Event{ev}); err != nil {
				return err
			}
		}
		if s, ok := x.job.(Settler); ok && retry == nil {
			if err := s.Settle(ctx, tx, final); err != nil {
				return err
			}
		}
		if !x.adhoc {
			if err := tx.CompleteWork(ctx, x.work); err != nil {
				return err
			}
		}
		if retry != nil {
			return tx.EnqueueWork(ctx, *retry)
		}
		return nil
	})
	if err != nil {
		return stale(err)
	}
	x.observeState(ctx, prev.State, final.State)
	x.rec.Operation = final
	return nil
}

// announced reports whether moving an operation from prev to next is a
// delivery.updated event: a new state, or the first read confirmation of a
// delivered operation.
func announced(prev, next model.Operation) bool {
	return next.State != prev.State || (prev.ReadConfirmedAt == nil && next.ReadConfirmedAt != nil)
}

// deliveryEvent is the delivery.updated event of the move from prev to
// next, under the job's audience, restricted when the job is not at hand.
func (x *execution) deliveryEvent(id model.ID, prev, next model.Operation, trigger string) (model.Event, error) {
	var audience model.Audience
	if x.job != nil {
		audience = x.job.Audience()
	}
	if audience.Visibility == "" {
		audience = model.Audience{Visibility: model.Restricted}
	}
	return operations.DeliveryEvent(id, x.rec, prev.State, next, audience, trigger, x.e.Clock().Now())
}

// dropWork completes an item whose operation needs nothing more.
func (rt *Runtime) dropWork(ctx context.Context, tenant model.TenantID, w store.Work) error {
	err := rt.transactClaimed(ctx, tenant, w, func(tx store.Tx) error { return tx.CompleteWork(ctx, w) })
	return stale(err)
}

// stale maps a fence or revision conflict to errStale: another owner holds
// the item and continues from the recorded evidence.
func stale(err error) error {
	if model.IsCode(err, model.Conflict) || model.IsCode(err, model.NotFound) {
		return errStale
	}
	return err
}

// opState is the operation state for evidence state s: a provider-side
// queue is provider acceptance.
func opState(s model.DeliveryState) model.DeliveryState {
	if s == model.Queued {
		return model.ProviderAccepted
	}
	return s
}

func mergeRefs(have, add []model.ExternalRef) []model.ExternalRef {
	out := slices.Clone(have)
	for _, r := range add {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// AsFailure returns err as a safe *model.Error; an untyped error becomes
// model.Unavailable without its text.
func AsFailure(err error) *model.Error {
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return cloneError(me)
	}
	return model.NewError(model.Unavailable, "operation dependency unavailable")
}

// deferWork completes the item and enqueues it again after the base retry
// delay, without an attempt and without changing the operation.
func (x *execution) deferWork(ctx context.Context) error {
	id, err := operations.NewID(ctx, x.e)
	if err != nil {
		return err
	}
	retry := store.Work{
		ID: id, Kind: x.work.Kind, Resource: x.work.Resource,
		NotBefore: x.e.Clock().Now().Add(x.e.Retry().BaseDelay),
	}
	err = x.rt.transactClaimed(ctx, x.rec.Operation.Ref.Tenant, x.work, func(tx store.Tx) error {
		if err := tx.CompleteWork(ctx, x.work); err != nil {
			return err
		}
		return tx.EnqueueWork(ctx, retry)
	})
	return stale(err)
}
