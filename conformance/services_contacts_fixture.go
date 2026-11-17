package conformance

import (
	"context"
	"net/http"
	"slices"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

const chatType model.TypeKey = "com.example.conformance.contacts"

const (
	chatSchema = "com.example.conformance.contacts-config"
	chatNS     = "com.example.conformance.contacts-user"
	chatOne    = "one"
	chatTwo    = "two"
)

// chatAdapter is a chat channel whose webhook instance hands the suite the
// engine-bound sink and records every send.
type chatAdapter struct {
	mu    sync.Mutex
	sink  adapter.Sink
	sends []adapter.SendRequest
}

func (*chatAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: chatType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: chatSchema, Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend},
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{chatNS},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
		SenderKinds:         []string{model.ContactKindBot},
	}
}
func (*chatAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *chatAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return chatInstance{a}, nil
}

func (a *chatAdapter) sent() []adapter.SendRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sends)
}

type chatInstance struct{ a *chatAdapter }

func (chatInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilityReceiveHTTP, Available: true}, {Name: model.CapabilitySend, Available: true},
	}, nil
}
func (chatInstance) Close(context.Context) error { return nil }
func (i chatInstance) Handler(s adapter.Sink) http.Handler {
	i.a.mu.Lock()
	i.a.sink = s
	i.a.mu.Unlock()
	return http.NotFoundHandler()
}

func (i chatInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	i.a.mu.Lock()
	i.a.sends = append(i.a.sends, r)
	i.a.mu.Unlock()
	ref := model.ExternalRef{Channel: r.Route.Channel, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindMessage, ID: "sent-" + string(r.Attempt)}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{ref}}, nil
}

var _ adapter.Sender = chatInstance{}

// contactsPolicy allows every decision unless deny refuses it, records
// those the staff principal's calls ask, and runs before, when set, ahead
// of deciding. Allows carry policy
// version pv-1 and reason allow_<action>. It reads no evidence context
// unless deny does: an authorizer ignoring Assurance and Evidence decides
// as before.
type contactsPolicy struct {
	mu     sync.Mutex
	deny   func(policy.Authorization) bool
	before func(policy.Authorization)
	log    []policy.Authorization
}

func (p *contactsPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	if a.Principal.Actor == contactsActor {
		p.log = append(p.log, a)
	}
	deny, before := p.deny, p.before
	p.mu.Unlock()
	if before != nil {
		before(a)
	}
	if deny != nil && deny(a) {
		return policy.Decision{PolicyVersion: "pv-1", ReasonCode: "deny_" + a.Action}, nil
	}
	return policy.Decision{Allow: true, PolicyVersion: "pv-1", ReasonCode: "allow_" + a.Action}, nil
}

// set replaces the deny rule and clears the log.
func (p *contactsPolicy) set(deny func(policy.Authorization) bool) {
	p.mu.Lock()
	p.deny, p.log = deny, nil
	p.mu.Unlock()
}

// hook replaces the before hook.
func (p *contactsPolicy) hook(fn func(policy.Authorization)) {
	p.mu.Lock()
	p.before = fn
	p.mu.Unlock()
}

// take returns the decisions asked since the last take or set and clears
// them.
func (p *contactsPolicy) take() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.log
	p.log = nil
	return out
}

// denyIn refuses actions on resources in inbox.
func denyIn(in model.InboxRef, actions ...string) func(policy.Authorization) bool {
	return func(a policy.Authorization) bool {
		return a.Resource.Inbox == in.ID && slices.Contains(actions, a.Action)
	}
}

// cascadeLimits is the suite's policy.CascadePolicy: it answers rule,
// policy.CascadeNeverRefuse while rule is nil, and records every question.
type cascadeLimits struct {
	mu    sync.Mutex
	rule  func(model.Principal, model.InboxRef) int
	asked []cascadeQuestion
}

type cascadeQuestion struct {
	p     model.Principal
	inbox model.InboxRef
}

func (l *cascadeLimits) ContactCascadeThreshold(_ context.Context, p model.Principal, in model.InboxRef) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, cascadeQuestion{p, in})
	if l.rule == nil {
		return policy.CascadeNeverRefuse, nil
	}
	return l.rule(p, in), nil
}

// set replaces the rule and clears the questions.
func (l *cascadeLimits) set(rule func(model.Principal, model.InboxRef) int) {
	l.mu.Lock()
	l.rule, l.asked = rule, nil
	l.mu.Unlock()
}

// questions returns the questions asked since the last set.
func (l *cascadeLimits) questions() []cascadeQuestion {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.asked)
}

// everywhere answers n for every tenant and inbox.
func everywhere(n int) func(model.Principal, model.InboxRef) int {
	return func(model.Principal, model.InboxRef) int { return n }
}

// denyRedactOf refuses privacy.redact on the identities.
func denyRedactOf(ids ...model.Identity) func(policy.Authorization) bool {
	return func(a policy.Authorization) bool {
		if a.Action != model.ActionPrivacyRedact {
			return false
		}
		return slices.ContainsFunc(ids, func(id model.Identity) bool { return a.Resource == id.Ref.Ref() })
	}
}
