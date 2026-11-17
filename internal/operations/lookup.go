package operations

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Lookup finds the operation the client's principal claimed under the
// operation key (principal tenant, principal actor, Kind, Target, ID) and
// returns it as Get would, after the same model.ActionOperationRead check.
// A target in another tenant is not found before any read; an unclaimed,
// foreign or purged key is not found. It writes nothing and emits no event.
func (s Service) Lookup(ctx context.Context, req model.OperationLookup) (model.Operation, error) {
	if s.c.Engine().IsClosed() {
		return model.Operation{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Operation{}, err
	}
	p := s.c.Principal()
	if req.Target.Tenant != p.Tenant {
		return model.Operation{}, engine.ErrNotFound()
	}
	key := store.OperationKey{Tenant: p.Tenant, Caller: p.Actor, Kind: req.Kind, Target: req.Target, ID: req.ID}
	var rec store.OperationRecord
	err := s.c.Engine().Store().View(ctx, p.Tenant, func(r store.Reader) error {
		var err error
		rec, err = r.OperationByKey(ctx, key)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Operation{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Operation{}, err
	}
	return s.readable(ctx, rec.Operation.Ref.Ref(), rec)
}
