package ingress_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
)

// receivedPayloads decodes every message.received payload in evs.
func receivedPayloads(t *testing.T, evs []model.Event) []ingress.MessageReceivedPayload {
	t.Helper()
	var out []ingress.MessageReceivedPayload
	for _, ev := range evs {
		if ev.Kind != model.EventMessageReceived {
			continue
		}
		var p ingress.MessageReceivedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// identityOf returns the one identity of source in the inbox and its
// contact.
func identityOf(h *harness, in model.InboxRef, source string) (model.Identity, model.Contact) {
	h.T.Helper()
	for _, id := range h.Identities(in) {
		if id.SourceID != source {
			continue
		}
		for _, c := range h.Contacts() {
			if c.Ref == id.Contact {
				return id, c
			}
		}
		h.T.Fatalf("identity %s has no contact", source)
	}
	h.T.Fatalf("no identity for %s", source)
	return model.Identity{}, model.Contact{}
}

// A declared sender kind is recorded on the identity and provisional
// contact ingress claims, on the message author and in message.received;
// no kind is a person.
func TestSenderKindRecordedAtClaim(t *testing.T) {
	for _, tc := range []struct{ kind, want string }{
		{"", model.ContactKindPerson},
		{model.ContactKindPerson, model.ContactKindPerson},
		{model.ContactKindBot, model.ContactKindBot},
		{model.ContactKindAgent, model.ContactKindAgent},
	} {
		t.Run(tc.want+"/"+tc.kind, func(t *testing.T) {
			h := newHarness(t, chatType)
			in := h.Configure(perContact(false), dest)
			env := h.Inbound("e1", "m1", "u1", dest)
			env.SenderKind = tc.kind
			h.MustAccept(env)
			id, c := identityOf(h, in.Ref, "u1")
			if id.Kind != tc.want || c.Kind != tc.want {
				t.Fatalf("identity kind %q, contact kind %q; want %q", id.Kind, c.Kind, tc.want)
			}
			if msgs := h.Messages(in.Ref); len(msgs) != 1 || msgs[0].Author.Kind != tc.want || msgs[0].Author.Identity != id.Ref {
				t.Fatalf("messages %+v, want one authored by a %s", msgs, tc.want)
			}
			if p := receivedPayloads(t, h.Events()); len(p) != 1 || p[0].AuthorKind != tc.want {
				t.Fatalf("message.received payloads %+v, want author_kind %q", p, tc.want)
			}
		})
	}
}

// A system actor's message reports author_kind system.
func TestSystemActorAuthorKind(t *testing.T) {
	h := newHarness(t, chatType)
	room := h.Configure(model.Selection{Mode: model.ModePerRoom}, "room")
	sys := h.Inbound("e1", "m1", "", "room", h.Ref(model.RefKindRoom, "r1", ""))
	sys.Sender, sys.SystemActor = model.ExternalRef{}, true
	h.MustAccept(sys)
	if msgs := h.Messages(room.Ref); len(msgs) != 1 || msgs[0].Author.Kind != model.AuthorKindSystem {
		t.Fatalf("messages %+v", msgs)
	}
	if p := receivedPayloads(t, h.Events()); len(p) != 1 || p[0].AuthorKind != model.AuthorKindSystem || p[0].Author != nil {
		t.Fatalf("message.received payloads %+v, want author_kind system and no author", p)
	}
}

// The kind recorded at claim wins: a later envelope reporting another
// kind for the same sender changes no identity or contact, fails nothing
// and authors the message with the stored kind.
func TestStoredSenderKindWins(t *testing.T) {
	for _, tc := range []struct{ first, later, want string }{
		{"", model.ContactKindBot, model.ContactKindPerson},
		{model.ContactKindBot, "", model.ContactKindBot},
		{model.ContactKindAgent, model.ContactKindBot, model.ContactKindAgent},
	} {
		t.Run(tc.want+"/"+tc.later, func(t *testing.T) {
			h := newHarness(t, chatType)
			in := h.Configure(perContact(false), dest)
			first := h.Inbound("e1", "m1", "u1", dest)
			first.SenderKind = tc.first
			h.MustAccept(first)
			before, contact := identityOf(h, in.Ref, "u1")
			later := h.Inbound("e2", "m2", "u1", dest)
			later.SenderKind = tc.later
			h.MustAccept(later)
			after, c := identityOf(h, in.Ref, "u1")
			if !reflect.DeepEqual(after, before) || !reflect.DeepEqual(c, contact) {
				t.Fatalf("identity %+v / contact %+v changed to %+v / %+v", before, contact, after, c)
			}
			for _, m := range h.Messages(in.Ref) {
				if m.Author.Kind != tc.want {
					t.Fatalf("message %s author kind %q, want the stored %q", m.Ref.ID, m.Author.Kind, tc.want)
				}
			}
			if p := receivedPayloads(t, h.Events()); len(p) != 2 || p[1].AuthorKind != tc.want {
				t.Fatalf("message.received payloads %+v, want author_kind %q", p, tc.want)
			}
		})
	}
}

// A message update claims its sender with the reported kind, as a new
// message does.
func TestSenderKindOnUpdateClaim(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	react := h.Inbound("e2", "m1", "reactor", dest)
	react.Kind, react.SenderKind = model.EnvelopeReactionAdded, model.ContactKindBot
	react.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "+1"}}}
	h.MustAccept(react)
	if id, c := identityOf(h, in.Ref, "reactor"); id.Kind != model.ContactKindBot || c.Kind != model.ContactKindBot {
		t.Fatalf("reacting identity kind %q, contact kind %q; want bot", id.Kind, c.Kind)
	}
}

