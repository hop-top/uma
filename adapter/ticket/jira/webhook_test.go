package jira_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/model"
)

// Signature vector published with the webhooks documentation:
// https://developer.atlassian.com/cloud/jira/platform/webhooks/ (secure
// admin webhooks). A literal copy of the documented values, not a recording.
func TestSignatureDocumentedVector(t *testing.T) {
	const want = "sha256=a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9"
	secret, body := []byte("It's a Secret to Everybody"), []byte("Hello World!")
	if !jira.Verified(want, body, secret) {
		t.Fatal("the documented signature is refused")
	}
	for name, sig := range map[string]string{
		"empty":           "",
		"no method":       "a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9",
		"other method":    "sha512=a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9",
		"sha1":            "sha1=a4771c39fbe90f317c7824e83ddef3caae9cb3d9",
		"not hex":         "sha256=zz771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9",
		"truncated":       want[:len(want)-2],
		"wrong signature": "sha256=" + strings.Repeat("0", 64),
		"upper method":    "SHA256=a4771c39fbe90f317c7824e83ddef3caae9cb3d976c214ace1f2937e133263c9",
	} {
		if jira.Verified(sig, body, secret) {
			t.Errorf("%s: signature accepted", name)
		}
	}
	if jira.Verified(want, []byte("Hello World?"), secret) || jira.Verified(want, body, []byte("another")) || jira.Verified(want, body, nil) {
		t.Error("tampered body, wrong secret or no secret accepted")
	}
}

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

type hookRig struct {
	*rig
	sink *recordingSink
	h    http.Handler
}

func newHookRig(t *testing.T) *hookRig {
	r := newRig(t)
	sink := &recordingSink{}
	return &hookRig{rig: r, sink: sink, h: r.inst.(adapter.HTTPReceiver).Handler(sink)}
}

// stand-in payloads: built from the documented shape of admin webhooks
// (webhookEvent, timestamp, user, issue, comment, changelog) and the REST
// API's user, issue and comment objects. A recording replaces them.
func person(id, kind string) map[string]any {
	return map[string]any{"accountId": id, "displayName": "Name of " + id, "accountType": kind, "active": true}
}

func issueObj(fields map[string]any) map[string]any {
	f := map[string]any{
		"summary": "Export fails", "labels": []string{"zeta", "alpha"}, "project": map[string]any{"key": "SBX"},
		"status":   map[string]any{"name": "In Progress", "statusCategory": map[string]any{"key": "indeterminate"}},
		"reporter": person("712020:reporter", "atlassian"), "creator": person("712020:reporter", "atlassian"),
		"assignee": person("712020:assignee", "atlassian"),
		"created":  "2026-10-11T11:00:00.000+0000", "updated": "2026-10-11T11:30:00.000+0000",
	}
	for k, v := range fields {
		f[k] = v
	}
	return map[string]any{"id": issueID, "key": "SBX-1", "self": "https://" + host + "/rest/api/2/issue/" + issueID, "fields": f}
}

func commentObj(id, author string, kind string) map[string]any {
	return map[string]any{
		"self": "https://" + host + "/rest/api/3/issue/" + issueID + "/comment/" + id, "id": id,
		"author": person(author, kind), "updateAuthor": person(author, kind), "body": adfOf("Hello"),
		"created": "2026-10-11T11:10:00.000+0000", "updated": "2026-10-11T11:10:00.000+0000", "jsdPublic": true,
	}
}

