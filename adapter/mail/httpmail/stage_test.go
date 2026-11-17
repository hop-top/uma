package httpmail_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/adapter/mail/stage"
	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// postmarkConfig reads attachments the way Postmark documents them: Name,
// Content (base64), ContentType and ContentLength.
func postmarkConfig() httpmail.Config {
	cfg := baseConfig()
	cfg.Formats = []string{httpmail.FormatJSON, httpmail.FormatRFC822}
	cfg.JSONFields.Attachments = "Attachments"
	cfg.JSONFields.AttachmentFields = httpmail.AttachmentFields{Name: "Name", MimeType: "ContentType", SizeBytes: "ContentLength", Content: "Content"}
	return cfg
}

// jsonWith is a message to support@ whose attachments member is the given
// JSON array.
func jsonWith(id, attachments string) string {
	return fmt.Sprintf(`{"from":"jane@peers.test","to":"support@example.com","subject":"files","body":"see attached","message_id":%q,"Attachments":%s}`, id, attachments)
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

type descriptor struct {
	Filename string `json:"filename"`
	Staged   bool   `json:"staged"`
	Size     int64  `json:"size"`
	Digest   string `json:"digest"`
	URL      string `json:"url"`
}

// descriptors reads the PartsSchema part of m: the attachments described
// and which of them have staged bytes.
func descriptors(t *testing.T, m model.Message) []descriptor {
	t.Helper()
	for _, p := range m.Content.Parts {
		if p.Kind == model.PartStructured && p.Structured.Name == httpmail.PartsSchema {
			var rec struct {
				Attachments []descriptor `json:"attachments"`
			}
			if err := json.Unmarshal(p.Structured.Data, &rec); err != nil {
				t.Fatal(err)
			}
			return rec.Attachments
		}
	}
	t.Fatalf("no %s part in %+v", httpmail.PartsSchema, m.Content.Parts)
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

func (r *engineRig) mustPost(contentType, body string) {
	r.t.Helper()
	if rec := r.post(contentType, body, true); rec.Code != http.StatusOK {
		r.t.Fatalf("post: %d %s", rec.Code, rec.Body.String())
	}
}

// Bytes a JSON entry embeds are staged through the engine's sink; the entry
// that carries only a URL is described and its URL never fetched.
func TestEngineStagesEmbeddedJSONAttachments(t *testing.T) {
	var fetched atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fetched.Add(1) }))
	defer files.Close()
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{}, postmarkConfig(), "support")
	body := jsonWith("<j1@peers.test>", fmt.Sprintf(`[
	  {"Name":"invoice.pdf","Content":%q,"ContentType":"application/pdf","ContentLength":999},
	  {"Name":"notes.txt","Content":%q,"ContentType":"text/plain"},
	  {"Name":"remote.bin","ContentType":"application/octet-stream","ContentLength":12,"url":%q}]`,
		b64("bytes of invoice"), b64("bytes of notes"), files.URL+"/remote.bin"))
	for range 2 { // and a redelivery
		r.mustPost("application/json", body)
	}
	msgs := r.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (redelivery deduplicated)", len(msgs))
	}
	refs := attachmentRefs(msgs[0])
	if len(refs) != 2 {
		t.Fatalf("parts %+v, want both embedded attachments staged", msgs[0].Content.Parts)
	}
	for i, want := range []string{"bytes of invoice", "bytes of notes"} {
		att, got := r.readAttachment(refs[i])
		if got != want || att.Metadata.Audience.Visibility != model.Public {
			t.Errorf("attachment %d = %+v %q", i, att, got)
		}
	}
	sum := sha256.Sum256([]byte("bytes of invoice"))
	ds := descriptors(t, msgs[0])
	if len(ds) != 3 || !ds[0].Staged || !ds[1].Staged || ds[2].Staged {
		t.Fatalf("descriptors %+v, want staged true true false", ds)
	}
	// The size and digest of staged bytes are the bytes', not the claim.
	if ds[0].Size != int64(len("bytes of invoice")) || ds[0].Digest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("descriptor %+v keeps the claim", ds[0])
	}
	if ds[2].URL == "" || ds[2].Size != 12 {
		t.Errorf("URL-only descriptor %+v", ds[2])
	}
	if n := fetched.Load(); n != 0 {
		t.Errorf("the adapter fetched a sender-supplied URL %d times", n)
	}
	for _, p := range msgs[0].Content.Parts {
		if p.Structured != nil && strings.Contains(string(p.Structured.Data), b64("bytes of invoice")) {
			t.Error("embedded bytes kept in the description")
		}
	}
}

