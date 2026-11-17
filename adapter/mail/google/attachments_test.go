package google

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

// withAttachments is a multipart message with one base64 attachment per
// name, the bytes of each being "bytes of <name>".
func withAttachments(msgID string, names ...string) []byte {
	var b strings.Builder
	b.WriteString("From: jane@example.org\r\nTo: support@example.test\r\nSubject: files\r\nDate: Sun, 11 Oct 2026 12:00:00 +0000\r\nMessage-ID: " + msgID + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nplease see attached\r\n")
	for _, n := range names {
		fmt.Fprintf(&b, "--b\r\nContent-Type: application/octet-stream; name=%q\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n",
			n, n, base64.StdEncoding.EncodeToString([]byte("bytes of "+n)))
	}
	b.WriteString("--b--\r\n")
	return []byte(b.String())
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

// descriptors reads the PartAttachment descriptors of c, in order.
func descriptors(t *testing.T, c model.Content) []attachmentPart {
	t.Helper()
	var out []attachmentPart
	for _, p := range c.Parts {
		if p.Kind == model.PartStructured && p.Structured.Name == PartAttachment {
			var a attachmentPart
			if err := json.Unmarshal(p.Structured.Data, &a); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
	}
	return out
}

// The attachments of a fetched message are staged through the engine's
// sink: the message names each staged attachment and its descriptor says so.
func TestEngineStagesAttachments(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{})
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)
	r.g.deliver(testMailbox, "m1", "thread-1", withAttachments("<a1@example.org>", "invoice.pdf", "notes.txt"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	msgs := r.waitMessages(engineInboxA, 1)
	refs := attachmentRefs(msgs[0].Content)
	if len(refs) != 2 {
		t.Fatalf("parts %+v, want both attachments staged", msgs[0].Content.Parts)
	}
	client, err := r.engine.For(model.Principal{Tenant: testChan.Tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"invoice.pdf", "notes.txt"} {
		att, rc, err := client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: refs[i]})
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
	if n := r.g.count("GET /gmail/v1/users/" + testMailbox + "/messages/m1/attachments"); n != 0 {
		t.Errorf("%d attachments.get calls: format=raw already holds the bytes", n)
	}
}

// Without a blob store the message is delivered as before, described only.
func TestEngineWithoutBlobStoreDescribesAttachments(t *testing.T) {
	r := newEngineRig(t)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.receive(testChan, testMailbox)
	r.g.deliver(testMailbox, "m1", "thread-1", withAttachments("<a2@example.org>", "invoice.pdf"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	msgs := r.waitMessages(engineInboxA, 1)
	if len(attachmentRefs(msgs[0].Content)) != 0 {
		t.Errorf("staged without a blob store: %+v", msgs[0].Content.Parts)
	}
	if ds := descriptors(t, msgs[0].Content); len(ds) != 1 || ds[0].Staged || ds[0].Filename != "invoice.pdf" {
		t.Errorf("descriptors = %+v", ds)
	}
}

// stagingSink is a sink that can also stage; staging answers stageErr.
type stagingSink struct {
	testSink
	stageErr error
}

func (s *stagingSink) StageAttachment(_ context.Context, _ adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return model.AttachmentRef{}, err
	}
	if s.stageErr != nil {
		return model.AttachmentRef{}, s.stageErr
	}
	return model.AttachmentRef{Inbox: engineInboxA, ID: "00000c00-0000-7000-8000-0000000000c1"}, nil
}

// A staging failure a retry can fix fails the fetch, so the message is
// read again; an attachment the engine refuses is described only.
func TestStagingFailures(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.deliver(e.box.user, "m1", "thread-1", withAttachments("<a3@example.org>", "invoice.pdf"))
	s := &stagingSink{stageErr: model.NewError(model.Unavailable, "blob store down")}
	if _, _, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), s, "m1"); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("fetchIngress = %v, want the staging error", err)
	}
	s.stageErr = model.NewError(model.Validation, "media type refused")
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), s, "m1")
	if err != nil || skip {
		t.Fatalf("fetchIngress = %v %v, want the message delivered", skip, err)
	}
	if len(attachmentRefs(in.Envelope.Content)) != 0 {
		t.Errorf("parts %+v, want none staged", in.Envelope.Content.Parts)
	}
	if ds := descriptors(t, in.Envelope.Content); len(ds) != 1 || ds[0].Staged {
		t.Errorf("descriptors = %+v", ds)
	}
}

// Staged attachments and descriptors share the content part limit: the
// descriptors past what fits are counted in the payload, and the content
// stays valid.
func TestManyAttachmentsStayWithinTheContentLimit(t *testing.T) {
	e := newEnv(t, Config{})
	var names []string
	for i := range 70 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	e.g.deliver(e.box.user, "m1", "thread-1", withAttachments("<a4@example.org>", names...))
	s := &stagingSink{}
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), s, "m1")
	if err != nil || skip {
		t.Fatalf("fetchIngress = %v %v", skip, err)
	}
	if err := in.Envelope.Content.Validate(); err != nil {
		t.Fatalf("content: %v", err)
	}
	if n := len(attachmentRefs(in.Envelope.Content)); n != stage.MaxAttachments {
		t.Errorf("staged %d, want %d", n, stage.MaxAttachments)
	}
	var p messagePayload
	if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if got := len(descriptors(t, in.Envelope.Content)) + p.AttachmentsOmitted; got != len(names) {
		t.Errorf("described %d + omitted %d, want %d", len(descriptors(t, in.Envelope.Content)), p.AttachmentsOmitted, len(names))
	}
}

