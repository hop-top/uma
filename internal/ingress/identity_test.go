package ingress_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

func providerMessages(h *harness, in model.InboxRef, id string) []model.Message {
	h.T.Helper()
	var got []model.Message
	h.View(func(r store.Reader) error {
		var err error
		got, err = r.MessagesByReference(h.Ctx, in, model.ProviderMessageRef(h.Channel.Ref, id), 10)
		return err
	})
	return got
}

// One provider-assigned message reported under two event IDs, as a webhook
// and a poll both report it, is one message with one message.received;
// later updates of it apply without ambiguity.
func TestSecondEventForProviderMessageCreatesNoMessage(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("webhook-event", "same-message", "alice", dest))
	before := len(h.Events())
	r := h.MustAccept(h.Inbound("poll-event", "same-message", "alice", dest))
	if !r.Committed || r.Duplicate {
		t.Fatalf("second event receipt = %+v, want a committed, distinct event", r)
	}
	if got := providerMessages(h, in.Ref, "same-message"); len(got) != 1 {
		t.Fatalf("same provider message stored %d times via distinct event IDs; want one", len(got))
	}
	if evs := h.Events()[before:]; len(evs) != 0 {
		t.Fatalf("second event emitted %v, want nothing", eventKinds(evs))
	}
	if n := received(h.Events()); n != 1 {
		t.Fatalf("message.received = %d, want 1", n)
	}

	edit := h.Inbound("edit-event", "same-message", "alice", dest)
	edit.Kind, edit.SourceRevision, edit.OccurredAt = model.EnvelopeMessageEdited, "2", t0.Add(time.Minute)
	edit.Content.Parts[0].Text = "edited"
	h.MustAccept(edit)
	if got := providerMessages(h, in.Ref, "same-message"); len(got) != 1 || got[0].Content.Parts[0].Text != "edited" {
		t.Fatalf("edit after a second event: %+v", got)
	}
}

// A second report carrying a newer version of the message is that
// message's update, never a new message.
func TestSecondEventWithNewerContentUpdatesTheMessage(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	h.MustAccept(h.Inbound("webhook-event", "same-message", "alice", dest))
	later := h.Inbound("poll-event", "same-message", "alice", dest)
	later.OccurredAt, later.SourceRevision = t0.Add(time.Minute), "2"
	later.Content.Parts[0].Text = "corrected"
	before := len(h.Events())
	h.MustAccept(later)
	got := providerMessages(h, in.Ref, "same-message")
	if len(got) != 1 || got[0].Content.Parts[0].Text != "corrected" {
		t.Fatalf("messages = %+v, want one holding the newer content", got)
	}
	if kinds := eventKinds(h.Events()[before:]); len(kinds) != 1 || kinds[0] != model.EventMessageUpdated {
		t.Fatalf("second event emitted %v, want one message.updated", kinds)
	}
}

// Sender-chosen IDs carry no identity: two mails reusing one Message-ID are
// two messages.
func TestSenderChosenMessageIDIsNeverUnique(t *testing.T) {
	h := newHarness(t, model.TypeSmtpMail)
	in := h.Configure(perContact(false), dest)
	h.MustAccept(h.Inbound("e1", "<x@a.example>", "alice", dest))
	h.MustAccept(h.Inbound("e2", "<x@a.example>", "mallory", dest))
	if got := providerMessages(h, in.Ref, "<x@a.example>"); len(got) != 2 {
		t.Fatalf("mails reusing a Message-ID stored %d times, want 2", len(got))
	}
}
