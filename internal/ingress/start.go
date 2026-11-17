package ingress

import (
	"context"
	"slices"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Start is the conversation selection of one outbound-first message
// (Conversations.Start): uma speaks first, to Recipient through Inbox.
type Start struct {
	Engine *engine.Engine
	// Principal is the caller: the actor of the events the selection
	// emits.
	Principal  model.Principal
	Channel    model.Channel
	Descriptor adapter.Descriptor
	Inbox      model.Inbox
	Recipient  model.ExternalRef
	// Legacy, when set, is the recipient's reference under the spelling an
	// identity already stored for it was claimed under, found by the caller
	// before the unit. It differs from Recipient in ID only. In a
	// per-contact inbox the start claims that identity rather than
	// Recipient's, so its conversations are the candidates; a conversation
	// the start creates still routes to Recipient. The zero value is a start
	// to Recipient's own identity.
	Legacy      model.ExternalRef
	Operation   model.OperationRef
	Correlation model.CorrelationID
	Now         time.Time
}

// startKinds are the recipient reference kinds of the scoped modes a start
// may open a conversation in.
var startKinds = map[string]string{
	model.ModePerRoom:   model.RefKindRoom,
	model.ModePerThread: model.RefKindThread,
}

// scopeKinds are the reference kinds that scope a conversation, never a
// per-contact recipient.
var scopeKinds = []string{model.RefKindRoom, model.RefKindThread, model.RefKindWorkItem}

// CheckStart checks that a start to recipient can open a conversation in
// inbox, whose channel's adapter is described by d: an enabled inbox with
// a valid selection in a mode a start supports, and a recipient on the
// inbox's channel that the mode can route to. It reads nothing.
func CheckStart(inbox model.Inbox, d adapter.Descriptor, recipient model.ExternalRef) error {
	if inbox.Disabled {
		return model.NewError(model.Disabled, "inbox disabled")
	}
	if err := inbox.Selection.Validate(); err != nil {
		return err
	}
	if recipient.Channel != inbox.Channel {
		return model.NewError(model.Validation, "recipient: must belong to the inbox's channel")
	}
	mode := inbox.Selection.Mode
	switch {
	case mode == model.ModePerContact:
		if !slices.Contains(d.IdentityNamespaces, recipient.Namespace) {
			return model.NewError(model.Validation, "recipient: namespace not declared by the adapter")
		}
		if slices.Contains(scopeKinds, recipient.Kind) {
			return model.NewError(model.Validation, "recipient: a per-contact start needs a sender route, not a "+recipient.Kind+" reference")
		}
	case startKinds[mode] != "":
		if recipient.Kind != startKinds[mode] {
			return model.NewError(model.Validation, "recipient: "+mode+" selection requires a "+startKinds[mode]+" reference")
		}
	default:
		return engine.Unsupported(mode + " conversation start")
	}
	return nil
}

// OpenConversation selects or creates, in tx, the conversation of a start
// and reports whether it created it. It claims the recipient's identity in
// a per-contact inbox, with its provisional contact when new, exactly as
// ingress claims a sender's, then applies the inbox's selection policy to
// the recipient's candidates with event class policy.EventOutboundStart:
// reuse, participant addition, reopen or creation under the selector's
// create key. A created conversation routes to the recipient. Its
// conversation.changed events are command-triggered by the start's
// operation. It runs CheckStart first, on the inbox as tx reads it.
//
// With Start.Legacy set, the identity is claimed under that reference. The
// claim must find the identity the caller saw: one it creates is refused as
// model.Conflict and rolled back, so a start never leaves an identity under
// a spelling the adapter no longer keys on.
func OpenConversation(ctx context.Context, tx store.Tx, s Start) (model.Conversation, bool, error) {
	if err := CheckStart(s.Inbox, s.Descriptor, s.Recipient); err != nil {
		return model.Conversation{}, false, err
	}
	env := model.Envelope{Audience: model.Audience{Visibility: model.Internal}, OccurredAt: s.Now}
	if s.Inbox.Selection.Mode == model.ModePerContact {
		env.Sender = s.Recipient
	} else {
		env.References = []model.ExternalRef{s.Recipient}
	}
	op := s.Operation
	var claimAs *model.ExternalRef
	if !s.Legacy.IsZero() {
		want := s.Recipient
		want.ID = s.Legacy.ID
		if s.Legacy != want || s.Legacy.Validate() != nil {
			return model.Conversation{}, false, model.NewError(model.Validation, "legacy recipient: must differ from the recipient in ID only")
		}
		if s.Inbox.Selection.Mode == model.ModePerContact {
			claimAs = &s.Legacy
		}
	}
	u := &ingressUnit{
		s:   &Sink{e: s.Engine, principal: s.Principal, channel: s.Channel, desc: s.Descriptor},
		ctx: ctx, tx: tx, in: model.Ingress{Call: model.Call{Correlation: s.Correlation}, Envelope: env},
		env: env, now: s.Now.UTC(), inbox: s.Inbox, start: &op, claimAs: claimAs,
	}
	sender, err := u.resolveSender()
	if err != nil {
		return model.Conversation{}, false, err
	}
	conv, created, err := u.selectConversation(sender)
	if err != nil {
		return model.Conversation{}, false, err
	}
	if len(u.events) > 0 {
		if err := tx.AppendEvents(ctx, u.events); err != nil {
			return model.Conversation{}, false, err
		}
	}
	return conv, created, nil
}
