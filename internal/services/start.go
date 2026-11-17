package services

import (
	"context"
	"errors"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Start queues the first message of an outbound-first conversation; see
// uma.Conversations.Start. It checks the request, then, before any
// decision, that the inbox exists in the principal's tenant, that its
// channel's adapter declares model.CapabilityStartConversation and that
// the inbox and recipient admit a start (ingress.CheckStart), the recipient
// first normalized by an adapter that implements adapter.RecipientNormalizer,
// so identity, conversation and fingerprint see one spelling. Submission
// then authorizes model.ActionConversationStart on the inbox and, in the
// claiming unit, rereads the inbox and opens the conversation
// (ingress.OpenConversation) before recording the message.
//
// An adapter that also implements adapter.LegacyRecipientReuser may have the
// start reuse an identity stored under the recipient as typed; see
// legacyRecipient. That lookup is read-only and runs here, before the
// claiming unit, which gains no read.
func (s Conversations) Start(ctx context.Context, req model.StartConversation) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var inbox model.Inbox
	var ch model.Channel
	if err := view(ctx, c, func(r store.Reader) (err error) {
		if inbox, err = startInbox(ctx, r, req.Inbox); err != nil {
			return err
		}
		ch, err = r.GetChannel(ctx, inbox.Channel)
		return err
	}); err != nil {
		if model.IsCode(err, model.NotFound) {
			return model.Acceptance{}, engine.ErrNotFound()
		}
		return model.Acceptance{}, err
	}
	if err := operations.RequireCapability(e, ch.Type, model.CapabilityStartConversation); err != nil {
		return model.Acceptance{}, err
	}
	f, desc, _ := e.Adapter(ch.Type)
	desc = engine.CloneDescriptor(desc)
	typed := req.Recipient
	if req.Recipient, err = normalizeRecipient(f, inbox, typed); err != nil {
		return model.Acceptance{}, err
	}
	if err := ingress.CheckStart(inbox, desc, req.Recipient); err != nil {
		return model.Acceptance{}, err
	}
	legacy, err := legacyRecipient(ctx, c, f, inbox, typed, req.Recipient)
	if err != nil {
		return model.Acceptance{}, err
	}
	open := func(ctx context.Context, tx store.Tx, op model.OperationRef) (model.Conversation, bool, error) {
		cur, err := startInbox(ctx, tx, req.Inbox)
		if err != nil {
			return model.Conversation{}, false, err
		}
		return ingress.OpenConversation(ctx, tx, ingress.Start{
			Engine: e, Principal: c.Principal(), Channel: ch, Descriptor: desc, Inbox: cur,
			Recipient: req.Recipient, Legacy: legacy, Operation: op, Correlation: req.Correlation, Now: e.Clock().Now(),
		})
	}
	return operations.Start(ctx, c, operations.StartSubmission{
		Request: req, Source: ch.Type, Open: open, Legacy: legacy, Deferred: !e.Held().StartCreateAtHold,
	})
}

// normalizeRecipient returns the recipient under the spelling the channel's
// adapter keys its identity on, when the factory implements
// adapter.RecipientNormalizer; otherwise, and for a recipient of another
// channel (ingress.CheckStart refuses it), recipient unchanged. The result
// differs from recipient in ID only: anything else is an adapter fault.
func normalizeRecipient(f adapter.Factory, inbox model.Inbox, recipient model.ExternalRef) (model.ExternalRef, error) {
	n, ok := f.(adapter.RecipientNormalizer)
	if !ok || recipient.Channel != inbox.Channel {
		return recipient, nil
	}
	got, err := n.NormalizeRecipient(recipient)
	if err != nil {
		msg := "recipient: refused by the adapter"
		var me *model.Error
		if errors.As(err, &me) && me.SafeMessage != "" {
			msg = "recipient: " + me.SafeMessage
		}
		return model.ExternalRef{}, model.NewError(model.Validation, msg)
	}
	want := recipient
	want.ID = got.ID
	if got != want || got.Validate() != nil {
		return model.ExternalRef{}, model.NewError(model.Permanent, "adapter returned an invalid normalized recipient")
	}
	return got, nil
}

// legacyRecipient returns typed when the start should reuse the identity
// stored under it, else the zero reference. It does so only in a per-contact
// inbox, for a recipient the adapter normalized to another spelling and
// agrees (adapter.LegacyRecipientReuser) to look up as typed; then only when
// the normalized spelling has no identity in the inbox and the typed one has.
// That is at most two exact point reads in one read unit, both before the
// claiming unit; a start whose recipient the adapter left as typed makes
// none. When both spellings have an identity the normalized one wins.
func legacyRecipient(ctx context.Context, c *engine.Client, f adapter.Factory, inbox model.Inbox, typed, normalized model.ExternalRef) (model.ExternalRef, error) {
	if typed.ID == normalized.ID || inbox.Selection.Mode != model.ModePerContact {
		return model.ExternalRef{}, nil
	}
	if r, ok := f.(adapter.LegacyRecipientReuser); !ok || !r.ReuseLegacyRecipient(typed) {
		return model.ExternalRef{}, nil
	}
	found := func(r store.Reader, ref model.ExternalRef) (bool, error) {
		_, err := r.Identity(ctx, model.IdentityKey{Inbox: inbox.Ref, Namespace: ref.Namespace, SourceID: ref.ID})
		if model.IsCode(err, model.NotFound) {
			return false, nil
		}
		return err == nil, err
	}
	var reuse bool
	err := view(ctx, c, func(r store.Reader) error {
		ok, err := found(r, normalized)
		if ok || err != nil {
			return err
		}
		reuse, err = found(r, typed)
		return err
	})
	if err != nil || !reuse {
		return model.ExternalRef{}, err
	}
	return typed, nil
}

// startInbox reads ref and requires the stored record to carry exactly
// that reference; a mismatch is not found.
func startInbox(ctx context.Context, r store.Reader, ref model.InboxRef) (model.Inbox, error) {
	in, err := r.GetInbox(ctx, ref)
	if err != nil {
		return model.Inbox{}, err
	}
	if in.Ref != ref {
		return model.Inbox{}, engine.ErrNotFound()
	}
	return in, nil
}