func rawWithAttachments(id string, names ...string) string {
	var b strings.Builder
	b.WriteString("From: jane@peers.test\nTo: support@example.com\nSubject: files\nMessage-ID: " + id + "\n")
	b.WriteString("MIME-Version: 1.0\nContent-Type: multipart/mixed; boundary=b\n\n--b\nContent-Type: text/plain\n\nplease see attached\n")
	for _, n := range names {
		fmt.Fprintf(&b, "--b\nContent-Type: application/octet-stream; name=%q\nContent-Disposition: attachment; filename=%q\nContent-Transfer-Encoding: base64\n\n%s\n", n, n, b64("bytes of "+n))
	}
	b.WriteString("--b--\n")
	return strings.ReplaceAll(b.String(), "\n", "\r\n")
}

func TestEngineStagesRFC822Attachments(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{}, postmarkConfig(), "support")
	raw := rawWithAttachments("<r1@peers.test>", "invoice.pdf", "notes.txt")
	for range 2 {
		r.mustPost("message/rfc822", raw)
	}
	msgs := r.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1 (redelivery deduplicated)", len(msgs))
	}
	refs := attachmentRefs(msgs[0])
	if len(refs) != 2 {
		t.Fatalf("parts %+v, want both attachments staged", msgs[0].Content.Parts)
	}
	for i, name := range []string{"invoice.pdf", "notes.txt"} {
		att, got := r.readAttachment(refs[i])
		if got != "bytes of "+name || att.Metadata.Name != name {
			t.Errorf("attachment %d = %+v %q", i, att, got)
		}
	}
	for _, d := range descriptors(t, msgs[0]) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Filename)
		}
	}
}

// One message to two destinations stages its attachments once in each, and
// a redelivery restages none again.
func TestEngineStagesPerDestination(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{}, postmarkConfig(), "support", "sales")
	raw := strings.Replace(rawWithAttachments("<r2@peers.test>", "a.txt"), "To: support@example.com", "To: support@example.com, sales@example.com", 1)
	for range 2 {
		r.mustPost("message/rfc822", raw)
	}
	msgs := r.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	seen := map[model.ID]bool{}
	for _, m := range msgs {
		refs := attachmentRefs(m)
		if len(refs) != 1 {
			t.Fatalf("message %v: parts %+v", m.Ref, m.Content.Parts)
		}
		if _, got := r.readAttachment(refs[0]); got != "bytes of a.txt" {
			t.Errorf("bytes %q", got)
		}
		seen[refs[0].ID] = true
	}
	if len(seen) != 2 {
		t.Errorf("destinations share an attachment: %v", seen)
	}
}

// Beyond the bounds, attachments are described only, and the message is
// still delivered.
func TestEngineAttachmentsBeyondTheBoundAreDescribedOnly(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{}, postmarkConfig(), "support")
	var names []string
	for i := range stage.MaxAttachments + 2 {
		names = append(names, fmt.Sprintf("f%02d.txt", i))
	}
	r.mustPost("message/rfc822", rawWithAttachments("<r3@peers.test>", names...))
	msgs := r.messages()
	if len(msgs) != 1 || len(attachmentRefs(msgs[0])) != stage.MaxAttachments {
		t.Fatalf("messages %d, staged %d, want 1 and %d", len(msgs), len(attachmentRefs(msgs[0])), stage.MaxAttachments)
	}
	for i, d := range descriptors(t, msgs[0]) {
		if d.Staged != (i < stage.MaxAttachments) {
			t.Errorf("descriptor %d (%s) staged = %v", i, d.Filename, d.Staged)
		}
	}
}

