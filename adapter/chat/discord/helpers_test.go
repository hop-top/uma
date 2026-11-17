package discord

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	testToken                    = "bot-token-0123456789" // #nosec G101 -- fixture value
	testApp                      = "1100000000000000001"
	testBotUser                  = "1100000000000000002"
	testGuild                    = "1100000000000000003"
	testRoom                     = "1100000000000000010"
	testThread                   = "1100000000000000011"
	testPerson                   = "1100000000000000020"
	testOther                    = "1100000000000000021"
	testChannelID model.ID       = "00000ad0-0000-7000-8000-00000000000a"
	testOp        model.ID       = "00000ad0-0000-7000-8000-0000000000f1"
	testAttempt   model.ID       = "00000ad0-0000-7000-8000-0000000000f2"
)

var testNow = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

var testChannel = model.ChannelRef{Tenant: testTenant, ID: testChannelID}

// testKey is the application's key pair: Discord signs with the private
// key, the adapter verifies with the public one.
var testKey = ed25519.NewKeyFromSeed(sha256Sum("discord test application key"))

func sha256Sum(s string) []byte {
	b := sha256.Sum256([]byte(s))
	return b[:]
}

func publicKeyHex(k ed25519.PrivateKey) string {
	return hex.EncodeToString(k.Public().(ed25519.PublicKey))
}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// credentials answers the bot token and the public key.
type credentials struct {
	mu       sync.Mutex
	token    string
	key      string
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
	case CredentialPurposeAPI:
		return ports.Secret{Bytes: []byte(c.token)}, nil
	case CredentialPurposeInteractions:
		return ports.Secret{Bytes: []byte(c.key)}, nil
	}
	return nil, model.NewError(model.Unsupported, "unknown purpose")
}

func newCredentials() *credentials {
	return &credentials{token: testToken, key: publicKeyHex(testKey)}
}

func testConfig() Config {
	return Config{ApplicationID: testApp, BotUserID: testBotUser, GatewayURL: "wss://" + fakeGatewayHost}
}

// sinkRecorder is the engine's sink: it records what it accepts, answers
// what its script says, and keeps the receiver cursor.
type sinkRecorder struct {
	mu     sync.Mutex
	got    []model.Ingress
	fail   func(model.Ingress) error
	cursor string
	// hold, when set, blocks each Accept until it is closed.
	hold chan struct{}
}

func (s *sinkRecorder) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	hold := s.hold
	s.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return model.IngressReceipt{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		if err := s.fail(in); err != nil {
			return model.IngressReceipt{}, err
		}
	}
	if it := in.Envelope.Interaction; it != nil {
		if err := it.Validate(); err != nil {
			return model.IngressReceipt{}, err
		}
	}
	s.got = append(s.got, in)
	if in.ReceiverCursor != "" {
		s.cursor = in.ReceiverCursor
	}
	return model.IngressReceipt{Committed: true, Durable: true}, nil
}

func (s *sinkRecorder) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, nil
}

func (s *sinkRecorder) ReceiverCursor(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursor, nil
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

func (s *sinkRecorder) ingresses() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.got...)
}

var (
	_ adapter.Sink       = (*sinkRecorder)(nil)
	_ adapter.CursorSink = (*sinkRecorder)(nil)
)

// rig is an instance wired to a fake Discord.
type rig struct {
	f     *fakeDiscord
	acct  *fakeAccount
	x     *Instance
	creds *credentials
	cfg   Config
	clock *fixedClock
}

func newRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	clock := &fixedClock{testNow}
	f := newFakeDiscord(func() time.Time { return clock.t })
	t.Cleanup(f.close)
	acct := f.account("acct1", testToken, testBotUser, testApp)
	cfg := testConfig()
	cfg.APIURL = f.apiURL("acct1")
	for _, m := range mod {
		m(&cfg)
	}
	creds := newCredentials()
	inst, err := NewFactory().Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "discord",
	}, adapter.Dependencies{Credentials: creds, HTTP: f.client(), Clock: clock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return &rig{f: f, acct: acct, x: inst.(*Instance), creds: creds, cfg: cfg, clock: clock}
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

// callsOf returns the account's calls whose method and path match.
func (r *rig) callsOf(method, pathPrefix string) []fakeCall {
	var out []fakeCall
	for _, c := range r.f.calls("acct1") {
		if c.Method == method && len(c.Path) >= len(pathPrefix) && c.Path[:len(pathPrefix)] == pathPrefix {
			out = append(out, c)
		}
	}
	return out
}

func (c fakeCall) args(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(c.Body, &m); err != nil {
		t.Fatalf("call %s %s body %q: %v", c.Method, c.Path, c.Body, err)
	}
	return m
}

// msgRef is the provider message reference of a message in channel.
func msgRef(channel, id string) *model.ExternalRef {
	r := model.ProviderMessageRef(testChannel, messageID(channel, id))
	return &r
}

// sid is the snowflake of t plus n.
func sid(t time.Time, n uint64) string {
	base, _ := strconv.ParseUint(snowflakeAt(t), 10, 64)
	return strconvU(base + n)
}

func strconvU(n uint64) string { return strconv.FormatUint(n, 10) }

type (
	portsRequest    = ports.CredentialRequest
	portsCredential = ports.Credential
)

// credentialFunc is a credential port answering with f.
type credentialFunc func(ports.CredentialRequest) (ports.Credential, error)

func (f credentialFunc) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	return f(r)
}

func secretOf(s string) ports.Credential { return ports.Secret{Bytes: []byte(s)} }
