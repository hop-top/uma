package github_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

const tenant model.TenantID = "github-tenant"

const (
	botID           int64  = 41898282
	sandboxRepo     string = "example/uma-sandbox"
	apiToken        string = "ghs_fixtureInstallationToken0000000000" // #nosec G101 -- fixture value, not a credential
	webhookSecretID string = "github-cred"                            // #nosec G101 -- credential reference name
)

var (
	chanA  = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c0a1"}
	opA    = model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000a0a1"}
	tryOne = model.ID("00000000-0000-7000-8000-00000000b0b1")
	tryTwo = model.ID("00000000-0000-7000-8000-00000000b0b2")
	t0     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
)

// tokenHost resolves the API token and the webhook secret and records each
// request.
type tokenHost struct {
	mu      sync.Mutex
	reqs    []ports.CredentialRequest
	revoked bool
	kind    ports.CredentialKind
}

func (h *tokenHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	switch {
	case h.revoked:
		return nil, model.NewError(model.Unauthorized, "consent revoked")
	case r.Purpose == github.CredentialPurposeWebhook:
		return ports.Secret{Bytes: []byte(hookSecret)}, nil
	case h.kind == ports.KindOAuth2:
		return ports.OAuth2{AccessToken: apiToken, ExpiresAt: t0.Add(time.Hour)}, nil
	}
	return ports.Secret{Bytes: []byte(apiToken)}, nil
}

func (h *tokenHost) fresh() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.reqs {
		if r.Fresh {
			n++
		}
	}
	return n
}

type sendRig struct {
	t     *testing.T
	gh    *fakeGitHub
	clock *portstest.FixedClock
	creds *tokenHost
	inst  adapter.Instance
}

func replyConfig(api string) github.Config {
	return github.Config{APIURL: api, Replies: true, AccountID: botID}
}

func newSendRig(t *testing.T) *sendRig {
	t.Helper()
	clock := portstest.NewFixedClock(t0)
	r := &sendRig{t: t, clock: clock, creds: &tokenHost{}}
	r.gh = newFakeGitHub(t, clock.Now)
	r.gh.account("a", botID)
	r.inst = r.open(replyConfig(r.gh.apiURL("a")))
	return r
}

func (r *sendRig) open(cfg github.Config) adapter.Instance {
	r.t.Helper()
	f := github.NewFactory(github.Options{})
	inst, err := f.Open(r.t.Context(), model.Channel{
		Ref: chanA, Type: github.Type, Configuration: cfg.Extension(), CredentialRef: webhookSecretID, Revision: "ch1",
	}, adapter.Dependencies{Credentials: r.creds, Clock: r.clock, HTTP: r.gh.client()})
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	r.t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func issueRoute(n string) model.ExternalRef {
	return model.ExternalRef{Channel: chanA, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: n, ParentID: sandboxRepo}
}

func sendReq(attempt model.ID, text string) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: opA, Attempt: attempt, Route: issueRoute("7"),
			Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

func (r *sendRig) send(attempt model.ID) (adapter.Outcome, error) {
	return r.inst.(adapter.Sender).Send(r.t.Context(), sendReq(attempt, "Thanks, looking into it."))
}

func (r *sendRig) reconcile(attempt model.ID) (adapter.Outcome, error) {
	e := sendReq(attempt, "").Effect
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{Effect: e})
}

func (r *sendRig) posts() []fakeCall {
	var out []fakeCall
	for _, c := range r.gh.calls("a") {
		if c.Method == http.MethodPost {
			out = append(out, c)
		}
	}
	return out
}

// wantCode fails unless err is a *model.Error with code, and returns a
// copy of it.
func wantCode(t *testing.T, err error, code model.ErrorCode) model.Error {
	t.Helper()
	me, ok := err.(*model.Error)
	if !ok || me.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
	return *me
}

