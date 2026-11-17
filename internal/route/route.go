// Package route loads a conversation's stored reply route with the inbox
// and channel its outbound effects use, and checks it before an attempt.
// The Streams and Interactions services share it.
package route

import (
	"context"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Route is a conversation with the inbox and channel its effects use.
type Route struct {
	Conv    model.Conversation
	Inbox   model.Inbox
	Channel model.Channel
}

// Load reads conversation ref with its inbox and channel.
func Load(ctx context.Context, r store.Reader, ref model.ConversationRef) (Route, error) {
	var rt Route
	var err error
	if rt.Conv, err = r.GetConversation(ctx, ref); err != nil {
		return rt, err
	}
	if rt.Inbox, err = r.GetInbox(ctx, rt.Conv.Ref.Inbox); err != nil {
		return rt, err
	}
	rt.Channel, err = r.GetChannel(ctx, rt.Inbox.Channel)
	return rt, err
}

// Check requires a stored reply route on the conversation's own channel:
// no other destination is ever substituted.
func Check(rt Route) error {
	switch r := rt.Conv.ReplyRoute; {
	case r.IsZero():
		return &model.Error{Code: model.Permanent, SafeMessage: "conversation has no stored reply route", Retry: model.NoRetry}
	case r.Channel != rt.Inbox.Channel || rt.Channel.Ref != rt.Inbox.Channel:
		return &model.Error{Code: model.Permanent, SafeMessage: "stored reply route is not on the conversation's channel", Retry: model.NoRetry}
	}
	return nil
}

// Recheck revalidates the stored route and reauthorizes the originating
// actor of rec on the conversation under audience.
func Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord, rt Route, audience model.Audience) error {
	if err := Check(rt); err != nil {
		return err
	}
	return actor.Authorize(ctx, policy.Authorization{
		Action: rec.Key.Kind, Resource: rt.Conv.Ref.Ref(), Audience: audience,
	}, rt.Conv.Ref.Ref())
}

// Declares fails with model.Unsupported unless the adapter registered for
// t declares every capability in caps.
func Declares(e *engine.Engine, t model.TypeKey, caps ...model.Capability) error {
	_, desc, ok := e.Adapter(t)
	if !ok {
		return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter not registered", Retry: model.NoRetry}
	}
	for _, c := range caps {
		if !slices.Contains(desc.Capabilities, c) {
			return &model.Error{Code: model.Unsupported, SafeMessage: "channel adapter does not declare " + string(c), Retry: model.NoRetry}
		}
	}
	return nil
}

// NotFound masks any not-found store answer, so a foreign or mismatched
// resource is never confirmed to exist.
func NotFound(err error) error {
	if model.IsCode(err, model.NotFound) {
		return engine.ErrNotFound()
	}
	return err
}
