package operations

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// intentReplay names the stored intent of a replay command.
const intentReplay = "uma.intent.operation.replay"

type replayIntent struct {
	Target model.OperationRef `json:"target"`
}

// Replay queues a dead-lettered operation again, as a committed operation
// of its own, after authorizing the client's principal for
// model.ActionOperationReplay on the operation with its current revision.
// The operation returns to queued with its failure cleared and its
// quarantined work requeued with a fresh claim count; a delivery.updated
// event records the change. Execution then reauthorizes the originating
// actor as for any attempt. An operation in any other state is
// model.Conflict; a set ExpectedRevision must equal the operation's
// revision. Retrying with the same operation ID returns the stored replay.
func (s Service) Replay(ctx context.Context, req model.Lookup[model.OperationRef], cmd model.Command) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	if e.IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := engine.FirstErr(req.Validate(), req.Ref.Validate(), cmd.Validate(model.RevisionOptional)); err != nil {
		return model.Acceptance{}, err
	}
	if req.Ref.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var rec store.OperationRecord
	err := e.Store().View(ctx, req.Ref.Tenant, func(r store.Reader) error {
		var err error
		rec, err = r.Operation(ctx, req.Ref)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Ref.Ref()
	if err := c.Authorize(ctx, policy.Authorization{
		Action: model.ActionOperationReplay, Resource: res, Revision: rec.Operation.Revision,
	}, res); err != nil {
		return model.Acceptance{}, err
	}
	if err := RefuseHold(cmd); err != nil {
		return model.Acceptance{}, err
	}
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	ids := make([]model.ID, 5)
	for i := range ids {
		if ids[i], err = NewID(ctx, e); err != nil {
			return model.Acceptance{}, err
		}
	}
	intent, err := json.Marshal(replayIntent{Target: req.Ref})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Permanent, "operation intent cannot be stored")
	}
	tenant := req.Ref.Tenant
	opRef := model.OperationRef{Tenant: tenant, ID: ids[0]}
	replay := store.OperationRecord{
		Operation: model.Operation{Ref: opRef, State: model.Committed, Result: &res, Revision: model.Revision(ids[1])},
		Key: store.OperationKey{
			Tenant: tenant, Caller: c.Principal().Actor, Kind: model.ActionOperationReplay, Target: res, ID: cmd.Operation,
		},
		FingerprintVersion: FingerprintVersion1,
		Fingerprint:        replayFingerprint(req.Ref, cmd.ExpectedRevision),
		Command:            model.Extension{Name: intentReplay, Version: 1, Data: intent},
	}
	var got store.OperationRecord
	var created bool
	err = Transact(ctx, e, tenant, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimOperation(ctx, replay)
		if err != nil || !created {
			return err
		}
		return requeue(ctx, e, tx, c.Principal().Actor, req.Ref, cmd.ExpectedRevision, model.Revision(ids[2]), ids[3], ids[4])
	})
	if err != nil {
		if model.IsCode(err, model.Conflict) && !created {
			return model.Acceptance{}, model.NewError(model.Conflict, "operation ID already used with a different payload")
		}
		return model.Acceptance{}, err
	}
	return model.Acceptance{
		Operation: Shown(got),
		Durable:   e.Store().Properties().Durable,
		Replayed:  !created,
	}, nil
}

// requeue moves a dead-lettered operation back to queued within tx and
// returns its quarantined work to the queue, or enqueues fresh work when
// none is stored.
func requeue(ctx context.Context, e *engine.Engine, tx store.Tx, actor model.ActorID, ref model.OperationRef,
	expected, rev model.Revision, eventID, workID model.ID,
) error {
	cur, err := tx.Operation(ctx, ref)
	if err != nil {
		return err
	}
	if cur.Operation.State != model.DeadLettered {
		return model.NewError(model.Conflict, "operation is not dead-lettered")
	}
	if expected != "" && cur.Operation.Revision != expected {
		return model.NewError(model.Conflict, "operation revision changed")
	}
	next := cur.Operation.Clone()
	next.State, next.Failure, next.Revision, next.NextAttemptAt = model.Queued, nil, rev, nil
	if err := tx.UpdateOperation(ctx, next, cur.Operation.Revision, store.Fence{}); err != nil {
		return err
	}
	ev, err := DeliveryEvent(eventID, cur, cur.Operation.State, next, model.Audience{Visibility: model.Restricted},
		model.TriggerCommand, e.Clock().Now())
	if err != nil {
		return err
	}
	ev.Actor = actor
	if err := tx.AppendEvents(ctx, []model.Event{ev}); err != nil {
		return err
	}
	quarantined, err := tx.QuarantinedWork(ctx, ref.Ref())
	if err != nil {
		return err
	}
	for _, w := range quarantined {
		if err := tx.RequeueWork(ctx, w.ID); err != nil {
			return err
		}
	}
	if len(quarantined) > 0 {
		return nil
	}
	return tx.EnqueueWork(ctx, store.Work{ID: workID, Kind: WorkKindOperation, Resource: ref.Ref()})
}

// replayFingerprint returns the version 1 fingerprint of a replay: target
// and expected revision.
func replayFingerprint(ref model.OperationRef, expected model.Revision) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionOperationReplay))
	f.Set("target", Field().Ref(ref.Ref()))
	f.Set("expected_revision", Field().Str(string(expected)))
	return f.Sum()
}