func TestSendPostsCommentWithMarkerAndReportsItsID(t *testing.T) {
	r := newSendRig(t)
	out, err := r.send(tryOne)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	stored := r.gh.stored("a", sandboxRepo+"#7")
	if len(stored) != 1 {
		t.Fatalf("stored comments = %d, want 1", len(stored))
	}
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != commentRef(chanA, stored[0].ID) {
		t.Fatalf("outcome = %+v, want provider_accepted with the comment reference", out)
	}
	posts := r.posts()
	if posts[0].Path != "/repos/example/uma-sandbox/issues/7/comments" || posts[0].Auth != "Bearer "+apiToken {
		t.Fatalf("post = %+v", posts[0])
	}
	body := stored[0].Body
	if !strings.HasPrefix(body, "Thanks, looking into it.\n\n<!-- uma:") || !strings.Contains(body, string(opA.ID)+":"+string(tryOne)) {
		t.Fatalf("body = %q, want the text and the attempt marker", body)
	}
	req := r.creds.reqs[0]
	if req.Purpose != github.CredentialPurposeAPI || !req.Accepts(ports.KindOAuth2) || !req.Accepts(ports.KindSecret) {
		t.Fatalf("credential request = %+v", req)
	}
}

func TestSendAcceptsInstallationToken(t *testing.T) {
	r := newSendRig(t)
	r.creds.kind = ports.KindOAuth2
	if out, err := r.send(tryOne); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	r.clock.Advance(2 * time.Hour) // past the token expiry: re-resolved fresh, still expired
	_, err := r.send(tryTwo)
	wantCode(t, err, model.Unauthorized)
	if r.creds.fresh() != 1 {
		t.Fatalf("fresh resolutions = %d, want 1", r.creds.fresh())
	}
	if n := len(r.posts()); n != 1 {
		t.Fatalf("posts = %d, want 1: an expired token is never sent", n)
	}
}

func TestSendRefusesWhatGitHubCannotCarry(t *testing.T) {
	r := newSendRig(t)
	cases := map[string]adapter.SendRequest{}
	private := sendReq(tryOne, "x")
	private.Audience.Visibility = model.Internal
	cases["internal audience"] = private
	foreign := sendReq(tryOne, "x")
	foreign.Route.Channel.ID = "00000000-0000-7000-8000-00000000c0b2"
	cases["other channel route"] = foreign
	pathy := sendReq(tryOne, "x")
	pathy.Route.ParentID = "example/../admin"
	cases["path in repository"] = pathy
	attach := sendReq(tryOne, "x")
	attach.Content.Parts = append(attach.Content.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: "org.example.card", Version: 1}})
	cases["structured part"] = attach
	long := sendReq(tryOne, strings.Repeat("a", 65536))
	cases["body over the limit"] = long
	for name, req := range cases {
		_, err := r.inst.(adapter.Sender).Send(t.Context(), req)
		if me, ok := err.(*model.Error); !ok || me.Retry != model.NoRetry {
			t.Errorf("%s: error = %v, want a final error", name, err)
		}
	}
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
}

