package microsoft

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const (
	fileType      = "#microsoft.graph.fileAttachment"
	itemType      = "#microsoft.graph.itemAttachment"
	referenceType = "#microsoft.graph.referenceAttachment"
)

// stagingSink is a sink that can also stage: it records each request and
// its bytes, and answers staging from stageErr.
type stagingSink struct {
	testSink
	smu      sync.Mutex
	reqs     []adapter.StageRequest
	bodies   []string
	stageErr func(n int) error
}

func (s *stagingSink) StageAttachment(_ context.Context, r adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	b, err := io.ReadAll(body)
	if err != nil {
		return model.AttachmentRef{}, err
	}
	s.smu.Lock()
	defer s.smu.Unlock()
	n := len(s.reqs)
	s.reqs, s.bodies = append(s.reqs, r), append(s.bodies, string(b))
	if s.stageErr != nil {
		if err := s.stageErr(n); err != nil {
			return model.AttachmentRef{}, err
		}
	}
	return model.AttachmentRef{Inbox: model.InboxRef{Tenant: testChan.Tenant, ID: "00000c00-0000-7000-8000-0000000000a1"}, ID: model.ID(fmt.Sprintf("00000c00-0000-7000-8000-%012d", n+1))}, nil
}

// deliverFiles delivers a message with a file attachment per name, the
// bytes of each being "bytes of <name>".
func deliverFiles(e *env, id string, names ...string) { deliverFilesAt(e, id, time.Time{}, names...) }

// deliverFilesAt is deliverFiles for a message received at at.
func deliverFilesAt(e *env, id string, at time.Time, names ...string) {
	var atts []gAttachment
	for _, n := range names {
		atts = append(atts, gAttachment{ODataType: fileType, ID: "att-" + n, Name: n, ContentType: "application/pdf", Size: int64(len("bytes of " + n))})
		e.g.files["att-"+n] = []byte("bytes of " + n)
	}
	e.g.deliver(e.box.name, gMessage{
		ID: id, InternetMessageID: "<" + id + "@peer.test>", Subject: "files", ReceivedDateTime: at,
		From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}},
		Body: &gItemBody{ContentType: "text", Content: "see attached"},
	}, atts...)
}

func stagedRefs(c model.Content) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range c.Parts {
		if p.Kind == model.PartAttachment && p.Attachment != nil {
			out = append(out, *p.Attachment)
		}
	}
	return out
}

func described(t *testing.T, c model.Content) []attachmentPart {
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

func fetchWith(t *testing.T, e *env, sink adapter.Sink, id string) (model.Ingress, error) {
	t.Helper()
	in, skip, err := e.inst.fetchIngress(t.Context(), e.inst.c.session(), sink, id)
	if err == nil && skip {
		t.Fatalf("fetchIngress(%s) skipped", id)
	}
	return in, err
}

func valueCalls(e *env) int {
	return e.g.count("GET /v1.0/users/" + testMailbox + "/messages/m1/attachments/att-")
}

// A file attachment's raw bytes ($value) are staged through the sink under
// its own key, with the name, media type and size of what was downloaded;
// the descriptor reports it staged.
func TestStagesFileAttachments(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFiles(e, "m1", "invoice.pdf", "notes.pdf")
	s := &stagingSink{}
	in, err := fetchWith(t, e, s, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(stagedRefs(in.Envelope.Content)); n != 2 || len(s.reqs) != 2 {
		t.Fatalf("staged parts %d, requests %d, want 2", n, len(s.reqs))
	}
	r := s.reqs[0]
	if s.bodies[0] != "bytes of invoice.pdf" || r.Destination != in.Envelope.Destination || r.Metadata.Name != "invoice.pdf" ||
		r.Metadata.MediaType != "application/pdf" || r.Metadata.Size != int64(len(s.bodies[0])) || r.Metadata.Audience.Visibility != model.Public {
		t.Errorf("request %+v body %q", r, s.bodies[0])
	}
	if s.reqs[0].Key == s.reqs[1].Key || len(s.reqs[0].Key) > 255 {
		t.Errorf("keys %q %q", s.reqs[0].Key, s.reqs[1].Key)
	}
	for _, d := range described(t, in.Envelope.Content) {
		if !d.Staged {
			t.Errorf("%s described as not staged", d.Name)
		}
	}
	// A redelivery restages under the same keys.
	s2 := &stagingSink{}
	if _, err := fetchWith(t, e, s2, "m1"); err != nil || s2.reqs[0].Key != s.reqs[0].Key || s2.reqs[1].Key != s.reqs[1].Key {
		t.Errorf("keys %q vs %q (%v): not stable across fetches", s2.reqs[0].Key, s.reqs[0].Key, err)
	}
}

// Only file attachments have raw bytes to stage; an item attachment (an
// Outlook item) and a reference attachment (a link) are described only and
// never requested.
func TestOtherAttachmentTypesAreDescribedOnly(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.files["att-file"] = []byte("file bytes")
	e.g.deliver(e.box.name, gMessage{
		ID: "m1", Subject: "mixed", From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}},
		Body: &gItemBody{ContentType: "text", Content: "mixed"},
	},
		gAttachment{ODataType: itemType, ID: "att-item", Name: "forwarded", Size: 100},
		gAttachment{ODataType: referenceType, ID: "att-ref", Name: "link.docx", Size: 10},
		gAttachment{ODataType: fileType, ID: "att-file", Name: "f.bin", ContentType: "application/octet-stream", Size: 10})
	s := &stagingSink{}
	in, err := fetchWith(t, e, s, "m1")
	if err != nil {
		t.Fatal(err)
	}
	ds := described(t, in.Envelope.Content)
	if len(ds) != 3 || ds[0].Staged || ds[1].Staged || !ds[2].Staged || len(s.reqs) != 1 {
		t.Fatalf("descriptors %+v, requests %d, want only the file staged", ds, len(s.reqs))
	}
	if n := valueCalls(e); n != 1 {
		t.Errorf("%d $value calls, want 1", n)
	}
}