func TestEngineEmbeddedJSONBeyondTheBoundAreDescribedOnly(t *testing.T) {
	r := newEngineRigBlobs(t, &portstest.MemoryBlobs{}, postmarkConfig(), "support")
	var entries []string
	for i := range stage.MaxAttachments + 2 {
		entries = append(entries, fmt.Sprintf(`{"Name":"f%02d.txt","Content":%q,"ContentType":"text/plain"}`, i, b64("x")))
	}
	entries = append(entries, `{"Name":"bad.txt","Content":"not base64!","ContentType":"text/plain"}`)
	r.mustPost("application/json", jsonWith("<j2@peers.test>", "["+strings.Join(entries, ",")+"]"))
	msgs := r.messages()
	if len(msgs) != 1 || len(attachmentRefs(msgs[0])) != stage.MaxAttachments {
		t.Fatalf("messages %d, staged %d, want 1 and %d", len(msgs), len(attachmentRefs(msgs[0])), stage.MaxAttachments)
	}
	for i, d := range descriptors(t, msgs[0]) {
		if d.Staged != (i < stage.MaxAttachments) {
			t.Errorf("descriptor %d (%s) staged = %v", i, d.Filename, d.Staged)
		}
	}
}

// Without a blob store the engine's sink cannot stage: the message is
// delivered as described.
func TestEngineWithoutBlobStoreDescribesOnly(t *testing.T) {
	r := newEngineRig(t, postmarkConfig(), "support")
	r.mustPost("message/rfc822", rawWithAttachments("<r4@peers.test>", "a.txt"))
	r.mustPost("application/json", jsonWith("<j3@peers.test>", fmt.Sprintf(`[{"Name":"b.txt","Content":%q}]`, b64("b"))))
	msgs := r.messages()
	if len(msgs) != 2 {
		t.Fatalf("messages = %d, want 2", len(msgs))
	}
	for _, m := range msgs {
		if n := len(attachmentRefs(m)); n != 0 {
			t.Errorf("%d attachments staged without a blob store", n)
		}
		for _, d := range descriptors(t, m) {
			if d.Staged {
				t.Errorf("%s described as staged", d.Filename)
			}
		}
	}
}

// stager is a sink that stages through fail and records every request.
type stager struct {
	*captureSink
	mu   sync.Mutex
	reqs []adapter.StageRequest
	fail error
}

func (s *stager) StageAttachment(_ context.Context, r adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = io.Copy(io.Discard, body)
	s.reqs = append(s.reqs, r)
	if s.fail != nil {
		return model.AttachmentRef{}, s.fail
	}
	return model.AttachmentRef{Inbox: inbox, ID: model.ID("00000a00-0000-7000-8000-" + r.Key[5:17])}, nil
}

func (s *stager) requests() []adapter.StageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]adapter.StageRequest(nil), s.reqs...)
}

func newStagerRig(t *testing.T, cfg httpmail.Config, opts ...func(*httpmail.Options)) (*rig, *stager) {
	t.Helper()
	r := newRig(t, cfg, opts...)
	s := &stager{captureSink: r.sink}
	r.h = r.inst.(adapter.HTTPReceiver).Handler(s)
	return r, s
}

// A sink that cannot stage at all is not asked, and the message is
// delivered as described.
func TestPlainSinkDescribesOnly(t *testing.T) {
	r := newRig(t, postmarkConfig())
	wantStatus(t, r.postJSON(jsonWith("<j4@peers.test>", fmt.Sprintf(`[{"Name":"a.txt","Content":%q}]`, b64("a")))), http.StatusOK)
	got := r.sink.ingresses()
	if len(got) != 1 {
		t.Fatalf("ingresses = %d", len(got))
	}
	for _, p := range got[0].Envelope.Content.Parts {
		if p.Kind == model.PartAttachment {
			t.Error("attachment part from a sink that cannot stage")
		}
	}
}