func TestSendOutcomeMapping(t *testing.T) {
	reset := t0.Add(17 * time.Minute)
	for _, tc := range []struct {
		name    string
		answer  answer
		state   model.DeliveryState
		binding string
		code    model.ErrorCode
		retry   model.RetryAdvice
		wait    time.Duration
		applied bool
	}{
		{name: "422 rejected", answer: statusAnswer(422, `{"message":"Validation Failed","documentation_url":"`+docsURL+`","status":"422"}`, nil), state: model.Rejected, code: model.Validation, retry: model.NoRetry},
		{name: "403 forbidden", answer: statusAnswer(403, `{"message":"Resource not accessible by integration","documentation_url":"`+docsURL+`","status":"403"}`, nil), code: model.Unauthorized, retry: model.NoRetry},
		{name: "404 inaccessible", answer: statusAnswer(404, `{"message":"Not Found","documentation_url":"`+docsURL+`","status":"404"}`, nil), state: model.Rejected, binding: model.WorkItemBindingInaccessible, code: model.NotFound, retry: model.NoRetry},
		{name: "410 deleted", answer: statusAnswer(410, `{"message":"This issue was deleted","documentation_url":"`+docsURL+`","status":"410"}`, nil), state: model.Rejected, binding: model.WorkItemBindingDeleted, code: model.NotFound, retry: model.NoRetry},
		{name: "301 moved", answer: statusAnswer(301, `{"message":"Moved Permanently","url":"https://api.github.com/repositories/1/issues/7/comments"}`, map[string]string{"Location": "https://api.github.com/repositories/1/issues/7/comments"}), code: model.NotFound, retry: model.NoRetry},
		{name: "403 primary rate limit", answer: statusAnswer(403, `{"message":"API rate limit exceeded for installation ID 1.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`, map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa(reset.Unix()),
		}), code: model.RateLimited, retry: model.RetrySameOperation, wait: 17 * time.Minute},
		{name: "403 secondary rate limit", answer: statusAnswer(403, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again.","documentation_url":"https://docs.github.com/free-pro-team@latest/rest/overview/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`, map[string]string{
			"Retry-After": "90",
		}), code: model.RateLimited, retry: model.RetrySameOperation, wait: 90 * time.Second},
		{name: "429 without wait", answer: statusAnswer(429, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`, nil), code: model.RateLimited, retry: model.RetrySameOperation, wait: time.Minute},
		{name: "429 plain with Retry-After", answer: statusAnswer(429, `{"message":"Too Many Requests"}`, map[string]string{"Retry-After": "30"}), code: model.RateLimited, retry: model.RetrySameOperation, wait: 30 * time.Second},
		{name: "403 plain with Retry-After", answer: statusAnswer(403, `{"message":"Forbidden"}`, map[string]string{"Retry-After": "45"}), code: model.RateLimited, retry: model.RetrySameOperation, wait: 45 * time.Second},
		{name: "500 unknown", answer: statusAnswer(500, `{"message":"Server Error"}`, nil), code: model.OutcomeUnknown, retry: model.ReconcileFirst},
		{name: "502 unknown", answer: statusAnswer(502, `{"message":"Server Error"}`, nil), code: model.OutcomeUnknown, retry: model.ReconcileFirst},
		{name: "503 unknown", answer: statusAnswer(503, `{"message":"Service Unavailable"}`, nil), code: model.OutcomeUnknown, retry: model.ReconcileFirst},
		{name: "answer lost", answer: answerLost, code: model.OutcomeUnknown, retry: model.ReconcileFirst, applied: true},
		{name: "request lost after write", answer: requestLost, code: model.OutcomeUnknown, retry: model.ReconcileFirst},
		{name: "unreachable", answer: unreachable, code: model.Transient, retry: model.RetrySameOperation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newSendRig(t)
			r.gh.respond("a", tc.answer)
			out, err := r.send(tryOne)
			if tc.state != "" {
				if err != nil || out.State != tc.state || out.Binding != tc.binding || out.Failure == nil || out.Failure.Code != tc.code || out.Failure.Retry != tc.retry {
					t.Fatalf("Send = %+v, %v; want %s %s", out, err, tc.state, tc.code)
				}
			} else {
				me := wantCode(t, err, tc.code)
				if me.Retry != tc.retry || me.RetryAfter != tc.wait {
					t.Fatalf("error = %+v, want retry %s after %v", me, tc.retry, tc.wait)
				}
				if strings.Contains(me.Error(), apiToken) {
					t.Fatal("error discloses the token")
				}
			}
			if posts := r.posts(); len(posts) != 1 || posts[0].Applied != tc.applied {
				t.Fatalf("posts = %+v, want exactly one, applied %v", posts, tc.applied)
			}
		})
	}
}

func TestSendRateLimitShortCircuitDispatchesNothing(t *testing.T) {
	r := newSendRig(t)
	// Reset far in wall-clock future: go-github refuses locally until then.
	reset := time.Now().Add(time.Hour).Unix()
	r.gh.respond("a", statusAnswer(403, `{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`, map[string]string{
		"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa(reset),
	}))
	_, err := r.send(tryOne)
	wantCode(t, err, model.RateLimited)
	_, err = r.send(tryTwo)
	if me := wantCode(t, err, model.RateLimited); me.RetryAfter <= 0 {
		t.Fatalf("short-circuit retry after = %v, want the reset wait", me.RetryAfter)
	}
	if n := len(r.posts()); n != 1 {
		t.Fatalf("posts = %d, want 1: the second send never left the client", n)
	}
}

func TestSendRefreshesCredentialOnce(t *testing.T) {
	r := newSendRig(t)
	unauthorized := statusAnswer(401, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest","status":"401"}`, nil)
	r.gh.respond("a", unauthorized)
	if out, err := r.send(tryOne); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send after one 401 = %+v, %v; want accepted on the fresh token", out, err)
	}
	r.gh.respond("a", unauthorized, unauthorized)
	_, err := r.send(tryTwo)
	wantCode(t, err, model.Unauthorized)
	if r.creds.fresh() != 2 || len(r.posts()) != 4 {
		t.Fatalf("fresh = %d posts = %d, want 2 and 4", r.creds.fresh(), len(r.posts()))
	}
}

