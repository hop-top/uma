package teams

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
	// testAppID, testTenantID and testToken are fixture values.
	testAppID                = "00000000-0000-4000-8000-0000000000a1"
	testTenantID             = "00000000-0000-4000-8000-0000000000e1"
	testToken                = "connector-token-not-real" // #nosec G101 -- fixture value
	testBot                  = "28:" + testAppID
	testUser                 = "29:1user-alice"
	testOther                = "29:1user-bob"
	testChannelID   model.ID = "00000ad0-0000-7000-8000-00000000000a"
	testOp          model.ID = "00000ad0-0000-7000-8000-0000000000f1"
	testAttempt     model.ID = "00000ad0-0000-7000-8000-0000000000f2"
	testPrefix               = "acct"
	testGroupChat            = "19:group-chat-1@thread.v2"
	testTeamChannel          = "19:general-1@thread.tacv2"
	testPersonal             = "a:1personal-alice"
)

var testNow = time.Unix(1_700_000_000, 0).UTC()

var testChannel = model.ChannelRef{Tenant: testTenant, ID: testChannelID}

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// credentials answers the Bot Connector token and records what was asked.
type credentials struct {
	mu       sync.Mutex
	token    string
	expired  bool
	err      error
	requests []ports.CredentialRequest
}

func newCredentials() *credentials { return &credentials{token: testToken} }

func (c *credentials) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests = append(c.requests, r)
	if c.err != nil {
		return nil, c.err
	}
	if r.Purpose != CredentialPurposeConnector {
		return nil, model.NewError(model.Unsupported, "unknown purpose")
	}
	o := ports.OAuth2{AccessToken: c.token}
	if c.expired && !r.Fresh {
		o.ExpiresAt = testNow.Add(-time.Minute)
	}
	return o, nil
}

func (c *credentials) asked() []ports.CredentialRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.CredentialRequest(nil), c.requests...)
}

func testConfig(f *fakeTeams) Config {
	return Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: f.serviceURL(testPrefix)}
}

// rig is an instance over the stand-in Bot Framework.
type rig struct {
	clock *fixedClock
	f     *fakeTeams
	x     *Instance
	creds *credentials
	sink  *sinkRecorder
}

func newRig(t *testing.T, mod ...func(*adapter.Dependencies)) *rig {
	t.Helper()
	clock := &fixedClock{testNow}
	f := newFakeTeams(clock.Now)
	f.account(testPrefix, testBot, testTenantID, testToken)
	creds := newCredentials()
	deps := adapter.Dependencies{Credentials: creds, HTTP: f.client(), Clock: clock}
	for _, m := range mod {
		m(&deps)
	}
	inst, err := NewFactory(Options{OpenIDMetadataURL: fakeMetadataURL}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: testConfig(f).Extension(), CredentialRef: "teams",
	}, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return &rig{clock: clock, f: f, x: inst.(*Instance), creds: creds, sink: &sinkRecorder{}}
}

// post sends act to the instance's handler with a valid token, adjusted
// by mod, and returns the recorder.
func (r *rig) post(t *testing.T, act map[string]any, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(act)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.f.token(testAppID, r.f.serviceURL(testPrefix)))
	for _, m := range mod {
		m(req)
	}
	rec := httptest.NewRecorder()
	r.x.Handler(r.sink).ServeHTTP(rec, req)
	return rec
}

// activityOf is a Teams activity of type kind from from in conversation
// conv, as the Bot Connector posts it.
func (r *rig) activityOf(kind, id, from, conv, convType string) map[string]any {
	return map[string]any{
		"type": kind, "id": id, "timestamp": testNow.Format(time.RFC3339Nano), "serviceUrl": r.f.serviceURL(testPrefix),
		"channelId": "msteams", "from": map[string]any{"id": from, "name": "Alice", "aadObjectId": "aad-alice"},
		"recipient":    map[string]any{"id": testBot, "name": "uma"},
		"conversation": map[string]any{"id": conv, "conversationType": convType, "tenantId": testTenantID},
		"channelData":  map[string]any{"tenant": map[string]any{"id": testTenantID}},
	}
}

func (r *rig) effect(route model.ExternalRef) adapter.Effect {
	return adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: route, Audience: model.Audience{Visibility: model.Public},
	}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

// sinkRecorder is the engine's sink: it records what it accepts, and
// answers what its script says.
type sinkRecorder struct {
	mu   sync.Mutex
	got  []model.Ingress
	fail func(model.Ingress) error
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

func (s *sinkRecorder) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
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
