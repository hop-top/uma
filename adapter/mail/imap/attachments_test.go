package imap_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// withAttachments is a multipart mail with one base64 attachment per name,
// the bytes of each being "bytes of <name>".
func withAttachments(id string, names ...string) []byte {
	var b strings.Builder
	b.WriteString("From: Alice <" + peer + ">\nTo: " + mailUser + "\nSubject: files\nDate: Sun, 11 Oct 2026 09:00:00 +0000\nMessage-ID: " + id + "\n")
	b.WriteString("MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nplease see attached\n")
	for _, n := range names {
		fmt.Fprintf(&b, "--b\nContent-Type: application/octet-stream; name=%q\nContent-Disposition: attachment; filename=%q\nContent-Transfer-Encoding: base64\n\n%s\n",
			n, n, base64.StdEncoding.EncodeToString([]byte("bytes of "+n)))
	}
	b.WriteString("--b--\n")
	return []byte(crlf(b.String()))
}

func attachmentRefs(c model.Content) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range c.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil {
			out = append(out, *p.Attachment)
		}
	}
	return out
}

type descriptor struct {
	Filename string `json:"filename"`
	Staged   bool   `json:"staged"`
}

func descriptors(t *testing.T, c model.Content) []descriptor {
	t.Helper()
	for _, p := range c.Parts {
		if p.Kind == model.PartStructured && p.Structured.Name == imap.PartsSchema {
			var rec struct {
				Attachments []descriptor `json:"attachments"`
			}
			if err := json.Unmarshal(p.Structured.Data, &rec); err != nil {
				t.Fatal(err)
			}
			return rec.Attachments
		}
	}
	t.Fatalf("no %s part in %+v", imap.PartsSchema, c.Parts)
	return nil
}

// The attachments of a retrieved mail are staged through the engine's
// sink: the message names each staged attachment and its descriptor says
// so.
func TestEngineStagesAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, passwordHost(), nil, &portstest.MemoryBlobs{})
	r.srv.deliver("INBOX", withAttachments("<a1@example.org>", "invoice.pdf", "notes.txt"), time.Now())
	r.receive()
	msgs := r.awaitMessages(1)
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	refs := attachmentRefs(msgs[0].Content)
	if len(refs) != 2 {
		t.Fatalf("parts %+v, want both attachments staged", msgs[0].Content.Parts)
	}
	for i, name := range []string{"invoice.pdf", "notes.txt"} {
		att, rc, err := r.client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: refs[i]})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		got, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil || string(got) != "bytes of "+name || att.Metadata.Name != name || att.Metadata.Audience.Visibility != model.Public {
			t.Errorf("attachment %d = %+v %q, %v", i, att, got, err)
		}
	}
	for _, d := range descriptors(t, msgs[0].Content) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Filename)
		}
	}
}

// Without a blob store the mail is delivered as before, described only.
func TestEngineWithoutBlobStoreDescribesAttachments(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", withAttachments("<a2@example.org>", "invoice.pdf"), time.Now())
	r.receive()
	msgs := r.awaitMessages(1)
	if len(msgs) != 1 || len(attachmentRefs(msgs[0].Content)) != 0 {
		t.Fatalf("messages %+v, want one delivered without staged attachments", msgs)
	}
	if ds := descriptors(t, msgs[0].Content); len(ds) != 1 || ds[0].Staged || ds[0].Filename != "invoice.pdf" {
		t.Errorf("descriptors = %+v", ds)
	}
}

// Past the bound, attachments are described and not staged.
func TestEngineStagesAtMostMaxAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, passwordHost(), nil, &portstest.MemoryBlobs{})
	var names []string
	for i := range stage.MaxAttachments + 2 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	r.srv.deliver("INBOX", withAttachments("<a3@example.org>", names...), time.Now())
	r.receive()
	msgs := r.awaitMessages(1)
	if len(msgs) != 1 || len(attachmentRefs(msgs[0].Content)) != stage.MaxAttachments {
		t.Fatalf("messages %d, staged %d, want %d", len(msgs), len(attachmentRefs(msgs[0].Content)), stage.MaxAttachments)
	}
	ds := descriptors(t, msgs[0].Content)
	if len(ds) != len(names) {
		t.Fatalf("described %d, want all %d", len(ds), len(names))
	}
	for i, d := range ds {
		if d.Staged != (i < stage.MaxAttachments) {
			t.Errorf("%s staged = %v", d.Filename, d.Staged)
		}
	}
}

