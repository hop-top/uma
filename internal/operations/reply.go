package operations

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// WorkKindOperation is the work kind of outbound operation execution, the
// kind Runtime.Run executes.
const WorkKindOperation = "operation.execute"

// IntentReply names the stored intent of a reply operation.
const IntentReply = "uma.intent.reply"

// ReplyIntent is the durable intent of a reply: everything a worker needs to
// execute it after a restart. The route is not part of it: execution always
// reads the conversation's current stored route.
type ReplyIntent struct {
	Conversation model.ConversationRef `json:"conversation"`
	InReplyTo    *model.MessageRef     `json:"in_reply_to,omitempty"`
	Content      model.Content         `json:"content"`
	Audience     model.Audience        `json:"audience"`
	// InResponseTo is the interaction the reply answers; Correlation the
	// call's correlation. Both are carried so a held reply is written, on
	// approval, as the reply would have been.
	InResponseTo *model.InteractionRef `json:"in_response_to,omitempty"`
	Correlation  model.CorrelationID   `json:"correlation,omitempty"`
	// Recipients addresses the reply beyond its route, normalized; nil
	// when the reply names none. An approval of a held reply sends them as
	// they were proposed.
	Recipients *model.ReplyRecipients `json:"recipients,omitempty"`
}

// replyAudience is the audience of every reply: a reply is public by
// definition; internal notes are a separate operation.
func replyAudience() model.Audience { return model.Audience{Visibility: model.Public} }

// Reply queues a public reply on the conversation's stored route. It
// returns once the operation, its outbound message with a message.created
// event and its pending work commit, with state model.Queued; Runtime.Run
// delivers it. The channel's adapter must declare the send capability.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts.
//
// An explicit InReplyTo is the reply's parent. Without one, the parent is
// the conversation's latest inbound message the caller may read; see
// defaultReplyParent. The fingerprint covers the request as sent, so a
// retry stays a replay after newer messages arrive. A reply naming
// InResponseTo is correlated with that interaction, which must exist in
// the conversation and be readable by the caller: the correlation is
// recorded on the message and in message.created, and never claims the
// interaction's initial response, counts a follow-up or reads its token.
func Reply(ctx context.Context, c *engine.Client, req model.Reply) (model.Acceptance, error) {
	if c.Engine().IsClosed() {
		return model.Acceptance{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	recipients, err := normalizedRecipients(req.Recipients)
	if err != nil {
		return model.Acceptance{}, err
	}
	var conv model.Conversation
	var parent *model.Message
	var source model.TypeKey
	var answered *model.Interaction
	err = c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) error {
		var err error
		if conv, err = r.GetConversation(ctx, req.Conversation); err != nil {
			return err
		}
		if answered, err = loadAnswered(ctx, r, req.InResponseTo); err != nil {
			return err
		}
		if parent, err = loadReplyParent(ctx, r, req.InReplyTo); err != nil {
			return err
		}
		inbox, err := r.GetInbox(ctx, conv.Ref.Inbox)
		if err != nil {
			return err
		}
		ch, err := r.GetChannel(ctx, inbox.Channel)
		if err != nil {
			return err
		}
		source = ch.Type
		if err := outboundCapability(c.Engine(), ch.Type, model.CapabilitySend); err != nil {
			return err
		}
		if recipients != nil {
			return outboundCapability(c.Engine(), ch.Type, model.CapabilityReplyRecipients)
		}
		return nil
	})
	if model.IsCode(err, model.NotFound) {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Acceptance{}, err
	}
	if err := checkReplyParent(ctx, c, req, parent); err != nil {
		return model.Acceptance{}, err
	}
	if err := checkAnswered(ctx, c, req, answered); err != nil {
		return model.Acceptance{}, err
	}
	inReplyTo := req.InReplyTo
	if inReplyTo == nil {
		if inReplyTo, err = defaultReplyParent(ctx, c, req.Conversation); err != nil {
			return model.Acceptance{}, err
		}
	}
	intent := ReplyIntent{
		Conversation: req.Conversation, InReplyTo: inReplyTo,
		Content: req.Content.Clone(), Audience: replyAudience(),
		InResponseTo: req.InResponseTo, Correlation: req.Correlation,
		Recipients: recipients,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Validation, "reply content cannot be stored")
	}
	out, err := newOutbound(ctx, c, req.Conversation, req.Correlation, source, outboundMessage{
		content: intent.Content, audience: intent.Audience, inReplyTo: intent.InReplyTo, inResponseTo: req.InResponseTo,
		origin: model.MessageOriginReply,
	})
	if err != nil {
		return model.Acceptance{}, err
	}
	result := out.ref.Ref()
	expected := req.ExpectedRevision
	return submit(ctx, c, submission{
		kind:        model.ActionMessageReply,
		target:      req.Conversation.Ref(),
		owner:       conv.Ref.Ref(),
		command:     req.Command,
		audience:    intent.Audience,
		fingerprint: replyFingerprint(req, intent.Audience),
		intent:      model.Extension{Name: IntentReply, Version: 1, Data: data},
		workKind:    WorkKindOperation,
		precheck: func(ctx context.Context, tx store.Tx) error {
			if expected == "" {
				return nil
			}
			cur, err := tx.GetConversation(ctx, req.Conversation)
			if err != nil {
				return err
			}
			if cur.Revision != expected {
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			return nil
		},
		apply:         out.write,
		result:        &result,
		resultMessage: &out.ref,
	})
}

