package google

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

var testChan = model.ChannelRef{Tenant: "t1", ID: "00000000-0000-7000-8000-0000000000c1"}

const (
	testMailbox = "desk@example.test"
	testPeer    = "peer@remote.test"
	testTopic   = "projects/uma-test/topics/gmail-push"
	testAud     = "https://hooks.example.test/gmail"
	testPushSA  = "push@uma-test.iam.gserviceaccount.com"
)

var testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// testCreds is a host credential port: OAuth2 tokens for PurposeGmail
// ("tok-1", then "tok-fresh" once Fresh is asked).
type testCreds struct {
	mu   sync.Mutex
	reqs []ports.CredentialRequest
	err  error
}

func (c *testCreds) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
	if c.err != nil {
		return nil, c.err
	}
	if r.Fresh {
		return ports.OAuth2{AccessToken: "tok-fresh"}, nil
	}
	return ports.OAuth2{AccessToken: "tok-1"}, nil
}

func (c *testCreds) requests() []ports.CredentialRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.CredentialRequest(nil), c.reqs...)
}

// testSink records accepted ingress; refuse, when set, refuses some.
type testSink struct {
	mu     sync.Mutex
	got    []model.Ingress
	refuse func(model.Ingress) error
}

func (s *testSink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
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

func (s *testSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "no interactions")
}

func (s *testSink) ids() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, in := range s.got {
		if in.Envelope.Kind != model.EnvelopeConversationState {
			out = append(out, in.Envelope.MessageID)
		}
	}
	return out
}

// env is one opened test instance over a fake Gmail.
type env struct {
	g     *fakeGmail
	f     *Factory
	inst  *instance
	creds *testCreds
	clock *portstest.FixedClock
	box   *fakeBox
}

type envOption func(*Config, *adapter.Dependencies)

func withProtocol(p adapter.ProtocolState) envOption {
	return func(_ *Config, d *adapter.Dependencies) { d.Protocol = p }
}

// pushConfig configures push notifications.
func pushConfig(c *Config, _ *adapter.Dependencies) {
	c.Topic, c.PushAudience, c.PushServiceAccount = testTopic, testAud, testPushSA
}

func newEnv(t *testing.T, cfg Config, opts ...envOption) *env {
	t.Helper()
	g := newFakeGmail(t)
	if cfg.User == "" {
		cfg.User = testMailbox
	}
	e := &env{
		g: g, creds: &testCreds{}, clock: portstest.NewFixedClock(testNow),
		box: g.box(cfg.User, testChan),
	}
	g.now = e.clock.Now
	deps := adapter.Dependencies{Credentials: e.creds, Clock: e.clock, HTTP: g.client()}
	for _, o := range opts {
		o(&cfg, &deps)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.f = NewFactory(Options{Endpoint: g.endpoint(), CertsURL: g.certsURL(), PageSize: 2, Jitter: func() float64 { return 0.5 }})
	inst, err := e.f.Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "cred-1",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	e.inst = inst.(*instance)
	return e
}

// inbound delivers a plain message from peer.
func (e *env) inbound(id, peer, text string, extra ...string) {
	e.g.deliver(e.box.user, id, "thread-"+id, rawMessage(peer, testMailbox, "<"+id+"@remote.test>", "hello", text, extra...))
}

func route(peer, parent string) model.ExternalRef {
	return model.ExternalRef{Channel: testChan, Namespace: IdentityNamespace, Kind: refKindAddress, ID: peer, ParentID: parent}
}

func effect(r model.ExternalRef, attempt model.ID) adapter.Effect {
	return adapter.Effect{
		Operation: model.OperationRef{Tenant: testChan.Tenant, ID: "00000000-0000-7000-8000-0000000000e1"},
		Attempt:   attempt, Route: r, Audience: model.Audience{Visibility: model.Public},
	}
}

func textContent(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func wantCode(t *testing.T, err error, code model.ErrorCode) *model.Error {
	t.Helper()
	if model.CodeOf(err) != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	me, _ := err.(*model.Error)
	return me
}

// claims are push token claims; zero fields take valid defaults.
type claims map[string]any

func (e *env) validClaims() claims {
	return claims{
		"iss": "https://accounts.google.com", "aud": testAud, "email": testPushSA, "email_verified": true,
		"iat": e.clock.Now().Add(-time.Minute).Unix(), "exp": e.clock.Now().Add(59 * time.Minute).Unix(),
		"sub": "113774264463038321964", "azp": "113774264463038321964",
	}
}

// signToken signs c with key under kid, as Google signs push tokens.
func signToken(t *testing.T, key *rsa.PrivateKey, kid string, c claims) string {
	t.Helper()
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	body := enc(c)
	sum := sha256.Sum256([]byte(head + "." + body))
	sig, err := rsa.SignPKCS1v15(nil, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// pushBody is a Pub/Sub push body carrying a Gmail notification.
func pushBody(email string, historyID int64) string {
	data, _ := json.Marshal(map[string]string{"emailAddress": email, "historyId": strconv.FormatInt(historyID, 10)})
	b, _ := json.Marshal(map[string]any{
		"message": map[string]any{
			"data": base64.StdEncoding.EncodeToString(data), "messageId": "2070443601311540", "publishTime": "2026-10-11T12:00:00.000Z",
		},
		"subscription": "projects/uma-test/subscriptions/gmail-push",
	})
	return string(b)
}

// memProtocol is an in-memory adapter.ProtocolState.
type memProtocol struct {
	mu    sync.Mutex
	ext   map[string]model.Extension
	rev   map[string]model.Revision
	saves int
}

func (p *memProtocol) Load(_ context.Context, key string) (model.Extension, model.Revision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.ext[key]
	if !ok {
		return model.Extension{}, "", model.NewError(model.NotFound, "no state")
	}
	return e, p.rev[key], nil
}

func (p *memProtocol) CompareAndSwap(_ context.Context, key string, expected model.Revision, e model.Extension) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ext == nil {
		p.ext, p.rev = map[string]model.Extension{}, map[string]model.Revision{}
	}
	if p.rev[key] != expected {
		return model.NewError(model.Conflict, "stale revision")
	}
	p.saves++
	p.ext[key], p.rev[key] = e, model.Revision("r"+strconv.Itoa(p.saves))
	return nil
}

func contextWithCancel(t *testing.T) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	return ctx, cancel
}

// waitFor polls cond for up to five seconds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met")
		}
		time.Sleep(time.Millisecond)
	}
}
