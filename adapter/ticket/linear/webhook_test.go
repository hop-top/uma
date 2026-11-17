package linear_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	linear "hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The payloads below are stand-ins: documented-shape webhook bodies built
// from https://linear.app/developers/webhooks (envelope: action, type,
// actor, data, url, createdAt, updatedFrom, organizationId, webhookId,
// webhookTimestamp) and the IssueWebhookPayload and CommentWebhookPayload
// types of Linear's published schema
// (https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql).
// They are not recorded deliveries; the recording guide lists the real ones
// that replace them.

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

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func stamp(t time.Time) int64 { return t.UnixMilli() }

type obj = map[string]any

func issueData(id string, over obj) obj {
	d := obj{
		"id": id, "identifier": "ENG-7", "number": 7, "title": "Export fails", "url": "https://linear.app/example/issue/ENG-7/export-fails",
		"description": "It crashes.", "createdAt": "2026-10-11T11:00:00.000Z", "updatedAt": "2026-10-11T11:30:00.000Z",
		"creatorId": personID, "assigneeId": nil, "teamId": "t1", "stateId": "s1",
		"team": obj{"id": "t1", "key": "ENG", "name": "Engineering"}, "state": obj{"id": "s1", "name": "In Progress", "type": "started", "color": "#fff"},
		"labelIds": []string{}, "labels": []obj{}, "priority": 0,
	}
	for k, v := range over {
		d[k] = v
	}
	return d
}

func commentData(id string, over obj) obj {
	d := obj{
		"id": id, "body": "It crashes on export.", "createdAt": "2026-10-11T11:40:00.000Z", "updatedAt": "2026-10-11T11:40:00.000Z",
		"issueId": issueA, "userId": personID, "user": obj{"id": personID, "name": "Pat", "email": "p@example.test", "url": "https://linear.app/example/profiles/pat"},
		"issue": obj{"id": issueA, "identifier": "ENG-7", "title": "Export fails", "url": "https://linear.app/example/issue/ENG-7/export-fails", "teamId": "t1", "team": obj{"id": "t1", "key": "ENG", "name": "Engineering"}},
	}
	for k, v := range over {
		d[k] = v
	}
	return d
}

func payload(typ, action string, data obj, at time.Time, extra obj) []byte {
	p := obj{
		"action": action, "type": typ, "data": data, "url": "https://linear.app/example/issue/ENG-7", "createdAt": at.UTC().Format(time.RFC3339Nano),
		"organizationId": "0b6c9f4e-1a2d-4e3f-8a9b-5c6d7e8f9a01", "webhookId": "5d4c3b2a-1f0e-4d9c-8b7a-6f5e4d3c2b11",
		"webhookTimestamp": stamp(at), "actor": obj{"id": personID, "type": "user", "name": "Pat"},
	}
	for k, v := range extra {
		p[k] = v
	}
	b, _ := json.Marshal(p)
	return b
}