// keyStager records every staging request and answers with a reference
// derived from the key, so two stagings of one key agree.
type keyStager struct {
	reqs []adapter.StageRequest
	err  error
}

func (s *keyStager) StageAttachment(_ context.Context, r adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	if _, err := io.Copy(io.Discard, body); err != nil {
		return model.AttachmentRef{}, err
	}
	if s.err != nil {
		return model.AttachmentRef{}, s.err
	}
	s.reqs = append(s.reqs, r)
	return model.AttachmentRef{Inbox: engineInboxA, ID: model.ID(fmt.Sprintf("att-%d", len(s.reqs)))}, nil
}

// stagerSink lets a keyStager stand where the receiver's sink would.
type stagerSink struct {
	testSink
	*keyStager
}

// A history read stages the attachments as the receiver does: the same
// requests (key, destination, metadata) for the same message.
func TestHistoryStagesAsTheReceiverDoes(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.hold(e.box.user, "m1", "thread-1", withAttachments("<h1@example.org>", "invoice.pdf", "notes.txt"), testNow.Add(-time.Hour))
	hist := &keyStager{}
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10, Stager: hist})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	if got := attachmentRefs(p.Envelopes[0].Content); len(got) != 2 {
		t.Fatalf("parts %+v, want both attachments staged", p.Envelopes[0].Content.Parts)
	}
	for _, d := range descriptors(t, p.Envelopes[0].Content) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Filename)
		}
	}
	live := &keyStager{}
	in, _, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), &stagerSink{keyStager: live}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist.reqs) != 2 || !reflect.DeepEqual(hist.reqs, live.reqs) {
		t.Errorf("history staged %+v\nreceiver staged %+v\nwant the same requests", hist.reqs, live.reqs)
	}
	if !reflect.DeepEqual(p.Envelopes[0], in.Envelope) {
		t.Errorf("history envelope differs from the receiver's:\n%+v\n%+v", p.Envelopes[0], in.Envelope)
	}
}

// A staging failure a retry can fix fails the read, so it is read again; an
// attachment the engine refuses is described only.
func TestHistoryStagingFailures(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.hold(e.box.user, "m1", "thread-1", withAttachments("<h2@example.org>", "invoice.pdf"), testNow.Add(-time.Hour))
	s := &keyStager{err: model.NewError(model.Unavailable, "blob store down")}
	if _, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10, Stager: s}); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("History = %v, want the staging error", err)
	}
	s.err = model.NewError(model.Validation, "media type refused")
	p, err := e.inst.History(t.Context(), adapter.HistoryRequest{Since: historySince, Limit: 10, Stager: s})
	if err != nil || len(p.Envelopes) != 1 {
		t.Fatalf("History = %+v, %v", p, err)
	}
	if len(attachmentRefs(p.Envelopes[0].Content)) != 0 {
		t.Errorf("refused attachment staged: %+v", p.Envelopes[0].Content.Parts)
	}
	if ds := descriptors(t, p.Envelopes[0].Content); len(ds) != 1 || ds[0].Staged {
		t.Errorf("descriptors = %+v", ds)
	}
}

func importFrom(r *engineRig) (uma.ImportReport, error) {
	ch := testChan
	return r.engine.Runtime().Import(r.ctx, engineCall, uma.ImportRequest{Requester: "operator", Channel: &ch, Since: historySince})
}

// Import then live delivery: the stored message holds its bytes once, and
// the live delivery, a duplicate, stages no second copy.
func TestEngineImportStagesThenReceiveHoldsBytesOnce(t *testing.T) {
	blobs := &portstest.MemoryBlobs{}
	r := newEngineRigBlobs(t, blobs)
	r.seed(testChan, engineInboxA, testMailbox, "support@example.test")
	r.g.hold(testMailbox, "m1", "thread-1", withAttachments("<h3@example.org>", "invoice.pdf"), testNow.Add(-time.Hour))
	if rep, err := importFrom(r); err != nil || rep.Imported != 1 || !rep.Complete {
		t.Fatalf("import %+v, %v", rep, err)
	}
	msgs := r.messages(engineInboxA)
	if len(msgs) != 1 || len(attachmentRefs(msgs[0].Content)) != 1 {
		t.Fatalf("messages %+v, want one with its attachment", msgs)
	}
	client, err := r.engine.For(model.Principal{Tenant: testChan.Tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	_, rc, err := client.Attachments().Open(r.ctx, model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: attachmentRefs(msgs[0].Content)[0]})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if b, _ := io.ReadAll(rc); string(b) != "bytes of invoice.pdf" {
		t.Errorf("bytes = %q", b)
	}
	_ = rc.Close()
	r.receive(testChan, testMailbox)
	r.g.deliver(testMailbox, "m1", "thread-1", withAttachments("<h3@example.org>", "invoice.pdf"))
	if code := r.push(testChan, testMailbox); code != http.StatusNoContent {
		t.Fatalf("push = %d", code)
	}
	waitFor(t, func() bool { return r.g.count("GET /gmail/v1/users/"+testMailbox+"/messages/m1?format=raw") >= 1 })
	if msgs := r.messages(engineInboxA); len(msgs) != 1 {
		t.Errorf("messages = %d, want 1", len(msgs))
	}
	if n := len(blobs.Keys()); n != 1 {
		t.Errorf("blobs = %d, want 1: no second copy", n)
	}
}