// replyFingerprint returns the version 1 fingerprint of a reply: kind,
// target conversation, in-reply-to message, content, audience, expected
// revision and, when the reply names any, its normalized recipients.
// Correlation is excluded.
func replyFingerprint(req model.Reply, audience model.Audience) []byte {
	f := NewFingerprint()
	f.Set("kind", Field().Str(model.ActionMessageReply))
	f.Set("target", Field().Ref(req.Conversation.Ref()))
	in := Field().Present(req.InReplyTo != nil)
	if req.InReplyTo != nil {
		in.Ref(req.InReplyTo.Ref())
	}
	f.Set("in_reply_to", in)
	f.Set("content", Field().Content(req.Content))
	f.Set("audience", Field().Audience(audience))
	f.Set("expected_revision", Field().Str(string(req.ExpectedRevision)))
	if req.InResponseTo != nil {
		// Present only when set, so a reply without it keeps its fingerprint.
		f.Set("in_response_to", Field().Ref(req.InResponseTo.Ref()))
	}
	if recipients, _ := normalizedRecipients(req.Recipients); recipients != nil {
		// Present only when set, so a reply without it keeps its fingerprint.
		w := Field().Present(recipients.ReplyAll)
		for _, list := range [][]string{recipients.Cc, recipients.Bcc} {
			w.Str(strconv.Itoa(len(list)))
			for _, a := range list {
				w.Str(a)
			}
		}
		f.Set("recipients", w)
	}
	return f.Sum()
}

// normalizedRecipients returns the normalized recipients of a reply, nil
// when it adds none (an empty value asks for nothing).
func normalizedRecipients(r *model.ReplyRecipients) (*model.ReplyRecipients, error) {
	if r == nil || r.IsZero() {
		return nil, nil
	}
	n, err := r.Normalize()
	if err != nil {
		return nil, err
	}
	return &n, nil
}

// RequireCapability fails with model.Unsupported unless the adapter
// registered for t declares capability c.
func RequireCapability(e *engine.Engine, t model.TypeKey, c model.Capability) error {
	return outboundCapability(e, t, c)
}

// outboundCapability fails with model.Unsupported unless the adapter
// registered for t declares capability c.
func outboundCapability(e *engine.Engine, t model.TypeKey, c model.Capability) error {
	_, desc, ok := e.Adapter(t)
	if !ok {
		return model.NewError(model.Unsupported, "channel adapter not registered")
	}
	if !slices.Contains(desc.Capabilities, c) {
		return model.NewError(model.Unsupported, "channel adapter does not declare "+string(c))
	}
	return nil
}

// DecodeReplyIntent reads a stored reply intent.
func DecodeReplyIntent(e model.Extension) (ReplyIntent, error) {
	var in ReplyIntent
	if e.Name != IntentReply || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}

