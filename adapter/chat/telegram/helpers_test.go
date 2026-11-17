package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const (
	testTenant model.TenantID = "tenant-1"
	// testSecret is the webhook's secret token and testToken the bot token;
	// both are fixture values.
	testSecret             = "webhook-secret_0123456789-abcdef" // #nosec G101 -- fixture value
	testToken              = "7000001:AAtestTokenNotReal"       // #nosec G101 -- fixture value
	testBotID     int64    = 7000001
	testBotName            = "uma_test_bot"
	testChannelID model.ID = "00000ad0-0000-7000-8000-00000000000a"
	testOp        model.ID = "00000ad0-0000-7000-8000-0000000000f1"
	testAttempt   model.ID = "00000ad0-0000-7000-8000-0000000000f2"
)

var testNow = time.Unix(1_700_000_000, 0).UTC()

var testChannel = model.ChannelRef{Tenant: testTenant, ID: testChannelID}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// credentials answers the webhook secret token and the bot token, and
// records what was asked.
type credentials struct {
	mu       sync.Mutex
	secret   string
	token    string
	err      error
	requests []ports.CredentialRequest
}

func newCredentials() *credentials { return &credentials{secret: testSecret, token: testToken} }

func (c *credentials) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, r)
	if c.err != nil {
		return nil, c.err
	}
	switch r.Purpose {
	case CredentialPurposeWebhook:
		return ports.Secret{Bytes: []byte(c.secret)}, nil
	case CredentialPurposeAPI:
		return ports.Secret{Bytes: []byte(c.token)}, nil
	}
	return nil, model.NewError(model.Unsupported, "unknown purpose")
}

func (c *credentials) purposes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.requests {
		out = append(out, r.Purpose)
	}
	return out
}

func testConfig() Config {
	return Config{BotID: testBotID, BotUsername: testBotName}
}

// open returns an instance of cfg with the deps adjusted by mod.
func open(t *testing.T, cfg Config, mod ...func(*adapter.Dependencies)) *Instance {
	t.Helper()
	deps := adapter.Dependencies{Credentials: newCredentials(), Clock: &fixedClock{testNow}}
	for _, m := range mod {
		m(&deps)
	}
	inst, err := NewFactory(Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "telegram",
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

// post is one request to a handler, authenticated unless secret is "-".
type post struct {
	body        []byte
	contentType string
	secret      string
	method      string
	mod         func(*http.Request)
}

func jsonPost(v any) post {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return post{body: b, contentType: "application/json", secret: testSecret, method: http.MethodPost}
}

func (p post) send(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(p.method, "/telegram", bytes.NewReader(p.body))
	if p.contentType != "" {
		req.Header.Set("Content-Type", p.contentType)
	}
	if p.secret != "-" {
		req.Header.Set(HeaderSecretToken, p.secret)
	}
	if p.mod != nil {
		p.mod(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Update shapes, as the Bot API documents Update and Message
// (https://core.telegram.org/bots/api#update).
type obj = map[string]any

const (
	testPerson      int64 = 424242
	testGroup       int64 = -1001234567890
	testChannelChat int64 = -1009876543210
)

func privateChat(user int64) obj { return obj{"id": user, "type": "private", "first_name": "Pat"} }
func groupChat() obj             { return obj{"id": testGroup, "type": "supergroup", "title": "team"} }
func person(id int64) obj        { return obj{"id": id, "is_bot": false, "first_name": "Pat"} }

// textMessage is a message of user in chat; extra overrides or, with a nil
// value, removes fields.
func textMessage(chat obj, from obj, id int64, text string, extra obj) obj {
	m := obj{"message_id": id, "from": from, "chat": chat, "date": testNow.Unix() + id, "text": text}
	for k, v := range extra {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

func updateOf(id int64, field string, v obj) obj { return obj{"update_id": id, field: v} }

type adapterDeps = adapter.Dependencies

func cancelled(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx, cancel
}

// rig is an instance wired to a fake bot.
type rig struct {
	f     *fakeTelegram
	acct  *fakeAccount
	x     *Instance
	creds *credentials
	cfg   Config
}

func newRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	f := newFakeTelegram(func() time.Time { return testNow })
	acct := f.account("acct1", testBotID, testToken)
	cfg := testConfig()
	cfg.APIURL = f.apiURL("acct1")
	for _, m := range mod {
		m(&cfg)
	}
	creds := newCredentials()
	inst, err := NewFactory(Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "telegram",
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

func (r *rig) callsOf(method string) []fakeCall { return r.f.callsOf("acct1", method) }

type (
	contextT = context.Context
	credReq  = ports.CredentialRequest
	credT    = ports.Credential
)

type credentialFunc func(context.Context, ports.CredentialRequest) (ports.Credential, error)

func (f credentialFunc) Resolve(ctx context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	return f(ctx, r)
}

func secretOf(s string) ports.Credential { return ports.Secret{Bytes: []byte(s)} }

func jsonBytes(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
