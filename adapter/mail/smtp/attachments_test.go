package smtp_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

// withAttachments is a multipart mail with one base64 attachment per name,
// the bytes of each being "bytes of <name>".
func withAttachments(id string, names ...string) string {
	var b strings.Builder
	b.WriteString("From: jane@example.org\nTo: support@example.com\nSubject: files\nMessage-ID: " + id + "\n")
	b.WriteString("MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nplease see attached\n")
	for _, n := range names {
		fmt.Fprintf(&b, "--b\nContent-Type: application/octet-stream; name=%q\nContent-Disposition: attachment; filename=%q\nContent-Transfer-Encoding: base64\n\n%s\n",
			n, n, base64.StdEncoding.EncodeToString([]byte("bytes of "+n)))
	}
	b.WriteString("--b--\n")
	return b.String()
}

func attachmentRefs(m model.Message) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range m.Content.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil {
			out = append(out, *p.Attachment)
		}
	}
	return out
}

// descriptors reads the PartsSchema part of m: which attachments were
// described and which of them have staged bytes.
func descriptors(t *testing.T, m model.Message) []struct {
	Filename string `json:"filename"`
	Staged   bool   `json:"staged"`
} {
	t.Helper()
	for _, p := range m.Content.Parts {
		if p.Kind == model.PartStructured && p.Structured.Name == smtp.PartsSchema {
			var rec struct {
				Attachments []struct {
					Filename string `json:"filename"`
					Staged   bool   `json:"staged"`
				} `json:"attachments"`
			}
			if err := json.Unmarshal(p.Structured.Data, &rec); err != nil {
				t.Fatal(err)
			}
			return rec.Attachments
		}
	}
	t.Fatalf("no %s part in %+v", smtp.PartsSchema, m.Content.Parts)
	return nil
}

func (r *engineRig) readAttachment(ref model.AttachmentRef) (model.Attachment, string) {
	r.t.Helper()
	att, rc, err := r.client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: ref})
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return att, string(b)
}

// The attachments of a received mail are staged through the engine's sink:
// the message names each staged attachment, its descriptor says so, and a
// redelivery of the mail restages the same ones.
func TestEngineStagesAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{})
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)
	raw := withAttachments("<a1@example.org>", "invoice.pdf", "notes.txt")
	for range 2 {
		if err := m.send(t, janeTx, raw); err != nil {
			t.Fatal(err)
		}
	}
	msgs := r.messages(inboxA)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (redelivery deduplicated)", len(msgs))
	}
	refs := attachmentRefs(msgs[0])
	if len(refs) != 2 {
		t.Fatalf("parts %+v, want both attachments staged", msgs[0].Content.Parts)
	}
	for i, name := range []string{"invoice.pdf", "notes.txt"} {
		att, got := r.readAttachment(refs[i])
		if got != "bytes of "+name || att.Metadata.Name != name || att.Metadata.Audience.Visibility != model.Public {
			t.Errorf("attachment %d = %+v %q", i, att, got)
		}
	}
	for _, d := range descriptors(t, msgs[0]) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Filename)
		}
	}
}

// Past the bound, attachments are described and not staged.
func TestEngineStagesAtMostMaxAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{})
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)
	var names []string
	for i := range stage.MaxAttachments + 2 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	if err := m.send(t, janeTx, withAttachments("<a2@example.org>", names...)); err != nil {
		t.Fatal(err)
	}
	msg := r.messages(inboxA)[0]
	if n := len(attachmentRefs(msg)); n != stage.MaxAttachments {
		t.Fatalf("staged %d, want %d", n, stage.MaxAttachments)
	}
	staged := 0
	ds := descriptors(t, msg)
	if len(ds) != len(names) {
		t.Fatalf("described %d, want all %d", len(ds), len(names))
	}
	for i, d := range ds {
		if d.Staged != (i < stage.MaxAttachments) {
			t.Errorf("%s staged = %v", d.Filename, d.Staged)
		}
		if d.Staged {
			staged++
		}
	}
	if staged != stage.MaxAttachments {
		t.Errorf("descriptors mark %d staged, want %d", staged, stage.MaxAttachments)
	}
}

// Without a blob store the mail is delivered as before, described only.
func TestEngineWithoutBlobStoreDescribesAttachments(t *testing.T) {
	r := newEngineRig(t)
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)
	if err := m.send(t, janeTx, withAttachments("<a3@example.org>", "invoice.pdf")); err != nil {
		t.Fatalf("send = %v, want 250: a missing blob store never refuses mail", err)
	}
	msg := r.messages(inboxA)[0]
	if n := len(attachmentRefs(msg)); n != 0 {
		t.Errorf("staged %d without a blob store", n)
	}
	if ds := descriptors(t, msg); len(ds) != 1 || ds[0].Staged || ds[0].Filename != "invoice.pdf" {
		t.Errorf("descriptors = %+v", ds)
	}
}

// A message to two destinations of one channel stages its attachments
// once per destination, in that destination's inbox.
func TestEngineStagesPerDestination(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{})
	r.seed(channelB, inboxB, "", "support", "sales")
	m := r.receive(channelB)
	tx := janeTx
	tx.rcpts = []string{"support@example.com", "sales@example.com"}
	if err := m.send(t, tx, withAttachments("<a4@example.org>", "invoice.pdf")); err != nil {
		t.Fatal(err)
	}
	msgs := r.messages(inboxB)
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want one per destination", len(msgs))
	}
	var seen []string
	for _, msg := range msgs {
		refs := attachmentRefs(msg)
		if len(refs) != 1 {
			t.Fatalf("message %+v: %d staged", msg.Ref, len(refs))
		}
		_, got := r.readAttachment(refs[0])
		seen = append(seen, got)
	}
	if !bytes.Equal([]byte(seen[0]), []byte(seen[1])) || seen[0] != "bytes of invoice.pdf" {
		t.Errorf("attachments = %q", seen)
	}
}

// flakyBlobs fails its first Stage, as a blob store that is briefly down.
type flakyBlobs struct {
	*portstest.MemoryBlobs
	failed bool
}

func (f *flakyBlobs) Stage(ctx context.Context, k ports.BlobKey, r io.Reader, size int64) error {
	if !f.failed {
		f.failed = true
		return errors.New("blob store down")
	}
	return f.MemoryBlobs.Stage(ctx, k, r, size)
}

// A blob store that fails is a temporary failure of the delivery, not a
// message delivered without its attachments: the sender retries.
func TestEngineStagingFailureStaysWithSender(t *testing.T) {
	r := newEngineRigBlobs(t, &flakyBlobs{MemoryBlobs: &portstest.MemoryBlobs{}})
	r.seed(channelA, inboxA, "", "support")
	m := r.receive(channelA)
	raw := withAttachments("<a5@example.org>", "invoice.pdf")
	if err := m.send(t, janeTx, raw); replyCode(err) != 451 {
		t.Fatalf("send = %v, want 451", err)
	}
	if n := len(r.messages(inboxA)); n != 0 {
		t.Fatalf("messages = %d, want none: the message is not delivered without its attachment", n)
	}
	if err := m.send(t, janeTx, raw); err != nil {
		t.Fatalf("retry: %v", err)
	}
	msgs := r.messages(inboxA)
	if len(msgs) != 1 || len(attachmentRefs(msgs[0])) != 1 {
		t.Fatalf("messages = %+v, want one with its attachment", msgs)
	}
}
