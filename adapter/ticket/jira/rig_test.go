package jira_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

const tenant model.TenantID = "jira-tenant"

const (
	host       = "example.atlassian.net"
	hookSecret = "webhook-fixture-secret" // #nosec G101 -- fixture value, not a credential
	issueID    = "10001"
)

var (
	chanA  = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c0a1"}
	opA    = model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000a0a1"}
	tryOne = model.ID("00000000-0000-7000-8000-00000000b0b1")
	tryTwo = model.ID("00000000-0000-7000-8000-00000000b0b2")
	t0     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
)

// credHost resolves the credential of a purpose: the webhook secret or
// cred for the API, and counts requests made with Fresh set.
type credHost struct {
	mu   sync.Mutex
	cred ports.Credential
	reqs []ports.CredentialRequest
}

func (h *credHost) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if r.Purpose == jira.CredentialPurposeWebhook {
		return ports.Secret{Bytes: []byte(hookSecret)}, nil
	}
	return h.cred, nil
}

func (h *credHost) fresh() int {
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
	j     *fakeJira
	a     *fakeAccount
	clock *portstest.FixedClock
	creds *credHost
	inst  adapter.Instance
}

func replyConfig() jira.Config {
	return jira.Config{SiteURL: "https://" + host, Replies: true, AccountID: ownAccount}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clock := portstest.NewFixedClock(t0)
	r := &rig{t: t, clock: clock, creds: &credHost{cred: ports.Password{User: "bot@example.com", Secret: []byte("api-token")}}}
	r.j = newFakeJira(t, clock.Now)
	r.a = r.j.account(host)
	r.j.putIssue(r.a, issueID, "SBX-1", "new")
	r.inst = r.open(replyConfig())
	return r
}

func (r *rig) open(cfg jira.Config) adapter.Instance {
	r.t.Helper()
	inst, err := jira.NewFactory().Open(r.t.Context(), model.Channel{
		Ref: chanA, Type: jira.Type, Configuration: cfg.Extension(), CredentialRef: "jira-cred", Revision: "ch1",
	}, adapter.Dependencies{Credentials: r.creds, Clock: r.clock, HTTP: r.j.client()})
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func route(id string) model.ExternalRef {
	return model.ExternalRef{Channel: chanA, Namespace: jira.Namespace, Kind: model.RefKindWorkItem, ID: id}
}

func commentRef(id string) model.ExternalRef { return model.ProviderMessageRef(chanA, "comment:"+id) }

func effect(attempt model.ID) adapter.Effect {
	return adapter.Effect{Operation: opA, Attempt: attempt, Route: route(issueID), Audience: model.Audience{Visibility: model.Public}}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func (r *rig) send(attempt model.ID, body string) (adapter.Outcome, error) {
	return r.inst.(adapter.Sender).Send(r.t.Context(), adapter.SendRequest{Effect: effect(attempt), Content: text(body)})
}

func (r *rig) reconcile(attempt model.ID) (adapter.Outcome, error) {
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{Effect: effect(attempt), Capability: model.CapabilitySend})
}

func (r *rig) posts() []fakeCall {
	var out []fakeCall
	for _, c := range r.j.callsOf(r.a) {
		if c.Method == http.MethodPost {
			out = append(out, c)
		}
	}
	return out
}

// target returns the effect of a mutation of comment id.
func (r *rig) target(attempt model.ID, id string) adapter.Effect {
	e := effect(attempt)
	ref := commentRef(id)
	e.Target = &ref
	return e
}
