package operations

import (
	"context"
	"encoding/json"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// outbound is the message a reply operation produces: recorded with its
// message.created event in the claiming unit, before any provider delivery,
// so the conversation holds what uma said and an echo of it is recognized
// at ingress. Its IDs are allocated before the unit; its revision is the
// one submit allocates for apply.
type outbound struct {
	e      *engine.Engine
	actor  model.ActorID
	ref    model.MessageRef
	event  model.ID
	source model.TypeKey
	msg    outboundMessage
	corr   model.CorrelationID
}

// outboundMessage is what an outbound message records: its content and
// audience, the message it answers, the interaction it answers or is
// correlated with, its message.created origin (model.MessageOriginReply,
// model.MessageOriginNote, model.MessageOriginStart or
// model.MessageOriginResponse) and, for a start, whether it created its
// conversation.
type outboundMessage struct {
	content             model.Content
	audience            model.Audience
	inReplyTo           *model.MessageRef
	inResponseTo        *model.InteractionRef
	origin              string
	conversationCreated bool
}

func newOutbound(ctx context.Context, c *engine.Client, conv model.ConversationRef, corr model.CorrelationID, source model.TypeKey, msg outboundMessage) (*outbound, error) {
	ids := make([]model.ID, 2)
	for i := range ids {
		id, err := NewID(ctx, c.Engine())
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	return &outbound{
		e: c.Engine(), actor: c.Principal().Actor, ref: model.MessageRef{Conversation: conv, ID: ids[0]},
		event: ids[1], source: source, msg: msg, corr: corr,
	}, nil
}

// messageCreated is the version 1 message.created payload of an outbound
// message.
type messageCreated struct {
	Message             model.MessageRef   `json:"message"`
	Trigger             string             `json:"trigger"`
	Origin              string             `json:"origin"`
	Operation           model.OperationRef `json:"operation"`
	Source              model.TypeKey      `json:"source,omitempty"`
	InReplyTo           *model.MessageRef  `json:"in_reply_to,omitempty"`
	ConversationCreated bool               `json:"conversation_created,omitempty"`
	// InResponseTo is the interaction the message answers or is
	// correlated with.
	InResponseTo *model.InteractionRef `json:"in_response_to,omitempty"`
}

func (o *outbound) write(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
	now := o.e.Clock().Now().UTC()
	m := model.Message{
		Ref: o.ref, Content: o.msg.content.Clone(), Audience: o.msg.audience,
		OccurredAt: now, RecordedAt: now, Revision: rev, Source: o.source,
		Origin: &model.Origin{Actor: o.actor, Operation: op}, InReplyTo: o.msg.inReplyTo,
		InResponseTo: o.msg.inResponseTo,
	}
	task, err := parentTask(ctx, tx, o.msg.inReplyTo)
	if err != nil {
		return err
	}
	m.Task = task
	if err := tx.PutMessage(ctx, m, ""); err != nil {
		return err
	}
	data, err := json.Marshal(messageCreated{
		Message: o.ref, Trigger: model.TriggerCommand, Origin: o.msg.origin, Operation: op,
		Source: o.source, InReplyTo: o.msg.inReplyTo, ConversationCreated: o.msg.conversationCreated,
		InResponseTo: o.msg.inResponseTo,
	})
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	res := o.ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: o.event, Kind: model.EventMessageCreated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		Actor: o.actor, OccurredAt: now, RecordedAt: now, Correlation: o.corr,
		Audience: o.msg.audience, Revision: rev,
		Payload: model.Extension{Name: model.SchemaMessageCreated, Version: model.PayloadVersion1, Data: data},
	}})
}

// parentTask returns the task of a reply's parent, read in the claiming
// unit: a reply to a message bound to a task joins that task's history
// (Message.Task). A parent gone since the reply was accepted binds none.
func parentTask(ctx context.Context, tx store.Tx, parent *model.MessageRef) (*model.TaskRef, error) {
	if parent == nil {
		return nil, nil
	}
	p, err := tx.GetMessage(ctx, *parent)
	switch {
	case model.IsCode(err, model.NotFound):
		return nil, nil
	case err != nil:
		return nil, err
	case p.Ref != *parent || p.Task == nil:
		return nil, nil
	}
	t := *p.Task
	return &t, nil
}

