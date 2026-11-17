package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Reconcilable loads the operation req names and authorizes the client's
// principal to reconcile it: model.ActionOperationRead as Get asks it
// (this is what reveals the operation, so a foreign or unknown operation is
// not found and a denial reveals no state), then
// model.ActionOperationReconcile on the same operation at its current
// revision. It returns the stored record and the operation as Get would
// show it. Nothing is written.
func (s Service) Reconcilable(ctx context.Context, req model.Lookup[model.OperationRef]) (store.OperationRecord, model.Operation, error) {
	if s.c.Engine().IsClosed() {
		return store.OperationRecord{}, model.Operation{}, engine.ErrClosed()
	}
	if err := engine.FirstErr(req.Validate(), req.Ref.Validate()); err != nil {
		return store.OperationRecord{}, model.Operation{}, err
	}
	if req.Ref.Tenant != s.c.Principal().Tenant {
		return store.OperationRecord{}, model.Operation{}, engine.ErrNotFound()
	}
	var rec store.OperationRecord
	err := s.c.Engine().Store().View(ctx, req.Ref.Tenant, func(r store.Reader) error {
		var err error
		rec, err = r.Operation(ctx, req.Ref)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return store.OperationRecord{}, model.Operation{}, engine.ErrNotFound()
	}
	if err != nil {
		return store.OperationRecord{}, model.Operation{}, err
	}
	res := req.Ref.Ref()
	view, err := s.readable(ctx, res, rec)
	if err != nil {
		return store.OperationRecord{}, model.Operation{}, err
	}
	err = s.c.Authorize(ctx, policy.Authorization{
		Action: model.ActionOperationReconcile, Resource: res, Revision: rec.Operation.Revision,
	}, res)
	if err != nil {
		return store.OperationRecord{}, model.Operation{}, err
	}
	return rec, view, nil
}