// loadReplyParent reads a reply's explicit parent through the reply's own
// conversation: the store key is the full reference chain, so a message of
// another conversation, inbox or tenant is not found under it.
func loadReplyParent(ctx context.Context, r store.Reader, ref *model.MessageRef) (*model.Message, error) {
	if ref == nil {
		return nil, nil
	}
	m, err := r.GetMessage(ctx, *ref)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// checkReplyParent admits a loaded reply parent. The parent must lie in the
// reply's conversation, and the principal must be allowed to read it under
// its audience; either failure is the same not found as a missing message,
// so a reply never probes messages its author cannot read. Only then is a
// non-public parent rejected: a public reply never answers an internal or
// restricted message, which would carry non-public content to the
// provider audience. Authorization runs outside any store unit.
func checkReplyParent(ctx context.Context, c *engine.Client, req model.Reply, parent *model.Message) error {
	if parent == nil {
		return nil
	}
	if req.InReplyTo == nil || parent.Ref != *req.InReplyTo || !parent.Ref.In(req.Conversation) {
		return engine.ErrNotFound()
	}
	switch err := c.Authorize(ctx, engine.MessageRead(*parent), req.InReplyTo.Ref()); {
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		return engine.ErrNotFound()
	case err != nil:
		return err
	}
	if engine.ReadAudience(parent.Audience).Visibility != model.Public {
		return model.NewError(model.Validation, "in_reply_to: a public reply cannot answer a non-public message")
	}
	return nil
}

// loadAnswered reads the interaction a reply is correlated with through
// the reply's own conversation, nil when it names none.
func loadAnswered(ctx context.Context, r store.Reader, ref *model.InteractionRef) (*model.Interaction, error) {
	if ref == nil {
		return nil, nil
	}
	in, err := r.Interaction(ctx, *ref)
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// checkAnswered admits a loaded interaction: it must lie in the reply's
// conversation and the principal must be allowed to read it under its
// audience. Either failure is the same not found as a missing interaction,
// so a reply never probes interactions its author cannot read.
func checkAnswered(ctx context.Context, c *engine.Client, req model.Reply, in *model.Interaction) error {
	if in == nil {
		return nil
	}
	if req.InResponseTo == nil || in.Ref != *req.InResponseTo || !in.Ref.In(req.Conversation) {
		return engine.ErrNotFound()
	}
	switch err := c.Authorize(ctx, engine.InteractionRead(*in), in.Ref.Ref()); {
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		return engine.ErrNotFound()
	default:
		return err
	}
}

// parentPage is the page size defaultReplyParent reads with.
const parentPage = 20

// defaultReplyParent returns the parent of a reply that names none: the
// conversation's latest inbound message the caller may read under its own
// audience, or nil when there is none. Inbound means not uma-authored
// (no Origin). A deleted, redacted or non-public message is never chosen:
// a public reply never answers one. The store lists the conversation's
// inbound messages newest first and the read stops at the first one the
// caller may read, so its cost follows how many newer inbound messages are
// not chosen, never the length of the conversation. Candidates are read in
// store units and authorized outside them.
func defaultReplyParent(ctx context.Context, c *engine.Client, conv model.ConversationRef) (*model.MessageRef, error) {
	page := model.PageRequest{Limit: parentPage}
	for {
		var p model.Page[model.Message]
		if err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) (err error) {
			p, err = r.InboundMessages(ctx, conv, page)
			return err
		}); err != nil {
			return nil, err
		}
		for _, m := range p.Items {
			if !m.Ref.In(conv) || !inboundParent(m) {
				continue
			}
			switch err := c.Authorize(ctx, engine.MessageRead(m), m.Ref.Ref()); {
			case err == nil:
				ref := m.Ref
				return &ref, nil
			case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
				continue
			default:
				return nil, err
			}
		}
		if p.Complete || p.Next == "" || p.Next == page.After {
			return nil, nil
		}
		page.After = p.Next
	}
}

// inboundParent reports whether m may be a reply's default parent: an
// inbound, undeleted, unredacted, public message that is no echo of uma's
// own.
func inboundParent(m model.Message) bool {
	return m.Origin == nil && m.EchoOf == nil && !m.Redacted && !m.Deleted && engine.ReadAudience(m.Audience).Visibility == model.Public
}
