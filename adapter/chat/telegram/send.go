package telegram

import (
	"context"
	"encoding/json"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

type replyParameters struct {
	MessageID int64 `json:"message_id"`
	// AllowSendingWithoutReply sends the message even if the one it answers
	// is gone: a reply that loses its anchor is still a reply worth
	// delivering.
	AllowSendingWithoutReply bool `json:"allow_sending_without_reply"`
}

type sendArgs struct {
	ChatID          int64            `json:"chat_id"`
	ThreadID        int64            `json:"message_thread_id,omitempty"`
	Text            string           `json:"text"`
	ParseMode       string           `json:"parse_mode,omitempty"`
	ReplyParameters *replyParameters `json:"reply_parameters,omitempty"`
	ReplyMarkup     json.RawMessage  `json:"reply_markup,omitempty"`
}

// sentMessage is what sendMessage answers: the Message sent. The chat ID of
// the answer names the chat the message is in.
type sentMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      chat  `json:"chat"`
}

// Send posts content to the chat the route names: a chat, a forum topic of
// it, or a user, whose private chat has the user's ID. The message answers
// the one the request names, when that is a message of the same chat, and
// lands in its forum topic.
//
// Only public audiences are sent: a chat's members read a message. A user's
// private chat is reached only when the user has started the bot and not
// blocked it ("Forbidden: bot can't initiate conversation with a user"); that
// refusal is final and surfaces as a rejection, not a retry.
//
// Telegram has no idempotency key and no call that finds a message again, so
// an answer lost on the way leaves the outcome unknown for good: the
// operation is never repeated by the adapter.
func (x *Instance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	t, err := x.parseRoute(r.Route)
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
	args := sendArgs{ChatID: t.chat, ThreadID: t.thread, Text: msg.text, ParseMode: msg.parseMode, ReplyMarkup: msg.keyboard}
	x.answerTarget(&args, r)
	return x.postMessage(ctx, &args)
}

// postMessage calls sendMessage and reports the message Telegram names.
func (x *Instance) postMessage(ctx context.Context, args *sendArgs) (adapter.Outcome, error) {
	var sent sentMessage
	if err := x.call(ctx, "sendMessage", args, &sent); err != nil {
		return failure(ctx, err, writeCall)
	}
	chatID := sent.Chat.ID
	if chatID == 0 {
		chatID = args.ChatID
	}
	if sent.MessageID <= 0 {
		return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "Telegram answer names no message; the message may have been sent")
	}
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(chatID, sent.MessageID))},
	}, nil
}

// answerTarget makes the send a reply to the message the request answers,
// and, on a route that names no topic, lands it in that message's topic.
func (x *Instance) answerTarget(args *sendArgs, r adapter.SendRequest) {
	t := r.Target
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage {
		return
	}
	chatID, id, ok := parseMessageID(t.ID)
	if !ok || chatID != args.ChatID {
		return
	}
	args.ReplyParameters = &replyParameters{MessageID: id, AllowSendingWithoutReply: true}
	if args.ThreadID != 0 || r.Thread == nil {
		return
	}
	for _, m := range r.Thread.Metadata {
		if m.Name != PayloadSchema {
			continue
		}
		var p struct {
			Thread int64 `json:"thread"`
		}
		if json.Unmarshal(m.Data, &p) == nil && p.Thread > 0 {
			args.ThreadID = p.Thread
			return
		}
	}
}

var _ adapter.Sender = (*Instance)(nil)
