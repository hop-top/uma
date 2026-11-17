package bot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"

	"hop.top/uma/model"
)

// Turn is one routed event handed to a handler.
type Turn struct {
	// Event is the delivered event.
	Event model.Event
	// Route and Handler name the route that claimed the event.
	Route   Route
	Handler string
	// Conversation is the conversation the event names; zero when it names
	// none.
	Conversation model.ConversationRef
	// Message is the message of a message.received event, read through the
	// bot's client; nil for other kinds.
	Message *model.Message
	// Interaction is the interaction of an interaction.received event,
	// read through the bot's client; nil for other kinds. Its response
	// token is never part of it.
	Interaction *model.Interaction

	bot       *Bot
	mu        sync.Mutex
	replies   int
	responses int
	followUps int
}

// OperationID returns the operation ID of the turn's command named key:
// a digest of the bot, the event ID, the handler and key. The same key in a
// redelivered turn yields the same ID, so the command replays.
func (t *Turn) OperationID(key string) model.OperationID {
	h := sha256.New()
	for _, s := range []string{string(t.bot.cfg.Self), string(t.Event.ID), t.Handler, key} {
		h.Write(strconv.AppendInt(nil, int64(len(s)), 10))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	return model.OperationID("bot." + hex.EncodeToString(h.Sum(nil)))
}

// Reply replies publicly in the turn's conversation, in reply to the
// turn's message when there is one. The n-th Reply of a turn uses
// OperationID("reply/<n>"): a rerun handler that replies the same content in
// the same order replays instead of sending again. A reply on an
// interaction turn names the interaction (Reply.InResponseTo) for
// correlation only: it is a public message, not a response. A reply that
// lost to a concurrent unit is resubmitted as Step resubmits its own units.
func (t *Turn) Reply(ctx context.Context, content model.Content) (model.Acceptance, error) {
	if t.Conversation == (model.ConversationRef{}) {
		return model.Acceptance{}, model.NewError(model.Validation, "bot: the event names no conversation to reply in")
	}
	t.mu.Lock()
	n := t.replies
	t.replies++
	t.mu.Unlock()
	req := model.Reply{
		Command:      model.Command{Call: t.call(), Operation: t.OperationID("reply/" + strconv.Itoa(n))},
		Conversation: t.Conversation,
		Content:      content,
	}
	if t.Message != nil {
		ref := t.Message.Ref
		req.InReplyTo = &ref
	}
	if t.Interaction != nil {
		ref := t.Interaction.Ref
		req.InResponseTo = &ref
	}
	var a model.Acceptance
	err := t.bot.resubmit(ctx, func() (err error) {
		a, err = t.bot.cfg.Client.Messages().Reply(ctx, req)
		return err
	})
	return a, err
}

// ReplyText is Reply(ctx, TextContent(s)): the same public reply, operation
// ID and acceptance. It changes no audience and no delivery mode.
func (t *Turn) ReplyText(ctx context.Context, s string) (model.Acceptance, error) {
	return t.Reply(ctx, TextContent(s))
}

// Watch makes the bot watch the turn's conversation: later messages there
// route to the Watched handler.
func (t *Turn) Watch(ctx context.Context) error {
	w, err := t.watch()
	if err != nil {
		return err
	}
	return t.bot.cfg.Watches.Add(ctx, w)
}

// Unwatch stops the bot watching the turn's conversation. Ingestion and
// history are unaffected.
func (t *Turn) Unwatch(ctx context.Context) error {
	w, err := t.watch()
	if err != nil {
		return err
	}
	return t.bot.cfg.Watches.Remove(ctx, w)
}

func (t *Turn) watch() (Watch, error) {
	switch {
	case t.bot.cfg.Watches == nil:
		return Watch{}, model.NewError(model.Validation, "bot: no watch store configured")
	case t.Conversation == (model.ConversationRef{}):
		return Watch{}, model.NewError(model.Validation, "bot: the event names no conversation to watch")
	}
	return Watch{Bot: t.bot.cfg.Self, Conversation: t.Conversation}, nil
}

// call carries the event's correlation into the turn's requests.
func (t *Turn) call() model.Call {
	c := t.Event.Correlation
	if c.Validate() != nil {
		c = "bot"
	}
	return model.Call{Correlation: c}
}

// ackOperation is the operation ID of acknowledging the batch token: the
// same batch acknowledged again replays.
func ackOperation(token string) model.OperationID {
	sum := sha256.Sum256([]byte(token))
	return model.OperationID("bot.ack." + hex.EncodeToString(sum[:]))
}
