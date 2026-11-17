package gitlab_test

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

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
)

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

// The payloads below are stand-ins shaped by GitLab's webhook events
// documentation, until a sandbox project's deliveries are captured
// (docs/recording/gitlab.md): the issue event ("Work item events", object_kind
// issue) and the comment event ("Comment events", object_kind note, noteable
// type Issue), https://docs.gitlab.com/user/project/integrations/webhook_events/.

type payload map[string]any

func hookUserJSON(id int64, username string) map[string]any {
	return map[string]any{"id": id, "name": "Name " + username, "username": username, "avatar_url": "https://example.test/a.png"}
}

func projectJSON() map[string]any {
	return map[string]any{"id": sandboxProject, "name": "uma-sandbox", "path_with_namespace": sandboxPath, "web_url": "https://gitlab.example.com/" + sandboxPath}
}

func issueAttrs(action, state, description string) map[string]any {
	return map[string]any{
		"id": 3000007, "iid": 7, "title": "Export fails", "description": description, "state": state, "action": action,
		"confidential": false, "author_id": 5000001, "assignee_ids": []int64{5000002}, "project_id": sandboxProject,
		"created_at": "2026-10-11T10:00:00.000Z", "updated_at": "2026-10-11T10:05:00.000Z",
		"url": "https://gitlab.example.com/" + sandboxPath + "/-/issues/7", "type": "Issue",
	}
}

func issueEvent(action, state, description string, changes map[string]any) payload {
	p := payload{
		"object_kind": "issue", "event_type": "issue", "user": hookUserJSON(5000001, "person-one"), "project": projectJSON(),
		"object_attributes": issueAttrs(action, state, description),
		"labels":            []any{map[string]any{"id": 1, "title": "bug"}, map[string]any{"id": 2, "title": "area::export"}},
	}
	if changes != nil {
		p["changes"] = changes
	}
	return p
}

func noteEvent(action, body string, id int64, author, actor int64, modify func(map[string]any)) payload {
	username := "actor"
	if actor == botID {
		// The generated username of a project access token's bot user.
		username = "project_700000001_bot_4ffca233d8298ea1"
	}
	attrs := map[string]any{
		"id": id, "note": body, "noteable_type": "Issue", "noteable_id": 3000007, "author_id": author, "project_id": sandboxProject,
		"internal": false, "system": false, "action": action,
		"created_at": "2026-10-11T10:10:00.000Z", "updated_at": "2026-10-11T10:10:00.000Z",
		"url": "https://gitlab.example.com/" + sandboxPath + "/-/issues/7#note_" + string(rune('0'+id%10)),
	}
	if action == "update" {
		attrs["updated_at"] = "2026-10-11T10:20:00.000Z"
	}
	if modify != nil {
		modify(attrs)
	}
	return payload{
		"object_kind": "note", "event_type": "note", "user": hookUserJSON(actor, username), "project": projectJSON(),
		"object_attributes": attrs,
		"issue": map[string]any{
			"id": 3000007, "iid": 7, "title": "Export fails", "description": "It crashes.", "state": "opened", "author_id": 5000001,
			"assignee_ids": []int64{}, "labels": []any{map[string]any{"id": 1, "title": "bug"}},
			"created_at": "2026-10-11T10:00:00.000Z", "updated_at": "2026-10-11T10:05:00.000Z", "confidential": false,
		},
	}
}

type hookRig struct {
	t    *testing.T
	inst adapter.Instance
	sink *recordingSink
	h    http.Handler
}

func newHookRig(t *testing.T, cfg gitlab.Config) *hookRig {
	r := newRig(t)
	inst := r.open(cfg)
	sink := &recordingSink{}
	return &hookRig{t: t, inst: inst, sink: sink, h: inst.(adapter.HTTPReceiver).Handler(sink)}
}

func hookRequest(event string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("User-Agent", "GitLab/17.0")
	r.Header.Set(gitlab.HeaderEvent, event)
	r.Header.Set(gitlab.HeaderToken, hookToken)
	r.Header.Set(gitlab.HeaderIdempotency, "9f1b5c7e-0000-4000-8000-000000000001")
	return r
}

