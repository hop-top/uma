package operations

import (
	"context"
	"encoding/json"
	"strings"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// IntentStart names the stored intent of a start operation.
const IntentStart = "uma.intent.start"

// StartIntent is the durable intent of a start: the inbox it was
// authorized on, the conversation its selection chose, and the message.
// As with a reply, execution reads the conversation's current stored
// route; a start answers no message.
//
// A held start that creates its conversation on approval stores the
// intent before any conversation exists: Conversation is empty, and
// Recipient (as normalized), Legacy, Message (the ID its message will be
// written under, allocated when held) and Correlation are what the approval
// selects the conversation and writes the message from. The conversation
// the approval chose is then the one its result message names, which
// Effective fills in for a worker.
type StartIntent struct {
	Inbox        model.InboxRef        `json:"inbox"`
	Conversation model.ConversationRef `json:"conversation"`
	Content      model.Content         `json:"content"`
	Audience     model.Audience        `json:"audience"`
	// ConversationCreated is set when the start created its conversation
	// in the unit that held it, for the message.created event of its
	// approval.
	ConversationCreated bool                `json:"conversation_created,omitempty"`
	Recipient           *model.ExternalRef  `json:"recipient,omitempty"`
	Legacy              *model.ExternalRef  `json:"legacy,omitempty"`
	Message             model.ID            `json:"message,omitempty"`
	Correlation         model.CorrelationID `json:"correlation,omitempty"`
}

// Opener selects or creates, inside the claiming unit of a new start, the
// conversation of the start's recipient, writing it, the recipient's
// identity and their events, and reports whether it created the
// conversation. It runs only for a new operation, never for a replay; op
// is the operation the start commits as.
type Opener func(ctx context.Context, tx store.Tx, op model.OperationRef) (model.Conversation, bool, error)

// StartSubmission is one Conversations.Start: the validated request, the
// type of the inbox's channel and the opener of its conversation.
//
// Deferred, for a start that carries a Hold, holds it before its
// conversation exists: Open does not run, the intent keeps the Recipient
// and Legacy to select the conversation by when approved, and the operation
// names no result until then (see OpeningApplier). Otherwise Open runs in the
// claiming unit, a held start then naming its conversation and message at
// hold time.
type StartSubmission struct {
	Request  model.StartConversation
	Source   model.TypeKey
	Open     Opener
	Deferred bool
	Legacy   model.ExternalRef
}

// Start queues the first message of an outbound-first conversation. In
// one unit it claims the operation key (kind model.ActionConversationStart,
// target the inbox), runs the opener and records the outbound message
// with its message.created event (origin start) and pending work, with
// state model.Queued; Runtime.Run delivers it. The channel's adapter must
// declare model.CapabilityStartConversation. Retrying with the same
// operation ID and payload returns the stored operation without selecting
// again; a different payload conflicts.
func Start(ctx context.Context, c *engine.Client, s StartSubmission) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := outboundCapability(c.Engine(), s.Source, model.CapabilityStartConversation); err != nil {
		return model.Acceptance{}, err
	}
	req := s.Request
	audience := replyAudience()
	if _, err := json.Marshal(req.Content); err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "start content cannot be stored")
	}
	out, err := newOutbound(ctx, c, model.ConversationRef{}, req.Correlation, s.Source, outboundMessage{
		content: req.Content.Clone(), audience: audience, origin: model.MessageOriginStart,
	})
	if err != nil {
		return model.Acceptance{}, err
	}
	inbox := req.Inbox.Ref()
	if s.Deferred && req.Hold != nil {
		return submit(ctx, c, submission{
			kind: model.ActionConversationStart, target: inbox, owner: inbox, command: req.Command,
			audience: audience, fingerprint: startFingerprint(req, audience), workKind: WorkKindOperation,
			holdScope: startHoldScope(req.Recipient),
			intent:    model.Extension{Name: IntentStart, Version: 1},
			open: func(context.Context, store.Tx, model.OperationRef) (opened, error) {
				in := StartIntent{
					Inbox: req.Inbox, Content: out.msg.content, Audience: audience,
					Recipient: &req.Recipient, Message: out.ref.ID, Correlation: req.Correlation,
				}
				if !s.Legacy.IsZero() {
					in.Legacy = &s.Legacy
				}
				data, err := json.Marshal(in)
				if err != nil {
					return opened{}, model.NewError(model.Validation, "start content cannot be stored")
				}
				return opened{intent: model.Extension{Name: IntentStart, Version: 1, Data: data}}, nil
			},
		})
	}
	return submit(ctx, c, submission{
		kind:        model.ActionConversationStart,
		target:      inbox,
		owner:       inbox,
		command:     req.Command,
		audience:    audience,
		fingerprint: startFingerprint(req, audience),
		workKind:    WorkKindOperation,
		holdScope:   startHoldScope(req.Recipient),
		intent:      model.Extension{Name: IntentStart, Version: 1},
		open: func(ctx context.Context, tx store.Tx, op model.OperationRef) (opened, error) {
			conv, created, err := s.Open(ctx, tx, op)
			if err != nil {
				return opened{}, err
			}
			out.ref.Conversation, out.msg.conversationCreated = conv.Ref, created
			data, err := json.Marshal(StartIntent{
				Inbox: req.Inbox, Conversation: conv.Ref, Content: out.msg.content, Audience: audience,
				ConversationCreated: created && req.Hold != nil,
			})
			if err != nil {
				return opened{}, model.NewError(model.Validation, "start content cannot be stored")
			}
			return opened{intent: model.Extension{Name: IntentStart, Version: 1, Data: data}, message: out.ref}, nil
		},
		apply: out.write,
	})
}

