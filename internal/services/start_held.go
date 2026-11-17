package services

import (
	"context"
	"encoding/json"
	"reflect"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A start is always holdable. By default (engine.HeldPolicy.StartCreateAtHold
// false) its conversation is selected or created when it is approved:
// Start stores the intent with the recipient, nothing else, and the
// approval runs the inbox's selection policy as the start would have, then
// writes the message under the ID allocated when it was held. A rejected or
// expired proposal leaves no conversation, identity or event. With
// StartCreateAtHold the conversation is opened in the unit that holds the
// start, which names it and its message, and the approval only writes the
// message.

// applyHeldStart writes the message of an approved start and returns it: in
// the conversation its hold named, or, for a start held before its
// conversation existed, in the conversation the selection chooses now.
func applyHeldStart(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) (model.MessageRef, error) {
	in, err := operations.DecodeStartIntent(intent)
	if err != nil {
		return model.MessageRef{}, model.NewError(model.Validation, "args: not readable as this intent")
	}
	inbox, err := startInbox(ctx, tx, in.Inbox)
	if err != nil {
		return model.MessageRef{}, err
	}
	ch, err := tx.GetChannel(ctx, inbox.Channel)
	if err != nil {
		return model.MessageRef{}, err
	}
	if err := operations.RequireCapability(e, ch.Type, model.CapabilityStartConversation); err != nil {
		return model.MessageRef{}, err
	}
	var msg model.MessageRef
	created := in.ConversationCreated
	switch {
	case op.ResultMessage != nil:
		msg = *op.ResultMessage
	case in.Recipient == nil || in.Message == "":
		return model.MessageRef{}, model.NewError(model.Permanent, "held start names neither its message nor its recipient")
	default:
		_, desc, _ := e.Adapter(ch.Type)
		var legacy model.ExternalRef
		if in.Legacy != nil {
			legacy = *in.Legacy
		}
		conv, made, err := ingress.OpenConversation(ctx, tx, ingress.Start{
			Engine: e, Principal: model.Principal{Tenant: key.Tenant, Actor: key.Caller}, Channel: ch,
			Descriptor: engine.CloneDescriptor(desc), Inbox: inbox, Recipient: *in.Recipient, Legacy: legacy,
			Operation: op.Ref, Correlation: in.Correlation, Now: e.Clock().Now(),
		})
		if err != nil {
			return model.MessageRef{}, err
		}
		msg, created = model.MessageRef{Conversation: conv.Ref, ID: in.Message}, made
	}
	if err := operations.WriteStartMessage(ctx, e, tx, key.Caller, op.Ref, msg, ch.Type, in, created, rev); err != nil {
		return model.MessageRef{}, err
	}
	return msg, nil
}

// validateHeldStartEdit admits the edit of a held start: only the content
// may change. The inbox, the recipient, the conversation and the rest are
// the claim's.
func validateHeldStartEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	var in, old operations.StartIntent
	if err := json.Unmarshal(edited.Data, &in); err != nil {
		return model.NewError(model.Validation, "args: not readable as this intent")
	}
	if err := json.Unmarshal(stored.Data, &old); err != nil {
		return err
	}
	if in.Inbox.Ref() != key.Target {
		return model.NewError(model.Validation, "args: inbox must be the operation's target")
	}
	probe := in
	probe.Content = old.Content
	if !reflect.DeepEqual(probe, old) {
		return model.NewError(model.Validation, "args: only the content can change")
	}
	if err := in.Content.ValidateIn(in.Inbox); err != nil {
		return model.NewError(model.Validation, "args: "+err.Error())
	}
	return nil
}
