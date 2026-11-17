package ingress_test

import (
	"encoding/json"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

func messageReceivedOf(t *testing.T, h *harness, msg model.MessageRef) model.MessageReceivedPayload {
	t.Helper()
	for _, ev := range h.Events() {
		if ev.Kind != model.EventMessageReceived {
			continue
		}
		var p model.MessageReceivedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		if p.Message == msg {
			return p
		}
	}
	t.Fatalf("no message.received event for %v", msg)
	return model.MessageReceivedPayload{}
}

// An inbound message naming the provider message it replies to records
// that message's ref on the message and on its message.received event.
func TestInboundReplyResolvesToStoredMessage(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	reply := h.Inbound("e2", "m2", "u1", dest)
	reply.InReplyTo = "m1"
	h.MustAccept(reply)
	parent, got := h.MessageByProviderID(in.Ref, "m1"), h.MessageByProviderID(in.Ref, "m2")
	if got.InReplyTo == nil || *got.InReplyTo != parent.Ref {
		t.Fatalf("InReplyTo = %v, want %v", got.InReplyTo, parent.Ref)
	}
	if p := messageReceivedOf(t, h, got.Ref); p.InReplyTo == nil || *p.InReplyTo != parent.Ref {
		t.Fatalf("event in_reply_to = %v, want %v", p.InReplyTo, parent.Ref)
	}
	if first := h.MessageByProviderID(in.Ref, "m1"); first.InReplyTo != nil {
		t.Fatalf("a message naming no parent has InReplyTo %v", first.InReplyTo)
	}
	var replies []model.Message
	h.View(func(r store.Reader) error {
		page, err := r.Replies(h.Ctx, parent.Ref, model.PageRequest{Limit: 10})
		replies = page.Items
		return err
	})
	if len(replies) != 1 || replies[0].Ref != got.Ref {
		t.Fatalf("Replies = %v, want the inbound reply", replies)
	}
}

// A parent uma does not hold, or holds in another conversation, leaves the
// reply link unset: the provider's own payload still carries it.
func TestInboundReplyUnresolvedIsOmitted(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	unknown := h.Inbound("e1", "m1", "u1", dest)
	unknown.InReplyTo = "never-seen"
	h.MustAccept(unknown)
	if got := h.MessageByProviderID(in.Ref, "m1"); got.InReplyTo != nil {
		t.Fatalf("unknown parent resolved to %v", got.InReplyTo)
	}
	if p := messageReceivedOf(t, h, h.MessageByProviderID(in.Ref, "m1").Ref); p.InReplyTo != nil {
		t.Fatalf("event in_reply_to = %v", p.InReplyTo)
	}
}

func TestInboundReplyOnlyOnNewMessages(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	react := h.Inbound("e2", "m1", "u2", dest)
	react.Kind = model.EnvelopeReactionAdded
	react.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "👍"}}}
	react.InReplyTo = "m1"
	if _, err := h.Accept(react); !model.IsCode(err, model.Validation) {
		t.Fatalf("reaction naming a reply parent: err %v, want validation", err)
	}
}

// A mail reply names its parent by Message-ID, however the stored message
// holds it.
func TestInboundMailReplyResolvesByMessageID(t *testing.T) {
	h := newHarness(t, model.TypeSmtpMail)
	in := h.Configure(perContact(false), dest)
	first := h.Inbound("e1", "graph-1", "alice", dest)
	first.References = model.EmailHeaders{MessageID: "<a1@a.example>"}.Normalize(h.Channel.Ref).References
	h.MustAccept(first)
	reply := h.Inbound("e2", "graph-2", "alice", dest)
	hdr := model.EmailHeaders{MessageID: "<a2@a.example>", InReplyTo: "<a1@a.example>"}.Normalize(h.Channel.Ref)
	reply.References, reply.InReplyTo = hdr.References, hdr.InReplyTo
	h.MustAccept(reply)
	parent, got := h.MessageByProviderID(in.Ref, "graph-1"), h.MessageByProviderID(in.Ref, "graph-2")
	if got.InReplyTo == nil || *got.InReplyTo != parent.Ref {
		t.Fatalf("InReplyTo = %v, want %v", got.InReplyTo, parent.Ref)
	}
}