func TestReconcileFindsTheAttemptsComment(t *testing.T) {
	r := newSendRig(t)
	issue := sandboxRepo + "#7"
	// Noise: a stranger copying uma's marker, and uma's comment for
	// another attempt; then pages of unrelated comments.
	r.gh.seed("a", issue, 99, "copied\n\n<!-- uma:"+string(opA.ID)+":"+string(tryOne)+" -->", t0)
	r.gh.respond("a", answerLost)
	if _, err := r.send(tryTwo); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Send = %v, want outcome unknown", err)
	}
	for range 150 {
		r.gh.seed("a", issue, 99, "+1", t0)
	}
	r.gh.respond("a", answerLost)
	if _, err := r.send(tryOne); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Send = %v, want outcome unknown", err)
	}
	stored := r.gh.stored("a", issue)
	want := stored[len(stored)-1]
	out, err := r.reconcile(tryOne)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != commentRef(chanA, want.ID) {
		t.Fatalf("Reconcile = %+v, %v; want the account's comment %d on page 2", out, err, want.ID)
	}
	gets := 0
	for _, c := range r.gh.calls("a") {
		if c.Method == http.MethodGet {
			gets++
			if !strings.Contains(c.Query, "since=2026-10-04T12%3A00%3A00Z") || !strings.Contains(c.Query, "per_page=100") {
				t.Errorf("list query = %q, want since the reconcile window and 100 per page", c.Query)
			}
		}
	}
	if gets != 2 {
		t.Fatalf("list calls = %d, want 2 pages", gets)
	}
}

func TestReconcileWithoutCommentAllowsRetry(t *testing.T) {
	r := newSendRig(t)
	r.gh.respond("a", requestLost)
	if _, err := r.send(tryOne); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Send = %v", err)
	}
	if n := len(r.posts()); n != 1 {
		t.Fatalf("posts before reconcile = %d, want 1: never a second create before reconciling", n)
	}
	out, err := r.reconcile(tryOne)
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile = %+v, %v; want failed not_found", out, err)
	}
}

func TestReconcileInaccessibleIssue(t *testing.T) {
	r := newSendRig(t)
	cfg := replyConfig(r.gh.apiURL("missing"))
	inst := r.open(cfg)
	_, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: sendReq(tryOne, "").Effect})
	wantCode(t, err, model.NotFound)
}

func TestRepliesDisabledByDefault(t *testing.T) {
	r := newSendRig(t)
	inst := r.open(github.Config{APIURL: r.gh.apiURL("a")})
	st, err := inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		// Refetch only reads, and work item acts (with their
		// reconciliation) post no comment: they need no replies. Sending,
		// editing, deleting and reacting do.
		switch s.Name {
		case model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact:
			if s.Available || s.Reason != model.Disabled {
				t.Errorf("capability %+v, want disabled without replies", s)
			}
		default:
			if !s.Available {
				t.Errorf("capability %+v, want available without replies", s)
			}
		}
	}
	if _, err := inst.(adapter.Sender).Send(t.Context(), sendReq(tryOne, "x")); model.CodeOf(err) != model.Disabled {
		t.Fatalf("Send with replies disabled = %v, want disabled", err)
	}
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
}

func TestNoHTTPClientIsUnavailable(t *testing.T) {
	inst, err := github.NewFactory(github.Options{}).Open(t.Context(), model.Channel{
		Ref: chanA, Type: github.Type, Configuration: replyConfig("").Extension(), CredentialRef: "c", Revision: "1",
	}, adapter.Dependencies{Credentials: &tokenHost{}, Clock: portstest.NewFixedClock(t0)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inst.(adapter.Sender).Send(t.Context(), sendReq(tryOne, "x"))
	wantCode(t, err, model.Unavailable)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