func (h *hookRig) serve(r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func (h *hookRig) post(event string, p payload) []model.Envelope {
	h.t.Helper()
	before := len(h.sink.envelopes())
	body, _ := json.Marshal(p)
	if code, msg := h.serve(hookRequest(event, body)); code != http.StatusOK {
		h.t.Fatalf("%s delivery = %d %q, want 200", event, code, msg)
	}
	return h.sink.envelopes()[before:]
}

func TestWebhookRefusesUnverifiedDeliveries(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	body, _ := json.Marshal(noteEvent("create", "hi", 11, 5000001, 5000001, nil))
	for name, mutate := range map[string]func(*http.Request){
		"missing":    func(r *http.Request) { r.Header.Del(gitlab.HeaderToken) },
		"empty":      func(r *http.Request) { r.Header.Set(gitlab.HeaderToken, "") },
		"wrong":      func(r *http.Request) { r.Header.Set(gitlab.HeaderToken, "other") },
		"prefix":     func(r *http.Request) { r.Header.Set(gitlab.HeaderToken, hookToken[:5]) },
		"longer":     func(r *http.Request) { r.Header.Set(gitlab.HeaderToken, hookToken+"x") },
		"case":       func(r *http.Request) { r.Header.Set(gitlab.HeaderToken, strings.ToUpper(hookToken)) },
		"in a query": func(r *http.Request) { r.Header.Del(gitlab.HeaderToken); r.URL.RawQuery = "token=" + hookToken },
		"bearer style": func(r *http.Request) {
			r.Header.Del(gitlab.HeaderToken)
			r.Header.Set("Authorization", "Bearer "+hookToken)
		},
	} {
		r := hookRequest("Note Hook", body)
		mutate(r)
		if code, _ := h.serve(r); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
	if n := len(h.sink.envelopes()); n != 0 {
		t.Fatalf("%d envelopes reached the sink for refused deliveries", n)
	}
	if code, _ := h.serve(hookRequest("Note Hook", body)); code != http.StatusOK {
		t.Fatalf("verified delivery = %d, want 200", code)
	}
}

func TestWebhookRefusalsBeforeDecoding(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	get := hookRequest("Note Hook", nil)
	get.Method = http.MethodGet
	if code, _ := h.serve(get); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	// An unverified delivery learns nothing about the payload.
	bad := hookRequest("Note Hook", []byte("not json"))
	bad.Header.Set(gitlab.HeaderToken, "wrong")
	if code, _ := h.serve(bad); code != http.StatusUnauthorized {
		t.Errorf("unverified malformed = %d, want 401", code)
	}
	if code, _ := h.serve(hookRequest("Note Hook", []byte("not json"))); code != http.StatusBadRequest {
		t.Errorf("verified malformed = %d, want 400", code)
	}
	limited := hookRequest("Note Hook", bytes.Repeat([]byte("x"), 100))
	limited.Body = http.MaxBytesReader(httptest.NewRecorder(), limited.Body, 10)
	if code, _ := h.serve(limited); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized = %d, want 413", code)
	}
	_ = h.inst.Close(t.Context())
	if code, _ := h.serve(hookRequest("Note Hook", []byte("{}"))); code != http.StatusServiceUnavailable {
		t.Errorf("closed instance = %d, want 503", code)
	}
}

func TestWebhookNoSecretRefusesEverything(t *testing.T) {
	r := newRig(t)
	r.creds.revoked = true
	h := &hookRig{t: t, sink: &recordingSink{}}
	h.h = r.open(gitlab.Config{}).(adapter.HTTPReceiver).Handler(h.sink)
	body, _ := json.Marshal(noteEvent("create", "hi", 11, 5000001, 5000001, nil))
	if code, _ := h.serve(hookRequest("Note Hook", body)); code != http.StatusServiceUnavailable {
		t.Fatalf("unresolvable secret = %d, want 503", code)
	}
}

func TestWebhookIssueOpened(t *testing.T) {
	h := newHookRig(t, gitlab.Config{LocalFields: []string{"assignees"}})
	envs := h.post("Issue Hook", issueEvent("open", "opened", "It crashes on export.", nil))
	if len(envs) != 1 {
		t.Fatalf("envelopes = %+v, want the description", envs)
	}
	e := envs[0]
	if e.Kind != model.EnvelopeMessage || e.MessageID != "issue:3000007" || e.Destination != sandboxPath ||
		e.Sender != (model.ExternalRef{Channel: chanA, Namespace: gitlab.Namespace, Kind: gitlab.RefKindUser, ID: "5000001"}) ||
		e.References[0] != issueRoute("7") || e.Content.Parts[0].Text != "It crashes on export." ||
		e.Audience.Visibility != model.Public || e.Assurance != "verified" || e.SourceRevision != "2026-10-11T10:05:00Z" {
		t.Fatalf("envelope = %+v", e)
	}
	s := e.WorkItem
	if s == nil || s.Status != model.WorkItemOpen || s.RawStatus != "opened" || s.Subject != "Export fails" || s.Binding != model.WorkItemBindingActive ||
		len(s.Assignees) != 1 || s.Assignees[0].ID != "5000002" || len(s.LocalFields) != 1 || s.Requester.ID != "5000001" || s.Validate() != nil {
		t.Fatalf("work item = %+v", s)
	}
	if len(s.Fields) != 1 || s.Fields[0].Name != gitlab.FieldLabels || string(s.Fields[0].Data) != `["area::export","bug"]` {
		t.Fatalf("fields = %+v, want sorted labels", s.Fields)
	}
	if string(h.sink.got[0].Correlation) != "gitlab-9f1b5c7e-0000-4000-8000-000000000001" {
		t.Fatalf("correlation = %q", h.sink.got[0].Correlation)
	}
}

func TestWebhookIssueStateChanges(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	for _, tc := range []struct{ action, state string }{{"close", "closed"}, {"reopen", "opened"}, {"update", "opened"}} {
		envs := h.post("Issue Hook", issueEvent(tc.action, tc.state, "desc", nil))
		if len(envs) != 1 || envs[0].Kind != model.EnvelopeWorkItem || envs[0].WorkItem.RawStatus != tc.state {
			t.Fatalf("%s: envelopes = %+v", tc.action, envs)
		}
	}
	// An opened issue without a description reports its state only.
	envs := h.post("Issue Hook", issueEvent("open", "opened", "  ", nil))
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeWorkItem {
		t.Fatalf("empty description = %+v", envs)
	}
	// The confidential issue hook is the same event.
	if envs = h.post("Confidential Issue Hook", issueEvent("close", "closed", "d", nil)); len(envs) != 1 || envs[0].WorkItem.Status != model.WorkItemClosed {
		t.Fatalf("confidential = %+v", envs)
	}
}

func TestWebhookDescriptionEdit(t *testing.T) {
	h := newHookRig(t, gitlab.Config{Replies: true, AccountID: botID})
	changes := map[string]any{"description": map[string]any{"previous": "old", "current": "new"}}
	envs := h.post("Issue Hook", issueEvent("update", "opened", "new text", changes))
	if len(envs) != 2 || envs[0].Kind != model.EnvelopeWorkItem || envs[1].Kind != model.EnvelopeMessageEdited ||
		envs[1].MessageID != "issue:3000007" || envs[1].Content.Parts[0].Text != "new text" {
		t.Fatalf("envelopes = %+v", envs)
	}
	if len(h.sink.got[1].Envelope.References) != 1 {
		t.Fatalf("an edit by another user names no attempt: %+v", h.sink.got[1].Envelope.References)
	}
	// The edit falls back to recording the description when it is unknown.
	h.sink.refuse = func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited {
			return model.NewError(model.NotFound, "unknown message")
		}
		return nil
	}
	h.post("Issue Hook", issueEvent("update", "opened", "newer", changes))
	last := h.sink.envelopes()
	if last[len(last)-1].Kind != model.EnvelopeMessage || last[len(last)-1].MessageID != "issue:3000007" {
		t.Fatalf("fallback = %+v", last[len(last)-1])
	}
	// A cleared description deletes the message.
	envs = h.post("Issue Hook", issueEvent("update", "opened", "", changes))
	if len(envs) != 2 || envs[1].Kind != model.EnvelopeMessageDeleted {
		t.Fatalf("cleared = %+v", envs)
	}
}

