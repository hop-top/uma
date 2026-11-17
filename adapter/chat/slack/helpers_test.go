package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const (
	testTenant    model.TenantID = "tenant-1"
	testSecret                   = "signing-secret-0123456789abcdef" // #nosec G101 -- fixture value
	testToken                    = "xoxb-test-token"                 // #nosec G101 -- fixture value
	testAppToken                 = "xapp-1-A0APP-1-test"             // #nosec G101 -- fixture value
	testTeam                     = "T0001"
	testApp                      = "A0APP"
	testBotUser                  = "U0BOT"
	testBotID                    = "B0BOT"
	testRoom                     = "C0ROOM"
	testPerson                   = "U0PERSON"
	testTS                       = "1700000000.000100"
	testChannelID model.ID       = "00000ad0-0000-7000-8000-00000000000a"
)

var testNow = time.Unix(1_700_000_000, 0).UTC()

var testChannel = model.ChannelRef{Tenant: testTenant, ID: testChannelID}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// credentials answers the signing secret, the bot token and the app-level
// token.
type credentials struct {
	mu       sync.Mutex
	secret   string
	token    string
	appToken string
	err      error
	requests []ports.CredentialRequest
}

func (c *credentials) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, r)
	if c.err != nil {
		return nil, c.err
	}
	switch r.Purpose {
	case CredentialPurposeSigning:
		return ports.Secret{Bytes: []byte(c.secret)}, nil
	case CredentialPurposeAPI:
		return ports.Secret{Bytes: []byte(c.token)}, nil
	case CredentialPurposeSocket:
		return ports.Secret{Bytes: []byte(c.appToken)}, nil
	}
	return nil, model.NewError(model.Unsupported, "unknown purpose")
}

func newCredentials() *credentials {
	return &credentials{secret: testSecret, token: testToken, appToken: testAppToken}
}

func testConfig() Config {
	return Config{TeamID: testTeam, BotUserID: testBotUser, BotID: testBotID, AppID: testApp}
}

// open returns an instance of cfg with the deps adjusted by mod.
func open(t *testing.T, cfg Config, mod ...func(*adapter.Dependencies)) *Instance {
	t.Helper()
	deps := adapter.Dependencies{Credentials: newCredentials(), Clock: &fixedClock{testNow}}
	for _, m := range mod {
		m(&deps)
	}
	inst, err := NewFactory(Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "slack",
	}, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return inst.(*Instance)
}

// sinkRecorder is the engine's sink: it records what it accepts, and
// answers what its script says.
type sinkRecorder struct {
	mu    sync.Mutex
	got   []model.Ingress
	fail  func(model.Ingress) error
	eval  func(model.InteractionInput) (model.InteractionDecision, error)
	evals []model.InteractionInput
}

func (s *sinkRecorder) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		if err := s.fail(in); err != nil {
			return model.IngressReceipt{}, err
		}
	}
	s.got = append(s.got, in)
	return model.IngressReceipt{Committed: true, Durable: true}, nil
}

func (s *sinkRecorder) EvaluateInteraction(_ context.Context, in model.InteractionInput) (model.InteractionDecision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evals = append(s.evals, in)
	if s.eval != nil {
		return s.eval(in)
	}
	return model.InteractionDecision{}, nil
}

func (s *sinkRecorder) envelopes() []model.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Envelope
	for _, in := range s.got {
		out = append(out, in.Envelope)
	}
	return out
}

var _ adapter.Sink = (*sinkRecorder)(nil)

// request is one signed request to a handler.
type request struct {
	contentType string
	body        []byte
	at          time.Time
	secret      string
	headers     map[string]string
	method      string
}

func jsonRequest(v any) request {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return request{contentType: "application/json", body: b, at: testNow, secret: testSecret, method: http.MethodPost}
}

func (r request) send(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(r.method, "/slack", bytes.NewReader(r.body))
	req.Header.Set("Content-Type", r.contentType)
	ts := strconv.FormatInt(r.at.Unix(), 10)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, sign(r.secret, ts, r.body))
	for k, v := range r.headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// callback wraps an inner event in the Events API envelope
// (https://docs.slack.dev/apis/events-api/).
func callback(eventID string, event map[string]any) map[string]any {
	return map[string]any{
		"type": "event_callback", "team_id": testTeam, "api_app_id": testApp,
		"event_id": eventID, "event_time": testNow.Unix(), "event": event,
		"authorizations": []any{map[string]any{"team_id": testTeam, "user_id": testBotUser, "is_bot": true}},
	}
}

func messageEvent(user, text, ts string, extra map[string]any) map[string]any {
	e := map[string]any{"type": "message", "channel": testRoom, "channel_type": "channel", "user": user, "text": text, "ts": ts, "event_ts": ts}
	for k, v := range extra {
		if v == nil {
			delete(e, k)
		} else {
			e[k] = v
		}
	}
	return e
}

type adapterDeps = adapter.Dependencies

const (
	testOp      model.ID = "00000ad0-0000-7000-8000-0000000000f1"
	testAttempt model.ID = "00000ad0-0000-7000-8000-0000000000f2"
)

// rig is an instance wired to a fake workspace.
type rig struct {
	f     *fakeSlack
	acct  *fakeAccount
	x     *Instance
	creds *credentials
	cfg   Config
}

func newRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	f := newFakeSlack(func() time.Time { return testNow })
	acct := f.account("acct1", testTeam, testBotUser, testToken)
	cfg := testConfig()
	cfg.APIURL = f.apiURL("acct1")
	for _, m := range mod {
		m(&cfg)
	}
	creds := newCredentials()
	inst, err := NewFactory(Options{ResponseBaseURL: f.responseBase()}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "slack",
	}, adapter.Dependencies{Credentials: creds, HTTP: f.client(), Clock: &fixedClock{testNow}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return &rig{f: f, acct: acct, x: inst.(*Instance), creds: creds, cfg: cfg}
}

// effect is an effect on route, public, of the standard operation.
func (r *rig) effect(route model.ExternalRef) adapter.Effect {
	return adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: route, Audience: model.Audience{Visibility: model.Public},
	}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

// wantCalls returns the account's calls of method.
func (r *rig) callsOf(method string) []fakeCall {
	var out []fakeCall
	for _, c := range r.f.calls("acct1") {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (c fakeCall) args(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("call %s body %q: %v", c.Method, c.Body, err)
	}
	return m
}

type credentialFunc func(context.Context, ports.CredentialRequest) (ports.Credential, error)

func (f credentialFunc) Resolve(ctx context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	return f(ctx, r)
}

// rewriteClient passes requests through and lets fn rewrite an answer.
type rewriteClient struct {
	next ports.HTTPClient
	fn   func(req *http.Request, body []byte) []byte
}

func (c rewriteClient) Do(r *http.Request) (*http.Response, error) {
	res, err := c.next.Do(r)
	if err != nil {
		return nil, err
	}
	b, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	res.Body = io.NopCloser(bytes.NewReader(c.fn(r, b)))
	return res, nil
}
