package runtime

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// refetchEffector runs refetch operations, which ingress records when two
// observations of a provider resource tie (see package ingress): one step
// reads the resource again through the adapter's Refetcher, applies the
// copy as authoritative through the channel's sink, and settles the
// operation committed. A step that dies before settling repeats: the read
// changes nothing at the provider and the copy, keyed by the operation, is
// a duplicate. A retryable failure leaves the item to be claimed again once
// its lease expires, so retries and quarantine apply as to any item; any
// other failure settles the operation failed and keeps the stored copy.
type refetchEffector struct{}

func (refetchEffector) Intent() string { return operations.IntentRefetch }

// Decode is never reached: Step runs the operation.
func (refetchEffector) Decode(model.Extension) (Job, error) {
	return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "refetch runs as a step", Retry: model.NoRetry}
}

func (refetchEffector) Step(ctx context.Context, e *engine.Engine, w store.Work, rec store.OperationRecord) error {
	in, err := operations.DecodeRefetchIntent(rec.Command)
	if err != nil {
		return settleStep(ctx, e, w, rec, AsFailure(err))
	}
	snk, err := ingress.Bind(ctx, e, model.Principal{Tenant: rec.Operation.Ref.Tenant, Actor: rec.Key.Caller}, in.Channel)
	if err != nil {
		return stepFailed(ctx, e, w, rec, err)
	}
	inst, desc, _, err := OpenOutbound(ctx, e, snk.Channel())
	if err != nil {
		return stepFailed(ctx, e, w, rec, err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	rf, ok := inst.(adapter.Refetcher)
	if !ok {
		return settleStep(ctx, e, w, rec, &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter cannot refetch", Retry: model.NoRetry})
	}
	if f := capabilityReady(ctx, desc, inst, model.CapabilityRefetch); f != nil {
		return stepFailed(ctx, e, w, rec, f)
	}
	env, err := rf.Refetch(ctx, adapter.RefetchRequest{Target: in.Target, Destination: in.Destination, References: in.References})
	if err != nil {
		return stepFailed(ctx, e, w, rec, safeAdapterError(ctx, err, "refetch failed"))
	}
	if env.Destination == "" {
		env.Destination = in.Destination
	}
	if _, err := snk.AcceptRefetched(ctx, rec.Operation.Ref, model.Ingress{Envelope: env}); err != nil {
		return stepFailed(ctx, e, w, rec, err)
	}
	return settleStep(ctx, e, w, rec, nil)
}

// stepFailed leaves the item for its next claim on a retryable failure and
// settles the operation failed on any other.
func stepFailed(ctx context.Context, e *engine.Engine, w store.Work, rec store.OperationRecord, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f := AsFailure(err)
	if engine.Retryable(f) {
		return model.NewError(model.Transient, "refetch unavailable; retried after the lease")
	}
	return settleStep(ctx, e, w, rec, noRetry(f))
}

// settleStep settles a refetch operation, committed without failure or
// failed with it, with its delivery event, and completes the item under
// its fence.
func settleStep(ctx context.Context, e *engine.Engine, w store.Work, rec store.OperationRecord, failure *model.Error) error {
	rev, err := operations.NewRevision(ctx, e)
	if err != nil {
		return err
	}
	id, err := operations.NewID(ctx, e)
	if err != nil {
		return err
	}
	next := rec.Operation.Clone()
	next.State, next.Failure, next.Revision = model.Committed, nil, rev
	if failure != nil {
		next.State, next.Failure = model.Failed, failure
	}
	ev, err := operations.DeliveryEvent(id, rec, rec.Operation.State, next, model.Audience{Visibility: model.Internal},
		model.TriggerSystem, e.Clock().Now())
	if err != nil {
		return err
	}
	err = operations.Transact(ctx, e, next.Ref.Tenant, func(tx store.Tx) error {
		if err := tx.UpdateOperation(ctx, next, rec.Operation.Revision, w.Fence); err != nil {
			return err
		}
		if err := tx.AppendEvents(ctx, []model.Event{ev}); err != nil {
			return err
		}
		return tx.CompleteWork(ctx, w)
	})
	return stale(err)
}