func TestWebhookNotes(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	envs := h.post("Note Hook", noteEvent("create", "Same here.", 11, 5000003, 5000003, nil))
	if len(envs) != 1 {
		t.Fatalf("envelopes = %+v", envs)
	}
	e := envs[0]
	if e.Kind != model.EnvelopeMessage || e.MessageID != "note:11" || e.Destination != sandboxPath || e.References[0] != issueRoute("7") ||
		e.Sender.ID != "5000003" || e.Content.Parts[0].Text != "Same here." || e.Audience.Visibility != model.Public ||
		e.WorkItem == nil || e.WorkItem.Status != model.WorkItemOpen || e.SenderKind != "" {
		t.Fatalf("created = %+v", e)
	}

	// An edit names its editor, not the author.
	envs = h.post("Note Hook", noteEvent("update", "Same here, fixed.", 11, 5000003, 5000009, nil))
	e = envs[0]
	if len(envs) != 1 || e.Kind != model.EnvelopeMessageEdited || e.MessageID != "note:11" || e.Sender.ID != "5000009" ||
		e.Content.Parts[0].Text != "Same here, fixed." || e.SourceRevision != "2026-10-11T10:20:00Z" {
		t.Fatalf("edited = %+v", e)
	}

	// Internal notes keep their audience.
	envs = h.post("Confidential Note Hook", noteEvent("create", "Private.", 12, 5000003, 5000003, func(a map[string]any) { a["internal"] = true }))
	if len(envs) != 1 || envs[0].Audience.Visibility != model.Internal {
		t.Fatalf("internal = %+v", envs)
	}

	// An older GitLab states no action: equal times are a creation.
	envs = h.post("Note Hook", noteEvent("", "No action.", 13, 5000003, 5000003, func(a map[string]any) { delete(a, "action") }))
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeMessage {
		t.Fatalf("no action = %+v", envs)
	}
}

