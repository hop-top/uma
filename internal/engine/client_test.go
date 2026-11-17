package engine_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

func conversationRef(t model.TenantID, inbox model.ID) model.Ref {
	return model.Ref{Tenant: t, Kind: model.KindConversation, Inbox: inbox, ID: enginetest.ConvOne}
}

func readConversation(r model.Ref) policy.Authorization {
	return policy.Authorization{Action: model.ActionConversationRead, Resource: r}
}

func newClient(t *testing.T, cfg engine.Config) *engine.Client {
	t.Helper()
	c, err := enginetest.MustNew(t, cfg).For(enginetest.PrincipalA())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// switchable is an Authorizer whose verdict the test changes between calls.
type switchable struct {
	ok    atomic.Bool
	Calls atomic.Int32
	seen  atomic.Pointer[policy.Authorization]
}

func (s *switchable) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	s.Calls.Add(1)
	s.seen.Store(&a)
	return policy.Decision{Allow: s.ok.Load()}, nil
}

func TestForValidatesPrincipal(t *testing.T) {
	e := enginetest.MustNew(t, enginetest.ValidConfig())
	for _, p := range []model.Principal{{}, {Tenant: enginetest.TenantA}, {Actor: enginetest.Actor}} {
		c, err := e.For(p)
		if c != nil {
			t.Fatalf("For(%+v) returned a client", p)
		}
		enginetest.RequireCode(t, err, model.Validation, "")
	}
}

// A client holds references, not grants: For consults no policy, and each
// operation consults the policy as it is at that moment.
func TestClientConsultsCurrentPolicy(t *testing.T) {
	auth := &switchable{}
	auth.ok.Store(true)
	cfg := enginetest.ValidConfig()
	cfg.Authorizer = auth
	c := newClient(t, cfg)
	if n := auth.Calls.Load(); n != 0 {
		t.Fatalf("For consulted policy %d times", n)
	}
	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	if err := c.Authorize(context.Background(), readConversation(r), r); err != nil {
		t.Fatalf("allowed: %v", err)
	}
	auth.ok.Store(false)
	enginetest.RequireCode(t, c.Authorize(context.Background(), readConversation(r), r), model.Unauthorized, "")
	if n := auth.Calls.Load(); n != 2 {
		t.Fatalf("policy calls = %d, want 2", n)
	}
}

// The policy sees the client's trusted principal, whatever the request says.
func TestAuthorizeUsesClientPrincipal(t *testing.T) {
	auth := &switchable{}
	auth.ok.Store(true)
	cfg := enginetest.ValidConfig()
	cfg.Authorizer = auth
	c := newClient(t, cfg)
	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	req := readConversation(r)
	req.Principal = model.Principal{Tenant: enginetest.TenantB, Actor: "forged"}
	if err := c.Authorize(context.Background(), req, r); err != nil {
		t.Fatal(err)
	}
	if got := auth.seen.Load().Principal; got != enginetest.PrincipalA() {
		t.Fatalf("policy saw principal %+v", got)
	}
}

func TestAuthorizeDeniesWithoutExplicitAllow(t *testing.T) {
	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	cases := []struct {
		name string
		auth policy.AuthorizeFunc
		code model.ErrorCode
	}{
		{"zero Decision", func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{}, nil
		}, model.Unauthorized},
		{"allow with error", func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, errors.New("policy backend: secret detail")
		}, model.Unavailable},
		{"typed error", func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, model.NewError(model.RateLimited, "slow down")
		}, model.RateLimited},
		{"invalid typed error", func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, &model.Error{Code: "bogus", SafeMessage: "secret detail"}
		}, model.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enginetest.ValidConfig()
			cfg.Authorizer = tc.auth
			err := newClient(t, cfg).Authorize(context.Background(), readConversation(r), r)
			enginetest.RequireCode(t, err, tc.code, "")
			if strings.Contains(err.Error(), "secret detail") {
				t.Fatalf("policy error text leaked: %v", err)
			}
		})
	}
}

