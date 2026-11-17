package services_test

import (
	"context"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

const (
	deskActor     model.ActorID = "desk-1"
	externalActor model.ActorID = "external-1"
)

// deskPolicy grants the rig's actor everything, a desk actor everything
// but channel.read, and an external participant conversation.read at the
// public audience only, as a helpdesk-style host would.
type deskPolicy struct {
	mu     sync.Mutex
	checks []policy.Authorization
}

func (p *deskPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	p.checks = append(p.checks, a)
	p.mu.Unlock()
	switch a.Principal.Actor {
	case deskActor:
		return policy.Decision{Allow: a.Action != model.ActionChannelRead}, nil
	case externalActor:
		public := a.Audience.Visibility == "" || a.Audience.Visibility == model.Public
		return policy.Decision{Allow: a.Action == model.ActionConversationRead && public}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (p *deskPolicy) asked(actor model.ActorID) []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []policy.Authorization
	for _, a := range p.checks {
		if a.Principal.Actor == actor {
			out = append(out, a)
		}
	}
	return out
}

func withPolicy(p policy.Authorizer) enginetest.RigOption {
	return func(cfg *engine.Config, _ *enginetest.OutFactory) { cfg.Authorizer = p }
}

func clientAs(t *testing.T, r *rig, actor model.ActorID) enginetest.Client {
	t.Helper()
	c, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Conversations.Capabilities reports the capability statuses of the
// conversation's channel to a desk actor that holds conversation.read but
// not channel.read, with the reason a capability the instance disables
// carries, and the same statuses Configuration.Capabilities reports.
func TestConversationCapabilitiesDeskActor(t *testing.T) {
	p := &deskPolicy{}
	r := newRig(t, enginetest.WithMutations, withPolicy(p))
	r.F.Withdraw(model.CapabilityEdit, model.Disabled)
	desk := clientAs(t, r, deskActor)
	lookup := model.Lookup[model.ConversationRef]{Call: call(), Ref: r.Conv.Ref}
	got, err := desk.Conversations().Capabilities(r.Ctx, lookup)
	if err != nil {
		t.Fatalf("Capabilities as desk: %v", err)
	}
	want, err := r.C.Configuration().Capabilities(r.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: r.Ch.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(got, want, func(a, b model.CapabilityStatus) bool {
		return a.Name == b.Name && a.Available == b.Available && a.Reason == b.Reason && a.Limits.Name == b.Limits.Name
	}) {
		t.Fatalf("conversation statuses %+v, want the channel's %+v", got, want)
	}
	i := slices.IndexFunc(got, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityEdit })
	if i < 0 || got[i].Available || got[i].Reason != model.Disabled {
		t.Fatalf("edit status %+v, want unavailable (disabled)", got)
	}
	if _, err := desk.Configuration().Capabilities(r.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: r.Ch.Ref}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("Configuration.Capabilities as desk: %v, want unauthorized", err)
	}
	asked := p.asked(deskActor)
	j := slices.IndexFunc(asked, func(a policy.Authorization) bool { return a.Action == model.ActionConversationRead })
	if j < 0 || asked[j].Resource != r.Conv.Ref.Ref() || asked[j].Audience.Visibility != model.Internal || asked[j].Revision != r.Conv.Revision {
		t.Fatalf("decisions %+v, want conversation.read on the conversation at the internal audience", asked)
	}
}

// An external participant that may read the conversation's public content
// may not read its capability statuses, and no adapter instance opens for
// it.
func TestConversationCapabilitiesExternalRefused(t *testing.T) {
	p := &deskPolicy{}
	r := newRig(t, enginetest.WithMutations, withPolicy(p))
	ext := clientAs(t, r, externalActor)
	if _, err := ext.Conversations().Get(r.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: r.Conv.Ref}); err != nil {
		t.Fatalf("external Get: %v", err)
	}
	opens := r.F.Opens
	_, err := ext.Conversations().Capabilities(r.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: r.Conv.Ref})
	if !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("external Capabilities: %v, want unauthorized", err)
	}
	if r.F.Opens != opens {
		t.Fatalf("an instance opened for a refused caller")
	}
}

// A conversation in another tenant, or under another inbox than the
// reference names, is not found; a malformed reference is a validation
// error.
func TestConversationCapabilitiesScope(t *testing.T) {
	r := newRig(t)
	foreign := r.Conv.Ref
	foreign.Inbox.Tenant = tenantB
	moved := r.Conv.Ref
	moved.Inbox.ID = inboxTwo
	for name, ref := range map[string]model.ConversationRef{"foreign tenant": foreign, "other inbox": moved} {
		_, err := r.C.Conversations().Capabilities(r.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: ref})
		if !model.IsCode(err, model.NotFound) {
			t.Fatalf("%s: %v, want not found", name, err)
		}
	}
	_, err := r.C.Conversations().Capabilities(r.Ctx, model.Lookup[model.ConversationRef]{Call: call()})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("empty ref: %v, want validation", err)
	}
}

// An adapter instance that fails to open fails the read with its error,
// as Configuration.Capabilities does.
func TestConversationCapabilitiesOpenFailure(t *testing.T) {
	r := newRig(t)
	r.F.FailOpen(model.NewError(model.Unavailable, "provider down"))
	_, err := r.C.Conversations().Capabilities(r.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: r.Conv.Ref})
	if !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Capabilities with a failing adapter: %v, want unavailable", err)
	}
}