func TestWebhookTimestampForms(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	envs := h.post("Note Hook", noteEvent("create", "Old format.", 14, 5000003, 5000003, func(a map[string]any) {
		a["created_at"], a["updated_at"] = "2026-10-11 10:10:00 UTC", "2026-10-11 10:10:00 UTC"
	}))
	if envs[0].SourceRevision != "2026-10-11T10:10:00Z" || envs[0].OccurredAt.IsZero() {
		t.Fatalf("envelope = %+v, want the older timestamp form read", envs[0])
	}
}

func TestWebhookDropsWhatIsNotAnIssueMessage(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	for name, p := range map[string]payload{
		"system note":   noteEvent("create", "closed", 20, 5000001, 5000001, func(a map[string]any) { a["system"] = true }),
		"merge request": noteEvent("create", "hi", 21, 5000001, 5000001, func(a map[string]any) { a["noteable_type"] = "MergeRequest" }),
		"commit":        noteEvent("create", "hi", 22, 5000001, 5000001, func(a map[string]any) { a["noteable_type"] = "Commit" }),
	} {
		if envs := h.post("Note Hook", p); len(envs) != 0 {
			t.Errorf("%s: %+v, want dropped", name, envs)
		}
	}
	for _, event := range []string{"Push Hook", "Merge Request Hook", "Pipeline Hook", "Emoji Hook", "Member Hook", ""} {
		if envs := h.post(event, payload{"object_kind": "push"}); len(envs) != 0 {
			t.Errorf("%s: %+v, want dropped", event, envs)
		}
	}
	// A work item that is not an issue (a task) is dropped.
	wi := issueEvent("open", "opened", "x", nil)
	wi["object_kind"] = "work_item"
	if envs := h.post("Issue Hook", wi); len(envs) != 0 {
		t.Errorf("work_item: %+v, want dropped", envs)
	}
}

func TestWebhookMalformed(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	mutate := func(p payload, f func(payload)) payload { f(p); return p }
	for name, tc := range map[string]struct {
		event string
		p     payload
	}{
		"header names a note, payload an issue": {"Note Hook", issueEvent("open", "opened", "x", nil)},
		"header names an issue, payload a note": {"Issue Hook", noteEvent("create", "x", 1, 1, 1, nil)},
		"no project id":                         {"Issue Hook", mutate(issueEvent("open", "opened", "x", nil), func(p payload) { p["project"] = map[string]any{"path_with_namespace": sandboxPath} })},
		"project path":                          {"Issue Hook", mutate(issueEvent("open", "opened", "x", nil), func(p payload) { p["project"] = map[string]any{"id": 1, "path_with_namespace": "../x"} })},
		"no issue iid": {"Issue Hook", mutate(issueEvent("open", "opened", "x", nil), func(p payload) {
			p["object_attributes"].(map[string]any)["iid"] = 0
		})},
		"no action": {"Issue Hook", mutate(issueEvent("open", "opened", "x", nil), func(p payload) {
			delete(p["object_attributes"].(map[string]any), "action")
		})},
		"note without issue": {"Note Hook", mutate(noteEvent("create", "x", 1, 1, 1, nil), func(p payload) { delete(p, "issue") })},
		"note without id":    {"Note Hook", noteEvent("create", "x", 0, 1, 1, nil)},
	} {
		body, _ := json.Marshal(tc.p)
		if code, _ := h.serve(hookRequest(tc.event, body)); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, code)
		}
	}
	if n := len(h.sink.envelopes()); n != 0 {
		t.Fatalf("%d envelopes for malformed deliveries", n)
	}
}