func hookRequest(event, delivery string, body []byte) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/webhooks/linear", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("User-Agent", "Linear-Webhook")
	r.Header.Set(linear.HeaderEvent, event)
	r.Header.Set(linear.HeaderDelivery, delivery)
	r.Header.Set(linear.HeaderSignature, sign(hookSecret, body))
	r.Header.Set("Linear-Timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	return r
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

func (h *hookRig) serve(r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec.Code, strings.TrimSpace(rec.Body.String())
}

func (h *hookRig) post(event, delivery string, body []byte) []model.Envelope {
	h.t.Helper()
	before := len(h.sink.envelopes())
	if code, text := h.serve(hookRequest(event, delivery, body)); code != http.StatusOK {
		h.t.Fatalf("%s delivery = %d %q, want 200", event, code, text)
	}
	return h.sink.envelopes()[before:]
}

func TestWebhookRefusesUnverifiedDeliveries(t *testing.T) {
	h := newHookRig(t)
	body := payload("Comment", "create", commentData(botID, nil), t0, nil)
	for name, mutate := range map[string]func(*http.Request){
		"missing":      func(r *http.Request) { r.Header.Del(linear.HeaderSignature) },
		"wrong secret": func(r *http.Request) { r.Header.Set(linear.HeaderSignature, sign("other", body)) },
		"other body": func(r *http.Request) {
			r.Header.Set(linear.HeaderSignature, sign(hookSecret, payload("Issue", "create", issueData(issueA, nil), t0, nil)))
		},
		"not hex":     func(r *http.Request) { r.Header.Set(linear.HeaderSignature, strings.Repeat("zz", 32)) },
		"truncated":   func(r *http.Request) { r.Header.Set(linear.HeaderSignature, sign(hookSecret, body)[:62]) },
		"prefixed":    func(r *http.Request) { r.Header.Set(linear.HeaderSignature, "sha256="+sign(hookSecret, body)) },
		"empty":       func(r *http.Request) { r.Header.Set(linear.HeaderSignature, "") },
		"body edited": func(r *http.Request) { r.Body = http.NoBody; r.ContentLength = 0 },
	} {
		r := hookRequest("Comment", "d-"+name, body)
		mutate(r)
		code, text := h.serve(r)
		if code != http.StatusUnauthorized || text != http.StatusText(http.StatusUnauthorized) {
			t.Errorf("%s signature = %d %q, want 401 with the status text only", name, code, text)
		}
	}
	if n := len(h.sink.envelopes()); n != 0 {
		t.Fatalf("%d envelopes reached the sink from refused deliveries", n)
	}
}

func TestWebhookReplayWindow(t *testing.T) {
	h := newHookRig(t)
	for name, c := range map[string]struct {
		at   time.Time
		want int
	}{
		"now":            {t0, 200},
		"59s old":        {t0.Add(-59 * time.Second), 200},
		"exactly 60s":    {t0.Add(-time.Minute), 200},
		"61s old":        {t0.Add(-61 * time.Second), 401},
		"an hour old":    {t0.Add(-time.Hour), 401},
		"61s in future":  {t0.Add(61 * time.Second), 401},
		"far in future":  {t0.Add(24 * time.Hour), 401},
		"zero timestamp": {time.UnixMilli(0), 400},
	} {
		body := payload("Comment", "create", commentData(botID, nil), c.at, nil)
		if c.at.UnixMilli() == 0 {
			body = payload("Comment", "create", commentData(botID, nil), t0, obj{"webhookTimestamp": 0})
		}
		// The unsigned Linear-Timestamp header never decides: a fresh header
		// with a stale signed body is stale.
		if code, _ := h.serve(hookRequest("Comment", "d-"+name, body)); code != c.want {
			t.Errorf("%s = %d, want %d", name, code, c.want)
		}
	}
	// A body without the field is not a Linear delivery.
	var m obj
	_ = json.Unmarshal(payload("Comment", "create", commentData(botID, nil), t0, nil), &m)
	delete(m, "webhookTimestamp")
	body, _ := json.Marshal(m)
	if code, _ := h.serve(hookRequest("Comment", "d-none", body)); code != http.StatusBadRequest {
		t.Errorf("missing webhookTimestamp = %d, want 400", code)
	}
}

func TestWebhookMalformedAndIgnored(t *testing.T) {
	h := newHookRig(t)
	good := payload("Comment", "create", commentData(botID, nil), t0, nil)
	get := httptest.NewRequest(http.MethodGet, "/webhooks/linear", nil)
	if code, _ := h.serve(get); code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d, want 405", code)
	}
	noDelivery := hookRequest("Comment", "", good)
	noDelivery.Header.Del(linear.HeaderDelivery)
	if code, _ := h.serve(noDelivery); code != http.StatusBadRequest {
		t.Errorf("no delivery id = %d, want 400", code)
	}
	if code, _ := h.serve(hookRequest("Issue", "d1", good)); code != http.StatusBadRequest {
		t.Errorf("Linear-Event disagreeing with the signed type = %d, want 400", code)
	}
	notJSON := []byte("{not json")
	if code, _ := h.serve(hookRequest("Comment", "d2", notJSON)); code != http.StatusBadRequest {
		t.Errorf("signed non-JSON = %d, want 400", code)
	}
	for name, body := range map[string][]byte{
		"no id":     payload("Comment", "create", commentData("", nil), t0, nil),
		"bad id":    payload("Comment", "create", commentData("abc", nil), t0, nil),
		"no team":   payload("Comment", "create", commentData(botID, obj{"issue": obj{"id": issueA, "team": obj{}}}), t0, nil),
		"no actor":  payload("Comment", "create", commentData(botID, obj{"userId": "", "user": nil}), t0, nil),
		"bad issue": payload("Issue", "create", issueData("abc", nil), t0, nil),
	} {
		ev := "Comment"
		if strings.HasPrefix(name, "bad issue") {
			ev = "Issue"
		}
		if code, _ := h.serve(hookRequest(ev, "d-"+name, body)); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	// Events the adapter does not consume, and comments off issues, are
	// acknowledged and dropped.
	for name, c := range map[string]struct {
		event string
		body  []byte
	}{
		"project":                   {"Project", payload("Project", "create", obj{"id": issueA}, t0, nil)},
		"reaction":                  {"Reaction", payload("Reaction", "create", obj{"id": issueA}, t0, nil)},
		"issue sla":                 {"IssueSLA", payload("IssueSLA", "set", obj{"id": issueA}, t0, nil)},
		"unknown action":            {"Comment", payload("Comment", "restore", commentData(botID, nil), t0, nil)},
		"comment on project update": {"Comment", payload("Comment", "create", commentData(botID, obj{"issueId": nil, "issue": nil, "projectUpdateId": issueB}), t0, nil)},
	} {
		if got := h.post(c.event, "d-"+name, c.body); len(got) != 0 {
			t.Errorf("%s reported %d envelopes, want none", name, len(got))
		}
	}
	_ = h.inst.Close(t.Context())
	if code, _ := h.serve(hookRequest("Comment", "d3", good)); code != http.StatusServiceUnavailable {
		t.Errorf("closed instance = %d, want 503", code)
	}
}

func TestWebhookSecretUnavailable(t *testing.T) {
	h := newHookRig(t)
	h.creds.revoked = true
	if code, _ := h.serve(hookRequest("Comment", "d", payload("Comment", "create", commentData(botID, nil), t0, nil))); code != http.StatusServiceUnavailable {
		t.Fatalf("unresolvable secret = %d, want 503", code)
	}
}

func TestWebhookDedupIdentityIsTheBodyDigest(t *testing.T) {
	h := newHookRig(t)
	body := payload("Comment", "create", commentData(botID, nil), t0, nil)
	a := h.post("Comment", "delivery-1", body)
	b := h.post("Comment", "delivery-2", body) // a captured body replayed under another GUID
	other := h.post("Comment", "delivery-1", payload("Comment", "create", commentData(issueB, nil), t0, nil))
	sum := sha256.Sum256(body)
	want := "sha256:" + hex.EncodeToString(sum[:])
	if a[0].EventID != want || b[0].EventID != want || other[0].EventID == want {
		t.Fatalf("event ids %q %q %q, want the body digest %q for the same body only", a[0].EventID, b[0].EventID, other[0].EventID, want)
	}
	if got := h.sink.got[0].Correlation; got != "linear-delivery-1" {
		t.Fatalf("correlation %q, want the delivery id", got)
	}
}

func TestWebhookIssueEvents(t *testing.T) {
	h := newHookRig(t)
	wi := issueRoute(issueA)

	created := payload("Issue", "create", issueData(issueA, obj{"labels": []obj{{"name": "bug"}, {"name": "api"}}, "assigneeId": botID}), t0, nil)
	got := h.post("Issue", "c1", created)
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessage || got[0].MessageID != "issue:"+issueA || got[0].Destination != "ENG" ||
		got[0].Sender.ID != personID || got[0].Content.Parts[0].Text != "It crashes." || got[0].References[0] != wi || got[0].WorkItem == nil {
		t.Fatalf("issue create = %+v", got)
	}
	s := got[0].WorkItem
	if s.Status != model.WorkItemOpen || s.RawStatus != "started:In Progress" || s.Subject != "Export fails" || s.Requester.ID != personID ||
		len(s.Assignees) != 1 || s.Assignees[0].ID != botID || string(s.Fields[0].Data) != `["api","bug"]` || s.Binding != model.WorkItemBindingActive || s.Validate() != nil {
		t.Fatalf("state = %+v", s)
	}

	// No description: the state only.
	got = h.post("Issue", "c2", payload("Issue", "create", issueData(issueA, obj{"description": nil}), t0, nil))
	if len(got) != 1 || got[0].Kind != model.EnvelopeWorkItem || got[0].MessageID != "" {
		t.Fatalf("issue create without description = %+v", got)
	}

	// State types.
	for typ, want := range map[string]string{"triage": "open", "backlog": "open", "unstarted": "open", "started": "open", "completed": "closed", "canceled": "closed", "duplicate": "closed"} {
		d := payload("Issue", "update", issueData(issueA, obj{"state": obj{"id": "s", "name": "X", "type": typ}}), t0, obj{"updatedFrom": obj{"stateId": "s0"}})
		got = h.post("Issue", "u-"+typ, d)
		if len(got) != 1 || got[0].WorkItem.Status != want || got[0].WorkItem.RawStatus != typ+":X" {
			t.Errorf("state type %s = %+v, want %s", typ, got, want)
		}
	}

	// A description edit adds the message edit.
	got = h.post("Issue", "u1", payload("Issue", "update", issueData(issueA, obj{"description": "Now it hangs."}), t0, obj{"updatedFrom": obj{"description": "It crashes."}}))
	if len(got) != 2 || got[1].Kind != model.EnvelopeMessageEdited || got[1].MessageID != "issue:"+issueA || got[1].Content.Parts[0].Text != "Now it hangs." {
		t.Fatalf("description edit = %+v", got)
	}
	// A cleared description deletes the message.
	got = h.post("Issue", "u2", payload("Issue", "update", issueData(issueA, obj{"description": ""}), t0, obj{"updatedFrom": obj{"description": "Now it hangs."}}))
	if len(got) != 2 || got[1].Kind != model.EnvelopeMessageDeleted {
		t.Fatalf("description cleared = %+v", got)
	}

	// Removal: the binding is deleted.
	got = h.post("Issue", "r1", payload("Issue", "remove", issueData(issueA, nil), t0, nil))
	if len(got) != 1 || got[0].WorkItem.Binding != model.WorkItemBindingDeleted {
		t.Fatalf("issue remove = %+v", got)
	}
}

func TestWebhookCommentEvents(t *testing.T) {
	h := newHookRig(t)
	wi := issueRoute(issueA)
	got := h.post("Comment", "c1", payload("Comment", "create", commentData(botID, nil), t0, nil))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessage || got[0].MessageID != "comment:"+botID || got[0].Destination != "ENG" ||
		got[0].Sender.ID != personID || got[0].SenderKind != "" || got[0].Content.Parts[0].Text != "It crashes on export." ||
		got[0].References[0] != wi || got[0].WorkItem == nil || got[0].WorkItem.Subject != "Export fails" || got[0].WorkItem.Validate() != nil {
		t.Fatalf("comment create = %+v", got)
	}

	// An edit names its editor; a person's edit is no echo.
	edited := commentData(botID, obj{"body": "It crashes on CSV export.", "editedAt": "2026-10-11T11:50:00.000Z", "updatedAt": "2026-10-11T11:50:00.000Z"})
	got = h.post("Comment", "u1", payload("Comment", "update", edited, t0, obj{"updatedFrom": obj{"body": "x"}}))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessageEdited || got[0].Content.Parts[0].Text != "It crashes on CSV export." || got[0].SourceRevision != "2026-10-11T11:50:00Z" || len(got[0].References) != 1 {
		t.Fatalf("comment edit = %+v", got)
	}
	if h.sink.got[len(h.sink.got)-1].Envelope.Kind != model.EnvelopeMessageEdited {
		t.Fatal("not recorded")
	}

	got = h.post("Comment", "d1", payload("Comment", "remove", commentData(botID, nil), t0, nil))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessageDeleted || got[0].MessageID != "comment:"+botID || got[0].SourceRevision != "" {
		t.Fatalf("comment remove = %+v", got)
	}

	// A bot's comment is an automated message from a bot sender.
	got = h.post("Comment", "b1", payload("Comment", "create", commentData(issueB, obj{"userId": "", "user": nil, "botActor": `{"id":"x"}`}), t0,
		obj{"actor": obj{"id": "client-1", "type": "OauthClient", "name": "Helper"}}))
	if len(got) != 1 || got[0].Kind != model.EnvelopeAutomatedMessage || got[0].SenderKind != model.ContactKindBot || got[0].Sender.ID != "client-1" {
		t.Fatalf("bot comment = %+v", got)
	}
}

func marked(text string, op model.OperationRef, attempt model.ID) string {
	return text + "\n\n<!-- uma:" + string(op.ID) + ":" + string(attempt) + " -->"
}

func TestWebhookEchoOfOwnComments(t *testing.T) {
	h := newHookRig(t)
	echo := model.EchoRef(chanA, opA, tryOne)
	own := obj{"userId": botID, "user": obj{"id": botID, "name": "uma"}}

	// Own comment with a marker: the echo of the attempt, text without it.
	got := h.post("Comment", "c1", payload("Comment", "create", commentData(issueB, merge(own, obj{"body": marked("On it.", opA, tryOne)})), t0, obj{"actor": obj{"id": botID, "type": "user"}}))
	if len(got) != 1 || got[0].Content.Parts[0].Text != "On it." || !hasRef(got[0], echo) {
		t.Fatalf("own comment = %+v, want echo %v", got, echo)
	}
	// Someone else's comment copying the marker is no echo.
	got = h.post("Comment", "c2", payload("Comment", "create", commentData(issueB, obj{"body": marked("On it.", opA, tryOne)}), t0, nil))
	if hasRef(got[0], echo) || got[0].Content.Parts[0].Text != "On it." {
		t.Fatalf("copied marker = %+v", got[0])
	}
	// The account's own edit is the echo of the edit attempt; a person's edit
	// of the same body is not.
	body := commentData(issueB, merge(own, obj{"body": marked("Fixed.", opA, tryTwo), "editedAt": "2026-10-11T11:50:00.000Z"}))
	got = h.post("Comment", "u1", payload("Comment", "update", body, t0, obj{"actor": obj{"id": botID, "type": "user"}}))
	if len(got) != 1 || !hasRef(got[0], model.EchoRef(chanA, opA, tryTwo)) || got[0].Content.Parts[0].Text != "Fixed." {
		t.Fatalf("own edit = %+v", got)
	}
	got = h.post("Comment", "u2", payload("Comment", "update", body, t0, nil))
	if hasRef(got[0], model.EchoRef(chanA, opA, tryTwo)) {
		t.Fatalf("a person's edit carries the echo: %+v", got[0])
	}
	// An application token's edit names the application as actor: the
	// comment's author, the account, still marks it as uma's own.
	got = h.post("Comment", "u3", payload("Comment", "update", body, t0, obj{"actor": obj{"id": "client-1", "type": "OauthClient"}}))
	if !hasRef(got[0], model.EchoRef(chanA, opA, tryTwo)) {
		t.Fatalf("application edit = %+v, want the echo", got[0])
	}
}

func merge(a, b obj) obj {
	out := obj{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func hasRef(e model.Envelope, ref model.ExternalRef) bool {
	for _, r := range e.References {
		if r == ref {
			return true
		}
	}
	return false
}

func TestWebhookSinkFailures(t *testing.T) {
	body := payload("Comment", "create", commentData(botID, nil), t0, nil)
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"retryable": {model.NewError(model.Transient, "db"), http.StatusServiceUnavailable},
		"final":     {model.NewError(model.Validation, "no"), http.StatusUnprocessableEntity},
	} {
		h := newHookRig(t)
		h.sink.refuse = func(model.Ingress) error { return c.err }
		if code, _ := h.serve(hookRequest("Comment", "d", body)); code != c.want {
			t.Errorf("%s = %d, want %d", name, code, c.want)
		}
	}
	// An edit of an unknown message records the message instead; a deletion
	// of one is dropped.
	h := newHookRig(t)
	h.sink.refuse = func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited || in.Envelope.Kind == model.EnvelopeMessageDeleted {
			return model.NewError(model.NotFound, "unknown message")
		}
		return nil
	}
	edit := payload("Comment", "update", commentData(botID, nil), t0, nil)
	if code, _ := h.serve(hookRequest("Comment", "e", edit)); code != http.StatusOK {
		t.Fatalf("edit of an unknown message = %d, want 200", code)
	}
	if got := h.sink.envelopes(); len(got) != 1 || got[0].Kind != model.EnvelopeMessage {
		t.Fatalf("recorded %+v, want the message itself", got)
	}
	if code, _ := h.serve(hookRequest("Comment", "r", payload("Comment", "remove", commentData(botID, nil), t0, nil))); code != http.StatusOK {
		t.Fatalf("deletion of an unknown message = %d, want 200", code)
	}
}

// emptySecretHost resolves an empty webhook secret: anyone could sign with
// it, so no delivery verifies.
type emptySecretHost struct{ keyHost }

func (h *emptySecretHost) Resolve(ctx context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose == linear.CredentialPurposeWebhook {
		return ports.Secret{}, nil
	}
	return h.keyHost.Resolve(ctx, r)
}

func TestWebhookEmptySecretVerifiesNothing(t *testing.T) {
	h := newHookRig(t)
	h.creds = nil
	inst, err := linear.NewFactory(linear.Options{}).Open(t.Context(), model.Channel{
		Ref: chanA, Type: linear.Type, Revision: "ch1", CredentialRef: webhookSecretID, Configuration: linear.Config{}.Extension(),
	}, adapter.Dependencies{Credentials: &emptySecretHost{}, Clock: h.clock, HTTP: h.lin})
	if err != nil {
		t.Fatal(err)
	}
	h.h = inst.(adapter.HTTPReceiver).Handler(h.sink)
	body := payload("Comment", "create", commentData(botID, nil), t0, nil)
	r := hookRequest("Comment", "d", body)
	r.Header.Set(linear.HeaderSignature, sign("", body))
	if code, _ := h.serve(r); code != http.StatusServiceUnavailable || len(h.sink.envelopes()) != 0 {
		t.Fatalf("empty secret = %d, want 503 and nothing accepted", code)
	}
}
