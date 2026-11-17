package ingress

import (
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Interactions. An envelope of kind interaction carries one structured
// interaction (a command, action or submission) its sender, the actor,
// performed. Ingress records it as an interaction, never as a message: one
// interaction record, with the provider's response token beside it in
// storage, and one interaction.received event. The raw ingress copy never
// holds the token. The envelope's event ID is the provider interaction ID,
// so a redelivery is a duplicate under the ordinary event claim.

// checkInteractionEnvelope validates an interaction envelope against the
// binding: a valid, acceptable interaction (the options kind is evaluated
// synchronously, never accepted) that its sender performed, with every
// reference on the bound channel, its context among the envelope's
// references, and nothing a message, work item or delivery report carries.
func (s *Sink) checkInteractionEnvelope(env model.Envelope) error {
	in := env.Interaction
	switch {
	case in == nil:
		return model.NewError(model.Validation, "envelope.interaction: required by an interaction envelope")
	case env.EventID == "" || env.EventID != in.ID:
		return model.NewError(model.Validation, "envelope.event_id: must be the interaction ID")
	case env.MessageID != "":
		return model.NewError(model.Validation, "envelope.message_id: an interaction names no message")
	case len(env.Content.Parts) > 0:
		return model.NewError(model.Validation, "envelope.content: an interaction carries no content")
	case env.SystemActor:
		return model.NewError(model.Validation, "envelope.system_actor: an interaction has an actor")
	case env.WorkItem != nil:
		return model.NewError(model.Validation, "envelope.work_item: an interaction carries none")
	case env.Delivery != nil:
		return model.NewError(model.Validation, "envelope.delivery: an interaction carries none")
	}
	if err := s.checkInteraction(*in); err != nil {
		return err
	}
	if in.Kind == model.InteractionOptions {
		return model.NewError(model.Validation, "interaction.kind: options are evaluated, not accepted")
	}
	if env.Sender != in.Actor {
		return model.NewError(model.Validation, "envelope.sender: must be the interaction actor")
	}
	for _, r := range env.References {
		if err := s.ownRef("envelope.references", r); err != nil {
			return err
		}
	}
	for _, r := range in.Context {
		if !slices.Contains(env.References, r) {
			return model.NewError(model.Validation, "interaction.context: must be among the envelope references")
		}
	}
	if err := s.checkSender(env); err != nil {
		return err
	}
	return env.Audience.Validate()
}

// rawInput is the ingress as the raw ingress record keeps it: the
// interaction without its response token, which lives in the interaction
// record alone. It leaves the unit's own copy untouched.
func (u *ingressUnit) rawInput() model.Ingress {
	in := u.in
	if i := in.Envelope.Interaction; i != nil {
		stripped := i.Clone()
		stripped.ResponseToken = ""
		in.Envelope.Interaction = &stripped
	}
	return in
}

// applyInteraction records an interaction: it claims the actor's identity,
// finds the conversation, stores the record with its token and queues
// interaction.received. It writes no message.
func (u *ingressUnit) applyInteraction() error {
	in := *u.env.Interaction
	actor, err := u.resolveSender()
	if err != nil {
		return err
	}
	src, err := u.sourceMessage(in)
	if err != nil {
		return err
	}
	conv, created, err := u.interactionConversation(actor, src)
	if err != nil {
		return err
	}
	id, err := u.newID()
	if err != nil {
		return err
	}
	rev, err := u.newRevision()
	if err != nil {
		return err
	}
	rec := model.Interaction{
		Ref:  model.InteractionRef{Conversation: conv.Ref, ID: id},
		Kind: in.Kind, Actor: u.author(actor), Bot: in.Bot, Action: in.Action, Command: in.Command,
		Arguments: in.Arguments, Fields: in.Fields, MessageExternal: in.Message, Context: in.Context,
		External: []model.ExternalRef{model.ProviderInteractionRef(u.s.channel.Ref, in.ID)},
		Audience: u.env.Audience, OccurredAt: u.env.OccurredAt, RecordedAt: u.now,
		Deadline: in.Deadline, RespondUntil: in.ResponseExpiresAt, Revision: rev,
		Source: u.s.channel.Type, Metadata: u.metadata(nil),
	}
	if rec.OccurredAt.IsZero() {
		rec.OccurredAt = u.now
	}
	if src != nil {
		ref := src.Ref
		rec.Message = &ref
	}
	if err := u.tx.PutInteraction(u.ctx, store.InteractionRecord{
		Interaction: rec, Ingress: u.id, ResponseToken: in.ResponseToken, TokenExpiresAt: tokenExpiry(in),
	}); err != nil {
		return err
	}
	return u.interactionReceived(rec, actor, created)
}

// tokenExpiry is the expiry stored with the response token: the input's
// while it holds a token, none otherwise.
func tokenExpiry(in model.InteractionInput) (t time.Time) {
	if in.ResponseToken == "" {
		return time.Time{}
	}
	return in.ResponseExpiresAt
}

// sourceMessage resolves the provider message the control sits on to the
// local message that holds it, when the adapter assigns provider message
// IDs and exactly one message of the inbox holds the reference. A
// sender-chosen ID never resolves: anyone could repeat it.
func (u *ingressUnit) sourceMessage(in model.InteractionInput) (*model.Message, error) {
	if in.Message == nil || !engine.ProviderAssigned(u.s.desc) {
		return nil, nil
	}
	msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, model.ProviderMessageRef(u.s.channel.Ref, in.Message.ID), matchLimit)
	if err != nil || len(msgs) != 1 {
		return nil, err
	}
	return &msgs[0], nil
}

// interactionConversation finds the conversation of an interaction. With a
// resolved source message the actor who owns or participates in its
// conversation joins it, without selection or status change. Otherwise the
// inbox's selection decides, as for a message, and may create the
// conversation.
func (u *ingressUnit) interactionConversation(actor *model.Identity, src *model.Message) (model.Conversation, bool, error) {
	if src != nil {
		conv, err := u.tx.GetConversation(u.ctx, src.Ref.Conversation)
		if err != nil {
			return model.Conversation{}, false, err
		}
		if conv.Owner != nil && *conv.Owner == actor.Ref || isParticipant(conv, actor.Ref) {
			return conv, false, nil
		}
	}
	return u.selectConversation(actor)
}
