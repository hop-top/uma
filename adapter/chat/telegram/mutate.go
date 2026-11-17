package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// messageTarget checks that e names a message of this channel and splits
// its provider message ID. The route is the conversation's reply route; a
// mutation acts on the target alone, which may be any message of the
// channel: Telegram answers for what the bot may change.
func (x *Instance) messageTarget(e adapter.Effect) (chatID, id int64, err error) {
	if err := x.ready(); err != nil {
		return 0, 0, err
	}
	if _, err := x.parseRoute(e.Route); err != nil {
		return 0, 0, err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a Telegram message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || t.ParentID != "" {
		return 0, 0, bad
	}
	chatID, id, ok := parseMessageID(t.ID)
	if !ok {
		return 0, 0, bad
	}
	return chatID, id, nil
}

type editArgs struct {
	ChatID      int64           `json:"chat_id"`
	MessageID   int64           `json:"message_id"`
	Text        string          `json:"text"`
	ParseMode   string          `json:"parse_mode,omitempty"`
	ReplyMarkup json.RawMessage `json:"reply_markup,omitempty"`
}

// notModified is the description of the 400 a bot gets for an edit that
// changes nothing; the reference does not document it. An edit to what the
// message already says is what was asked, so it is applied.
const notModified = "message is not modified"

// Edit replaces the text of the message the target names, through
// editMessageText. The content is the whole message: its text, parse mode
// and keyboard. A bot edits only its own messages; Telegram's refusal is a
// final rejection, never a new message. Whether an edit that names no
// keyboard drops the message's one is not documented; the adapter always
// states the keyboard it means.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	chatID, id, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Telegram messages are public to the chat"), nil
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	return x.editMessage(ctx, chatID, id, msg)
}

// editMessage calls editMessageText.
func (x *Instance) editMessage(ctx context.Context, chatID, id int64, msg rendered) (adapter.Outcome, error) {
	args := editArgs{ChatID: chatID, MessageID: id, Text: msg.text, ParseMode: msg.parseMode, ReplyMarkup: msg.keyboard}
	err := x.call(ctx, "editMessageText", &args, nil)
	if ae, ok := errors.AsType[*apiError](err); ok && ae.code == 400 && strings.Contains(strings.ToLower(ae.description), notModified) {
		err = nil
	}
	if err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the message the target names, through deleteMessage, within
// the limits the reference gives: "A message can only be deleted if it was
// sent less than 48 hours ago"; a bot deletes its own messages in private
// chats, groups and supergroups, others' only as an administrator with the
// right to, and in a channel only with can_post_messages or
// can_delete_messages. Telegram's refusal is a final rejection.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	chatID, id, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := x.call(ctx, "deleteMessage", map[string]int64{"chat_id": chatID, "message_id": id}, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

type reactArgs struct {
	ChatID    int64          `json:"chat_id"`
	MessageID int64          `json:"message_id"`
	Reaction  []reactionType `json:"reaction"`
}

// React sets the bot's reaction to the message the target names, or clears
// it, through setMessageReaction. The call sets the whole list of the bot's
// reactions and "as non-premium users, bots can set up to one reaction per
// message": a new reaction replaces the earlier one, and a removal clears
// whichever the bot holds, which is why the adapter declares single reaction
// cardinality. An emoji outside the list Telegram gives a bot (see
// reactionEmoji) is a final rejection, unsupported, before any call; a chat
// may allow fewer, which only Telegram's answer shows.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	chatID, id, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	args := reactArgs{ChatID: chatID, MessageID: id, Reaction: []reactionType{}}
	if !r.Remove {
		emoji, ok := telegramEmoji(r.Emoji)
		if !ok {
			return rejected(model.Unsupported, "Telegram does not let a bot react with this emoji"), nil
		}
		args.Reaction = append(args.Reaction, reactionType{Type: "emoji", Emoji: emoji})
	}
	if err := x.call(ctx, "setMessageReaction", &args, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

var (
	_ adapter.Editor  = (*Instance)(nil)
	_ adapter.Deleter = (*Instance)(nil)
	_ adapter.Reactor = (*Instance)(nil)
)
