package github_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1" // #nosec G505 -- signs the legacy header the handler must refuse
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

const hookSecret = "webhook-fixture-secret" // #nosec G101 -- fixture value, not a credential

// recordingSink records accepted ingress; refuse makes Accept fail.
type recordingSink struct {
	mu     sync.Mutex
	got    []model.Ingress
	refuse func(model.Ingress) error
}

func (s *recordingSink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refuse != nil {
		if err := s.refuse(in); err != nil {
			return model.IngressReceipt{}, err
		}
	}
	s.got = append(s.got, in)
	return model.IngressReceipt{Committed: true}, nil
}

func (s *recordingSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "no interactions")
}

func (s *recordingSink) envelopes() []model.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Envelope
	for _, in := range s.got {
		out = append(out, in.Envelope)
	}
	return out
}

// bodyDigest is the event ID of a delivery of body: its SHA-256.
func bodyDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "webhooks", name+".json")) // #nosec G304 -- fixed fixture directory
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// hookRequest is a signed GitHub delivery of a golden payload.
func hookRequest(event, delivery string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "GitHub-Hookshot/7f5d2d1")
	r.Header.Set(github.HeaderEvent, event)
	r.Header.Set(github.HeaderDelivery, delivery)
	r.Header.Set(github.HeaderSignature, sign(hookSecret, body))
	return r
}

type hookRig struct {
	t    *testing.T
	inst adapter.Instance
	sink *recordingSink
	h    http.Handler
}

func newHookRig(t *testing.T, cfg github.Config) *hookRig {
	r := newSendRig(t)
	inst := r.open(cfg)
	sink := &recordingSink{}
	return &hookRig{t: t, inst: inst, sink: sink, h: inst.(adapter.HTTPReceiver).Handler(sink)}
}

func (h *hookRig) serve(r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func (h *hookRig) post(event, name, delivery string) []model.Envelope {
	h.t.Helper()
	before := len(h.sink.envelopes())
	if code, body := h.serve(hookRequest(event, delivery, golden(h.t, name))); code != http.StatusOK {
		h.t.Fatalf("%s delivery = %d %q, want 200", name, code, body)
	}
	return h.sink.envelopes()[before:]
}

func TestWebhookRefusesUnverifiedDeliveries(t *testing.T) {
	h := newHookRig(t, github.Config{})
	body := golden(t, "issue_comment-created")
	legacy := hmac.New(sha1.New, []byte(hookSecret))
	legacy.Write(body)
	for name, mutate := range map[string]func(*http.Request){
		"missing":      func(r *http.Request) { r.Header.Del(github.HeaderSignature) },
		"wrong secret": func(r *http.Request) { r.Header.Set(github.HeaderSignature, sign("other", body)) },
		"other body": func(r *http.Request) {
			r.Header.Set(github.HeaderSignature, sign(hookSecret, golden(t, "issues-opened")))
		},
		"not hex": func(r *http.Request) { r.Header.Set(github.HeaderSignature, "sha256=zz") },
		"bare digest": func(r *http.Request) {
			r.Header.Set(github.HeaderSignature, strings.TrimPrefix(sign(hookSecret, body), "sha256="))
		},
		"sha1 only": func(r *http.Request) {
			r.Header.Del(github.HeaderSignature)
			r.Header.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(legacy.Sum(nil)))
		},
		"sha1 in place": func(r *http.Request) {
			r.Header.Set(github.HeaderSignature, "sha1="+hex.EncodeToString(legacy.Sum(nil)))
		},
	} {
		r := hookRequest("issue_comment", "d-"+name, body)
		mutate(r)
		code, text := h.serve(r)
		if code != http.StatusUnauthorized || text != http.StatusText(http.StatusUnauthorized) {
			t.Errorf("%s signature = %d %q, want 401 with the status text only", name, code, text)
		}
	}
	if n := len(h.sink.envelopes()); n != 0 {
		t.Fatalf("sink received %d envelopes from unverified deliveries, want 0", n)
	}
}

