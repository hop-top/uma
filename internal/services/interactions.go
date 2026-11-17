package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// GetInteraction returns a stored interaction the principal may read under
// the interaction's own audience, never with its response token. An
// interaction outside the conversation, inbox or tenant the reference names
// is not found. It is the body of Interactions.Get.
func GetInteraction(ctx context.Context, c *engine.Client, req model.Lookup[model.InteractionRef]) (model.Interaction, error) {
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Interaction{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Interaction{}, err
	}
	if req.Ref.Scope() != c.Principal().Tenant {
		return model.Interaction{}, engine.ErrNotFound()
	}
	var in model.Interaction
	if err := view(ctx, c, func(r store.Reader) (err error) {
		in, err = r.Interaction(ctx, req.Ref)
		return err
	}); err != nil {
		return model.Interaction{}, err
	}
	if in.Ref != req.Ref {
		return model.Interaction{}, engine.ErrNotFound()
	}
	if err := c.Authorize(ctx, engine.InteractionRead(in), in.Ref.Ref()); err != nil {
		return model.Interaction{}, err
	}
	return in.Clone(), nil
}

// ListInteractions pages the interactions the principal may read, each
// under its own audience, in recording order, optionally limited to
// Filter.Inboxes, Filter.Conversation and Filter.Identity, the actor's
// identity. Interactions the principal may not read are skipped without
// trace. No item carries a response token. It is the body of
// Interactions.List.
func ListInteractions(ctx context.Context, c *engine.Client, req model.ListRequest) (model.Page[model.Interaction], error) {
	return list(ctx, c, listQuery[model.Interaction]{
		name:   "interactions",
		fields: filterFields{inboxes: true, conversation: true, identity: true},
		read:   store.Reader.Interactions,
		within: func(f model.Filter, in model.Interaction) bool {
			return inInboxes(f, in.Ref.Conversation.Inbox) && inConversation(f, in.Ref.Conversation) &&
				(f.Identity == nil || *f.Identity == in.Actor.Identity)
		},
		admit: func(ctx context.Context, c *engine.Client, in model.Interaction) error {
			return c.Authorize(ctx, engine.InteractionRead(in), in.Ref.Ref())
		},
		clone: model.Interaction.Clone,
	}, req)
}
