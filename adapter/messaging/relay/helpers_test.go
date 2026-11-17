package relay_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

const testTenant model.TenantID = "relay-tenant"

var testChannel = model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0a1"}

var testNow = time.Unix(1_760_000_000, 0).UTC()

// webhookSecret and callbackSecret are fixture values, not credentials.
const (
	webhookSecret  = "relay-webhook-secret-0123456789abcdef"  // #nosec G101
	callbackSecret = "relay-callback-secret-fedcba9876543210" // #nosec G101
)

type clock struct{ t time.Time }

func (c clock) Now() time.Time { return c.t }

// creds resolves one fixed secret per (reference, purpose) and records every
// request.
type creds struct {
	mu     sync.Mutex
	values map[string]ports.Credential // by purpose, or reference + "|" + purpose
	reqs   []ports.CredentialRequest
	fail   error
}

func newCreds() *creds {
	return &creds{values: map[string]ports.Credential{
		relay.PurposeWebhook:  ports.Secret{Bytes: []byte(webhookSecret)},
		relay.PurposeCallback: ports.Secret{Bytes: []byte(callbackSecret)},
	}}
}

func (c *creds) set(key string, v ports.Credential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = v
}

func (c *creds) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r)
	if c.fail != nil {
		return nil, c.fail
	}
	if v, ok := c.values[r.Reference+"|"+r.Purpose]; ok {
		return v, nil
	}
	if v, ok := c.values[r.Purpose]; ok {
		return v, nil
	}
	return nil, model.NewError(model.Unauthorized, "no credential for purpose")
}

func (c *creds) requests() []ports.CredentialRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ports.CredentialRequest(nil), c.reqs...)
}

func channelOf(cfg relay.Config) model.Channel {
	return model.Channel{
		Ref: testChannel, Type: model.TypeRelayMessages, CredentialRef: "relay-account", Revision: "ch1",
		Configuration: cfg.Extension(),
	}
}

func open(t *testing.T, f *relay.Factory, cfg relay.Config, deps adapter.Dependencies) *relay.Instance {
	t.Helper()
	if deps.Clock == nil {
		deps.Clock = clock{testNow}
	}
	inst, err := f.Open(t.Context(), channelOf(cfg), deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.(*relay.Instance)
}

func secretOf(s string) ports.Credential { return ports.Secret{Bytes: []byte(s)} }

// good2 authenticates a bearer request with secret.
func good2(secret string) call { return call{body: hello, header: bearer(secret)} }

func depsWith(c *creds) adapter.Dependencies {
	return adapter.Dependencies{Credentials: c, Clock: clock{testNow}}
}

func oauthOf(tok string) ports.Credential { return ports.OAuth2{AccessToken: tok} }

func passwordOf(user, secret string) ports.Credential {
	return ports.Password{User: user, Secret: []byte(secret)}
}