func TestAttachmentsStageOnlyThroughAStagingSink(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFiles(e, "m1", "invoice.pdf")
	in, err := fetchWith(t, e, &testSink{}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if n := valueCalls(e); n != 0 || len(stagedRefs(in.Envelope.Content)) != 0 || described(t, in.Envelope.Content)[0].Staged {
		t.Errorf("%d $value calls, content %+v: a sink that cannot stage downloads nothing", n, in.Envelope.Content.Parts)
	}
}

// At most 10 attachments are downloaded and staged, and one over the size
// bound is not downloaded at all: both stay described.
func TestBoundsOnStagedAttachments(t *testing.T) {
	e := newEnv(t, Config{})
	var names []string
	for i := range 12 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	deliverFiles(e, "m1", names...)
	s := &stagingSink{}
	in, err := fetchWith(t, e, s, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if n := valueCalls(e); n != 10 || len(s.reqs) != 10 {
		t.Fatalf("%d downloads, %d staged, want 10", n, len(s.reqs))
	}
	for i, d := range described(t, in.Envelope.Content) {
		if d.Staged != (i < 10) {
			t.Errorf("%s staged = %v", d.Name, d.Staged)
		}
	}
	if err := in.Envelope.Content.Validate(); err != nil {
		t.Errorf("content: %v", err)
	}

	big := newEnv(t, Config{})
	big.g.deliver(big.box.name, gMessage{ID: "m1", From: &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}}, Body: &gItemBody{ContentType: "text", Content: "big"}},
		gAttachment{ODataType: fileType, ID: "att-big", Name: "big.iso", Size: 26 << 20},
		gAttachment{ODataType: fileType, ID: "att-small", Name: "small.bin", Size: 10})
	big.g.files["att-small"] = []byte("0123456789")
	s = &stagingSink{}
	in, err = fetchWith(t, big, s, "m1")
	if err != nil {
		t.Fatal(err)
	}
	ds := described(t, in.Envelope.Content)
	if len(ds) != 2 || ds[0].Staged || !ds[1].Staged || big.g.count("GET /v1.0/users/"+testMailbox+"/messages/m1/attachments/att-big") != 0 {
		t.Errorf("descriptors %+v: the oversize attachment is described, never downloaded", ds)
	}
}