// startHoldScope is the duplicate-guard scope of a held start: its
// recipient, as the start normalized it, so a second proposal to the same
// recipient in the inbox is a duplicate and one to another is not.
func startHoldScope(r model.ExternalRef) string {
	return strings.Join([]string{string(r.Channel.Tenant), string(r.Channel.ID), r.Namespace, r.Kind, r.ID, r.ParentID}, "\x00")
}

// startFingerprint returns the version 1 fingerprint of a start: kind,
// target inbox, recipient, content and audience. Correlation is excluded.
func startFingerprint(req model.StartConversation, audience model.Audience) []byte {
	r := req.Recipient
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionConversationStart))
	f.Set("target", Field().Ref(req.Inbox.Ref()))
	f.Set("recipient", Field().Str(string(r.Channel.Tenant)).Str(string(r.Channel.ID)).
		Str(r.Namespace).Str(r.Kind).Str(r.ID).Str(r.ParentID))
	f.Set("content", Field().Content(req.Content))
	f.Set("audience", Field().Audience(audience))
	return f.Sum()
}

// DecodeStartIntent reads a stored start intent.
func DecodeStartIntent(e model.Extension) (StartIntent, error) {
	var in StartIntent
	if e.Name != IntentStart || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// withResolvedConversation returns intent with its conversation filled from
// msg, the message an approval wrote, when the intent is a start held
// before its conversation was chosen. Any other intent is returned as is.
func withResolvedConversation(intent model.Extension, msg *model.MessageRef) model.Extension {
	if intent.Name != IntentStart || msg == nil {
		return intent
	}
	in, err := DecodeStartIntent(intent)
	if err != nil || in.Conversation != (model.ConversationRef{}) {
		return intent
	}
	in.Conversation = msg.Conversation
	data, err := json.Marshal(in)
	if err != nil {
		return intent
	}
	out := intent.Clone()
	out.Data = data
	return out
}

// WriteStartMessage writes, in tx, the first message of a start that an
// approval applies: msg with its message.created event (origin start),
// authored by actor, under op at revision rev. created reports whether the
// start created its conversation.
func WriteStartMessage(ctx context.Context, e *engine.Engine, tx store.Tx, actor model.ActorID, op model.OperationRef, msg model.MessageRef, source model.TypeKey, in StartIntent, created bool, rev model.Revision) error {
	event, err := NewID(ctx, e)
	if err != nil {
		return err
	}
	out := &outbound{e: e, actor: actor, ref: msg, event: event, source: source, corr: in.Correlation, msg: outboundMessage{
		content: in.Content.Clone(), audience: in.Audience, origin: model.MessageOriginStart, conversationCreated: created,
	}}
	return out.write(ctx, tx, op, rev)
}
