package conformance

import (
	"encoding/json"
	"reflect"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// contactsIngressSenderKind: a sender an adapter reports as a bot is
// stored as a bot identity on a bot provisional contact, authors its
// messages as a bot and is reported so in message.received. The kind
// recorded at the first claim wins over any later report, without error,
// and a bot identity seeded by ingress is never linked to a person
// contact.
func contactsIngressSenderKind(t *testing.T, env *contactsEnv) {
	bot, msg := env.arriveAs(env.one, "build-bot", model.ContactKindBot)
	c, err := env.contact(bot.Contact)
	if err != nil || bot.Kind != model.ContactKindBot || c.Kind != model.ContactKindBot {
		t.Fatalf("identity kind %q, contact %+v, %v; want a bot on a bot contact", bot.Kind, c, err)
	}
	if msg.Author.Kind != model.ContactKindBot || msg.Author.Identity != bot.Ref {
		t.Fatalf("message author %+v, want the bot identity", msg.Author)
	}
	if got := env.receivedAuthorKind(msg.Ref); got != model.ContactKindBot {
		t.Fatalf("message.received author_kind = %q, want bot", got)
	}

	// Stored kind wins, both ways.
	_, later := env.arriveAs(env.one, "build-bot", "")
	if again := env.byKey(env.one, "build-bot"); !reflect.DeepEqual(again, bot) || later.Author.Kind != model.ContactKindBot {
		t.Fatalf("bot identity %+v authored %q after a person report; want it unchanged", again, later.Author.Kind)
	}
	alice, _ := env.arriveAs(env.one, "alice", "")
	_, from := env.arriveAs(env.one, "alice", model.ContactKindBot)
	if again := env.byKey(env.one, "alice"); !reflect.DeepEqual(again, alice) || alice.Kind != model.ContactKindPerson ||
		from.Author.Kind != model.ContactKindPerson || env.receivedAuthorKind(from.Ref) != model.ContactKindPerson {
		t.Fatalf("person identity %+v authored %q after a bot report; want it unchanged", again, from.Author.Kind)
	}
	if c, err := env.contact(alice.Contact); err != nil || c.Kind != model.ContactKindPerson {
		t.Fatalf("person contact %+v, %v after a bot report", c, err)
	}

	// No bot in a person contact.
	_, err = env.link(env.linkReq(bot, env.person().Ref, "att-kind"))
	serviceCode(t, err, model.Validation)
	if again := env.byKey(env.one, "build-bot"); !reflect.DeepEqual(again, bot) {
		t.Fatalf("bot identity %+v after a refused link, want %+v", again, bot)
	}
}

// contactsIngressReplyLink: an inbound message naming the provider message
// it replies to records that message as InReplyTo, reports it on
// message.received and is found among the parent's replies; a parent the
// store does not hold leaves no link.
func contactsIngressReplyLink(t *testing.T, env *contactsEnv) {
	_, parent := env.arrive(env.one, "alice")
	e := env.envelope(env.one, "alice")
	e.InReplyTo = parent.External[0].ID
	if err := env.accept(e); err != nil {
		t.Fatalf("Accept(%s): %v", e.EventID, err)
	}
	var reply model.Message
	env.view(func(r store.Reader) error {
		ms, err := r.MessagesByReference(env.ctx, env.one, model.ProviderMessageRef(env.ch, e.MessageID), 2)
		if err != nil || len(ms) != 1 {
			t.Fatalf("reply lookup: %d messages, %v", len(ms), err)
		}
		reply = ms[0]
		page, err := r.Replies(env.ctx, parent.Ref, model.PageRequest{Limit: 10})
		if err != nil || len(page.Items) != 1 || page.Items[0].Ref != reply.Ref {
			t.Fatalf("Replies = %+v, %v; want the inbound reply", page.Items, err)
		}
		return nil
	})
	if reply.InReplyTo == nil || *reply.InReplyTo != parent.Ref {
		t.Fatalf("InReplyTo = %v, want %v", reply.InReplyTo, parent.Ref)
	}
	if got := env.received(reply.Ref).InReplyTo; got == nil || *got != parent.Ref {
		t.Fatalf("message.received in_reply_to = %v, want %v", got, parent.Ref)
	}
	if parent.InReplyTo != nil || env.received(parent.Ref).InReplyTo != nil {
		t.Fatalf("a message naming no parent records a reply link")
	}

	orphan := env.envelope(env.one, "alice")
	orphan.InReplyTo = "never-held"
	if err := env.accept(orphan); err != nil {
		t.Fatalf("Accept(%s): %v", orphan.EventID, err)
	}
	var stored model.Message
	env.view(func(r store.Reader) error {
		ms, err := r.MessagesByReference(env.ctx, env.one, model.ProviderMessageRef(env.ch, orphan.MessageID), 2)
		if err != nil || len(ms) != 1 {
			t.Fatalf("orphan lookup: %d messages, %v", len(ms), err)
		}
		stored = ms[0]
		return nil
	})
	if stored.InReplyTo != nil || env.received(stored.Ref).InReplyTo != nil {
		t.Fatalf("an unheld parent recorded a reply link %v", stored.InReplyTo)
	}
}

// receivedAuthorKind is the author_kind of m's message.received event.
func (env *contactsEnv) receivedAuthorKind(m model.MessageRef) string {
	return env.received(m).AuthorKind
}

// received is the message.received payload of m.
func (env *contactsEnv) received(m model.MessageRef) model.MessageReceivedPayload {
	env.t.Helper()
	var kind model.MessageReceivedPayload
	found := false
	env.view(func(r store.Reader) error {
		after, err := r.EventBase(env.ctx)
		if err != nil {
			return err
		}
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: []string{model.EventMessageReceived}}, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				var payload model.MessageReceivedPayload
				if err := json.Unmarshal(ev.Payload.Data, &payload); err != nil {
					return err
				}
				if payload.Message == m {
					kind, found = payload, true
				}
			}
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			after = p.Next
		}
	})
	if !found {
		env.t.Fatalf("no message.received for %s", m.ID)
	}
	return kind
}
