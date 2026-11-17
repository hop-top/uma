package gitlab_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

const tenant model.TenantID = "gitlab-tenant"

const (
	botID           int64  = 9100001
	sandboxProject  int64  = 700000001
	sandboxPath     string = "example/uma-sandbox"
	apiToken        string = "glpat-fixtureToken000000000" // #nosec G101 -- fixture value, not a credential
	hookToken       string = "webhook-fixture-token"       // #nosec G101 -- fixture value, not a credential
	webhookSecretID string = "gitlab-cred"                 // #nosec G101 -- credential reference name
)

var (
	chanA  = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c0a1"}
	opA    = model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000a0a1"}
	tryOne = model.ID("00000000-0000-7000-8000-00000000b0b1")
	tryTwo = model.ID("00000000-0000-7000-8000-00000000b0b2")
	t0     = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
)

// tokenHost resolves the API token and the webhook token and records each
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
	case r.Purpose == gitlab.CredentialPurposeWebhook:
		return ports.Secret{Bytes: []byte(hookToken)}, nil
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

type rig struct {
	t     *testing.T
	gl    *fakeGitLab
	clock *portstest.FixedClock
	creds *tokenHost
	inst  adapter.Instance
}

func replyConfig(api string) gitlab.Config {
	return gitlab.Config{APIURL: api, Replies: true, AccountID: botID}
}

func newRig(t *testing.T) *rig {
	t.Helper()
	clock := portstest.NewFixedClock(t0)
	r := &rig{t: t, clock: clock, creds: &tokenHost{}}
	r.gl = newFakeGitLab(t, clock.Now)
	r.gl.account("a", botID)
	r.gl.setIssue("a", sandboxProject, 7, 5000001, "opened", t0.Add(-48*time.Hour))
	r.inst = r.open(replyConfig(r.gl.apiURL("a")))
	return r
}

func (r *rig) open(cfg gitlab.Config) adapter.Instance {
	r.t.Helper()
	inst, err := gitlab.NewFactory(gitlab.Options{}).Open(r.t.Context(), model.Channel{
		Ref: chanA, Type: gitlab.Type, Configuration: cfg.Extension(), CredentialRef: webhookSecretID, Revision: "ch1",
	}, adapter.Dependencies{Credentials: r.creds, Clock: r.clock, HTTP: r.gl.client()})
	if err != nil {
		r.t.Fatalf("Open: %v", err)
	}
	r.t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func issueRoute(iid string) model.ExternalRef {
	return model.ExternalRef{
		Channel: chanA, Namespace: gitlab.Namespace, Kind: model.RefKindWorkItem,
		ID: iid, ParentID: strconv.FormatInt(sandboxProject, 10),
	}
}

func effect(attempt model.ID, route model.ExternalRef) adapter.Effect {
	return adapter.Effect{Operation: opA, Attempt: attempt, Route: route, Audience: model.Audience{Visibility: model.Public}}
}

func textOf(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func sendReq(attempt model.ID, text string) adapter.SendRequest {
	return adapter.SendRequest{Effect: effect(attempt, issueRoute("7")), Content: textOf(text)}
}

// wantCode fails unless err is a *model.Error with code, and returns a copy
// of it.
func wantCode(t *testing.T, err error, code model.ErrorCode) model.Error {
	t.Helper()
	me, ok := err.(*model.Error)
	if !ok || me.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
	return *me
}

func noteRef(id int64) model.ExternalRef {
	return model.ProviderMessageRef(chanA, "note:"+strconv.FormatInt(id, 10))
}
