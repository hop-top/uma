package telegram

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The provider calls the adapter's webhook as Telegram does: one Update for
// each event, in the documented shapes
// (https://core.telegram.org/bots/api#update), with the secret token in
// X-Telegram-Bot-Api-Secret-Token. What Telegram cannot say is not
// expressible: a message only some see, the deletion of a message (a bot is
// never told), a reaction summary (message_reaction_count is not read), a
// removal naming no emoji (the update lists the old reactions), a reaction
// with an emoji outside the list a bot may use, an echo of uma's own
// mutation (no marker rides on a message), a callback query with no message,
// a command with a response token (Telegram gives none), an interaction whose
// acknowledgement deadline has passed (it is the adapter's own, from the
// moment it reads the update), and submissions and dynamic options.

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Echo != nil || ev.Kind == conformance.InboundReactionSummary || ev.Kind == conformance.InboundDeleted {
		return conformance.ErrNotExpressible
	}
	if ev.Kind == conformance.InboundReactionRemoved && ev.Emoji == "" {
		return conformance.ErrNotExpressible
	}
	if ev.Kind == conformance.InboundReactionAdded || ev.Kind == conformance.InboundReactionRemoved {
		if _, ok := telegramEmoji(ev.Emoji); !ok {
			return conformance.ErrNotExpressible
		}
	}
	if in := ev.Interaction; in != nil {
		switch {
		case in.Deadline < 0, in.Kind == model.InteractionOptions, in.Kind == model.InteractionSubmission:
			return conformance.ErrNotExpressible
		case in.Kind == model.InteractionCommand && in.Token != "":
			return conformance.ErrNotExpressible
		case in.Kind != model.InteractionCommand && ev.MessageID == "":
			return conformance.ErrNotExpressible
		}
	}
	pre := p.prefix(ch)
	chat, id := splitMessage(ev.MessageID)
	if ev.Interaction == nil && ev.Kind == conformance.InboundMessage && p.f.find(pre, chat, id) == nil {
		p.f.put(pre, chat, userID(ev.Sender), ev.Text, id, p.topicOf(ev.Sender))
	}
	if p.polling {
		// Telegram queues the update; the receiver reads it with getUpdates.
		p.mu.Lock()
		if p.byMessage == nil {
			p.byMessage = map[string]string{}
		}
		if ev.Kind == conformance.InboundMessage && ev.Interaction == nil {
			p.byMessage[ev.MessageID] = ev.EventID
		}
		p.mu.Unlock()
		p.f.queueUpdate(pre, p.update(ev))
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hook == nil {
		p.hook = map[model.ChannelRef][]conformance.InboundEvent{}
	}
	p.hook[ch] = append(p.hook[ch], ev)
	return nil
}

func splitMessage(s string) (chat, id int64) {
	c, m, _ := strings.Cut(s, ":")
	chat, _ = strconv.ParseInt(c, 10, 64)
	id, _ = strconv.ParseInt(m, 10, 64)
	return chat, id
}

func userID(sender string) int64 {
	n, _ := strconv.ParseInt(sender, 10, 64)
	return n
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.hook[ch]
	p.hook[ch] = nil
	p.mu.Unlock()
	for _, ev := range evs {
		req := httptest.NewRequest(http.MethodPost, "/telegram", strings.NewReader(string(jsonBytes(p.update(ev)))))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderSecretToken, conformance.CredentialSecret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

// stamp is when a message of the suite was written: its date.
func (p *provider) stamp() time.Time { return p.now() }

// chatObject is the Chat of a peer's conversation.
func (p *provider) chatObject(sender string) obj {
	chat := p.chatOf(sender)
	switch p.mode {
	case model.ModePerThread:
		return obj{"id": chat, "type": "supergroup", "title": "forum", "is_forum": true}
	case model.ModePerContact:
		return obj{"id": chat, "type": "private", "first_name": "peer"}
	}
	return obj{"id": chat, "type": "supergroup", "title": "room"}
}

// update renders ev as the Update Telegram sends for it.
func (p *provider) update(ev conformance.InboundEvent) obj {
	updateID, _ := strconv.ParseInt(ev.EventID, 10, 64)
	chat, id := splitMessage(ev.MessageID)
	sender := userID(ev.Sender)
	from := obj{"id": sender, "is_bot": false, "first_name": "peer"}
	if len(ev.Claims) > 0 {
		// Unauthenticated: a name and a user in the payload, never the author.
		from["username"] = "claim" + ev.Claims[0]
	}
	message := func(text string) obj {
		m := obj{"message_id": id, "from": from, "chat": p.chatObject(ev.Sender), "date": p.stamp().Unix(), "text": text}
		if p.mode == model.ModePerThread {
			m["message_thread_id"], m["is_topic_message"] = p.topicOf(ev.Sender), true
		}
		if len(ev.Claims) > 0 {
			m["forward_origin"] = obj{"type": "user", "date": p.stamp().Unix(), "sender_user": obj{"id": userID(ev.Claims[0]), "is_bot": false, "first_name": "claim"}}
		}
		return m
	}
	switch ev.Kind {
	case conformance.InboundMessage:
		if ev.Interaction != nil {
			return p.interactionUpdate(ev, updateID, chat, id, from)
		}
		return updateOf(updateID, "message", message(ev.Text))
	case conformance.InboundEdited:
		m := message(ev.Text)
		m["edit_date"] = p.stamp().Add(ev.OccurredAfter).Unix()
		return updateOf(updateID, "edited_message", m)
	}
	// A reaction: the user's reactions before and after.
	e := obj{"type": "emoji", "emoji": ev.Emoji}
	old, now := []obj{}, []obj{}
	if ev.Kind == conformance.InboundReactionRemoved {
		old = []obj{e}
	} else {
		now = []obj{e}
	}
	return updateOf(updateID, "message_reaction", obj{
		"chat": p.chatObject(ev.Sender), "message_id": id, "user": from, "date": p.stamp().Add(ev.OccurredAfter).Unix(),
		"old_reaction": old, "new_reaction": now,
	})
}

// interactionUpdate renders a callback query on the message a control sits
// on, or a bot command.
func (p *provider) interactionUpdate(ev conformance.InboundEvent, updateID, chat, id int64, from obj) obj {
	in := ev.Interaction
	if in.Kind == model.InteractionCommand {
		text := "/" + in.Command
		if in.Arguments != "" {
			text += " " + in.Arguments
		}
		m := commandMessage(p.chatObject(ev.Sender), from, id+500, text, obj{"date": p.stamp().Unix()})
		if p.mode == model.ModePerThread {
			m["message_thread_id"] = p.topicOf(ev.Sender)
		}
		return updateOf(updateID, "message", m)
	}
	bot := obj{"id": testBotID, "is_bot": true, "first_name": "uma", "username": testBotName}
	msg := obj{"message_id": id, "from": bot, "chat": p.chatObject(ev.Sender), "date": p.stamp().Unix(), "text": "Approve?"}
	if p.mode == model.ModePerThread {
		msg["message_thread_id"] = p.topicOf(ev.Sender)
	}
	query := in.Token
	if query == "" {
		query = "cbq-" + ev.EventID
	}
	return callback(updateID, query, from, msg, in.Action)
}