// A body larger than the bound whatever the metadata said is dropped from
// staging, not read whole.
func TestDownloadOverBoundIsDescribedOnly(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFiles(e, "m1", "liar.bin")
	e.g.files["att-liar.bin"] = make([]byte, maxStagedBytes+1)
	s := &stagingSink{}
	in, err := fetchWith(t, e, s, "m1")
	if err != nil || len(s.reqs) != 0 || described(t, in.Envelope.Content)[0].Staged {
		t.Fatalf("err %v, requests %d, descriptors %+v", err, len(s.reqs), described(t, in.Envelope.Content))
	}
}

// A download that fails for good (the attachment is gone, or a type with no
// raw form) leaves that attachment described; the others still stage. One a
// repeat may fix fails the fetch.
func TestAttachmentDownloadFailures(t *testing.T) {
	for status, wantErr := range map[int]model.ErrorCode{
		http.StatusNotFound: "", http.StatusMethodNotAllowed: "", http.StatusForbidden: "",
		http.StatusServiceUnavailable: model.Transient, http.StatusInternalServerError: model.Transient, http.StatusTooManyRequests: model.RateLimited,
	} {
		e := newEnv(t, Config{})
		deliverFiles(e, "m1", "a.bin", "b.bin")
		e.g.valueStatus["att-a.bin"] = status
		s := &stagingSink{}
		in, err := fetchWith(t, e, s, "m1")
		if wantErr != "" {
			if model.CodeOf(err) != wantErr {
				t.Errorf("status %d: err = %v, want %s", status, err, wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("status %d: %v", status, err)
			continue
		}
		ds := described(t, in.Envelope.Content)
		if len(ds) != 2 || ds[0].Staged || !ds[1].Staged {
			t.Errorf("status %d: descriptors %+v, want only b.bin staged", status, ds)
		}
	}
}

func TestStagingRefusalsAndFailures(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFiles(e, "m1", "a.bin", "b.bin", "c.bin")
	// The engine refuses the first for good: it is described, the rest stage.
	s := &stagingSink{stageErr: func(n int) error {
		if n == 0 {
			return model.NewError(model.Validation, "media type refused")
		}
		return nil
	}}
	in, err := fetchWith(t, e, s, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if ds := described(t, in.Envelope.Content); ds[0].Staged || !ds[1].Staged || !ds[2].Staged {
		t.Errorf("descriptors %+v", ds)
	}
	// No blob store: the first answer holds for the rest.
	s = &stagingSink{stageErr: func(int) error { return model.NewError(model.Disabled, "no blob store") }}
	in, err = fetchWith(t, e, s, "m1")
	if err != nil || len(s.reqs) != 1 || len(stagedRefs(in.Envelope.Content)) != 0 {
		t.Errorf("err %v, requests %d: a missing blob store is tried once and the message delivered", err, len(s.reqs))
	}
	// A blob store that is down fails the fetch.
	s = &stagingSink{stageErr: func(int) error { return model.NewError(model.Unavailable, "blob store down") }}
	if _, err = fetchWith(t, e, s, "m1"); model.CodeOf(err) != model.Unavailable {
		t.Errorf("err = %v, want the staging error", err)
	}
}

// Over the real engine with a blob store: the staged attachment holds the
// downloaded bytes.
func TestEngineStagesAttachments(t *testing.T) {
	g := newFakeGraph(t)
	clock := portstest.NewFixedClock(time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC))
	st := memory.New(memory.Options{Clock: clock})
	f := NewFactory(Options{Endpoint: g.endpoint(), PageSize: 10})
	e, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{f},
		Extensions:  f.Extensions(),
		Credentials: &testCreds{secret: []byte("notification-secret")},
		HTTP:        g.srv.Client(),
		Blobs:       &portstest.MemoryBlobs{},
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x9b),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	g.mailbox(testMailbox, testChan)
	data, _ := json.Marshal(Config{Mailbox: testMailbox, Addresses: []string{"support@example.test"}})
	inbox := model.InboxRef{Tenant: testChan.Tenant, ID: "00000c00-0000-7000-8000-0000000000a1"}
	lock := false
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := st.Transact(t.Context(), testChan.Tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(t.Context(), model.Channel{Ref: testChan, Type: Type, CredentialRef: "cred", Revision: "ch1",
			Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data}}, ""); err != nil {
			return err
		}
		return tx.PutInbox(t.Context(), model.Inbox{Ref: inbox, Channel: testChan, Name: "graph", Destinations: []string{"support@example.test"}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	g.files["att-invoice.pdf"] = []byte("bytes of invoice.pdf")
	g.deliver(testMailbox, gMessage{
		ID: "m1", InternetMessageID: "<m1@peer.test>", Subject: "files",
		From:         &gRecipient{EmailAddress: gEmailAddress{Address: "peer@example.test"}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: "support@example.test"}}},
		Body:         &gItemBody{ContentType: "text", Content: "see attached"},
	}, gAttachment{ODataType: fileType, ID: "att-invoice.pdf", Name: "invoice.pdf", ContentType: "application/pdf", Size: 20})

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.Runtime().Receive(ctx, uma.RuntimeCall{Principal: model.Principal{Tenant: testChan.Tenant, Actor: "graph-service"}, Call: model.Call{Correlation: "graph-test"}}, testChan)
	}()
	t.Cleanup(func() { cancel(); <-done })
	var msgs []model.Message
	waitFor(t, func() bool {
		_ = st.View(t.Context(), testChan.Tenant, func(rd store.Reader) error {
			p, err := rd.Messages(t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 10}})
			msgs = p.Items
			return err
		})
		return len(msgs) == 1
	})
	refs := stagedRefs(msgs[0].Content)
	if len(refs) != 1 {
		t.Fatalf("parts %+v, want the attachment staged", msgs[0].Content.Parts)
	}
	client, err := e.For(model.Principal{Tenant: testChan.Tenant, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	att, rc, err := client.Attachments().Open(t.Context(), model.Lookup[model.AttachmentRef]{Call: model.Call{Correlation: "open"}, Ref: refs[0]})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "bytes of invoice.pdf" || att.Metadata.Name != "invoice.pdf" || att.Metadata.MediaType != "application/pdf" {
		t.Errorf("attachment %+v %q", att, got)
	}
}

// An attachment above the 8 MiB bound of an ordinary Graph answer is read
// whole, up to its own bound.
func TestLargeAttachmentIsStaged(t *testing.T) {
	e := newEnv(t, Config{})
	deliverFiles(e, "m1", "big.bin")
	e.g.files["att-big.bin"] = make([]byte, 9<<20)
	s := &stagingSink{}
	if _, err := fetchWith(t, e, s, "m1"); err != nil || len(s.bodies) != 1 || len(s.bodies[0]) != 9<<20 {
		t.Fatalf("err %v, staged %d", err, len(s.bodies))
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
	deliverFiles(e, "m1", names...)
	in, err := fetchWith(t, e, &stagingSink{}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Envelope.Content.Validate(); err != nil {
		t.Fatalf("content: %v", err)
	}
	if n := len(stagedRefs(in.Envelope.Content)); n != maxStagedAttachments {
		t.Errorf("staged %d, want %d", n, maxStagedAttachments)
	}
	var p messagePayload
	if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if got := len(described(t, in.Envelope.Content)); got+p.AttachmentsOmitted != len(names) {
		t.Errorf("described %d + omitted %d, want %d", got, p.AttachmentsOmitted, len(names))
	}
}

// Both ways a message arrives stage its attachments: the webhook, and the
// delta round that fills the gaps.
func TestWebhookAndDeltaStageAttachments(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: "https://hooks.example.test/graph"})
	deliverFiles(e, "m1", "invoice.pdf")
	hook := &stagingSink{}
	body, _ := json.Marshal(gNotificationCollection{Value: []gNotification{created(clientStateOf(t, e), "m1")}})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://hooks.example.test/graph", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.inst.Handler(hook).ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted || len(hook.got) != 1 || len(stagedRefs(hook.got[0].Envelope.Content)) != 1 {
		t.Fatalf("webhook answered %d, accepted %d", rec.Code, len(hook.got))
	}

	delta := &stagingSink{}
	if _, err := e.inst.round(t.Context(), delta); err != nil {
		t.Fatal(err)
	}
	if len(delta.got) != 1 || len(stagedRefs(delta.got[0].Envelope.Content)) != 1 {
		t.Fatalf("delta round accepted %d messages: %+v", len(delta.got), delta.got)
	}

	// A staging failure answers the notification 503, so Graph redelivers.
	e.g.valueStatus["att-invoice.pdf"] = http.StatusServiceUnavailable
	rec = httptest.NewRecorder()
	req = httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://hooks.example.test/graph", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	e.inst.Handler(&stagingSink{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("webhook answered %d, want 503", rec.Code)
	}
}

