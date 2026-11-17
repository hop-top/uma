package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// builtinValidators are the validators of the intents this package builds:
// an edit of a held reply, message edit, delete or reaction keeps the claimed
// target, audience and shape, and may change only the content the command
// carries. RegisterValidator can replace one.
var builtinValidators = map[string]Validator{
	IntentReply:  validateReplyEdit,
	IntentEdit:   validateEditEdit,
	IntentDelete: validateDeleteEdit,
	IntentReact:  validateReactEdit,
	IntentNote:   validateNoteEdit,
}

func badArgs(msg string) error { return model.NewError(model.Validation, "args: "+msg) }

// decodeArgs reads edited arguments into T.
func decodeArgs[T any](e model.Extension) (T, error) {
	var in T
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, badArgs("not readable as this intent")
	}
	return in, nil
}

func validateReplyEdit(ctx context.Context, r store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	in, err := decodeArgs[ReplyIntent](edited)
	if err != nil {
		return err
	}
	old, err := DecodeReplyIntent(stored)
	if err != nil {
		return err
	}
	if in.Conversation.Ref() != key.Target {
		return badArgs("conversation must be the operation's target")
	}
	if !reflect.DeepEqual(in.Audience, old.Audience) || !reflect.DeepEqual(in.Audience, replyAudience()) {
		return badArgs("audience cannot change")
	}
	if !reflect.DeepEqual(in.InResponseTo, old.InResponseTo) || in.Correlation != old.Correlation {
		return badArgs("in_response_to and correlation cannot change")
	}
	if !reflect.DeepEqual(in.Recipients, old.Recipients) {
		return badArgs("recipients cannot change")
	}
	if in.InReplyTo != nil {
		if !in.InReplyTo.In(in.Conversation) {
			return badArgs("in_reply_to must belong to the conversation")
		}
		if _, err := loadReplyParent(ctx, r, in.InReplyTo); err != nil {
			if model.IsCode(err, model.NotFound) {
				return badArgs("in_reply_to not found")
			}
			return err
		}
	}
	if err := in.Content.ValidateIn(in.Conversation.Inbox); err != nil {
		return badArgs(err.Error())
	}
	return nil
}

// sameMutation requires the shared part of an edited mutation intent to be
// the stored one but for nothing: message, audience, base and wait are the
// claim's, not the editor's.
func sameMutation(key store.OperationKey, in, old MutationIntent) error {
	if in.Message.Ref() != key.Target {
		return badArgs("message must be the operation's target")
	}
	a, _ := json.Marshal(in)
	b, _ := json.Marshal(old)
	if !bytes.Equal(a, b) {
		return badArgs("target, audience and base cannot change")
	}
	return nil
}

func validateEditEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	in, err := decodeArgs[EditIntent](edited)
	if err != nil {
		return err
	}
	old, err := DecodeEditIntent(stored)
	if err != nil {
		return err
	}
	if err := sameMutation(key, in.MutationIntent, old.MutationIntent); err != nil {
		return err
	}
	if err := in.Content.ValidateIn(in.Message.Conversation.Inbox); err != nil {
		return badArgs(err.Error())
	}
	return nil
}

func validateDeleteEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	in, err := decodeArgs[DeleteIntent](edited)
	if err != nil {
		return err
	}
	old, err := decodeIntent[DeleteIntent](stored, IntentDelete)
	if err != nil {
		return err
	}
	return sameMutation(key, in.MutationIntent, old.MutationIntent)
}

func validateReactEdit(_ context.Context, _ store.Reader, key store.OperationKey, stored, edited model.Extension) error {
	in, err := decodeArgs[ReactIntent](edited)
	if err != nil {
		return err
	}
	old, err := decodeIntent[ReactIntent](stored, IntentReact)
	if err != nil {
		return err
	}
	if err := sameMutation(key, in.MutationIntent, old.MutationIntent); err != nil {
		return err
	}
	if in.Emoji == "" {
		return badArgs("emoji required")
	}
	return nil
}