func TestAuthorizeKeepsContextErrors(t *testing.T) {
	cfg := enginetest.ValidConfig()
	cfg.Authorizer = policy.AuthorizeFunc(func(ctx context.Context, _ policy.Authorization) (policy.Decision, error) {
		return policy.Decision{Allow: true}, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	if err := newClient(t, cfg).Authorize(ctx, readConversation(r), r); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// Tenant and inbox ownership hold even when the policy allows everything.
// Foreign and mismatched references are never confirmed (not_found), and
// the policy is not asked about them.
func TestAuthorizeEnforcesOwnershipWhenPolicyAllows(t *testing.T) {
	own := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	foreignDest := model.Ref{Tenant: enginetest.TenantB, Kind: model.KindInbox, ID: enginetest.InboxTwo}
	cases := []struct {
		name  string
		req   policy.Authorization
		owner model.Ref
	}{
		{"foreign tenant", readConversation(conversationRef(enginetest.TenantB, enginetest.InboxOne)), conversationRef(enginetest.TenantB, enginetest.InboxOne)},
		{"stored in another tenant", readConversation(own), conversationRef(enginetest.TenantB, enginetest.InboxOne)},
		{"stored in another inbox", readConversation(own), conversationRef(enginetest.TenantA, enginetest.InboxTwo)},
		{"stored as another kind", readConversation(own), model.Ref{Tenant: enginetest.TenantA, Kind: model.KindAttachment, Inbox: enginetest.InboxOne, ID: enginetest.ConvOne}},
		{"foreign destination", policy.Authorization{
			Action: model.ActionConversationTransition, Resource: own, Destination: &foreignDest,
		}, own},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			auth := &enginetest.Allow{}
			cfg := enginetest.ValidConfig()
			cfg.Authorizer = auth
			enginetest.RequireCode(t, newClient(t, cfg).Authorize(context.Background(), tc.req, tc.owner), model.NotFound, "")
			if n := auth.Calls.Load(); n != 0 {
				t.Fatalf("policy consulted %d times for a foreign reference", n)
			}
		})
	}

	auth := &enginetest.Allow{}
	cfg := enginetest.ValidConfig()
	cfg.Authorizer = auth
	enginetest.RequireCode(t, newClient(t, cfg).Authorize(context.Background(),
		readConversation(model.Ref{Tenant: enginetest.TenantA, Kind: "bogus"}), model.Ref{Tenant: enginetest.TenantA, Kind: "bogus"}),
		model.Validation, "")
}

// Missing optional policies deny the operations that need them; there is
// no implicit default.
func TestMissingPoliciesDeny(t *testing.T) {
	e := enginetest.MustNew(t, enginetest.ValidConfig())
	_, err := e.IdentityVerifier("attestation")
	enginetest.RequireCode(t, err, model.Unsupported, "identity verifier not registered")
	_, err = e.ConversationSelector("default")
	enginetest.RequireCode(t, err, model.Unauthorized, "no conversation selector")
	_, err = e.InteractionValidator()
	enginetest.RequireCode(t, err, model.Unauthorized, "no interaction validator")

	cfg := enginetest.ValidConfig()
	cfg.IdentityVerifiers = map[string]policy.IdentityVerifier{"attestation": enginetest.OwnedVerifier{}}
	cfg.Selectors = map[string]policy.ConversationSelector{"default": enginetest.OwnedSelector{}}
	cfg.InteractionValidator = enginetest.OwnedValidator{}
	e = enginetest.MustNew(t, cfg)
	if _, err := e.IdentityVerifier("attestation"); err != nil {
		t.Error(err)
	}
	if _, err := e.ConversationSelector("default"); err != nil {
		t.Error(err)
	}
	if _, err := e.InteractionValidator(); err != nil {
		t.Error(err)
	}
}

// Decide returns the policy's decision, provenance included, under the same
// scope rules as Authorize: a foreign resource or destination is not found
// before the policy is asked, and a policy error is no decision.
func TestDecideReturnsDecisionUnderAuthorizeScope(t *testing.T) {
	r := conversationRef(enginetest.TenantA, enginetest.InboxOne)
	var calls atomic.Int32
	cfg := enginetest.ValidConfig()
	cfg.Authorizer = policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
		calls.Add(1)
		if a.Action == "fail" {
			return policy.Decision{Allow: true}, errors.New("backend down")
		}
		return policy.Decision{Allow: a.Action == model.ActionConversationRead, PolicyVersion: "v7", ReasonCode: "rc"}, nil
	})
	c := newClient(t, cfg)
	ctx := context.Background()
	d, err := c.Decide(ctx, readConversation(r), r)
	if err != nil || d != (policy.Decision{Allow: true, PolicyVersion: "v7", ReasonCode: "rc"}) {
		t.Fatalf("Decide = %+v, %v", d, err)
	}
	deny := readConversation(r)
	deny.Action = "other"
	if d, err := c.Decide(ctx, deny, r); err != nil || d.Allow || d.PolicyVersion != "v7" {
		t.Fatalf("denying Decide = %+v, %v", d, err)
	}
	enginetest.RequireCode(t, c.Authorize(ctx, deny, r), model.Unauthorized, "denied")
	failing := readConversation(r)
	failing.Action = "fail"
	if d, err := c.Decide(ctx, failing, r); !model.IsCode(err, model.Unavailable) || d.Allow {
		t.Fatalf("failing Decide = %+v, %v", d, err)
	}
	n := calls.Load()
	foreign := conversationRef(enginetest.TenantB, enginetest.InboxOne)
	_, err = c.Decide(ctx, readConversation(foreign), foreign)
	enginetest.RequireCode(t, err, model.NotFound, "")
	dest := model.Ref{Tenant: enginetest.TenantB, Kind: model.KindContact, ID: "c"}
	toForeign := readConversation(r)
	toForeign.Destination = &dest
	_, err = c.Decide(ctx, toForeign, r)
	enginetest.RequireCode(t, err, model.NotFound, "")
	if calls.Load() != n {
		t.Fatal("policy asked about an out-of-scope request")
	}
}
