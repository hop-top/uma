package events

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Get reads one committed event without subscription state.
//
// The call is authorized for model.ActionEventList on the tenant's event
// log before anything is read, as List is. One read unit then fetches the
// event by ID, and the event passes the gate Poll and List apply to each
// event: the principal must read its inbox and the resource it is about, at
// the event's audience.
//
// Every reason the principal receives no event is the one engine.ErrNotFound
// error, built fresh on each path so nothing but the call's own outcome can
// tell them apart: the event never existed, retention removed it, the ref is
// another tenant's, or the principal may not read it. A policy failure is
// not one of those reasons; it fails the call.
func (s Service) Get(ctx context.Context, req model.Lookup[model.EventRef]) (model.Event, error) {
	if s.c.Engine().IsClosed() {
		return model.Event{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Event{}, err
	}
	tenant := s.c.Principal().Tenant
	log := model.EventLogRef(tenant)
	if err := s.c.Authorize(ctx, policy.Authorization{Action: model.ActionEventList, Resource: log}, log); err != nil {
		return model.Event{}, err
	}
	if req.Ref.Tenant != tenant {
		return model.Event{}, engine.ErrNotFound()
	}
	var ev model.Event
	err := s.c.Engine().Store().View(ctx, tenant, func(r store.Reader) error {
		var err error
		ev, err = r.Event(ctx, req.Ref)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Event{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Event{}, err
	}
	ok, err := newEventGate(s.c).allows(ctx, ev)
	if err != nil {
		return model.Event{}, err
	}
	if !ok {
		return model.Event{}, engine.ErrNotFound()
	}
	return ev.Clone(), nil
}