// The event ID is a digest of the body: a retry is the same event, any other
// body a different one, whatever the headers say.
func TestWebhookEventIdentity(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	body, _ := json.Marshal(noteEvent("create", "hi", 30, 5000001, 5000001, nil))
	other, _ := json.Marshal(noteEvent("create", "hi!", 30, 5000001, 5000001, nil))
	for _, b := range [][]byte{body, body, other} {
		r := hookRequest("Note Hook", b)
		r.Header.Set(gitlab.HeaderIdempotency, "same-key-for-all")
		if code, _ := h.serve(r); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	}
	envs := h.sink.envelopes()
	sum := sha256.Sum256(body)
	sum2 := sha256.Sum256(other)
	if envs[0].EventID != "sha256:"+hex.EncodeToString(sum[:]) || envs[1].EventID != envs[0].EventID ||
		envs[2].EventID != "sha256:"+hex.EncodeToString(sum2[:]) || envs[2].EventID == envs[0].EventID {
		t.Fatalf("event IDs = %q %q %q", envs[0].EventID, envs[1].EventID, envs[2].EventID)
	}
}

func TestWebhookSinkFailures(t *testing.T) {
	h := newHookRig(t, gitlab.Config{})
	body, _ := json.Marshal(noteEvent("create", "hi", 31, 5000001, 5000001, nil))
	for _, tc := range []struct {
		err  error
		want int
	}{
		{model.NewError(model.Transient, "busy"), http.StatusServiceUnavailable},
		{model.NewError(model.Validation, "no"), http.StatusUnprocessableEntity},
	} {
		h.sink.refuse = func(model.Ingress) error { return tc.err }
		if code, _ := h.serve(hookRequest("Note Hook", body)); code != tc.want {
			t.Errorf("%v: status %d, want %d", tc.err, code, tc.want)
		}
	}
}

func TestWebhookEchoOfOwnNote(t *testing.T) {
	h := newHookRig(t, gitlab.Config{Replies: true, AccountID: botID})
	marked := "Thanks.\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryOne) + " -->"
	envs := h.post("Note Hook", noteEvent("create", marked, 40, botID, botID, nil))
	e := envs[0]
	if e.Content.Parts[0].Text != "Thanks." || e.SenderKind != model.ContactKindBot || e.Kind != model.EnvelopeAutomatedMessage {
		t.Fatalf("own note = %+v, want the bot's text without the marker", e)
	}
	if len(e.References) != 2 || e.References[1] != model.EchoRef(chanA, opA, tryOne) {
		t.Fatalf("references = %+v, want the echo of the attempt", e.References)
	}
	// The same body by anyone else is a person's note, with no echo.
	envs = h.post("Note Hook", noteEvent("create", marked, 41, 5000001, 5000001, nil))
	if len(envs[0].References) != 1 {
		t.Fatalf("copied marker = %+v, want no echo", envs[0].References)
	}
	// An edit made by the account settles the edit; by another user, not.
	envs = h.post("Note Hook", noteEvent("update", marked, 40, botID, botID, nil))
	if len(envs[0].References) != 2 || envs[0].References[1] != model.EchoRef(chanA, opA, tryOne) {
		t.Fatalf("own edit = %+v, want the echo", envs[0].References)
	}
	envs = h.post("Note Hook", noteEvent("update", marked, 40, botID, 5000001, nil))
	if len(envs[0].References) != 1 {
		t.Fatalf("edit by a person of marked text = %+v, want no echo", envs[0].References)
	}
}