// Ingress refuses a sender kind that is no contact kind, one the adapter
// did not declare, one without a sender, and one on an envelope kind that
// claims no sender, before storing anything.
func TestSenderKindRejected(t *testing.T) {
	cases := []struct {
		name string
		typ  model.TypeKey
		edit func(*harness, *model.Envelope)
		want string
	}{
		{"system", chatType, func(_ *harness, e *model.Envelope) { e.SenderKind = model.AuthorKindSystem }, "envelope.sender_kind: must be a contact kind"},
		{"unknown", chatType, func(_ *harness, e *model.Envelope) { e.SenderKind = "robot" }, "envelope.sender_kind: must be a contact kind"},
		{"undeclared bot", model.TypeSmtpMail, func(_ *harness, e *model.Envelope) { e.SenderKind = model.ContactKindBot }, "envelope.sender_kind: not declared by the adapter"},
		{"undeclared agent", model.TypeSmtpMail, func(_ *harness, e *model.Envelope) { e.SenderKind = model.ContactKindAgent }, "envelope.sender_kind: not declared by the adapter"},
		{"system actor", chatType, func(_ *harness, e *model.Envelope) {
			e.Sender, e.SystemActor, e.SenderKind = model.ExternalRef{}, true, model.ContactKindBot
		}, "envelope.sender_kind: requires a sender"},
		{"status without sender", chatType, func(_ *harness, e *model.Envelope) {
			e.Kind, e.Sender, e.Content, e.SenderKind = model.EnvelopeStatus, model.ExternalRef{}, model.Content{}, model.ContactKindBot
		}, "envelope.sender_kind: requires a sender"},
		{"status", chatType, func(_ *harness, e *model.Envelope) {
			e.Kind, e.Content, e.SenderKind = model.EnvelopeStatus, model.Content{}, model.ContactKindBot
		}, "envelope.sender_kind: the envelope kind claims no sender"},
		{"status person", chatType, func(_ *harness, e *model.Envelope) {
			e.Kind, e.Content, e.SenderKind = model.EnvelopeStatus, model.Content{}, model.ContactKindPerson
		}, "envelope.sender_kind: the envelope kind claims no sender"},
		{"work item", chatType, func(h *harness, e *model.Envelope) {
			e.Kind, e.Content, e.SenderKind = model.EnvelopeWorkItem, model.Content{}, model.ContactKindBot
			e.References = []model.ExternalRef{h.Ref(model.RefKindWorkItem, "1", "")}
			e.WorkItem = &model.WorkItemState{Subject: "s", Status: model.WorkItemOpen, Binding: model.WorkItemBindingActive}
		}, "envelope.sender_kind: the envelope kind claims no sender"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.typ)
			h.Configure(perContact(false), dest)
			env := h.Inbound("e1", "m1", "u1", dest)
			tc.edit(h, &env)
			before := len(h.Events())
			_, err := h.Accept(env)
			requireCode(t, err, model.Validation, tc.want)
			if len(h.Events()) != before || len(h.Contacts()) != 0 {
				t.Fatal("rejected envelope stored events or contacts")
			}
		})
	}
}

// An interaction claims its actor like a message does, so its sender kind
// is checked and recorded; kinds that claim no sender stay refused.
func TestClaimsSenderIncludesInteraction(t *testing.T) {
	for kind, want := range map[string]bool{
		model.EnvelopeMessage: true, model.EnvelopeAutomatedMessage: true,
		model.EnvelopeMessageEdited: true, model.EnvelopeInteraction: true,
		model.EnvelopeStatus: false, model.EnvelopeWorkItem: false,
	} {
		if got := ingress.ClaimsSender(kind); got != want {
			t.Errorf("ClaimsSender(%q) = %v, want %v", kind, got, want)
		}
	}
}