func TestWebhookVerifiesBeforeReadingHeadersOrBody(t *testing.T) {
	h := newHookRig(t, github.Config{})
	garbage := []byte("{not json")
	r := hookRequest("issues", "", garbage)
	r.Header.Set(github.HeaderSignature, sign("other", garbage))
	if code, _ := h.serve(r); code != http.StatusUnauthorized {
		t.Fatalf("unsigned malformed delivery = %d, want 401 before parsing", code)
	}
	r = hookRequest("issues", "d1", garbage)
	if code, _ := h.serve(r); code != http.StatusBadRequest {
		t.Fatalf("signed malformed delivery = %d, want 400", code)
	}
	r = hookRequest("issues", "", golden(t, "issues-opened"))
	if code, _ := h.serve(r); code != http.StatusBadRequest {
		t.Fatalf("delivery without X-GitHub-Delivery = %d, want 400", code)
	}
	if code, _ := h.serve(httptest.NewRequest(http.MethodGet, "/", nil)); code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", code)
	}
}

func TestWebhookFormEncodedPayload(t *testing.T) {
	h := newHookRig(t, github.Config{})
	body := []byte("payload=" + url.QueryEscape(string(golden(t, "issue_comment-created"))))
	r := hookRequest("issue_comment", "d-form", body)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if code, _ := h.serve(r); code != http.StatusOK || len(h.sink.envelopes()) != 1 {
		t.Fatalf("form delivery = %d, envelopes %d; want 200 and 1", code, len(h.sink.envelopes()))
	}
}

func TestWebhookIgnoresPullRequestsAndOtherEvents(t *testing.T) {
	h := newHookRig(t, github.Config{})
	h.post("issue_comment", "issue_comment-created-pull-request", "d-pr")
	h.post("ping", "ping", "d-ping")
	h.post("pull_request", "issues-opened", "d-other-event")
	if n := len(h.sink.envelopes()); n != 0 {
		t.Fatalf("envelopes = %d, want none for pull requests, pings and other events", n)
	}
}

func wiRef(id, repo string) model.ExternalRef {
	return model.ExternalRef{Channel: chanA, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: id, ParentID: repo}
}

func userRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: chanA, Namespace: github.Namespace, Kind: github.RefKindUser, ID: id}
}