// The staging key is bound to the message's bytes and the destination:
// stable across a redelivery, distinct per destination and position.
func TestStagingKeysAreStablePerMessageDestinationAndPosition(t *testing.T) {
	r, s := newStagerRig(t, postmarkConfig())
	two := fmt.Sprintf(`[{"Name":"a.txt","Content":%q},{"Name":"b.txt","Content":%q}]`, b64("a"), b64("b"))
	body := strings.Replace(jsonWith("<j5@peers.test>", two), `"to":"support@example.com"`, `"to":"support@example.com, sales@example.com"`, 1)
	wantStatus(t, r.postJSON(body), http.StatusOK)
	first := s.requests()
	wantStatus(t, r.postJSON(body), http.StatusOK)
	all := s.requests()
	if len(first) != 4 || len(all) != 8 {
		t.Fatalf("requests %d then %d, want 4 then 8", len(first), len(all))
	}
	keys := map[string]bool{}
	for n, q := range first {
		if q.Key != all[n+4].Key || q.Destination != all[n+4].Destination {
			t.Errorf("request %d changed across a redelivery: %+v vs %+v", n, q, all[n+4])
		}
		if len(q.Key) == 0 || len(q.Key) > 255 {
			t.Errorf("key %q", q.Key)
		}
		keys[q.Key] = true
	}
	if len(keys) != 4 {
		t.Errorf("keys collide across destinations or positions: %v", keys)
	}
	other := strings.Replace(body, "<j5@peers.test>", "<j6@peers.test>", 1)
	wantStatus(t, r.postJSON(other), http.StatusOK)
	if k := s.requests()[8].Key; keys[k] {
		t.Error("another message shares a staging key")
	}
}

// A staging failure a retry can fix is the sender's retry (503) and the
// delivery is not remembered as seen; the retry then lands once.
func TestRetryableStagingFailureAsksTheSenderToRetryAndReleasesTheReplaySlot(t *testing.T) {
	cfg := postmarkConfig()
	cfg.Auth = httpmail.Auth{Scheme: "hmac", Header: "X-Relay-Signature", Replay: &httpmail.AuthReplay{Source: "signature"}}
	store := webhookauth.NewMemoryReplayStore(16, fixedClock{now})
	r, s := newStagerRig(t, cfg, func(o *httpmail.Options) { o.ReplayStore = store })
	body := jsonWith("<j7@peers.test>", fmt.Sprintf(`[{"Name":"a.txt","Content":%q}]`, b64("a")))
	m := hmac.New(sha256.New, []byte(token))
	m.Write([]byte(body))
	rq := request{contentType: "application/json", body: body, noAuth: true, header: http.Header{"X-Relay-Signature": {hex.EncodeToString(m.Sum(nil))}}}

	s.fail = model.NewError(model.Transient, "blob store busy")
	wantStatus(t, r.do(rq), http.StatusServiceUnavailable)
	if n := len(r.sink.ingresses()); n != 0 {
		t.Errorf("%d ingresses accepted while staging failed", n)
	}
	// The sender retries: the first attempt was not processed, so this is
	// no replay.
	s.fail = nil
	wantStatus(t, r.do(rq), http.StatusOK)
	got := r.sink.ingresses()
	if len(got) != 1 {
		t.Fatalf("ingresses = %d, want 1", len(got))
	}
	var staged int
	for _, p := range got[0].Envelope.Content.Parts {
		if p.Kind == model.PartAttachment {
			staged++
		}
	}
	if staged != 1 {
		t.Errorf("staged parts = %d, want 1", staged)
	}
	// Now it was processed.
	wantStatus(t, r.do(rq), http.StatusConflict)
}