// stagingSink is the receiver test's sink that can also stage; staging
// answers stageErr while it is set.
type stagingSink struct {
	*sink
	stageErr error
	staged   int
}

func (s *stagingSink) StageAttachment(_ context.Context, r adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return model.AttachmentRef{}, err
	}
	if s.stageErr != nil {
		return model.AttachmentRef{}, s.stageErr
	}
	s.staged++
	return model.AttachmentRef{Inbox: inboxA, ID: "00000e00-0000-7000-8000-0000000000c1"}, nil
}

// A staging failure a retry can fix ends Run like a sink refusal: the
// cursor stays, and the next receiver delivers the message with its
// attachment.
func TestStagingFailureKeepsMessage(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	f := newFactory(t)
	ch := channel(channelA, config(srv, nil))
	srv.deliver("INBOX", withAttachments("<a4@example.org>", "invoice.pdf"), time.Now())
	s := &stagingSink{sink: newSink(), stageErr: model.NewError(model.Unavailable, "blob store down")}
	err := run(t, open(t, f, ch, adapter.Dependencies{Credentials: passwordCreds()}), s).result(t)
	if model.CodeOf(err) != model.Unavailable {
		t.Fatalf("Run = %v, want the staging error", err)
	}
	if n := len(s.envelopes()); n != 0 {
		t.Fatalf("delivered %d messages, want none without the attachment", n)
	}
	s.stageErr = nil
	run(t, open(t, f, ch, adapter.Dependencies{Credentials: passwordCreds()}), s)
	got := s.await(t, 1)
	if len(attachmentRefs(got[0].Content)) != 1 {
		t.Fatalf("content %+v, want the staged attachment", got[0].Content.Parts)
	}
}

// An engine refusal of one attachment (its size or media type checks)
// leaves it described; the message is delivered.
func TestRefusedAttachmentIsDescribedOnly(t *testing.T) {
	srv := newMailServer(t, serverOptions{sec: implicitTLS})
	srv.deliver("INBOX", withAttachments("<a5@example.org>", "invoice.pdf"), time.Now())
	inst := open(t, newFactory(t), channel(channelA, config(srv, nil)), adapter.Dependencies{Credentials: passwordCreds()})
	s := &stagingSink{sink: newSink(), stageErr: model.NewError(model.Validation, "media type refused")}
	run(t, inst, s)
	got := s.await(t, 1)
	if len(attachmentRefs(got[0].Content)) != 0 {
		t.Fatalf("content %+v, want no staged attachment", got[0].Content.Parts)
	}
	if ds := descriptors(t, got[0].Content); len(ds) != 1 || ds[0].Staged {
		t.Errorf("descriptors = %+v", ds)
	}
}

// openAll reads the staged attachments of c, keyed by name.
func openAll(t *testing.T, r *engineRig, c model.Content) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ref := range attachmentRefs(c) {
		att, rc, err := r.client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: ref})
		if err != nil {
			t.Fatalf("Open %v: %v", ref, err)
		}
		b, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[att.Metadata.Name] = string(b)
	}
	return out
}

// An imported mail's attachments are staged as the receiver stages them: the
// message names them and holds their bytes, and the descriptors say staged.
func TestEngineImportStagesAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, passwordHost(), nil, &portstest.MemoryBlobs{})
	r.srv.deliver("INBOX", withAttachments("<h1@example.org>", "invoice.pdf", "notes.txt"), time.Now().Add(-10*time.Minute))
	rep, err := importHistory(r, time.Now().Add(-time.Hour))
	if err != nil || rep.Imported != 1 || !rep.Complete {
		t.Fatalf("import %+v, %v", rep, err)
	}
	msgs := r.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	got := openAll(t, r, msgs[0].Content)
	if len(got) != 2 || got["invoice.pdf"] != "bytes of invoice.pdf" || got["notes.txt"] != "bytes of notes.txt" {
		t.Errorf("staged attachments = %v, want both with their bytes", got)
	}
	for _, d := range descriptors(t, msgs[0].Content) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Filename)
		}
	}
}

