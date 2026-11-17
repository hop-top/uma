package services

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Raw reads the provider's whole copy of a message on demand. It is
// authorized exactly as Get is, under the message's audience, before
// anything about the message is told. A redacted or deleted message is
// model.Conflict, as every other operation on one is: its content was
// scrubbed and the provider's copy is not served around that. The channel's
// adapter must declare model.CapabilityRawRead (model.Unsupported
// otherwise) and report it available. One synchronous provider read runs
// bounded by the adapter's own size limit; nothing is stored, recorded or
// evented, so a read leaves no copy and no trace in uma.
func (s Messages) Raw(ctx context.Context, req model.Lookup[model.MessageRef]) (model.RawMessage, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.RawMessage{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.RawMessage{}, err
	}
	if req.Ref.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.RawMessage{}, engine.ErrNotFound()
	}
	var m model.Message
	var rt route.Route
	if err := view(ctx, c, func(r store.Reader) (err error) {
		if m, err = r.GetMessage(ctx, req.Ref); err != nil {
			return err
		}
		if m.Ref != req.Ref {
			return engine.ErrNotFound()
		}
		rt, err = route.Load(ctx, r, req.Ref.Conversation)
		return err
	}); err != nil {
		return model.RawMessage{}, route.NotFound(err)
	}
	if err := c.Authorize(ctx, engine.MessageRead(m), req.Ref.Ref()); err != nil {
		return model.RawMessage{}, err
	}
	switch {
	case m.Redacted:
		return model.RawMessage{}, model.NewError(model.Conflict, "message redacted")
	case m.Deleted:
		return model.RawMessage{}, model.NewError(model.Conflict, "message deleted")
	}
	if err := route.Declares(e, rt.Channel.Type, model.CapabilityRawRead); err != nil {
		return model.RawMessage{}, err
	}
	ref := operations.ProviderMessageRef(m, rt.Channel.Ref)
	if ref == nil {
		return model.RawMessage{}, model.NewError(model.Conflict, "message has no provider copy")
	}
	inst, desc, _, err := runtime.OpenOutbound(ctx, e, rt.Channel)
	if err != nil {
		return model.RawMessage{}, runtime.AsFailure(err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	if f := runtime.CapabilityReady(ctx, desc, inst, model.CapabilityRawRead); f != nil {
		return model.RawMessage{}, f
	}
	out, err := inst.(adapter.RawReader).ReadRaw(ctx, adapter.RawRequest{Message: *ref})
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return model.RawMessage{}, ctx.Err()
	default:
		return model.RawMessage{}, runtime.AsFailure(err)
	}
	if len(out.Bytes) == 0 || out.MediaType == "" {
		return model.RawMessage{}, model.NewError(model.Permanent, "provider returned no message copy")
	}
	return model.RawMessage{Ref: req.Ref, MediaType: out.MediaType, Size: int64(len(out.Bytes)), Bytes: out.Bytes}, nil
}