// A refusal staging cannot get past costs the attachment only.
func TestRefusedOrDisabledStagingDeliversTheMessageDescribed(t *testing.T) {
	for _, code := range []model.ErrorCode{model.Validation, model.Conflict, model.Unauthorized, model.Permanent, model.Disabled} {
		t.Run(string(code), func(t *testing.T) {
			r, s := newStagerRig(t, postmarkConfig())
			s.fail = model.NewError(code, "refused")
			wantStatus(t, r.postJSON(jsonWith("<j8@peers.test>", fmt.Sprintf(`[{"Name":"a.txt","Content":%q}]`, b64("a")))), http.StatusOK)
			got := r.sink.ingresses()
			if len(got) != 1 {
				t.Fatalf("ingresses = %d, want 1", len(got))
			}
			for _, p := range got[0].Envelope.Content.Parts {
				if p.Kind == model.PartAttachment {
					t.Error("attachment part for a refused staging")
				}
			}
		})
	}
}

// An entry that carries only a URL is never fetched and never staged, even
// by a sink that can stage.
func TestURLOnlyAttachmentIsNeverFetched(t *testing.T) {
	var fetched atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { fetched.Add(1) }))
	defer files.Close()
	r, s := newStagerRig(t, postmarkConfig())
	wantStatus(t, r.postJSON(jsonWith("<j9@peers.test>", fmt.Sprintf(`[{"Name":"a.bin","url":%q,"type":"file"}]`, files.URL+"/a.bin"))), http.StatusOK)
	if n := fetched.Load(); n != 0 {
		t.Errorf("fetched %d times", n)
	}
	if n := len(s.requests()); n != 0 {
		t.Errorf("%d staging requests for a URL-only entry", n)
	}
	if len(r.sink.ingresses()) != 1 {
		t.Error("message not delivered")
	}
}

// An attachment over the size bound is described only; the others of the
// message are still staged.
func TestEmbeddedAttachmentOverTheSizeBoundIsDescribedOnly(t *testing.T) {
	cfg := postmarkConfig()
	cfg.MaxBodyBytes = 64 << 20
	r, s := newStagerRig(t, cfg)
	big := base64.StdEncoding.EncodeToString(make([]byte, stage.MaxAttachmentBytes+1))
	wantStatus(t, r.postJSON(jsonWith("<j10@peers.test>", fmt.Sprintf(`[{"Name":"big.bin","Content":%q,"ContentLength":1},{"Name":"small.txt","Content":%q}]`, big, b64("s")))), http.StatusOK)
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Metadata.Name != "small.txt" {
		t.Fatalf("staging requests %+v, want the small attachment only", reqs)
	}
}

// A message/rfc822 attachment over the size bound is described only too.
func TestRFC822AttachmentOverTheSizeBoundIsDescribedOnly(t *testing.T) {
	cfg := postmarkConfig()
	cfg.MaxBodyBytes = 64 << 20
	r, s := newStagerRig(t, cfg)
	var b strings.Builder
	b.WriteString("From: jane@peers.test\r\nTo: support@example.com\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n")
	for _, n := range []struct {
		name string
		size int
	}{{"big.bin", stage.MaxAttachmentBytes + 1}, {"small.bin", 10}} {
		fmt.Fprintf(&b, "--b\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=%q\r\nContent-Transfer-Encoding: base64\r\n\r\n%s\r\n", n.name, wrap76(base64.StdEncoding.EncodeToString(make([]byte, n.size))))
	}
	b.WriteString("--b--\r\n")
	wantStatus(t, r.do(request{contentType: "message/rfc822", body: b.String()}), http.StatusOK)
	reqs := s.requests()
	if len(reqs) != 1 || reqs[0].Metadata.Name != "small.bin" {
		t.Fatalf("staging requests %+v, want the small attachment only", reqs)
	}
}

// wrap76 breaks base64 into the lines MIME allows.
func wrap76(s string) string {
	var b strings.Builder
	for len(s) > 76 {
		b.WriteString(s[:76] + "\r\n")
		s = s[76:]
	}
	return b.String() + s
}
