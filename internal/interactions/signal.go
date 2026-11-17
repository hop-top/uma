package interactions

import (
	"context"
	"errors"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/route"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Signal sends an operational signal, such as typing, on the
// conversation's stored route under the requested audience. It calls the
// provider once, synchronously, bounded by ExpiresAt, and records nothing:
// no operation, event or message, so a signal is never replayed. The
// channel's adapter must declare signal. An expired signal, before or
// during the call, fails with model.DeadlineExpired; the adapter rejects an
// audience it cannot represent.
func (s Service) Signal(ctx context.Context, req model.Signal) error {
	e := s.c.Engine()
	if e.IsClosed() {
		return engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return err
	}
	if req.Conversation.Inbox.Tenant != s.c.Principal().Tenant {
		return engine.ErrNotFound()
	}
	if !e.Clock().Now().Before(req.ExpiresAt) {
		return expiredError()
	}
	var rt route.Route
	err := e.Store().View(ctx, s.c.Principal().Tenant, func(r store.Reader) error {
		var err error
		if rt, err = route.Load(ctx, r, req.Conversation); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return route.NotFound(err)
	}
	err = s.c.Authorize(ctx, policy.Authorization{
		Action: model.ActionConversationSignal, Resource: req.Conversation.Ref(), Audience: req.Audience,
	}, rt.Conv.Ref.Ref())
	if err != nil {
		return err
	}
	if err := route.Check(rt); err != nil {
		return err
	}
	done, err := e.Begin()
	if err != nil {
		return err
	}
	defer done()
	inst, desc, _, err := runtime.OpenOutbound(ctx, e, rt.Channel)
	if err != nil {
		return runtime.AsFailure(err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	if err := runtime.CapabilityReady(ctx, desc, inst, model.CapabilitySignal); err != nil {
		return err
	}
	remaining := req.ExpiresAt.Sub(e.Clock().Now())
	if remaining <= 0 {
		return expiredError()
	}
	callCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	err = inst.(adapter.Signaler).Signal(callCtx, adapter.SignalRequest{
		Route: rt.Conv.ReplyRoute, Kind: req.Kind, Audience: req.Audience, ExpiresAt: req.ExpiresAt,
	})
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return expiredError()
	}
	return runtime.AsFailure(err)
}