// RecordMessageReferences adds the provider message IDs (model.ProviderMessageRef)
// and email Message-IDs among op's provider references to the message op
// produced, under op's revision, so an echo of that message is recognized
// at ingress and a mail reply to it threads. An ID another message of the
// inbox already holds is skipped: one provider message ID names one
// message. When that message is an inbound copy, the provider's echo of
// this send recorded before the send's answer (it carried no echo
// reference), the copy is linked to the outbound message (Message.EchoOf)
// with a message.updated event of change echo; it is never deleted. Only a
// provider-assigned message ID links a copy: a sender-chosen one, such as
// an RFC 5322 Message-ID, proves nothing.
// Interactions recorded on a provider message ID of the message before it
// held that ID are linked to it (linkInteractions).
// Nothing changes when op produced no message, every ID is recorded and
// every interaction linked, so it may run more than once.
func RecordMessageReferences(ctx context.Context, tx store.Tx, e *engine.Engine, op model.Operation) error {
	if op.ResultMessage == nil || len(op.ProviderReferences) == 0 {
		return nil
	}
	m, err := tx.GetMessage(ctx, *op.ResultMessage)
	if model.IsCode(err, model.NotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// holds collects the IDs the message holds once this returns: those it
	// held already and those added.
	var add, holds []model.ExternalRef
	for _, r := range op.ProviderReferences {
		if !messageReference(r) {
			continue
		}
		if slices.Contains(m.External, r) {
			holds = append(holds, r)
			continue
		}
		held, err := tx.MessagesByReference(ctx, m.Ref.Conversation.Inbox, r, 1)
		if model.IsCode(err, model.Conflict) {
			continue
		}
		if err != nil {
			return err
		}
		if len(held) > 0 {
			if echoCopy(e, held[0], m, r) {
				if err := linkEcho(ctx, tx, e, held[0], m, op); err != nil {
					return err
				}
			}
			continue
		}
		add = append(add, r)
	}
	if len(add) > 0 {
		prev := m.Revision
		m.External = append(slices.Clone(m.External), add...)
		m.Revision = op.Revision
		if err := tx.PutMessage(ctx, m, prev); err != nil {
			return err
		}
	}
	return linkInteractions(ctx, tx, e, m, append(holds, add...))
}

// linkPage is the page size linkInteractions reads unlinked interactions in.
const linkPage = 100

// linkInteractions names m the local message (Interaction.Message) of every
// interaction of its inbox recorded on one of refs, provider message IDs m
// holds, while no message held it: a control pressed before the answer of
// the send that posted it was recorded. Ingress resolves the control's
// message the same way when the ID is already held, so whichever of the
// two units commits second links the interaction; when they overlap, each
// reads what the other writes and the store fails one, which reruns.
// As at ingress, only a provider message ID that m's adapter assigns
// links: a sender-chosen one proves nothing.
//
// A link emits no event. It completes what interaction.received could not
// name yet, as recording a sent message's provider IDs completes the
// message without one; a consumer that needs the message reads the
// interaction, whose revision the link moves.
func linkInteractions(ctx context.Context, tx store.Tx, e *engine.Engine, m model.Message, refs []model.ExternalRef) error {
	if len(refs) == 0 {
		return nil
	}
	if _, d, ok := e.Adapter(m.Source); !ok || !engine.ProviderAssigned(d) {
		return nil
	}
	for _, r := range refs {
		if r.Namespace != model.ProviderMessageNamespace || r.Kind != model.RefKindMessage {
			continue
		}
		var after model.Cursor
		for {
			p, err := tx.UnlinkedInteractions(ctx, m.Ref.Conversation.Inbox, r, model.PageRequest{Limit: linkPage, After: after})
			if err != nil {
				return err
			}
			for _, in := range p.Items {
				rev, err := NewRevision(ctx, e)
				if err != nil {
					return err
				}
				if _, err := tx.LinkInteraction(ctx, store.InteractionLink{Interaction: in.Ref, Message: m.Ref, Revision: rev}); err != nil {
					return err
				}
			}
			if p.Complete || p.Next == "" || p.Next == after {
				break
			}
			after = p.Next
		}
	}
	return nil
}

// echoCopy reports whether held, holding provider reference r of out, is an
// unlinked inbound copy of out: a provider message ID the adapter of out's
// channel type declares provider-assigned, held by a message uma did not
// author.
func echoCopy(e *engine.Engine, held, out model.Message, r model.ExternalRef) bool {
	if held.Origin != nil || held.EchoOf != nil || held.Ref == out.Ref ||
		r.Namespace != model.ProviderMessageNamespace || r.Kind != model.RefKindMessage {
		return false
	}
	_, d, ok := e.Adapter(out.Source)
	return ok && engine.ProviderAssigned(d)
}

// linkEcho marks copy as the echo of out and emits its message.updated
// event, provider-triggered by op's answer.
func linkEcho(ctx context.Context, tx store.Tx, e *engine.Engine, copied, out model.Message, op model.Operation) error {
	rev, err := NewRevision(ctx, e)
	if err != nil {
		return err
	}
	id, err := NewID(ctx, e)
	if err != nil {
		return err
	}
	prev := copied.Revision
	echoOf := out.Ref
	copied.EchoOf, copied.Revision = &echoOf, rev
	if err := tx.PutMessage(ctx, copied, prev); err != nil {
		return err
	}
	opRef := op.Ref
	data, err := json.Marshal(model.MessageUpdatedPayload{
		Message: copied.Ref, Change: model.MessageChangeEcho, Trigger: model.TriggerProvider,
		PreviousRevision: prev, Operation: &opRef,
	})
	if err != nil {
		return model.NewError(model.Permanent, "event payload encoding failed")
	}
	var actor model.ActorID
	if out.Origin != nil {
		actor = out.Origin.Actor
	}
	audience := copied.Audience
	if audience.Visibility == "" {
		audience = model.Audience{Visibility: model.Internal}
	}
	now := e.Clock().Now().UTC()
	res := copied.Ref.Ref()
	return tx.AppendEvents(ctx, []model.Event{{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventMessageUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res, Actor: actor,
		OccurredAt: now, RecordedAt: now, Audience: audience, Revision: rev,
		Payload: model.Extension{Name: model.SchemaMessageUpdated, Version: model.PayloadVersion1, Data: data},
	}})
}

// messageReference reports a provider reference naming the sent message
// itself: its provider message ID or its email Message-ID, which a mail
// reply's In-Reply-To and References name.
func messageReference(r model.ExternalRef) bool {
	switch {
	case r.Namespace == model.ProviderMessageNamespace && r.Kind == model.RefKindMessage:
		return true
	case r.Namespace == model.EmailHeaderNamespace && r.Kind == model.RefKindEmailMessageID:
		return true
	}
	return false
}

// ResponseMessage is the message an interaction response in mode message
// produces: recorded with its message.created event (origin response,
// correlated with the interaction) in the response's claiming unit, before
// any provider delivery, like a reply's outbound message.
type ResponseMessage struct{ o *outbound }

// NewResponseMessage allocates the IDs of the message that answers
// interaction under audience on the conversation interaction belongs to,
// as the channel type source will carry it.
func NewResponseMessage(ctx context.Context, c *engine.Client, interaction model.InteractionRef, corr model.CorrelationID, source model.TypeKey, content model.Content, audience model.Audience) (*ResponseMessage, error) {
	o, err := newOutbound(ctx, c, interaction.Conversation, corr, source, outboundMessage{
		content: content, audience: audience, inResponseTo: &interaction, origin: model.MessageOriginResponse,
	})
	if err != nil {
		return nil, err
	}
	return &ResponseMessage{o}, nil
}

// Ref is the message's reference.
func (m *ResponseMessage) Ref() model.MessageRef { return m.o.ref }

// Write records the message and its event under op, with revision rev; use
// it as the Apply step of the response's Submission.
func (m *ResponseMessage) Write(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error {
	return m.o.write(ctx, tx, op, rev)
}
