package extensions

import (
	"context"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// target is an extension target as storage records it: its scope, the
// channel whose adapter serves it, the provider route of an effect on it
// and its revision.
type target struct {
	owner    model.Ref
	channel  model.Channel
	route    model.ExternalRef
	revision model.Revision
}

// resolve reads ref's record and channel. A channel or inbox target routes
// to the channel itself; a conversation target to the conversation's
// stored reply route. Other kinds have no channel core can resolve and are
// model.Unsupported.
func resolve(ctx context.Context, r store.Reader, ref model.Ref) (target, error) {
	var t target
	switch ref.Kind {
	case model.KindChannel:
		cr, err := ref.AsChannel()
		if err != nil {
			return t, err
		}
		if t.channel, err = r.GetChannel(ctx, cr); err != nil {
			return t, err
		}
		t.owner, t.revision = t.channel.Ref.Ref(), t.channel.Revision
	case model.KindInbox:
		ir, err := ref.AsInbox()
		if err != nil {
			return t, err
		}
		in, err := r.GetInbox(ctx, ir)
		if err != nil {
			return t, err
		}
		if t.channel, err = r.GetChannel(ctx, in.Channel); err != nil {
			return t, err
		}
		t.owner, t.revision = in.Ref.Ref(), in.Revision
	case model.KindConversation:
		cr, err := ref.AsConversation()
		if err != nil {
			return t, err
		}
		conv, err := r.GetConversation(ctx, cr)
		if err != nil {
			return t, err
		}
		in, err := r.GetInbox(ctx, conv.Ref.Inbox)
		if err != nil {
			return t, err
		}
		if t.channel, err = r.GetChannel(ctx, in.Channel); err != nil {
			return t, err
		}
		t.owner, t.revision, t.route = conv.Ref.Ref(), conv.Revision, conv.ReplyRoute
		return t, nil
	default:
		return t, &model.Error{
			Code: model.Unsupported, SafeMessage: "extension target must be a channel, inbox or conversation", Retry: model.NoRetry,
		}
	}
	t.route = model.ExternalRef{Channel: t.channel.Ref}
	return t, nil
}

// messageTarget resolves the message an effect on a conversation names to
// its provider reference on the conversation's channel. A nil message is
// no target (nil, nil). A message that is not a live message of the
// conversation with a provider reference there is model.Validation.
func messageTarget(ctx context.Context, r store.Reader, t target, ref *model.MessageRef) (*model.ExternalRef, error) {
	if ref == nil {
		return nil, nil
	}
	m, err := r.GetMessage(ctx, *ref)
	switch {
	case model.IsCode(err, model.NotFound) || (err == nil && m.Ref != *ref):
		return nil, model.NewError(model.Validation, "message is not in the conversation")
	case err != nil:
		return nil, err
	case m.Deleted || m.Redacted:
		return nil, model.NewError(model.Validation, "message is deleted or redacted")
	}
	p := operations.ProviderMessageRef(m, t.route.Channel)
	if p == nil {
		return nil, model.NewError(model.Validation, "message has no provider message on the conversation's channel")
	}
	return p, nil
}
