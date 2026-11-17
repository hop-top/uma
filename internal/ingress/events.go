package ingress

import (
	"encoding/json"

	"hop.top/uma/model"
)

// Builtin event payloads emitted by ingress: references plus change summary,
// never message bodies. Every ingress event is provider-triggered.

// conversation.changed changes and message.updated changes.
const (
	changeCreated         = "created"
	changeStatus          = "status"
	changeParticipants    = "participants"
	changeEdited          = "edited"
	changeDeleted         = "deleted"
	changeReactionAdded   = "reaction_added"
	changeReactionRemoved = "reaction_removed"
	changeAudience        = model.MessageChangeAudienceNarrowed
)

type messageReceivedPayload struct {
	Message             model.MessageRef   `json:"message"`
	Trigger             string             `json:"trigger"`
	Source              model.TypeKey      `json:"source"`
	Author              *model.IdentityRef `json:"author,omitempty"`
	AuthorKind          string             `json:"author_kind"`
	InReplyTo           *model.MessageRef  `json:"in_reply_to,omitempty"`
	ConversationCreated bool               `json:"conversation_created,omitempty"`
}

type statusChange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type conversationChangedPayload struct {
	Conversation     model.ConversationRef `json:"conversation"`
	Changes          []string              `json:"changes"`
	Trigger          string                `json:"trigger"`
	Status           *statusChange         `json:"status,omitempty"`
	PreviousRevision model.Revision        `json:"previous_revision,omitempty"`
	Operation        *model.OperationRef   `json:"operation,omitempty"`
}

// event queues one committed event for append in the unit, caused by the
// ingress record.
func (u *ingressUnit) event(kind, schema string, res model.Ref, rev model.Revision, payload any) error {
	return u.eventAs(u.env.Audience, kind, schema, res, rev, payload)
}

// eventAs queues a builtin event under audience; an unset one is internal.
func (u *ingressUnit) eventAs(audience model.Audience, kind, schema string, res model.Ref, rev model.Revision, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	return u.appendEvent(audience, kind, res, rev, model.Extension{Name: schema, Version: model.PayloadVersion1, Data: data})
}

// appendEvent queues an event of kind carrying payload as is, under
// audience; an unset one is internal.
func (u *ingressUnit) appendEvent(audience model.Audience, kind string, res model.Ref, rev model.Revision, payload model.Extension) error {
	id, err := u.newID()
	if err != nil {
		return err
	}
	occurred := u.env.OccurredAt
	if occurred.IsZero() {
		occurred = u.now
	}
	if audience.Visibility == "" {
		audience = model.Audience{Visibility: model.Internal}
	}
	u.events = append(u.events, model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: kind,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		Actor: u.s.principal.Actor, OccurredAt: occurred.UTC(), RecordedAt: u.now,
		Correlation: u.in.Correlation, Causation: u.id, Audience: audience, Revision: rev,
		Payload: payload,
	})
	return nil
}

func conversationResource(c model.ConversationRef) model.Ref {
	return model.Ref{Tenant: c.Inbox.Tenant, Kind: model.KindConversation, Inbox: c.Inbox.ID, ID: c.ID}
}

func messageResource(m model.MessageRef) model.Ref {
	in := m.Conversation.Inbox
	return model.Ref{Tenant: in.Tenant, Kind: model.KindMessage, Inbox: in.ID, ID: m.ID}
}

// conversationChanged queues conversation.changed: provider-triggered at
// ingress, command-triggered by its operation for a start.
func (u *ingressUnit) conversationChanged(c model.Conversation, changes []string, status *statusChange, prev model.Revision) error {
	p := conversationChangedPayload{Conversation: c.Ref, Changes: changes, Trigger: model.TriggerProvider, Status: status, PreviousRevision: prev}
	if u.start != nil {
		op := *u.start
		p.Trigger, p.Operation = model.TriggerCommand, &op
	}
	return u.event(model.EventConversationChanged, model.SchemaConversationChanged, conversationResource(c.Ref), c.Revision, p)
}

// providerStateChanged queues conversation.changed provider_state for c,
// provider-triggered, internal audience: provider state is desk business.
func (u *ingressUnit) providerStateChanged(c model.Conversation, prev model.Revision) error {
	p := conversationChangedPayload{
		Conversation: c.Ref, Changes: []string{model.ConversationFieldProviderState},
		Trigger: model.TriggerProvider, PreviousRevision: prev,
	}
	return u.eventAs(model.Audience{Visibility: model.Internal}, model.EventConversationChanged, model.SchemaConversationChanged,
		conversationResource(c.Ref), c.Revision, p)
}

func (u *ingressUnit) messageReceived(m model.Message, sender *model.Identity, created bool) error {
	p := messageReceivedPayload{
		Message: m.Ref, Trigger: model.TriggerProvider, Source: m.Source, AuthorKind: m.Author.Kind, InReplyTo: m.InReplyTo, ConversationCreated: created,
	}
	if sender != nil {
		ref := sender.Ref
		p.Author = &ref
	}
	return u.event(model.EventMessageReceived, model.SchemaMessageReceived, messageResource(m.Ref), m.Revision, p)
}

// messageUpdated queues message.updated under the message's audience, as
// stored after the update.
func (u *ingressUnit) messageUpdated(m model.Message, change string, prev model.Revision, r *model.PayloadReaction) error {
	return u.eventAs(m.Audience, model.EventMessageUpdated, model.SchemaMessageUpdated, messageResource(m.Ref), m.Revision,
		model.MessageUpdatedPayload{
			Message: m.Ref, Change: change, Trigger: model.TriggerProvider, PreviousRevision: prev,
			SourceRevision: u.env.SourceRevision, Reaction: r,
		})
}

// interactionReceived queues interaction.received under the interaction's
// audience: identifiers only, never fields, arguments or the token.
func (u *ingressUnit) interactionReceived(in model.Interaction, actor *model.Identity, created bool) error {
	return u.eventAs(in.Audience, model.EventInteractionReceived, model.SchemaInteractionReceived, in.Ref.Ref(), in.Revision,
		model.InteractionReceivedPayload{
			Interaction: in.Ref, Trigger: model.TriggerProvider, Source: in.Source, Kind: in.Kind,
			Actor: actor.Ref, ActorKind: actor.Kind, Action: in.Action, Command: in.Command,
			Message: in.Message, ConversationCreated: created,
		})
}