func listCalls(e *env) int {
	return e.g.count("GET /v1.0/users/" + testMailbox + "/messages/m1/attachments?")
}

func payloadOf(t *testing.T, in model.Ingress) messagePayload {
	t.Helper()
	var p messagePayload
	if err := json.Unmarshal(in.Envelope.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// A paged attachment list is followed through @odata.nextLink
// (https://learn.microsoft.com/en-us/graph/paging): the attachments of the
// later pages are described, and staged within the bound, as the first
// page's are.
func TestAttachmentListPagesAreFollowed(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.attPage = 5
	var names []string
	for i := range 12 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	deliverFiles(e, "m1", names...)
	sink := &stagingSink{}
	in, err := fetchWith(t, e, sink, "m1")
	if err != nil {
		t.Fatal(err)
	}
	got := described(t, in.Envelope.Content)
	if len(got) != len(names) {
		t.Fatalf("described %d attachments, want %d", len(got), len(names))
	}
	for n, a := range got {
		if a.Name != names[n] {
			t.Errorf("attachment %d is %q, want %q", n, a.Name, names[n])
		}
		if want := n < maxStagedAttachments; a.Staged != want {
			t.Errorf("attachment %d staged=%v, want %v", n, a.Staged, want)
		}
	}
	if p := payloadOf(t, in); p.AttachmentsOmitted != 0 {
		t.Errorf("attachments_omitted %d, want 0", p.AttachmentsOmitted)
	}
	if n := listCalls(e); n != 3 {
		t.Errorf("%d list calls, want 3 pages", n)
	}
}

// attachments_omitted counts the attachments of every page, not the first
// page's.
func TestAttachmentsOmittedCountsLaterPages(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.attPage = 10
	var names []string
	for i := range 70 {
		names = append(names, fmt.Sprintf("f%02d.bin", i))
	}
	deliverFiles(e, "m1", names...)
	in, err := fetchWith(t, e, &stagingSink{}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if got, p := len(described(t, in.Envelope.Content)), payloadOf(t, in); got != maxAttachmentParts || p.AttachmentsOmitted != len(names)-maxAttachmentParts {
		t.Errorf("described %d, omitted %d; want %d and %d", got, p.AttachmentsOmitted, maxAttachmentParts, len(names)-maxAttachmentParts)
	}
}

// A list that never ends is read for at most maxAttachmentPages pages, and
// what was read is delivered.
func TestAttachmentListPagingIsBounded(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.attPage, e.g.attEndless = 2, true
	deliverFiles(e, "m1", "a.bin", "b.bin", "c.bin")
	in, err := fetchWith(t, e, &stagingSink{}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if n := listCalls(e); n != maxAttachmentPages {
		t.Errorf("%d list calls, want %d", n, maxAttachmentPages)
	}
	if got := len(described(t, in.Envelope.Content)); got != 3 {
		t.Errorf("described %d attachments, want 3", got)
	}
}

// A next link to another origin is not followed: the bearer token stays
// with the endpoint, and the first page is delivered.
func TestAttachmentListLinkToAnotherOriginIsNotFollowed(t *testing.T) {
	var hits int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(other.Close)
	e := newEnv(t, Config{})
	e.g.attNextLink = other.URL + "/v1.0/next"
	deliverFiles(e, "m1", "a.bin")
	in, err := fetchWith(t, e, &stagingSink{}, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Errorf("%d requests reached another origin", hits)
	}
	if got := len(described(t, in.Envelope.Content)); got != 1 {
		t.Errorf("described %d attachments, want 1", got)
	}
}

// A failure on a later page fails the fetch for a repeat, as a failure on
// the first page does: a partial list is not delivered as complete.
func TestAttachmentListLaterPageFailureFailsTheFetch(t *testing.T) {
	e := newEnv(t, Config{})
	e.g.attPage = 1
	deliverFiles(e, "m1", "a.bin", "b.bin")
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/attachments") && r.URL.Query().Get("$skiptoken") != "" {
			graphError(w, http.StatusServiceUnavailable, "ServiceUnavailable")
			return true
		}
		return false
	}
	if _, err := fetchWith(t, e, &stagingSink{}, "m1"); err == nil {
		t.Fatal("fetch of a message whose second attachment page failed succeeded")
	}
}
