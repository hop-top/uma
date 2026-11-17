package operations

import (
	"context"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// ExpireReport counts what one ExpireHeld sweep wrote.
type ExpireReport struct {
	// Expired is the held operations this sweep moved to expired.
	Expired int
	// CarriedOver is the held operations this sweep marked as carried over.
	CarriedOver int
}

// ExpireHeld sweeps the tenant's held operations once: a proposal whose
// deadline (HeldBlock.ExpiresAt) has passed becomes expired, and one still
// held a full window after it was held is marked carried over
// (HeldBlock.CarriedOverAt). Neither is a decision a person made: nothing
// is applied, no work is queued and no trust counter moves.
//
// "Now" is the engine clock, the one a decision judges ExpiresAt against, so
// an operation the sweep leaves held is one Approve still accepts and one it
// expires is one Approve refuses. A store shared by several processes
// requires their clocks to agree (store/postgres.Options.Clock).
//
// Each operation is its own unit and a compare-and-swap on the revision the
// sweep read: sweeps racing in one process or many, or against a decision,
// move an operation once, and the loser skips it. A sweep is idempotent: an
// expired operation is no longer held, and a marker is set once. window <= 0
// marks nothing. An operation another unit changed since the scan is left to
// the next sweep.
func ExpireHeld(ctx context.Context, e *engine.Engine, tenant model.TenantID, window time.Duration) (ExpireReport, error) {
	var rep ExpireReport
	if e.IsClosed() {
		return rep, engine.ErrClosed()
	}
	done, err := e.Begin()
	if err != nil {
		return rep, err
	}
	defer done()
	scan, err := heldRefs(ctx, e, tenant)
	if err != nil {
		return rep, err
	}
	return expireRefs(ctx, e, tenant, scan, window)
}

// expireRefs sweeps refs, each as its own unit.
func expireRefs(ctx context.Context, e *engine.Engine, tenant model.TenantID, refs []model.OperationRef, window time.Duration) (ExpireReport, error) {
	var rep ExpireReport
	for _, ref := range refs {
		id, err := NewID(ctx, e)
		if err != nil {
			return rep, err
		}
		evID, err := NewID(ctx, e)
		if err != nil {
			return rep, err
		}
		var out sweepOutcome
		err = Transact(ctx, e, tenant, func(tx store.Tx) error {
			var err error
			out, err = sweepOne(ctx, e, tx, ref, window, model.Revision(id), evID)
			return err
		})
		switch {
		case model.IsCode(err, model.Conflict):
			continue // lost to another unit; the next sweep sees its result
		case err != nil:
			return rep, err
		}
		switch out {
		case sweptExpired:
			rep.Expired++
		case sweptCarried:
			rep.CarriedOver++
		}
	}
	return rep, nil
}

// heldRefs lists every held operation of the tenant, page by page.
func heldRefs(ctx context.Context, e *engine.Engine, tenant model.TenantID) ([]model.OperationRef, error) {
	var refs []model.OperationRef
	req := model.ListRequest{Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: heldPage}}
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		for {
			p, err := r.Operations(ctx, req)
			if err != nil {
				return err
			}
			for _, rec := range p.Items {
				refs = append(refs, rec.Operation.Ref)
			}
			if p.Complete || p.Next == "" || p.Next == req.Page.After {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	return refs, err
}

type sweepOutcome int

const (
	sweptNothing sweepOutcome = iota
	sweptExpired
	sweptCarried
)

// sweepOne re-reads ref in tx and writes its expiry or carry-over marker,
// when due, under rev. An operation no longer held is left alone.
func sweepOne(ctx context.Context, e *engine.Engine, tx store.Tx, ref model.OperationRef, window time.Duration, rev model.Revision, eventID model.ID) (sweepOutcome, error) {
	cur, err := tx.Operation(ctx, ref)
	switch {
	case model.IsCode(err, model.NotFound):
		return sweptNothing, nil
	case err != nil:
		return sweptNothing, err
	}
	h := cur.Operation.Held
	if h == nil || cur.Operation.State != model.Held {
		return sweptNothing, nil
	}
	now := e.Clock().Now().UTC()
	next := cur.Operation.Clone()
	nh := next.Held
	var out sweepOutcome
	switch {
	case h.ExpiresAt != nil && !now.Before(*h.ExpiresAt):
		next.State = model.Expired
		nh.Decision, nh.DecidedAt = model.HeldExpired, &now
		out = sweptExpired
	case window > 0 && h.CarriedOverAt == nil && !now.Before(cur.Operation.UpdatedAt.Add(window)):
		nh.CarriedOverAt = &now
		out = sweptCarried
	default:
		return sweptNothing, nil
	}
	next.Revision = rev
	if err := tx.UpdateOperation(ctx, next, cur.Operation.Revision, store.Fence{}); err != nil {
		return sweptNothing, err
	}
	if out == sweptExpired {
		if err := recordExpiry(ctx, tx, store.OperationRecord{Operation: next, Key: cur.Key}, eventID, now); err != nil {
			return sweptNothing, err
		}
	}
	return out, nil
}

// recordExpiry is the one place the expired transition commits besides the
// operation row: it appends operation.expired in the sweep's unit, once per
// transition, so state and event commit together or not at all. Expiry
// moves no trust counter.
func recordExpiry(ctx context.Context, tx store.Tx, rec store.OperationRecord, eventID model.ID, at time.Time) error {
	ev, err := ExpiredEvent(eventID, rec, at)
	if err != nil {
		return err
	}
	return tx.AppendEvents(ctx, []model.Event{ev})
}
