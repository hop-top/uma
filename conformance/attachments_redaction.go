package conformance

import (
	"bytes"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// attachRedaction redacts an internal note holding an uploaded attachment
// through Privacy.Redact: the note becomes a tombstone under a new
// revision, the attachment record a tombstone without a locator, its bytes
// leave the blob store, and a stale expected revision is a conflict that
// changes nothing.
func attachRedaction(t *testing.T, env *attachEnv) {
	data := []byte("secret scan")
	ref := env.mustPrepare(publicMeta("scan.txt", data))
	if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	env.requireHeld(1)
	conv := env.conversation(env.inbox, model.ConversationOpen)
	a, err := env.client.Messages().AddInternalNote(env.ctx, model.InternalNote{
		Command: env.cmd(""), Conversation: conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{
			{Kind: model.PartText, Text: "secret note"},
			{Kind: model.PartAttachment, Attachment: &ref},
		}},
	})
	if err != nil {
		t.Fatalf("AddInternalNote: %v", err)
	}
	msg := *a.Operation.ResultMessage
	get := func() model.Message {
		t.Helper()
		m, err := env.client.Messages().Get(env.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "c"}, Ref: msg})
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		return m
	}
	before := get()
	redact := func(rev model.Revision) (model.Acceptance, error) {
		return env.client.Privacy().Redact(env.ctx, model.Redact{Command: env.cmd(rev), Target: msg.Ref(), Message: &msg})
	}

	_, err = redact("stale")
	requireCodeOf(t, err, model.Conflict)
	if m := get(); m.Redacted || m.Revision != before.Revision || len(m.Content.Parts) != 2 {
		t.Fatalf("note after a stale redaction = %+v, want it unchanged", m)
	}
	env.requireHeld(1)

	if _, err := redact(before.Revision); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	m := get()
	if !m.Redacted || len(m.Content.Parts) != 0 || m.Revision == before.Revision || m.Revision == "" ||
		m.Audience != before.Audience || m.Origin == nil || *m.Origin != *before.Origin {
		t.Fatalf("note after redaction = %+v, want a tombstone keeping audience and origin", m)
	}
	env.requireHeld(0)
	if rec := env.record(ref); rec.State != store.AttachmentRedacted || rec.Blob != nil ||
		rec.Attachment.Metadata.Name != "" || rec.Attachment.Metadata.Digest != "" {
		t.Fatalf("attachment record = %+v, want a tombstone", rec)
	}
	_, _, err = env.open(ref)
	requireCodeOf(t, err, model.Conflict)
}

// attachCascade redacts a conversation with Cascade, run by Runtime.Run:
// every note in it becomes a tombstone and the bytes of their attachments leave the blob
// store, while the conversation record stays.
func attachCascade(t *testing.T, env *attachEnv) {
	conv := env.conversation(env.inbox, model.ConversationOpen)
	var notes []model.MessageRef
	for i, text := range []string{"first", "second"} {
		data := []byte(text)
		ref := env.mustPrepare(publicMeta(text+".txt", data))
		if _, err := env.upload(ref, env.cmd(""), bytes.NewReader(data)); err != nil {
			t.Fatalf("Upload %d: %v", i, err)
		}
		a, err := env.client.Messages().AddInternalNote(env.ctx, model.InternalNote{
			Command: env.cmd(""), Conversation: conv.Ref, PolicyRef: "staff",
			Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}, {Kind: model.PartAttachment, Attachment: &ref}}},
		})
		if err != nil {
			t.Fatalf("AddInternalNote %d: %v", i, err)
		}
		notes = append(notes, *a.Operation.ResultMessage)
	}
	env.requireHeld(2)
	a, err := env.client.Privacy().Redact(env.ctx, model.Redact{
		Command: env.cmd(conv.Revision), Target: conv.Ref.Ref(), Cascade: true,
	})
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	if op := env.runUntil(a.Operation.Ref, func(op model.Operation) bool { return op.State != model.Queued }); op.State != model.Committed {
		t.Fatalf("cascade = %+v, want committed", op)
	}
	for _, ref := range notes {
		m, err := env.client.Messages().Get(env.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
		if err != nil || !m.Redacted || len(m.Content.Parts) != 0 {
			t.Errorf("note %s after the cascade = %+v, %v; want a tombstone", ref.ID, m, err)
		}
	}
	env.requireHeld(0)
	got, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "c"}, Ref: conv.Ref})
	if err != nil || got.Revision != conv.Revision {
		t.Errorf("conversation after the cascade = %+v, %v; want it unchanged", got, err)
	}
}
