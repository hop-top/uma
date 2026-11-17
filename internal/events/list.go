package events

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// List limits: a zero ListEvents.Limit scans DefaultListLimit events;
// larger requests are capped at MaxListLimit. They equal the Poll limits.
const (
	DefaultListLimit = defaultPollLimit
	MaxListLimit     = maxPollLimit
)

// List reads one page of the tenant event log without subscription state.
//
// The call is authorized for model.ActionEventList on the tenant's event
// log before anything is read. One read unit then resolves the start
// position (the retained beginning, the head, or req.After) and makes one
// store read of the limit's events through the filter, which the store
// applies whole (Inboxes, Conversation, Kinds and Since); nothing is
// written.
// Each scanned event passes a gate fresh to this call, as in Poll, and
// denied events are dropped without trace. Next and Complete are the store
// page's, so Next moves past denied events and is a checkpoint a
// subscription can open at. Events are returned as stored: List has no
// payload major to negotiate, and each event's Payload names its schema
// and version.
func (s Service) List(ctx context.Context, req model.ListEvents) (model.Page[model.Event], error) {
	if s.c.Engine().IsClosed() {
		return model.Page[model.Event]{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Page[model.Event]{}, err
	}
	if err := s.checkFilterRefs(req.Filter); err != nil {
		return model.Page[model.Event]{}, err
	}
	tenant := s.c.Principal().Tenant
	log := model.EventLogRef(tenant)
	if err := s.c.Authorize(ctx, policy.Authorization{Action: model.ActionEventList, Resource: log}, log); err != nil {
		return model.Page[model.Event]{}, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = DefaultListLimit
	}
	limit = min(limit, MaxListLimit)
	var page model.Page[model.Event]
	err := s.c.Engine().Store().View(ctx, tenant, func(r store.Reader) error {
		after, err := listStart(ctx, r, req)
		if err != nil {
			return err
		}
		page, err = r.Events(ctx, req.Filter, after, limit)
		if model.IsCode(err, model.CursorExpired) {
			return errListCursorExpired()
		}
		return err
	})
	if err != nil {
		return model.Page[model.Event]{}, err
	}
	kept, err := newEventGate(s.c).keep(ctx, page.Items)
	if err != nil {
		return model.Page[model.Event]{}, err
	}
	out := model.Page[model.Event]{Next: page.Next, Complete: page.Complete}
	for _, ev := range kept {
		out.Items = append(out.Items, ev.Clone())
	}
	return out, nil
}

// listStart resolves the position a List reads after within the unit:
// the retained beginning, the head, or the supplied checkpoint.
func listStart(ctx context.Context, r store.Reader, req model.ListEvents) (model.Cursor, error) {
	switch req.Start {
	case model.StartBeginning:
		return r.EventBase(ctx)
	case model.StartTail:
		return r.EventHead(ctx)
	}
	return req.After, nil
}

// errListCursorExpired reports a List position behind event retention with
// the recovery the caller must take.
func errListCursorExpired() error {
	return model.NewError(model.CursorExpired,
		"event list position is behind event retention; restart the list from "+
			"beginning or tail and reconcile from current resource state")
}
