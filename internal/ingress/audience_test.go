package ingress_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
)

// lastUpdate returns the latest message.updated event and its payload.
func lastUpdate(t *testing.T, h *harness) (model.Event, ingress.MessageUpdatedPayload) {
	t.Helper()
	var ev model.Event
	for _, e := range h.Events() {
		if e.Kind == model.EventMessageUpdated {
			ev = e
		}
	}
	var p ingress.MessageUpdatedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatalf("no message.updated event: %v", err)
	}
	return ev, p
}

// A provider narrowing a message's audience, such as a comment made
// private, narrows the stored message, even through an edit older than its
// content; an edit never widens it, and its events carry the message's
// audience.
func TestEditNarrowsAudienceNeverWidens(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	orig := h.Inbound("e0", "m1", "u1", dest)
	orig.OccurredAt = t0.Add(time.Minute)
	h.MustAccept(orig)
	internal := model.Audience{Visibility: model.Internal}
	edit := func(event string, at time.Time, a model.Audience, text string) model.Envelope {
		env := h.Inbound(event, "m1", "u1", dest)
		env.Kind, env.SourceRevision, env.OccurredAt, env.Audience = model.EnvelopeMessageEdited, event, at, a
		env.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: text}}}
		return env
	}

	// Older than the applied content: only the audience narrows.
	h.MustAccept(edit("e1", t0, internal, "stale text"))
	m := h.MessageByProviderID(in.Ref, "m1")
	if m.Audience != internal || m.Content.Parts[0].Text != "text of e0" {
		t.Fatalf("after a stale narrowing edit: audience %+v content %q", m.Audience, m.Content.Parts[0].Text)
	}
	ev, p := lastUpdate(t, h)
	if p.Change != model.MessageChangeAudienceNarrowed || ev.Audience != internal {
		t.Fatalf("event change %q audience %+v, want audience_narrowed under internal", p.Change, ev.Audience)
	}

	// A newer edit claiming a public audience changes the content only.
	h.MustAccept(edit("e2", t0.Add(2*time.Minute), model.Audience{Visibility: model.Public}, "new text"))
	m = h.MessageByProviderID(in.Ref, "m1")
	if m.Audience != internal || m.Content.Parts[0].Text != "new text" {
		t.Fatalf("after a widening edit: audience %+v content %q", m.Audience, m.Content.Parts[0].Text)
	}
	ev, p = lastUpdate(t, h)
	if p.Change != model.MessageChangeEdited || ev.Audience != internal {
		t.Fatalf("event change %q audience %+v, want edited under the message's internal audience", p.Change, ev.Audience)
	}

	// An edit without content changes only the audience.
	other := h.Inbound("e-other", "m2", "u1", dest)
	h.MustAccept(other)
	bare := edit("e4", t0.Add(4*time.Minute), model.Audience{Visibility: model.Restricted}, "")
	bare.MessageID, bare.Content = "m2", model.Content{}
	h.MustAccept(bare)
	if m2 := h.MessageByProviderID(in.Ref, "m2"); m2.Audience.Visibility != model.Restricted || m2.Content.Parts[0].Text != "text of e-other" {
		t.Fatalf("after an audience-only edit: audience %+v content %+v", m2.Audience, m2.Content)
	}
	_, p = lastUpdate(t, h)
	if p.Change != model.MessageChangeAudienceNarrowed {
		t.Fatalf("audience-only edit change %q, want audience_narrowed", p.Change)
	}

	// Internal and restricted are not ordered: neither replaces the other.
	h.MustAccept(edit("e3", t0.Add(3*time.Minute), model.Audience{Visibility: model.Restricted}, "newer text"))
	if m = h.MessageByProviderID(in.Ref, "m1"); m.Audience != internal {
		t.Fatalf("audience %+v after a restricted edit, want internal kept", m.Audience)
	}
}
