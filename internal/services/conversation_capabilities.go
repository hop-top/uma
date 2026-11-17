package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Capabilities reports the current capability status of the channel a
// conversation's inbox is bound to, as Configuration.Capabilities does,
// for a principal that may read the conversation's internal content:
// conversation.read on the conversation at its revision under the
// internal audience, never channel.read. It returns statuses only (name,
// availability, reason, limits); no channel configuration or credential
// reference leaves it. The read is refused before any adapter instance
// opens, so a caller that may not read it causes no provider I/O.
func (s Conversations) Capabilities(ctx context.Context, req model.Lookup[model.ConversationRef]) ([]model.CapabilityStatus, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return nil, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if req.Ref.Inbox.Tenant != c.Principal().Tenant {
		return nil, engine.ErrNotFound()
	}
	var conv model.Conversation
	var ch model.Channel
	if err := view(ctx, c, func(r store.Reader) error {
		if err := loadConversation(ctx, r, req.Ref, &conv); err != nil {
			return err
		}
		inbox, err := r.GetInbox(ctx, conv.Ref.Inbox)
		if err := exactRef(err, inbox.Ref == conv.Ref.Inbox); err != nil {
			return err
		}
		ch, err = r.GetChannel(ctx, inbox.Channel)
		return exactRef(err, ch.Ref == inbox.Channel)
	}); err != nil {
		return nil, err
	}
	if err := c.Authorize(ctx, conversationCapabilitiesRead(conv), req.Ref.Ref()); err != nil {
		return nil, err
	}
	inst, desc, _, err := runtime.OpenOutbound(ctx, e, ch)
	if err != nil {
		return nil, runtime.AsFailure(err)
	}
	defer func() { _ = inst.Close(context.WithoutCancel(ctx)) }()
	return currentCapabilities(ctx, desc, inst)
}

// conversationCapabilitiesRead authorizes reading a conversation's
// capability statuses: conversation.read under the internal audience, so
// a policy that limits a participant to public content refuses it.
func conversationCapabilitiesRead(conv model.Conversation) policy.Authorization {
	a := conversationRead(conv)
	a.Audience = model.Audience{Visibility: model.Internal}
	return a
}
