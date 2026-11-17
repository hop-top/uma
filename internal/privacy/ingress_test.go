package privacy_test

import (
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Redacting an inbound message clears the raw ingress copies of it, its
// creation and its later edits, and keeps their deduplication: a
// redelivered envelope is a duplicate and an edit arriving afterwards does
// not bring the content back.
func TestRedactInboundClearsRawIngress(t *testing.T) {
	h := enginetest.NewHarness(t, enginetest.ChatType)
	in := h.Configure(enginetest.PerContact(false), enginetest.Dest)
	created := h.Inbound("e-1", "pm-1", "alice", enginetest.Dest)
	created.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: secret}}}
	first := h.MustAccept(created)
	edit := h.Inbound("e-2", "pm-1", "alice", enginetest.Dest)
	edit.Kind = model.EnvelopeMessageEdited
	edit.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: secret + " edited"}}}
	second := h.MustAccept(edit)
	m := h.MessageByProviderID(in.Ref, "pm-1")
	if len(m.Content.Parts) == 0 {
		t.Fatal("inbound message has no content to redact")
	}

	ref := m.Ref
	if _, err := h.Client.Privacy().Redact(h.Ctx, model.Redact{
		Command: model.Command{Call: model.Call{Correlation: "c"}, Operation: "op-redact", ExpectedRevision: m.Revision},
		Target:  ref.Ref(), Message: &ref,
	}); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	h.View(func(r store.Reader) error {
		for _, id := range []model.ID{first.ID, second.ID} {
			rec, err := r.Ingress(h.Ctx, id)
			if err != nil {
				return err
			}
			env := rec.Input.Envelope
			if len(env.Content.Parts) != 0 || env.MessageID != "pm-1" || rec.DedupKey == "" || rec.Receipt.ID != id {
				t.Errorf("ingress %s = %+v, want content cleared and routing kept", id, rec)
			}
		}
		return nil
	})

	if again := h.MustAccept(created); !again.Duplicate {
		t.Errorf("redelivery receipt = %+v, want a duplicate", again)
	}
	late := h.Inbound("e-3", "pm-1", "alice", enginetest.Dest)
	late.Kind = model.EnvelopeMessageEdited
	late.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: secret}}}
	h.MustAccept(late)
	if got := h.MessageByProviderID(in.Ref, "pm-1"); !got.Redacted || len(got.Content.Parts) != 0 {
		t.Fatalf("message after a late edit = %+v, want the tombstone kept", got)
	}
	requireNoSecret(t, h.Events())
}
