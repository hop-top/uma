package ingress

import (
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// Extension events. An extension.event envelope carries one event of a
// kind an extension definition registers and the adapter declares. Ingress
// appends it to the event log as is, about the resolved inbox or the
// known message it names, and writes nothing else: no contact, identity,
// conversation, message or selection. Its payload is the adapter's: core
// checks its JSON and size only, and the adapter keeps it to references
// and a change summary (see model.EnvelopeExtensionEvent).

// checkExtensionEvent validates an extension.event envelope against the
// binding. The event must name a registered event kind the adapter
// declares, with the definition's capability declared too; an unknown or
// undeclared kind is model.Validation, a name of another extension kind or
// an unregistered version model.SchemaMismatch. It needs an event ID and
// an audience, a message ID only under provider-assigned IDs, and carries
// no sender, content, references, provider payload or state of another
// envelope kind.
func (s *Sink) checkExtensionEvent(env model.Envelope) error {
	switch {
	case env.Event == nil:
		return model.NewError(model.Validation, "envelope.event: required by an extension.event envelope")
	case env.EventID == "":
		return model.NewError(model.Validation, "envelope.event_id: required by an extension.event envelope")
	case env.MessageID != "" && !engine.ProviderAssigned(s.desc):
		return model.NewError(model.Validation, "envelope.message_id: an extension event names a message only under provider-assigned IDs")
	case !env.Sender.IsZero():
		return model.NewError(model.Validation, "envelope.sender: an extension event carries no sender")
	case env.SystemActor:
		return model.NewError(model.Validation, "envelope.system_actor: an extension event carries no actor")
	case env.SenderKind != "":
		return model.NewError(model.Validation, "envelope.sender_kind: an extension event carries no sender")
	case len(env.SenderAttributes) > 0:
		return model.NewError(model.Validation, "envelope.sender_attributes: an extension event carries no sender")
	case len(env.Content.Parts) > 0:
		return model.NewError(model.Validation, "envelope.content: an extension event carries no content")
	case len(env.References) > 0:
		return model.NewError(model.Validation, "envelope.references: an extension event carries none")
	case env.Payload.Name != "" || env.Payload.Version != 0 || len(env.Payload.Data) > 0:
		return model.NewError(model.Validation, "envelope.payload: an extension event carries its data on envelope.event")
	case env.Interaction != nil:
		return model.NewError(model.Validation, "envelope.interaction: an extension event carries none")
	case env.WorkItem != nil:
		return model.NewError(model.Validation, "envelope.work_item: an extension event carries none")
	case env.Delivery != nil:
		return model.NewError(model.Validation, "envelope.delivery: an extension event carries none")
	case env.Task != nil:
		return model.NewError(model.Validation, "envelope.task: an extension event carries none")
	}
	if err := env.Event.Validate(); err != nil {
		return err
	}
	if err := s.checkEventKind(*env.Event); err != nil {
		return err
	}
	return env.Audience.Validate()
}

// checkEventKind resolves the registered definition an extension event
// names, in the order of the extension definition lookup: no definition
// is an unknown kind, a definition of another kind or no such version a
// schema mismatch.
func (s *Sink) checkEventKind(x model.Extension) error {
	versions := s.e.ExtensionVersions(x.Name)
	if len(versions) == 0 {
		return model.NewError(model.Validation, "envelope.event.name: unknown event kind")
	}
	if versions[0].Kind != model.ExtensionKindEvent {
		return schemaMismatch("envelope.event.name: extension is not an event kind")
	}
	if !slices.Contains(s.desc.Events, x.Name) {
		return model.NewError(model.Validation, "envelope.event.name: unknown event kind for this adapter")
	}
	for _, d := range versions {
		if d.Version != x.Version {
			continue
		}
		if !slices.Contains(s.desc.Capabilities, d.RequiredCapability) {
			return model.NewError(model.Validation, "envelope.event: required capability not declared by the adapter")
		}
		return nil
	}
	return schemaMismatch("envelope.event.version: event version not registered")
}

func schemaMismatch(msg string) error {
	return &model.Error{Code: model.SchemaMismatch, SafeMessage: msg, Retry: model.NoRetry}
}

// applyExtensionEvent appends the envelope's event. Without a message ID
// it is about the inbox, under the envelope's audience. With one, it is
// about the known message: a non-public message gives the event its
// audience, a public one takes the envelope's, so the event is never wider
// than its message. An unknown message is model.NotFound and nothing is
// recorded, so the adapter keeps the event for redelivery; an event about
// a redacted or provider-deleted message is accepted and appends nothing.
func (u *ingressUnit) applyExtensionEvent() error {
	res := u.inbox.Ref.Ref()
	rev := u.inbox.Revision
	audience := u.env.Audience
	if u.env.MessageID != "" {
		msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID), 2)
		switch {
		case err != nil:
			return err
		case len(msgs) == 0:
			return model.NewError(model.NotFound, "target message not found")
		case len(msgs) > 1:
			return model.NewError(model.Conflict, "provider message ID stored more than once")
		}
		target := msgs[0]
		if target.Redacted || target.Deleted {
			return nil
		}
		res, rev = messageResource(target.Ref), target.Revision
		if target.Audience.Visibility != model.Public {
			audience = target.Audience
		}
	}
	ev := u.env.Event.Clone()
	return u.appendEvent(audience, ev.Name, res, rev, ev)
}
