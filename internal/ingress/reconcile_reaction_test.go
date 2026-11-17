package ingress_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// Inbound reactions are stored on the message, attributed to the
// reacting identity, and announced with a revision that moves.
func TestInboundReactionStoredAndRemoved(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	orig := h.MessageByProviderID(in.Ref, "m1")
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	fan, _ := identityOf(h, in.Ref, "fan")
	added := h.MessageByProviderID(in.Ref, "m1")
	if len(added.Reactions) != 1 || added.Reactions[0].Emoji != "👍" || added.Reactions[0].By == nil ||
		*added.Reactions[0].By != fan.Ref || added.Reactions[0].Count != 1 || added.ReactionsComplete ||
		added.Revision == orig.Revision || added.LastEdit != nil || string(added.Content.Parts[0].Text) != text(orig) {
		t.Fatalf("after the reaction: %+v", added)
	}
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionRemoved, "m1", "fan", "👍"))
	gone := h.MessageByProviderID(in.Ref, "m1")
	if len(gone.Reactions) != 0 || gone.Revision == added.Revision {
		t.Fatalf("after the removal: %+v", gone)
	}
	us := updatesOf(t, h, gone)
	if len(us) != 2 || us[0].Change != "reaction_added" || us[1].Change != "reaction_removed" {
		t.Fatalf("events = %v", changesOf(us))
	}
	if us[0].PreviousRevision != orig.Revision || us[0].event.Revision != added.Revision ||
		us[1].PreviousRevision != added.Revision || us[1].event.Revision != gone.Revision {
		t.Fatalf("revisions = %+v", us)
	}
	if r := us[0].Reaction; r == nil || r.Emoji != "👍" || r.By == nil || *r.By != fan.Ref {
		t.Fatalf("reaction payload = %+v", r)
	}
}

// A repeated add, a removal of an absent reaction and a redelivery under
// another event ID change nothing and emit nothing.
func TestRepeatedReactionEventIsIdempotent(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	before, evs := h.MessageByProviderID(in.Ref, "m1"), len(h.Events())
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	h.MustAccept(reacted(h, "e4", model.EnvelopeReactionRemoved, "m1", "fan", "🎉"))
	h.MustAccept(reacted(h, "e5", model.EnvelopeReactionRemoved, "m1", "stranger", "👍"))
	if got := h.MessageByProviderID(in.Ref, "m1"); got.Revision != before.Revision || len(got.Reactions) != 1 {
		t.Fatalf("message = %+v, want it unchanged", got)
	}
	if n := len(h.Events()); n != evs {
		t.Fatalf("repeated reactions emitted %d events", n-evs)
	}
	// Two people with the same emoji are two entries.
	h.MustAccept(reacted(h, "e6", model.EnvelopeReactionAdded, "m1", "other", "👍"))
	if got := h.MessageByProviderID(in.Ref, "m1"); len(got.Reactions) != 2 {
		t.Fatalf("reactions = %+v, want one per identity", got.Reactions)
	}
}

// A removal naming no emoji (WhatsApp) resolves to the sender's single
// reaction; with several it cannot be resolved.
func TestReactionRemovalWithoutEmojiResolvesSingle(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "other", "👍"))
	h.MustAccept(reacted(h, "e4", model.EnvelopeReactionRemoved, "m1", "fan", ""))
	got := h.MessageByProviderID(in.Ref, "m1")
	other, _ := identityOf(h, in.Ref, "other")
	if len(got.Reactions) != 1 || *got.Reactions[0].By != other.Ref {
		t.Fatalf("reactions = %+v, want only the other's", got.Reactions)
	}
	us := updatesOf(t, h, got)
	if last := us[len(us)-1]; last.Change != "reaction_removed" || last.Reaction == nil || last.Reaction.Emoji != "👍" {
		t.Fatalf("last event = %+v, want the removal naming the resolved emoji", last)
	}
	// Nothing left to resolve: idempotent.
	evs := len(h.Events())
	h.MustAccept(reacted(h, "e5", model.EnvelopeReactionRemoved, "m1", "fan", ""))
	if n := len(h.Events()); n != evs {
		t.Fatalf("removal of nothing emitted %d events", n-evs)
	}
	// Two reactions of one sender: ambiguous.
	h.MustAccept(reacted(h, "e6", model.EnvelopeReactionAdded, "m1", "other", "🎉"))
	_, err := h.Accept(reacted(h, "e7", model.EnvelopeReactionRemoved, "m1", "other", ""))
	requireCode(t, err, model.Validation, "emoji")
	// An add still names its emoji.
	_, err = h.Accept(reacted(h, "e8", model.EnvelopeReactionAdded, "m1", "other", ""))
	requireCode(t, err, model.Validation, "emoji")
}

