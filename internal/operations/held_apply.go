package operations

import (
	"context"
	"reflect"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Appliers and validators of the intents this package builds that write a
// local record when applied (builtinAppliers, builtinValidators): an
// approved reply or delivered note writes its outbound message and
// message.created event, in the unit of the approval, exactly as the
// submission's apply step would have; a state change writes nothing and
// only queues its work. A held reply, note or flag can so be approved, and
// edited within the claimed target.
var builtinAppliers = map[string]Applier{
	IntentReply: applyHeldReply,
	IntentNote:  applyHeldNote,
}

// stateEditValidators are the validators of the state intents whose edit
// is admitted: flag and unflag, whose kind (star, important) may change.
func stateEditValidators() map[string]Validator {
	out := map[string]Validator{}
	for _, v := range []model.StateVerb{model.StateVerbFlag, model.StateVerbUnflag} {
		if c, ok := v.Capability(); ok {
			out[StateIntentName(c)] = validateStateEdit
		}
	}
	return out
}

// heldOutbound builds the outbound message of a held operation from its
// claimed key and the result message named at hold time. The proposer is
// the author, the channel type is read from the conversation's inbox. It
// returns nil when the operation names no result message: a submission
// built by hand with a reply or note intent and no local write, which an
// approval only queues.
func heldOutbound(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey,
	conv model.ConversationRef, msg outboundMessage, corr model.CorrelationID,
) (*outbound, error) {
	if op.ResultMessage == nil {
		return nil, nil
	}
	inbox, err := tx.GetInbox(ctx, conv.Inbox)
	if err != nil {
		return nil, err
	}
	ch, err := tx.GetChannel(ctx, inbox.Channel)
	if err != nil {
		return nil, err
	}
	id, err := NewID(ctx, e)
	if err != nil {
		return nil, err
	}
	return &outbound{e: e, actor: key.Caller, ref: *op.ResultMessage, event: id, source: ch.Type, msg: msg, corr: corr}, nil
}

func applyHeldReply(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) error {
	in, err := decodeArgs[ReplyIntent](intent)
	if err != nil {
		return err
	}
	out, err := heldOutbound(ctx, e, tx, op, key, in.Conversation, outboundMessage{
		content: in.Content.Clone(), audience: in.Audience, inReplyTo: in.InReplyTo,
		inResponseTo: in.InResponseTo, origin: model.MessageOriginReply,
	}, in.Correlation)
	if err != nil || out == nil {
		return err
	}
	return out.write(ctx, tx, op.Ref, rev)
}

func applyHeldNote(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) error {
	in, err := decodeArgs[NoteIntent](intent)
	if err != nil {
		return err
	}
	out, err := heldOutbound(ctx, e, tx, op, key, in.Conversation, outboundMessage{
		content: in.Content.Clone(), audience: in.Audience, origin: model.MessageOriginNote,
	}, in.Correlation)
	if err != nil || out == nil {
		return err
	}
	return out.write(ctx, tx, op.Ref, rev)
}

func validateNoteEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	in, err := decodeArgs[NoteIntent](edited)
	if err != nil {
		return err
	}
	old, err := DecodeNoteIntent(stored)
	if err != nil {
		return err
	}
	if in.Conversation.Ref() != key.Target {
		return badArgs("conversation must be the operation's target")
	}
	if !reflect.DeepEqual(in.Audience, old.Audience) {
		return badArgs("audience cannot change")
	}
	if in.Correlation != old.Correlation {
		return badArgs("correlation cannot change")
	}
	if err := in.Content.ValidateIn(in.Conversation.Inbox); err != nil {
		return badArgs(err.Error())
	}
	return nil
}

// validateStateEdit admits the edit of a held flag or unflag: only the
// kind it names (star, important) may change. The conversation, channel,
// verb, scope, shape, destination, label changes and base snapshot are the
// claim's.
func validateStateEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	in, err := decodeArgs[StateIntent](edited)
	if err != nil {
		return err
	}
	old, err := decodeArgs[StateIntent](stored)
	if err != nil {
		return err
	}
	if in.Conversation.Ref() != key.Target {
		return badArgs("conversation must be the operation's target")
	}
	if in.Kind == "" {
		return badArgs("kind required")
	}
	in.Kind = old.Kind
	if !reflect.DeepEqual(in, old) {
		return badArgs("only the kind can change")
	}
	return nil
}