func event(name string, parts map[string]any) []byte {
	m := map[string]any{"timestamp": int64(1_791_718_200_000), "webhookEvent": name, "user": person("712020:actor", "atlassian")}
	for k, v := range parts {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return b
}

func hookRequest(body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/jira", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(jira.HeaderIdentifier, "d0e5b8c2-6f1a-4a49-9d5e-77d1c7f9e001")
	r.Header.Set(jira.HeaderSignature, sign(hookSecret, body))
	return r
}

func (h *hookRig) serve(r *http.Request) int {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec.Code
}

func (h *hookRig) deliver(body []byte) []model.Envelope {
	h.t.Helper()
	before := len(h.sink.envelopes())
	if code := h.serve(hookRequest(body)); code != http.StatusOK {
		h.t.Fatalf("status = %d, want 200", code)
	}
	return h.sink.envelopes()[before:]
}

func TestHandlerAuthentication(t *testing.T) {
	h := newHookRig(t)
	body := event("comment_created", map[string]any{"issue": issueObj(nil), "comment": commentObj("20010", "712020:a", "atlassian")})

	for name, tc := range map[string]struct {
		req  func() *http.Request
		want int
	}{
		"method":   {func() *http.Request { r := hookRequest(body); r.Method = http.MethodGet; return r }, http.StatusMethodNotAllowed},
		"unsigned": {func() *http.Request { r := hookRequest(body); r.Header.Del(jira.HeaderSignature); return r }, http.StatusUnauthorized},
		"wrong": {func() *http.Request {
			r := hookRequest(body)
			r.Header.Set(jira.HeaderSignature, sign("another", body))
			return r
		}, http.StatusUnauthorized},
		"sha1": {func() *http.Request {
			r := hookRequest(body)
			r.Header.Set(jira.HeaderSignature, "sha1="+hex.EncodeToString(make([]byte, 20)))
			return r
		}, http.StatusUnauthorized},
		"tampered": {func() *http.Request {
			r := hookRequest(body)
			r.Body = http.NoBody
			r2 := httptest.NewRequest(http.MethodPost, "/webhooks/jira", bytes.NewReader(append([]byte(" "), body...)))
			r2.Header = r.Header
			return r2
		}, http.StatusUnauthorized},
		"not json": {func() *http.Request { return hookRequest([]byte("signed but not json")) }, http.StatusBadRequest},
		"ok":       {func() *http.Request { return hookRequest(body) }, http.StatusOK},
	} {
		if got := h.serve(tc.req()); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
	if n := len(h.sink.envelopes()); n != 1 {
		t.Fatalf("sink got %d envelopes, want only the authenticated one", n)
	}

	// The handler of a closed instance answers 503.
	_ = h.inst.Close(t.Context())
	if got := h.serve(hookRequest(body)); got != http.StatusServiceUnavailable {
		t.Fatalf("closed: %d", got)
	}
}

// The event ID is the digest of the signed body: a redelivery (same body)
// is the same event whatever header it carries; another body is another.
func TestEventIDIsTheBodyDigest(t *testing.T) {
	h := newHookRig(t)
	body := event("comment_created", map[string]any{"issue": issueObj(nil), "comment": commentObj("20010", "712020:a", "atlassian")})
	sum := sha256.Sum256(body)
	want := "sha256:" + hex.EncodeToString(sum[:])
	h.deliver(body)
	r := hookRequest(body)
	r.Header.Set(jira.HeaderIdentifier, "another-identifier")
	h.serve(r)
	other := event("comment_created", map[string]any{"issue": issueObj(nil), "comment": commentObj("20011", "712020:a", "atlassian")})
	h.deliver(other)
	got := h.sink.envelopes()
	if len(got) != 3 || got[0].EventID != want || got[1].EventID != want || got[2].EventID == want {
		t.Fatalf("event IDs = %q %q %q, want %q twice then another", got[0].EventID, got[1].EventID, got[2].EventID, want)
	}
	if c := h.sink.got[0].Correlation; c != "jira-d0e5b8c2-6f1a-4a49-9d5e-77d1c7f9e001" {
		t.Fatalf("correlation = %q", c)
	}
}

func TestIssueCreatedReportsDescriptionAndState(t *testing.T) {
	h := newHookRig(t)
	got := h.deliver(event("jira:issue_created", map[string]any{"issue": issueObj(map[string]any{"description": adfOf("The export button does nothing.")})}))
	if len(got) != 1 {
		t.Fatalf("envelopes = %d, want the description message", len(got))
	}
	env, ws := got[0], got[0].WorkItem
	if env.Kind != model.EnvelopeMessage || env.MessageID != "issue:"+issueID || env.Destination != "SBX" || env.Content.Parts[0].Text != "The export button does nothing." ||
		env.Sender.ID != "712020:reporter" || env.Sender.Kind != jira.RefKindUser || env.Sender.Namespace != jira.Namespace || env.SenderKind != "" ||
		len(env.References) != 1 || env.References[0] != route(issueID) || env.Assurance != "verified" || env.Audience.Visibility != model.Public {
		t.Fatalf("description envelope = %+v", env)
	}
	if ws == nil || ws.Validate() != nil || ws.Subject != "Export fails" || ws.URL != "https://"+host+"/browse/SBX-1" || ws.Status != model.WorkItemOpen ||
		ws.RawStatus != "In Progress" || ws.Requester.ID != "712020:reporter" || len(ws.Assignees) != 1 || ws.Assignees[0].ID != "712020:assignee" ||
		ws.Binding != model.WorkItemBindingActive || len(ws.Fields) != 1 || ws.Fields[0].Name != jira.FieldLabels || string(ws.Fields[0].Data) != `["alpha","zeta"]` {
		t.Fatalf("work item state = %+v", ws)
	}
	if !env.OccurredAt.Equal(time.Date(2026, 10, 11, 11, 0, 0, 0, time.UTC)) || env.SourceRevision != "2026-10-11T11:30:00Z" {
		t.Fatalf("times: occurred %v revision %q", env.OccurredAt, env.SourceRevision)
	}

	// No description: the state alone.
	got = h.deliver(event("jira:issue_created", map[string]any{"issue": issueObj(nil)}))
	if len(got) != 1 || got[0].Kind != model.EnvelopeWorkItem || got[0].WorkItem == nil {
		t.Fatalf("issue without description = %+v", got)
	}
}

func TestIssueUpdatedAndDeleted(t *testing.T) {
	h := newHookRig(t)
	desc := map[string]any{"description": adfOf("Reworded.")}
	changed := map[string]any{"changelog": map[string]any{"id": "1", "items": []any{map[string]any{"field": "description"}}}}

	// A status change: the state alone.
	got := h.deliver(event("jira:issue_updated", map[string]any{"issue": issueObj(map[string]any{
		"status": map[string]any{"name": "Done", "statusCategory": map[string]any{"key": "done"}},
	}), "changelog": map[string]any{"items": []any{map[string]any{"field": "status"}}}}))
	if len(got) != 1 || got[0].WorkItem.Status != model.WorkItemClosed || got[0].WorkItem.RawStatus != "Done" {
		t.Fatalf("status change = %+v", got)
	}

	// A description change: the state and an edit of the description, which
	// records the description when it is unknown.
	body := event("jira:issue_updated", map[string]any{"issue": issueObj(desc), "changelog": changed["changelog"]})
	got = h.deliver(body)
	if len(got) != 2 || got[1].Kind != model.EnvelopeMessageEdited || got[1].MessageID != "issue:"+issueID || got[1].Content.Parts[0].Text != "Reworded." || got[1].Sender.ID != "712020:actor" {
		t.Fatalf("description change = %+v", got)
	}
	if fb := h.fallback(body); fb == nil || fb.Kind != model.EnvelopeMessage || fb.Sender.ID != "712020:reporter" || fb.WorkItem == nil {
		t.Fatalf("fallback = %+v", fb)
	}

	// A cleared description deletes the message.
	got = h.deliver(event("jira:issue_updated", map[string]any{"issue": issueObj(map[string]any{"description": nil}), "changelog": changed["changelog"]}))
	if len(got) != 2 || got[1].Kind != model.EnvelopeMessageDeleted {
		t.Fatalf("cleared description = %+v", got)
	}

	// A deleted issue binds deleted.
	got = h.deliver(event("jira:issue_deleted", map[string]any{"issue": issueObj(nil)}))
	if len(got) != 1 || got[0].WorkItem.Binding != model.WorkItemBindingDeleted {
		t.Fatalf("deleted issue = %+v", got)
	}

	// A move to another project keeps the issue's reference: the ID is the
	// identity, the project key only the destination.
	moved := issueObj(map[string]any{"project": map[string]any{"key": "OPS"}})
	moved["key"] = "OPS-9"
	got = h.deliver(event("jira:issue_updated", map[string]any{"issue": moved, "changelog": map[string]any{"items": []any{map[string]any{"field": "project"}}}}))
	if len(got) != 1 || got[0].Destination != "OPS" || got[0].References[0] != route(issueID) || got[0].WorkItem.URL != "https://"+host+"/browse/OPS-9" {
		t.Fatalf("moved issue = %+v", got)
	}
}

// fallback returns the envelope an edit of an unknown message records
// instead: the sink refuses the edit as not found.
func (h *hookRig) fallback(body []byte) *model.Envelope {
	rig := newHookRig(h.t)
	rig.sink.refuse = func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited {
			return model.NewError(model.NotFound, "unknown message")
		}
		return nil
	}
	if code := rig.serve(hookRequest(body)); code != http.StatusOK {
		h.t.Fatalf("status with a fallback = %d", code)
	}
	envs := rig.sink.envelopes()
	if len(envs) == 0 {
		return nil
	}
	return &envs[len(envs)-1]
}

func TestCommentEvents(t *testing.T) {
	h := newHookRig(t)
	iss := issueObj(nil)

	got := h.deliver(event("comment_created", map[string]any{"issue": iss, "comment": commentObj("20010", "712020:customer", "customer")}))
	if len(got) != 1 {
		t.Fatalf("created = %+v", got)
	}
	env := got[0]
	if env.Kind != model.EnvelopeMessage || env.MessageID != "comment:20010" || env.Content.Parts[0].Text != "Hello" || env.Sender.ID != "712020:customer" ||
		env.WorkItem == nil || env.WorkItem.Subject != "Export fails" || env.Destination != "SBX" || env.References[0] != route(issueID) ||
		!env.OccurredAt.Equal(time.Date(2026, 10, 11, 11, 10, 0, 0, time.UTC)) {
		t.Fatalf("created envelope = %+v", env)
	}

	// An app account is a bot and its message automated.
	got = h.deliver(event("comment_created", map[string]any{"issue": iss, "comment": commentObj("20011", "557058:app", "app")}))
	if got[0].Kind != model.EnvelopeAutomatedMessage || got[0].SenderKind != model.ContactKindBot {
		t.Fatalf("app comment = %+v", got[0])
	}

	// A restricted comment, and a Service Management internal note, are internal.
	restricted := commentObj("20012", "712020:assignee", "atlassian")
	restricted["visibility"] = map[string]any{"type": "role", "value": "Administrators"}
	note := commentObj("20013", "712020:assignee", "atlassian")
	note["jsdPublic"] = false
	for _, c := range []map[string]any{restricted, note} {
		got = h.deliver(event("comment_created", map[string]any{"issue": iss, "comment": c}))
		if got[0].Audience.Visibility != model.Internal {
			t.Errorf("comment %v audience = %q, want internal", c["id"], got[0].Audience.Visibility)
		}
	}

	// An edit names its editor and the edit time; a deletion states no time.
	edited := commentObj("20010", "712020:customer", "customer")
	edited["updated"] = "2026-10-11T11:20:00.000+0000"
	edited["updateAuthor"] = person("712020:assignee", "atlassian")
	edited["body"] = adfOf("Hello again")
	got = h.deliver(event("comment_updated", map[string]any{"issue": iss, "comment": edited}))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessageEdited || got[0].Content.Parts[0].Text != "Hello again" || got[0].Sender.ID != "712020:assignee" ||
		got[0].SourceRevision != "2026-10-11T11:20:00Z" {
		t.Fatalf("edit = %+v", got)
	}
	got = h.deliver(event("comment_deleted", map[string]any{"issue": iss, "comment": commentObj("20010", "712020:customer", "customer")}))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessageDeleted || got[0].MessageID != "comment:20010" || !got[0].OccurredAt.IsZero() || got[0].SourceRevision != "" {
		t.Fatalf("deletion = %+v", got)
	}
}

// A comment by the channel's account that carries the echo property reports
// the attempt it echoes; one by anyone else never does, a copy of the
// property included.
func TestOwnCommentEchoesItsAttempt(t *testing.T) {
	h := newHookRig(t)
	prop := []any{map[string]any{"key": jira.EchoPropertyKey, "value": map[string]any{"operation": opA.ID, "attempt": tryOne}}}
	own := commentObj("20020", ownAccount, "atlassian")
	own["properties"] = prop
	got := h.deliver(event("comment_created", map[string]any{"issue": issueObj(nil), "comment": own}))
	want := model.EchoRef(chanA, opA, tryOne)
	if len(got[0].References) != 2 || got[0].References[1] != want {
		t.Fatalf("own comment references = %+v, want the echo %+v", got[0].References, want)
	}
	foreign := commentObj("20021", "712020:other", "atlassian")
	foreign["properties"] = prop
	got = h.deliver(event("comment_created", map[string]any{"issue": issueObj(nil), "comment": foreign}))
	if len(got[0].References) != 1 {
		t.Fatalf("foreign comment references = %+v, want no echo", got[0].References)
	}
	// An edit by the account echoes its attempt; by anyone else it does not.
	upd := commentObj("20020", ownAccount, "atlassian")
	upd["properties"] = prop
	got = h.deliver(event("comment_updated", map[string]any{"issue": issueObj(nil), "comment": upd}))
	if len(got) != 1 || len(got[0].References) != 2 || got[0].References[1] != want {
		t.Fatalf("own edit = %+v", got)
	}
	upd["updateAuthor"] = person("712020:other", "atlassian")
	got = h.deliver(event("comment_updated", map[string]any{"issue": issueObj(nil), "comment": upd}))
	if len(got[0].References) != 1 {
		t.Fatalf("edit of uma's comment by another user = %+v, want no echo", got[0].References)
	}
}

func TestMalformedAndUnconsumedEvents(t *testing.T) {
	h := newHookRig(t)
	good := commentObj("20010", "712020:a", "atlassian")
	noIssue := map[string]any{"comment": good}
	for name, body := range map[string][]byte{
		"comment event without a comment":  event("comment_created", map[string]any{"issue": issueObj(nil)}),
		"issue event with a comment":       event("jira:issue_updated", map[string]any{"issue": issueObj(nil), "comment": good}),
		"comment without an issue":         event("comment_created", noIssue),
		"issue without an ID":              event("jira:issue_updated", map[string]any{"issue": map[string]any{"key": "SBX-1"}}),
		"issue ID that is not a number":    event("jira:issue_updated", map[string]any{"issue": map[string]any{"id": "SBX-1", "key": "SBX-1"}}),
		"comment without an ID":            event("comment_created", map[string]any{"issue": issueObj(nil), "comment": map[string]any{"author": person("a", "atlassian")}}),
		"comment without an author":        event("comment_created", map[string]any{"issue": issueObj(nil), "comment": map[string]any{"id": "20030", "body": adfOf("x")}}),
		"issue without a project or a key": event("jira:issue_updated", map[string]any{"issue": map[string]any{"id": issueID, "fields": map[string]any{}}}),
	} {
		if code := h.serve(hookRequest(body)); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
	// Events the adapter does not consume are acknowledged and dropped.
	for _, name := range []string{"worklog_created", "sprint_started", "jira:version_released", "user_created", ""} {
		if got := h.deliver(event(name, map[string]any{"issue": issueObj(nil)})); len(got) != 0 {
			t.Errorf("%q reported %+v", name, got)
		}
	}
}

func TestSinkFailuresMapToRetryableStatus(t *testing.T) {
	h := newHookRig(t)
	body := event("comment_created", map[string]any{"issue": issueObj(nil), "comment": commentObj("20010", "712020:a", "atlassian")})
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"transient":  {model.NewError(model.Transient, "busy"), http.StatusServiceUnavailable},
		"rejected":   {model.NewError(model.Validation, "no"), http.StatusUnprocessableEntity},
		"not found":  {model.NewError(model.NotFound, "no inbox"), http.StatusUnprocessableEntity},
		"duplicates": {nil, http.StatusOK},
	} {
		h.sink.refuse = func(model.Ingress) error { return tc.err }
		if got := h.serve(hookRequest(body)); got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
	}
}

// Comment bodies are ADF in REST API v3; the payload may also carry a plain
// string (wiki markup). Both are read; mentions, emoji, cards and lists keep
// their visible text.
func TestBodyFormats(t *testing.T) {
	h := newHookRig(t)
	rich := json.RawMessage(`{"version":1,"type":"doc","content":[
		{"type":"paragraph","content":[{"type":"text","text":"Hi "},{"type":"mention","attrs":{"id":"x","text":"@Support"}},{"type":"text","text":" see "},{"type":"inlineCard","attrs":{"url":"https://example.com/a"}},{"type":"hardBreak"},{"type":"text","text":"line two "},{"type":"emoji","attrs":{"shortName":":smile:","text":"😄"}}]},
		{"type":"bulletList","content":[{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}]},
		{"type":"mediaSingle","content":[{"type":"media","attrs":{"id":"m","type":"file"}}]},
		{"type":"codeBlock","content":[{"type":"text","text":"x := 1"}]}]}`)
	for name, tc := range map[string]struct {
		body json.RawMessage
		want string
	}{
		"rich":   {rich, "Hi @Support see https://example.com/a\nline two 😄\n\none\ntwo\n\nx := 1"},
		"string": {json.RawMessage(`"wiki *markup*"`), "wiki *markup*"},
		"null":   {json.RawMessage(`null`), ""},
	} {
		c := commentObj("20040", "712020:a", "atlassian")
		c["body"] = tc.body
		got := h.deliver(event("comment_created", map[string]any{"issue": issueObj(nil), "comment": c}))
		text := ""
		if len(got[0].Content.Parts) > 0 {
			text = got[0].Content.Parts[0].Text
		}
		if text != tc.want {
			t.Errorf("%s: text %q, want %q", name, text, tc.want)
		}
	}
}

var _ = context.Background
