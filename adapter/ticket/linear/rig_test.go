package linear_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	linear "hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

const tenant model.TenantID = "linear-tenant"

const (
	// botID is the Linear user the fixture credential acts as; issueA and
	// issueB are fixture issue UUIDs.
	botID           = "7c1f3a52-0d4e-4b8a-9a61-2f5d8e0b1c11"
	issueA          = "3b9e6d12-4c57-4f0a-8d21-6a7b5c4d3e01"
	issueB          = "3b9e6d12-4c57-4f0a-8d21-6a7b5c4d3e02"
	personID        = "9d2c8b41-6e13-4a7f-b5c0-1e8f7a6d5b20"
	apiKey          = "lin_api_fixtureKey000000000000000000" // #nosec G101 -- fixture value, not a credential
	webhookSecretID = "linear-cred"                          // #nosec G101 -- credential reference name
	hookSecret      = "linear-webhook-fixture-secret"        // #nosec G101 -- fixture value, not a credential
)

var (
	chanA  = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c0a1"}
	opA    = model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000a0a1"}
	tryOne = model.ID("00000000-0000-7000-8000-00000000b0b1")
	tryTwo = model.ID("00000000-0000-7000-8000-00000000b0b2")
	t0     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
)

// keyHost resolves the API credential and the webhook secret and records
// each request. kind selects a personal API key (the default) or an OAuth
// access token.
type keyHost struct {
	mu      sync.Mutex
	reqs    []ports.CredentialRequest
	revoked bool
	kind    ports.CredentialKind
}

func (h *keyHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	switch {
	case h.revoked:
		return nil, model.NewError(model.Unauthorized, "consent revoked")
	case r.Purpose == linear.CredentialPurposeWebhook:
		return ports.Secret{Bytes: []byte(hookSecret)}, nil
	case h.kind == ports.KindOAuth2:
		return ports.OAuth2{AccessToken: apiKey, ExpiresAt: t0.Add(time.Hour)}, nil
	}
	return ports.Secret{Bytes: []byte(apiKey)}, nil
}

func (h *keyHost) fresh() int {
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

type rig struct {
	t     *testing.T
	lin   *fakeLinear
	clock *portstest.FixedClock
	creds *keyHost
	inst  adapter.Instance
}

func replyConfig(api string) linear.Config {
	return linear.Config{APIURL: api, Replies: true, AccountID: botID}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clock := portstest.NewFixedClock(t0)
	r := &rig{t: t, clock: clock, creds: &keyHost{}, lin: newFakeLinear(clock.Now)}
	r.lin.account("a", botID)
	r.lin.putIssue("a", fakeIssue{ID: issueA, Identifier: "ENG-7", Title: "Export fails", Updated: t0})
	r.inst = r.open(replyConfig(r.lin.apiURL("a")))
	return r
}

func (r *rig) open(cfg linear.Config) adapter.Instance {
	r.t.Helper()
	inst, err := linear.NewFactory(linear.Options{}).Open(r.t.Context(), model.Channel{
		Ref: chanA, Type: linear.Type, Revision: "ch1", CredentialRef: webhookSecretID, Configuration: cfg.Extension(),
	}, adapter.Dependencies{Credentials: r.creds, Clock: r.clock, HTTP: r.lin})
	if err != nil {
		r.t.Fatal(err)
	}
	return inst
}

func issueRoute(id string) model.ExternalRef {
	return model.ExternalRef{Channel: chanA, Namespace: linear.Namespace, Kind: model.RefKindWorkItem, ID: id}
}

func commentRef(ch model.ChannelRef, id string) model.ExternalRef {
	return model.ProviderMessageRef(ch, "comment:"+id)
}

func sendReq(attempt model.ID, text string) adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: opA, Attempt: attempt, Route: issueRoute(issueA),
			Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

func (r *rig) send(attempt model.ID) (adapter.Outcome, error) {
	return r.inst.(adapter.Sender).Send(r.t.Context(), sendReq(attempt, "Thanks, looking into it."))
}

func (r *rig) reconcile(attempt model.ID) (adapter.Outcome, error) {
	e := sendReq(attempt, "").Effect
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilitySend})
}

// target is the effect of a mutation of comment id.
func target(attempt model.ID, id string) adapter.Effect {
	e := sendReq(attempt, "").Effect
	ref := commentRef(chanA, id)
	e.Target = &ref
	return e
}
