package runtime

import (
	"context"
	"strconv"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Poison quarantine: a work item whose earlier claims all ended without
// completing it, by crash, lease expiry or an execution error, is taken out
// of the queue once its claim count exceeds Work.MaxClaims, instead of being
// run again. Its operation becomes dead_lettered, visible through
// Operations.Get and a delivery.updated event, and nothing runs until an
// administrative replay. A settled operation only completes the item.

// poisoned reports whether w exceeded the claim budget.
func (rt *Runtime) poisoned(w store.Work) bool { return w.Claims > rt.e.Work().MaxClaims }

// quarantine dead-letters the operation w executes and quarantines w,
// under w's fence.
func (rt *Runtime) quarantine(ctx context.Context, tenant model.TenantID, w store.Work) error {
	e := rt.e
	opRef, err := w.Resource.AsOperation()
	if err != nil || opRef.Tenant != tenant {
		return stale(rt.transactClaimed(ctx, tenant, w, func(tx store.Tx) error { return tx.QuarantineWork(ctx, w) }))
	}
	var rec store.OperationRecord
	var hist operations.History
	err = e.Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		if rec, err = r.Operation(ctx, opRef); err != nil {
			return err
		}
		hist, err = operations.LoadHistory(ctx, r, opRef)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return stale(rt.transactClaimed(ctx, tenant, w, func(tx store.Tx) error { return tx.QuarantineWork(ctx, w) }))
	case err != nil:
		return err
	case settled(rec.Operation.State):
		return rt.dropWork(ctx, tenant, w)
	}
	rev, err := operations.NewRevision(ctx, e)
	if err != nil {
		return err
	}
	eventID, err := operations.NewID(ctx, e)
	if err != nil {
		return err
	}
	next := rec.Operation.Clone()
	next.State, next.Revision, next.NextAttemptAt = model.DeadLettered, rev, nil
	next.Failure = deadLetter(w.Claims-1, hist.Unresolved())
	audience := model.Audience{Visibility: model.Restricted}
	ev, err := operations.DeliveryEvent(eventID, rec, rec.Operation.State, next, audience, model.TriggerSystem, e.Clock().Now())
	if err != nil {
		return err
	}
	err = rt.transactClaimed(ctx, tenant, w, func(tx store.Tx) error {
		if err := tx.UpdateOperation(ctx, next, rec.Operation.Revision, w.Fence); err != nil {
			return err
		}
		if err := tx.AppendEvents(ctx, []model.Event{ev}); err != nil {
			return err
		}
		return tx.QuarantineWork(ctx, w)
	})
	return stale(err)
}

// deadLetter is the failure of an operation whose work was quarantined
// after unfinished claims. An unresolved attempt keeps the outcome unknown:
// the replay reconciles it before any new attempt.
func deadLetter(unfinished int, unresolved bool) *model.Error {
	msg := "work quarantined after " + strconv.Itoa(unfinished) + " unfinished claims; replay once the cause is fixed"
	if unresolved {
		return &model.Error{Code: model.OutcomeUnknown, SafeMessage: msg, Retry: model.ReconcileFirst}
	}
	return &model.Error{Code: model.Permanent, SafeMessage: msg, Retry: model.NoRetry}
}
