package telegram

import (
	"strings"
	"time"
	"unicode/utf16"

	"hop.top/uma/model"
)

// Windows of an interaction. Telegram states none: the client "will display
// a progress bar until you call answerCallbackQuery"
// (https://core.telegram.org/bots/api#callbackquery) and refuses an answer
// to a query that has become too old, without saying when. These are the
// adapter's own bound, short on purpose: AckDeadline is how long uma has to
// record the interaction before the progress bar is a bug, and AnswerWindow
// how long a response may use the query. A response after it is the host's to
// send as an ordinary message.
const (
	AckDeadline  = 30 * time.Second
	AnswerWindow = 30 * time.Second
)

// callback reports a callback query, the press of a button of an inline
// keyboard, as an interaction of kind action: Action is the button's
// callback_data, as it is (the data is the host's own to structure, and
// Telegram caps it at 64 bytes), and the actor is the update's "from".
// The query ID is the response token: answering needs the bot token as well,
// so the ID alone is no credential. The interaction itself is named by its
// update, so a redelivery is the same interaction whatever the query ID. A query on a message sent through inline
// mode has no chat, and a game query no data; both are acknowledged and not
// reported.
func (x *Instance) callback(updateID int64, q callbackQuery) ([]delivery, error) {
	if q.ID == "" || q.From.ID <= 0 {
		return nil, malformed("callback query ID or sender")
	}
	m := q.Message
	if m == nil || m.Chat.ID == 0 || m.MessageID <= 0 || q.Data == "" || q.From.ID == x.cfg.BotID {
		return nil, nil
	}
	at := x.now()
	ref := model.ProviderMessageRef(x.channel.Ref, messageID(m.Chat.ID, m.MessageID))
	in := model.InteractionInput{
		ID: "callback:" + idString(updateID), Kind: model.InteractionAction, Action: q.Data, Actor: x.userRef(q.From.ID), Message: &ref,
		Deadline: at.Add(AckDeadline), ResponseToken: q.ID, ResponseExpiresAt: at.Add(AnswerWindow),
	}
	return []delivery{x.interactionDelivery(in, m.Chat.ID, m.MessageThreadID, at)}, nil
}

// interactionDelivery wraps in in its envelope: the interaction ID is the
// event ID, and the chat and topic scope it as they scope a message.
func (x *Instance) interactionDelivery(in model.InteractionInput, chat, thread int64, at time.Time) delivery {
	in.Bot = ptr(x.userRef(x.cfg.BotID))
	refs := x.references(chat, thread)
	in.Context = append(in.Context, refs...)
	env := model.Envelope{
		EventID: in.ID, Destination: idString(x.cfg.BotID), Kind: model.EnvelopeInteraction, Sender: in.Actor, Assurance: "verified",
		References: refs, Audience: model.Audience{Visibility: model.Public}, OccurredAt: at.UTC(), Interaction: &in,
	}
	return delivery{ingress: x.ingress(env)}
}

// botCommand is a message that starts with a command entity.
type botCommand struct {
	name string
	args string
	// ours is false for a command addressed to another bot
	// ("/status@other_bot").
	ours bool
}

// command reads the leading command of a text message: a bot_command entity
// at offset 0, as "/name" or "/name@username". Commands of a caption or
// further into a text are text.
func (x *Instance) command(m message) (botCommand, bool) {
	for _, e := range m.Entities {
		if e.Type != "bot_command" || e.Offset != 0 {
			continue
		}
		word := strings.TrimPrefix(entityText(m.Text, e), "/")
		if word == "" {
			return botCommand{}, false
		}
		name, suffix, addressed := strings.Cut(word, "@")
		if name == "" {
			return botCommand{}, false
		}
		return botCommand{
			name: name, args: strings.TrimSpace(afterEntity(m.Text, e)),
			ours: !addressed || strings.EqualFold(suffix, x.cfg.BotUsername),
		}, true
	}
	return botCommand{}, false
}

// afterEntity is the text that follows an entity (offsets are UTF-16 code
// units).
func afterEntity(text string, e entity) string {
	u := utf16.Encode([]rune(text))
	if end := e.Offset + e.Length; end >= 0 && end <= len(u) {
		return string(utf16.Decode(u[end:]))
	}
	return ""
}

// commandDelivery reports a command addressed to the bot as an interaction of
// kind command, never as a message: Command is its name without the slash
// and the bot's suffix, Arguments the raw text after it. Telegram gives a
// command no response token; a response is a message to the chat. One message
// is one command, whatever update delivers it, so the interaction is named
// by the message. In a group a command without a suffix is the bot's own
// too: Telegram delivers one to a bot in privacy mode only when it means it.
func (x *Instance) commandDelivery(m message, c botCommand) ([]delivery, error) {
	sender, _, ok := x.author(m)
	if !ok {
		return nil, nil
	}
	at := time.Unix(m.Date, 0).UTC()
	in := model.InteractionInput{
		ID: "command:" + messageID(m.Chat.ID, m.MessageID), Kind: model.InteractionCommand, Command: c.name, Arguments: c.args,
		Actor: sender, Deadline: at.Add(AckDeadline),
	}
	return []delivery{x.interactionDelivery(in, m.Chat.ID, m.MessageThreadID, at)}, nil
}
