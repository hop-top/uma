package ingress_test

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// An inbound edit is the editing identity's: the original author stays,
// LastEdit names the provider identity, the sink's principal and the
// envelope's time. An audience-only edit leaves the editor record alone.
func TestInboundEditRecordsEditingIdentityAndKeepsAuthor(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	orig := h.MessageByProviderID(in.Ref, "m1")
	at := t0.Add(time.Minute)
	h.MustAccept(editedBy(h, "e2", "m1", "editor", at, "fixed"))
	got := h.MessageByProviderID(in.Ref, "m1")
	editor, _ := identityOf(h, in.Ref, "editor")
	if text(got) != "fixed" || got.Author != orig.Author {
		t.Fatalf("message = %+v, want new content under the original author %+v", got, orig.Author)
	}
	le := got.LastEdit
	if le == nil || le.By == nil || *le.By != editor.Ref || le.Actor != serviceAct || !le.At.Equal(at) || le.Operation != nil {
		t.Fatalf("last edit = %+v, want the editor %v by %s at %v", le, editor.Ref, serviceAct, at)
	}

	narrow := editedBy(h, "e3", "m1", "editor", at.Add(time.Minute), "")
	narrow.Content, narrow.Audience = model.Content{}, model.Audience{Visibility: model.Internal}
	h.MustAccept(narrow)
	after := h.MessageByProviderID(in.Ref, "m1")
	if after.Audience.Visibility != model.Internal || after.LastEdit == nil || *after.LastEdit.By != *le.By || !after.LastEdit.At.Equal(at) {
		t.Fatalf("after an audience-only edit: %+v, want the editor record kept", after)
	}
}

// A system actor's edit has no provider identity.
func TestSystemEditHasNoEditingIdentity(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	env := editedBy(h, "e2", "m1", "", t0.Add(time.Minute), "by the platform")
	env.Sender, env.SystemActor = model.ExternalRef{}, true
	h.MustAccept(env)
	got := h.MessageByProviderID(in.Ref, "m1")
	if le := got.LastEdit; le == nil || le.By != nil || le.Actor != serviceAct {
		t.Fatalf("last edit = %+v, want the sink with no provider identity", le)
	}
}

// An edit whose content equals what is stored writes nothing and emits
// nothing, whatever its source version: an unmarked echo of uma's own
// edit, or a redelivery under another event ID.
func TestIdenticalEditIsNoOp(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(editedBy(h, "e2", "m1", "editor", t0.Add(time.Minute), "fixed"))
	before, evs := h.MessageByProviderID(in.Ref, "m1"), len(h.Events())
	h.MustAccept(editedBy(h, "e3", "m1", "other", t0.Add(2*time.Minute), "fixed"))
	if got := h.MessageByProviderID(in.Ref, "m1"); got.Revision != before.Revision || got.LastEdit.At != before.LastEdit.At {
		t.Fatalf("identical edit rewrote the message: %+v", got)
	}
	if n := len(h.Events()); n != evs {
		t.Fatalf("identical edit emitted %d events", n-evs)
	}
}

// The edit event names refs, revisions and the change, never content.
func TestInboundEditEventCarriesNoContent(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(editedBy(h, "e2", "m1", "editor", t0.Add(time.Minute), "secret words"))
	m := h.MessageByProviderID(in.Ref, "m1")
	us := updatesOf(t, h, m)
	if len(us) != 1 || us[0].Change != "edited" {
		t.Fatalf("events = %v, want one edited", changesOf(us))
	}
	if us[0].PreviousRevision == m.Revision || us[0].PreviousRevision == "" {
		t.Fatalf("previous revision %q, revision %q", us[0].PreviousRevision, m.Revision)
	}
	for _, k := range []string{"content", "parts", "text"} {
		if _, ok := us[0].keys[k]; ok {
			t.Fatalf("payload carries %q: %s", k, us[0].event.Payload.Data)
		}
	}
}

// A provider deletion tombstones the message as deleted, not redacted:
// content and reactions go, the editor record stays, a repeat is a no-op.
func TestProviderDeleteMarksDeletedNotRedacted(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "author", dest))
	h.MustAccept(editedBy(h, "e2", "m1", "editor", t0.Add(time.Minute), "fixed"))
	h.MustAccept(reacted(h, "e3", model.EnvelopeReactionAdded, "m1", "fan", "👍"))
	edited := h.MessageByProviderID(in.Ref, "m1")
	del := h.Inbound("e4", "m1", "author", dest)
	del.Kind, del.Content = model.EnvelopeMessageDeleted, model.Content{}
	h.MustAccept(del)
	got := h.MessageByProviderID(in.Ref, "m1")
	if !got.Deleted || got.Redacted || len(got.Content.Parts) != 0 || len(got.Reactions) != 0 || got.LastEdit == nil ||
		!got.LastEdit.At.Equal(edited.LastEdit.At) {
		t.Fatalf("deleted message = %+v", got)
	}
	evs := len(h.Events())
	again := h.Inbound("e5", "m1", "author", dest)
	again.Kind, again.Content = model.EnvelopeMessageDeleted, model.Content{}
	h.MustAccept(again)
	if n := len(h.Events()); n != evs || h.MessageByProviderID(in.Ref, "m1").Revision != got.Revision {
		t.Fatalf("repeated deletion wrote")
	}
}