func TestWebhookIssueOpenedIsTheFirstMessage(t *testing.T) {
	h := newHookRig(t, github.Config{LocalFields: []string{model.WorkItemFieldStatus}})
	envs := h.post("issues", "issues-opened", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	if len(envs) != 1 {
		t.Fatalf("envelopes = %d, want 1", len(envs))
	}
	e := envs[0]
	if e.Kind != model.EnvelopeMessage || e.EventID != bodyDigest(golden(t, "issues-opened")) || e.MessageID != "issue:3000000007" ||
		e.Destination != sandboxRepo || e.Sender != userRef("5000001") || e.Audience.Visibility != model.Public {
		t.Fatalf("envelope = %+v", e)
	}
	if len(e.References) != 1 || e.References[0] != wiRef("7", sandboxRepo) {
		t.Fatalf("references = %+v, want the work item", e.References)
	}
	if e.Content.Parts[0].Text != "Exports over 2 GB stop at 99%." {
		t.Fatalf("content = %+v", e.Content)
	}
	w := e.WorkItem
	if w == nil || w.Subject != "Export fails on large files" || w.Status != model.WorkItemOpen || w.RawStatus != "open" ||
		w.URL != "https://github.com/example/uma-sandbox/issues/7" || w.Requester != userRef("5000001") ||
		w.Binding != model.WorkItemBindingActive || len(w.LocalFields) != 1 {
		t.Fatalf("work item = %+v", w)
	}
	if err := w.Validate(); err != nil {
		t.Fatal(err)
	}
	empty := h.post("issues", "issues-opened-empty", "d-empty")
	if len(empty) != 1 || empty[0].Kind != model.EnvelopeWorkItem || empty[0].WorkItem == nil || len(empty[0].Content.Parts) != 0 || !empty[0].Sender.IsZero() {
		t.Fatalf("empty description = %+v, want state only", empty)
	}
}

func TestWebhookIssueActionsReportState(t *testing.T) {
	h := newHookRig(t, github.Config{})
	labeled := h.post("issues", "issues-labeled", "d-labeled")
	if len(labeled) != 1 || labeled[0].Kind != model.EnvelopeWorkItem {
		t.Fatalf("labeled = %+v", labeled)
	}
	w := labeled[0].WorkItem
	if len(w.Assignees) != 1 || w.Assignees[0] != userRef("5000002") || len(w.Fields) != 2 ||
		w.Fields[0].Name != github.FieldLabels || string(w.Fields[0].Data) != `["bug","customer"]` ||
		w.Fields[1].Name != github.FieldMilestone || string(w.Fields[1].Data) != `{"number":3,"title":"October"}` {
		t.Fatalf("labeled state = %+v", w)
	}
	closed := h.post("issues", "issues-closed", "d-closed")[0].WorkItem
	if closed.Status != model.WorkItemClosed || closed.RawStatus != "closed:completed" {
		t.Fatalf("closed state = %+v", closed)
	}
	deleted := h.post("issues", "issues-deleted", "d-deleted")[0].WorkItem
	if deleted.Binding != model.WorkItemBindingDeleted {
		t.Fatalf("deleted binding = %q", deleted.Binding)
	}
	moved := h.post("issues", "issues-transferred", "d-moved")[0].WorkItem
	if moved.Binding != model.WorkItemBindingMoved || moved.Successor == nil || *moved.Successor != wiRef("42", "example/uma-archive") {
		t.Fatalf("transferred = %+v", moved)
	}
	for _, s := range []*model.WorkItemState{w, closed, deleted, moved} {
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWebhookIssueEditedReportsStateAndBodyEdit(t *testing.T) {
	h := newHookRig(t, github.Config{})
	envs := h.post("issues", "issues-edited", "d-edit")
	if len(envs) != 2 || envs[0].Kind != model.EnvelopeWorkItem || envs[1].Kind != model.EnvelopeMessageEdited {
		t.Fatalf("envelopes = %+v, want state then description edit", envs)
	}
	if envs[0].WorkItem.Subject != "Export fails on files over 2 GB" || envs[1].MessageID != "issue:3000000007" ||
		envs[1].Content.Parts[0].Text != "Exports over 2 GB stop at 99% on Windows." {
		t.Fatalf("envelopes = %+v", envs)
	}
}

func TestWebhookEditOfUnknownMessageRecordsIt(t *testing.T) {
	h := newHookRig(t, github.Config{})
	h.sink.refuse = func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited || in.Envelope.Kind == model.EnvelopeMessageDeleted {
			return model.NewError(model.NotFound, "target message not found")
		}
		return nil
	}
	envs := h.post("issue_comment", "issue_comment-edited", "d-edit-unknown")
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeMessage || envs[0].MessageID != "comment:2000000001" || envs[0].Content.Parts[0].Text != "Still happens on 1.4.3." {
		t.Fatalf("envelopes = %+v, want the edited comment recorded as its first observation", envs)
	}
	if envs := h.post("issue_comment", "issue_comment-deleted", "d-del-unknown"); len(envs) != 0 {
		t.Fatalf("deletion of an unknown comment = %+v, want dropped", envs)
	}
}

func TestWebhookComments(t *testing.T) {
	h := newHookRig(t, github.Config{})
	c := h.post("issue_comment", "issue_comment-created", "d-c1")
	if len(c) != 1 || c[0].Kind != model.EnvelopeMessage || c[0].MessageID != "comment:2000000001" || c[0].Sender != userRef("5000001") ||
		c[0].WorkItem == nil || c[0].References[0] != wiRef("7", sandboxRepo) {
		t.Fatalf("created = %+v", c)
	}
	e := h.post("issue_comment", "issue_comment-edited", "d-c2")
	if len(e) != 1 || e[0].Kind != model.EnvelopeMessageEdited || e[0].Content.Parts[0].Text != "Still happens on 1.4.3." || e[0].WorkItem != nil {
		t.Fatalf("edited = %+v", e)
	}
	d := h.post("issue_comment", "issue_comment-deleted", "d-c3")
	if len(d) != 1 || d[0].Kind != model.EnvelopeMessageDeleted || d[0].MessageID != "comment:2000000001" || !d[0].OccurredAt.IsZero() {
		t.Fatalf("deleted = %+v", d)
	}
	bot := h.post("issue_comment", "issue_comment-created-bot", "d-c4")
	if len(bot) != 1 || bot[0].Kind != model.EnvelopeAutomatedMessage || bot[0].SenderKind != model.ContactKindBot {
		t.Fatalf("bot comment = %+v, want automated from a bot", bot)
	}
	for _, env := range [][]model.Envelope{c, e, d} {
		if env[0].SenderKind != "" {
			t.Fatalf("user envelope = %+v, want a person sender", env[0])
		}
	}
}

// A Bot account is a bot sender wherever it authors or changes a message;
// User and Organization accounts are people.
func TestWebhookBotSenderKind(t *testing.T) {
	h := newHookRig(t, github.Config{})
	for i, tc := range []struct {
		event, fixture, account, want string
	}{
		{"issue_comment", "issue_comment-created", "Bot", model.ContactKindBot},
		{"issue_comment", "issue_comment-created", "Organization", ""},
		{"issue_comment", "issue_comment-deleted", "Bot", model.ContactKindBot},
		{"issue_comment", "issue_comment-edited", "Bot", model.ContactKindBot},
		{"issues", "issues-opened", "Bot", model.ContactKindBot},
		{"issues", "issues-opened", "User", ""},
	} {
		var ev map[string]any
		if err := json.Unmarshal(golden(t, tc.fixture), &ev); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"comment", "issue"} {
			if o, ok := ev[k].(map[string]any); ok && (k == "comment" || tc.event == "issues") {
				o["user"].(map[string]any)["type"] = tc.account
			}
		}
		ev["sender"].(map[string]any)["type"] = tc.account
		body, _ := json.Marshal(ev)
		before := len(h.sink.envelopes())
		h.serve(hookRequest(tc.event, fmt.Sprintf("d-kind-%d", i), body))
		var got []string
		for _, env := range h.sink.envelopes()[before:] {
			if !env.Sender.IsZero() {
				got = append(got, env.SenderKind)
			}
		}
		if len(got) == 0 || slices.ContainsFunc(got, func(k string) bool { return k != tc.want }) {
			t.Errorf("%s by %s: sender kinds %q, want %q", tc.fixture, tc.account, got, tc.want)
		}
	}
}

// ownComment is an issue_comment.created delivery of a comment uma posted
// for attempt, by userID.
func ownComment(t *testing.T, commentID, userID int64, attempt model.ID) []byte {
	t.Helper()
	return commentDelivery(t, commentID, userID, "Thanks, looking into it.\n\n<!-- uma:"+string(opA.ID)+":"+string(attempt)+" -->")
}

// commentDelivery is an issue_comment.created delivery of a comment with
// body by userID.
func commentDelivery(t *testing.T, commentID, userID int64, body string) []byte {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal(golden(t, "issue_comment-created"), &ev); err != nil {
		t.Fatal(err)
	}
	c := ev["comment"].(map[string]any)
	c["id"] = commentID
	c["body"] = body
	c["user"] = map[string]any{"login": "uma-sandbox[bot]", "id": userID, "type": "Bot"}
	b, _ := json.Marshal(ev)
	return b
}

func TestWebhookOwnCommentReportsEchoAttempt(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	h.serve(hookRequest("issue_comment", "d-own", ownComment(t, 2000000009, botID, tryOne)))
	h.serve(hookRequest("issue_comment", "d-forged", ownComment(t, 2000000010, 5000001, tryOne)))
	envs := h.sink.envelopes()
	if len(envs) != 2 {
		t.Fatalf("envelopes = %d, want 2", len(envs))
	}
	echo := model.EchoRef(chanA, opA, tryOne)
	if refs := envs[0].References; len(refs) != 2 || refs[1] != echo {
		t.Fatalf("own comment references = %+v, want the echo of the attempt", refs)
	}
	if refs := envs[1].References; len(refs) != 1 {
		t.Fatalf("another author's copied marker reported %+v; only the channel's account echoes", refs)
	}
	for _, e := range envs {
		if e.Content.Parts[0].Text != "Thanks, looking into it." {
			t.Fatalf("content %q keeps the marker", e.Content.Parts[0].Text)
		}
	}
}

func TestWebhookSinkFailures(t *testing.T) {
	h := newHookRig(t, github.Config{})
	h.sink.refuse = func(model.Ingress) error { return model.NewError(model.Unavailable, "store down") }
	if code, _ := h.serve(hookRequest("issue_comment", "d1", golden(t, "issue_comment-created"))); code != http.StatusServiceUnavailable {
		t.Fatalf("retryable sink failure = %d, want 503", code)
	}
	h.sink.refuse = func(model.Ingress) error { return model.NewError(model.Validation, "no inbox") }
	if code, _ := h.serve(hookRequest("issue_comment", "d1", golden(t, "issue_comment-created"))); code != http.StatusUnprocessableEntity {
		t.Fatalf("final sink failure = %d, want 422", code)
	}
}

func TestWebhookClosedAndSecretless(t *testing.T) {
	h := newHookRig(t, github.Config{})
	_ = h.inst.Close(t.Context())
	if code, _ := h.serve(hookRequest("issues", "d1", golden(t, "issues-opened"))); code != http.StatusServiceUnavailable {
		t.Fatalf("closed instance = %d, want 503", code)
	}
}
