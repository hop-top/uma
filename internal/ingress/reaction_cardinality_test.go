package ingress_test

import (
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// singleType is a chat adapter declaring single-reaction cardinality, as
// WhatsApp does.
const singleType model.TypeKey = "com.example.ingress.singlechat"

func singleHarness(t *testing.T) *harness {
	t.Helper()
	ex := enginetest.ChatExchange
	ex.Reactions = adapter.ReactionsSingle
	return newHarness(t, singleType, withAdapter(singleType, ex))
}

func emojisBy(m model.Message, by model.IdentityRef) []string {
	var out []string
	for _, r := range m.Reactions {
		if r.By != nil && *r.By == by {
			out = append(out, r.Emoji)
		}
	}
	return out
}

// Under single-reaction cardinality a reactor's new emoji replaces their
// earlier one, as an outbound reaction does: the earlier is removed and
// the new added, each a stored change with its own revision and event.
// Other reactors are untouched, and repeating the current emoji changes
// nothing.
func TestInboundSingleReactionReplaces(t *testing.T) {
	h := singleHarness(t)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "other", "👍"))
	before := h.MessageByProviderID(in.Ref, "m1")
	evs := len(h.Events())

	h.MustAccept(reacted(h, "e4", model.EnvelopeReactionAdded, "m1", "fan", "🎉"))
	fan, _ := identityOf(h, in.Ref, "fan")
	other, _ := identityOf(h, in.Ref, "other")
	got := h.MessageByProviderID(in.Ref, "m1")
	if e := emojisBy(got, fan.Ref); !slices.Equal(e, []string{"🎉"}) {
		t.Fatalf("fan's reactions = %v, want only 🎉 (%+v)", e, got.Reactions)
	}
	if e := emojisBy(got, other.Ref); !slices.Equal(e, []string{"👍"}) {
		t.Fatalf("other's reactions = %v, want 👍 untouched", e)
	}
	if err := model.ValidateReactions(got.Reactions); err != nil {
		t.Fatal(err)
	}
	us := updatesOf(t, h, got)
	us = us[len(us)-2:]
	if len(h.Events())-evs != 2 || us[0].Change != "reaction_removed" || us[0].Reaction == nil || us[0].Reaction.Emoji != "👍" ||
		us[1].Change != "reaction_added" || us[1].Reaction == nil || us[1].Reaction.Emoji != "🎉" {
		t.Fatalf("events = %v (%d new), want the 👍 removal then the 🎉 add", changesOf(us), len(h.Events())-evs)
	}
	if us[0].PreviousRevision != before.Revision || us[1].PreviousRevision != us[0].event.Revision || us[1].event.Revision != got.Revision {
		t.Fatalf("revisions do not chain: %+v", us)
	}
	for _, u := range us {
		if u.Reaction.By == nil || *u.Reaction.By != fan.Ref {
			t.Fatalf("event %+v does not name the reactor", u.Reaction)
		}
	}

	evs = len(h.Events())
	h.MustAccept(reacted(h, "e5", model.EnvelopeReactionAdded, "m1", "fan", "🎉"))
	if n := len(h.Events()); n != evs || h.MessageByProviderID(in.Ref, "m1").Revision != got.Revision {
		t.Fatalf("repeating the current emoji changed the message or emitted %d events", n-evs)
	}
}

// A removal without an emoji (WhatsApp) removes the reactor's reaction
// after a replacement, where it would be ambiguous if both were kept; a
// removal naming an emoji removes the reactor's reaction whatever its
// emoji.
func TestInboundSingleReactionRemoval(t *testing.T) {
	h := singleHarness(t)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "fan", "🎉"))
	h.MustAccept(reacted(h, "e4", model.EnvelopeReactionAdded, "m1", "other", "👍"))
	h.MustAccept(reacted(h, "e5", model.EnvelopeReactionRemoved, "m1", "fan", ""))
	other, _ := identityOf(h, in.Ref, "other")
	got := h.MessageByProviderID(in.Ref, "m1")
	if len(got.Reactions) != 1 || *got.Reactions[0].By != other.Ref {
		t.Fatalf("reactions = %+v, want only the other's", got.Reactions)
	}
	us := updatesOf(t, h, got)
	if last := us[len(us)-1]; last.Change != "reaction_removed" || last.Reaction == nil || last.Reaction.Emoji != "🎉" {
		t.Fatalf("last event = %+v, want the removal of 🎉", last)
	}

	h.MustAccept(reacted(h, "e6", model.EnvelopeReactionAdded, "m1", "fan", "🎉"))
	h.MustAccept(reacted(h, "e7", model.EnvelopeReactionRemoved, "m1", "fan", "👍"))
	fan, _ := identityOf(h, in.Ref, "fan")
	if e := emojisBy(h.MessageByProviderID(in.Ref, "m1"), fan.Ref); len(e) != 0 {
		t.Fatalf("fan's reactions = %v after a removal, want none", e)
	}
	evs := len(h.Events())
	h.MustAccept(reacted(h, "e8", model.EnvelopeReactionRemoved, "m1", "fan", ""))
	if n := len(h.Events()); n != evs {
		t.Fatalf("removal of nothing emitted %d events", n-evs)
	}
}

// Under multiple cardinality (the default) a second emoji adds an entry.
func TestInboundMultipleReactionsKept(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "fan", "🎉"))
	fan, _ := identityOf(h, in.Ref, "fan")
	if e := emojisBy(h.MessageByProviderID(in.Ref, "m1"), fan.Ref); !slices.Equal(e, []string{"👍", "🎉"}) {
		t.Fatalf("fan's reactions = %v, want both", e)
	}
}