// A count-only snapshot from a system actor stores per-emoji counts and
// never invents who reacted; a later snapshot replaces the counts and
// leaves attributed reactions alone.
func TestReactionSummaryStoresCountsWithoutIdentity(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	idents := len(h.Identities(in.Ref))
	at := t0.Add(time.Minute)
	h.MustAccept(summary(h, "e3", "m1", at, model.ReactionSummary{Counts: []model.ReactionCount{{Emoji: "👍", Count: 3}, {Emoji: "🎉", Count: 2}}}))
	got := h.MessageByProviderID(in.Ref, "m1")
	var thumbs, party *model.MessageReaction
	for i, r := range got.Reactions {
		switch {
		case r.Emoji == "👍" && !r.Attributed():
			thumbs = &got.Reactions[i]
		case r.Emoji == "🎉":
			party = &got.Reactions[i]
		}
	}
	if len(got.Reactions) != 3 || thumbs == nil || thumbs.Count != 3 || party == nil || party.Count != 2 || party.By != nil || party.Actor != "" {
		t.Fatalf("reactions = %+v, want an attributed 👍 plus count-only 👍 3 and 🎉 2", got.Reactions)
	}
	if err := model.ValidateReactions(got.Reactions); err != nil || got.ReactionsComplete {
		t.Fatalf("reactions invalid (%v) or complete: %+v", err, got)
	}
	if n := len(h.Identities(in.Ref)); n != idents {
		t.Fatalf("identities %d -> %d: a snapshot claimed one", idents, n)
	}
	us := updatesOf(t, h, got)
	if last := us[len(us)-1]; last.Change != "reaction_added" || last.Reaction == nil || last.Reaction.By != nil {
		t.Fatalf("last event = %+v, want a reaction change without a By", last)
	}
	// Replaced: 🎉 gone, 👍 lower.
	h.MustAccept(summary(h, "e4", "m1", at.Add(time.Minute), model.ReactionSummary{Counts: []model.ReactionCount{{Emoji: "👍", Count: 1}}}))
	got = h.MessageByProviderID(in.Ref, "m1")
	if len(got.Reactions) != 2 {
		t.Fatalf("reactions = %+v, want the attributed 👍 and count-only 👍 1", got.Reactions)
	}
	for _, r := range got.Reactions {
		if r.Emoji == "🎉" || (!r.Attributed() && r.Count != 1) {
			t.Fatalf("reactions = %+v", got.Reactions)
		}
	}
	// A summary that matches what is stored writes nothing.
	evs, rev := len(h.Events()), got.Revision
	h.MustAccept(summary(h, "e5", "m1", at.Add(2*time.Minute), model.ReactionSummary{Counts: []model.ReactionCount{{Emoji: "👍", Count: 1}}}))
	if n := len(h.Events()); n != evs || h.MessageByProviderID(in.Ref, "m1").Revision != rev {
		t.Fatalf("an unchanged summary wrote")
	}
}

// Only an authoritative snapshot (a refetched copy) completes the set.
func TestReactionsCompleteOnlyFromAuthoritativeSnapshot(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	snap := model.ReactionSummary{Counts: []model.ReactionCount{{Emoji: "👍", Count: 2}}, Complete: true}
	h.MustAccept(summary(h, "e2", "m1", t0.Add(time.Minute), snap))
	if got := h.MessageByProviderID(in.Ref, "m1"); got.ReactionsComplete {
		t.Fatalf("an event-derived snapshot completed the set")
	}
	in2 := model.Ingress{Call: model.Call{Correlation: "refetch"}, Envelope: summary(h, "e3", "m1", t0.Add(2*time.Minute), snap)}
	if _, err := h.Sink.AcceptRefetched(h.Ctx, model.OperationRef{Tenant: tenantA, ID: h.ID()}, in2); err != nil {
		t.Fatal(err)
	}
	if got := h.MessageByProviderID(in.Ref, "m1"); !got.ReactionsComplete {
		t.Fatalf("an authoritative snapshot left the set incomplete: %+v", got)
	}
}

// A deleted message takes no reaction.
func TestReactionOnDeletedMessageIgnored(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	del := h.Inbound("e2", "m1", "author", dest)
	del.Kind, del.Content = model.EnvelopeMessageDeleted, model.Content{}
	h.MustAccept(del)
	before, evs := h.MessageByProviderID(in.Ref, "m1"), len(h.Events())
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	h.MustAccept(summary(h, "e4", "m1", t0.Add(time.Hour), model.ReactionSummary{Counts: []model.ReactionCount{{Emoji: "👍", Count: 1}}}))
	if got := h.MessageByProviderID(in.Ref, "m1"); got.Revision != before.Revision || len(got.Reactions) != 0 {
		t.Fatalf("deleted message took a reaction: %+v", got)
	}
	if n := len(h.Events()); n != evs {
		t.Fatalf("reaction on a deleted message emitted %d events", n-evs)
	}
}

// A system actor's reaction must be a snapshot; anything else needs a
// sender.
func TestSystemReactionNeedsSummary(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	env := reacted(h, "e2", model.EnvelopeReactionAdded, "m1", "", "👍")
	env.Sender, env.SystemActor = model.ExternalRef{}, true
	_, err := h.Accept(env)
	requireCode(t, err, model.Validation, "reaction")
	bad := summary(h, "e3", "m1", t0, model.ReactionSummary{})
	bad.Content.Parts[0].Structured.Version = 2
	_, err = h.Accept(bad)
	requireCode(t, err, model.SchemaMismatch, "")
}