// The bounds of the receiver apply to a history read: past MaxAttachments the
// rest are described only.
func TestEngineImportStagesAtMostMaxAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, passwordHost(), nil, &portstest.MemoryBlobs{})
	var names []string
	for i := range stage.MaxAttachments + 2 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	r.srv.deliver("INBOX", withAttachments("<h2@example.org>", names...), time.Now().Add(-10*time.Minute))
	if rep, err := importHistory(r, time.Now().Add(-time.Hour)); err != nil || rep.Imported != 1 {
		t.Fatalf("import %+v, %v", rep, err)
	}
	msgs := r.messages()
	if len(msgs) != 1 || len(attachmentRefs(msgs[0].Content)) != stage.MaxAttachments {
		t.Fatalf("messages %d, want one with %d staged attachments", len(msgs), stage.MaxAttachments)
	}
}

// Without a blob store the import describes attachments only, as the receiver
// does.
func TestEngineImportWithoutBlobStoreDescribesAttachments(t *testing.T) {
	r := newEngineRig(t, passwordHost(), nil)
	r.srv.deliver("INBOX", withAttachments("<h3@example.org>", "invoice.pdf"), time.Now().Add(-10*time.Minute))
	if rep, err := importHistory(r, time.Now().Add(-time.Hour)); err != nil || rep.Imported != 1 {
		t.Fatalf("import %+v, %v", rep, err)
	}
	msgs := r.messages()
	if len(msgs) != 1 || len(attachmentRefs(msgs[0].Content)) != 0 {
		t.Fatalf("messages %+v, want one described only", msgs)
	}
	if ds := descriptors(t, msgs[0].Content); len(ds) != 1 || ds[0].Staged {
		t.Errorf("descriptors = %+v", ds)
	}
}

// Import then live delivery: the stored message holds its bytes once, and the
// live delivery, a duplicate, stages no second copy.
func TestEngineImportThenReceiveHoldsBytesOnce(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := newEngineRigBlobs(t, passwordHost(), nil, blobs)
	r.srv.deliver("INBOX", withAttachments("<h4@example.org>", "invoice.pdf"), time.Now().Add(-10*time.Minute))
	if rep, err := importHistory(r, time.Now().Add(-time.Hour)); err != nil || rep.Imported != 1 {
		t.Fatalf("import %+v, %v", rep, err)
	}
	before := len(blobs.Keys())
	if before != 1 {
		t.Fatalf("blobs after import = %d, want 1", before)
	}
	r.receive()
	r.waiter.drive(t, func() bool { return r.srv.commands("IDLE") >= 1 || len(r.waiter.requested()) > 0 })
	r.waiter.idle(t)
	msgs := r.messages()
	if len(msgs) != 1 || len(attachmentRefs(msgs[0].Content)) != 1 {
		t.Fatalf("messages %+v, want one with its attachment", msgs)
	}
	if n := len(blobs.Keys()); n != before {
		t.Errorf("blobs after the live delivery = %d, want %d: no second copy", n, before)
	}
	if got := openAll(t, r, msgs[0].Content); got["invoice.pdf"] != "bytes of invoice.pdf" {
		t.Errorf("attachments = %v", got)
	}
}

// Receive then import: the import stages under the receiver's keys, so the
// bytes are held once.
func TestEngineReceiveThenImportHoldsBytesOnce(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := newEngineRigBlobs(t, passwordHost(), nil, blobs)
	r.srv.deliver("INBOX", withAttachments("<h5@example.org>", "invoice.pdf"), time.Now().Add(-10*time.Minute))
	r.receive()
	r.awaitMessages(1)
	before := len(blobs.Keys())
	if before != 1 {
		t.Fatalf("blobs after receive = %d, want 1", before)
	}
	if rep, err := importHistory(r, time.Now().Add(-time.Hour)); err != nil || rep.Imported != 0 {
		t.Fatalf("import %+v, %v", rep, err)
	}
	if n := len(blobs.Keys()); n != before {
		t.Errorf("blobs after the import = %d, want %d: the same keys as the receiver", n, before)
	}
}
