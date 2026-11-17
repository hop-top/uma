package extensions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/extensions"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const (
	extKey   model.TypeKey = "com.example.chat.messages"
	plainKey model.TypeKey = "org.example.plain.messages"

	pinName    = "com.example.chat.messages.pin"
	pinAction  = "com.example.chat.messages.pin"
	lookupName = "com.example.chat.messages.lookup"
	resultName = "com.example.chat.messages.lookup_result"
	pinnedName = "com.example.chat.messages.pinned"

	chanID  model.ID = "00000004-0000-7000-8000-000000000001"
	plainID model.ID = "00000004-0000-7000-8000-000000000002"
	inboxID model.ID = "00000004-0000-7000-8000-000000000003"
	convID  model.ID = "00000004-0000-7000-8000-000000000004"
	plainIn model.ID = "00000004-0000-7000-8000-000000000005"
	plainCv model.ID = "00000004-0000-7000-8000-000000000006"
)

func definitions() []adapter.ExtensionDefinition {
	return []adapter.ExtensionDefinition{
		{
			Name: pinName, Version: 1, Kind: model.ExtensionKindEffect,
			InputSchema:        model.Extension{Name: pinName + "_input", Version: 1},
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: pinAction,
		},
		{
			Name: lookupName, Version: 1, Kind: model.ExtensionKindRead,
			ResultSchema:       model.Extension{Name: resultName, Version: 1},
			RequiredCapability: model.CapabilityExtensionQuery, AuthorizeAs: model.ActionConversationRead,
		},
		{
			Name: pinnedName, Version: 1, Kind: model.ExtensionKindEvent,
			RequiredCapability: model.CapabilityExtensionExecute, AuthorizeAs: model.ActionMessageRead,
		},
	}
}

// extFactory is a scripted adapter with extension reads and effects. Plain
// declares send only.
type extFactory struct {
	plain bool
	// extra are capabilities the adapter declares beyond the builtin ones.
	extra []model.Capability

	mu        sync.Mutex
	executes  []adapter.ExecuteRequest
	queries   []model.QueryExtension
	result    func(model.QueryExtension) (model.Extension, error)
	outcome   func(adapter.ExecuteRequest) (adapter.Outcome, error)
	withdrawn map[model.Capability]bool
	rule      func(model.Audience, model.Extension) []model.ValidationIssue
	validated []model.Extension
}

func (f *extFactory) Describe() adapter.Descriptor {
	d := adapter.Descriptor{
		Type: extKey, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(extKey) + ".config", Version: 1},
		Capabilities: []model.Capability{
			model.CapabilitySend, model.CapabilityExtensionQuery, model.CapabilityExtensionExecute,
		},
		Events: []string{model.EventDeliveryUpdated, pinnedName},
	}
	d.Capabilities = append(d.Capabilities, f.extra...)
	if f.plain {
		d.Type = plainKey
		d.ConfigurationSchema.Name = string(plainKey) + ".config"
		d.Capabilities = []model.Capability{model.CapabilitySend}
		d.Events = []string{model.EventDeliveryUpdated}
	}
	return d
}

func (f *extFactory) Validate(model.Extension) []model.ValidationIssue { return nil }

// ValidateAction applies the scripted rule, if any, and records the action.
func (f *extFactory) ValidateAction(a model.Audience, x model.Extension) []model.ValidationIssue {
	f.mu.Lock()
	f.validated = append(f.validated, x.Clone())
	rule := f.rule
	f.mu.Unlock()
	if rule == nil {
		return nil
	}
	return rule(a, x)
}

// bare hides every optional interface of the factory it wraps.
type bare struct{ adapter.Factory }

func (f *extFactory) Open(_ context.Context, ch model.Channel, _ adapter.Dependencies) (adapter.Instance, error) {
	if f.plain {
		return plainInstance{f}, nil
	}
	return &extInstance{f: f, ch: ch}, nil
}

func (f *extFactory) withdraw(c model.Capability) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withdrawn == nil {
		f.withdrawn = map[model.Capability]bool{}
	}
	f.withdrawn[c] = true
}

func (f *extFactory) statuses() []model.CapabilityStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []model.CapabilityStatus
	for _, c := range f.Describe().Capabilities {
		s := model.CapabilityStatus{Name: c, Available: !f.withdrawn[c]}
		if f.withdrawn[c] {
			s.Reason = model.Unauthorized
		}
		out = append(out, s)
	}
	return out
}

func (f *extFactory) executed() []adapter.ExecuteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.executes)
}

func (f *extFactory) validatedActions() []model.Extension {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.validated)
}

func (f *extFactory) queried() []model.QueryExtension {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.queries)
}

type plainInstance struct{ f *extFactory }

func (p plainInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return p.f.statuses(), nil
}
func (plainInstance) Close(context.Context) error { return nil }
func (plainInstance) Send(context.Context, adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

type extInstance struct {
	f  *extFactory
	ch model.Channel
}

func (x *extInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return x.f.statuses(), nil
}
func (x *extInstance) Close(context.Context) error { return nil }
func (x *extInstance) Send(context.Context, adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

func (x *extInstance) Execute(_ context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	x.f.mu.Lock()
	x.f.executes = append(x.f.executes, r)
	fn := x.f.outcome
	x.f.mu.Unlock()
	if fn != nil {
		return fn(r)
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		{Channel: x.ch.Ref, Namespace: "com.example.chat", Kind: "pin", ID: "p1"},
	}}, nil
}

func (x *extInstance) Query(_ context.Context, q model.QueryExtension) (model.Extension, error) {
	x.f.mu.Lock()
	x.f.queries = append(x.f.queries, q)
	fn := x.f.result
	x.f.mu.Unlock()
	if fn != nil {
		return fn(q)
	}
	return model.Extension{Name: resultName, Version: 1, Data: json.RawMessage(`{"pinned":true}`)}, nil
}

// recordingPolicy allows unless denied and records every request.
type recordingPolicy struct {
	mu     sync.Mutex
	deny   bool
	checks []policy.Authorization
}

func (p *recordingPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, a)
	return policy.Decision{Allow: !p.deny}, nil
}

func (p *recordingPolicy) set(deny bool) {
	p.mu.Lock()
	p.deny = deny
	p.mu.Unlock()
}

func (p *recordingPolicy) all() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.checks)
}

// env is an engine with the extension adapter and a plain adapter, one
// channel, inbox and conversation on each, and a runtime executing
// extension effects.
type env struct {
	t      *testing.T
	ctx    context.Context
	f      *extFactory
	plain  *extFactory
	pol    *recordingPolicy
	store  store.Store
	clock  *portstest.FixedClock
	e      *engine.Engine
	rt     *runtime.Runtime
	c      enginetest.Client
	ch     model.Channel
	inbox  model.Inbox
	conv   model.Conversation
	plainC model.Conversation
}

func newEnv(t *testing.T) *env {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(enginetest.TestEpochS, 0))
	x := &env{
		t: t, ctx: t.Context(), f: &extFactory{}, plain: &extFactory{plain: true}, pol: &recordingPolicy{},
		store: memory.New(memory.Options{Clock: clock}), clock: clock,
	}
	x.e = x.compose(definitions())
	x.rt = runtime.New(x.e, extensions.Effector())
	x.rt.SetJitter(func(time.Duration) time.Duration { return 0 })
	tenant := enginetest.TenantA
	x.ch = model.Channel{
		Ref: model.ChannelRef{Tenant: tenant, ID: chanID}, Type: extKey,
		Configuration: model.Extension{Name: string(extKey) + ".config", Version: 1}, Revision: "ch1",
	}
	plainCh := model.Channel{
		Ref: model.ChannelRef{Tenant: tenant, ID: plainID}, Type: plainKey,
		Configuration: model.Extension{Name: string(plainKey) + ".config", Version: 1}, Revision: "ch1",
	}
	x.inbox = model.Inbox{Ref: model.InboxRef{Tenant: tenant, ID: inboxID}, Channel: x.ch.Ref, Name: "support", Revision: "in1"}
	plainInbox := model.Inbox{Ref: model.InboxRef{Tenant: tenant, ID: plainIn}, Channel: plainCh.Ref, Name: "plain", Revision: "in1"}
	x.conv = model.Conversation{
		Ref: model.ConversationRef{Inbox: x.inbox.Ref, ID: convID}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		ReplyRoute: model.ExternalRef{Channel: x.ch.Ref, Namespace: "com.example.chat", Kind: "room", ID: "room-1"},
		CreatedAt:  clock.Now(), Revision: "cv1",
	}
	x.plainC = x.conv
	x.plainC.Ref = model.ConversationRef{Inbox: plainInbox.Ref, ID: plainCv}
	x.plainC.ReplyRoute.Channel = plainCh.Ref
	err := x.store.Transact(x.ctx, tenant, func(tx store.Tx) error {
		for _, ch := range []model.Channel{x.ch, plainCh} {
			if err := tx.PutChannel(x.ctx, ch, ""); err != nil {
				return err
			}
		}
		for _, in := range []model.Inbox{x.inbox, plainInbox} {
			if err := tx.PutInbox(x.ctx, in, ""); err != nil {
				return err
			}
		}
		if err := tx.PutConversation(x.ctx, x.conv, ""); err != nil {
			return err
		}
		return tx.PutConversation(x.ctx, x.plainC, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	if x.c, err = enginetest.For(x.e, enginetest.PrincipalA()); err != nil {
		t.Fatal(err)
	}
	return x
}

// compose builds an engine over the env's store with defs registered.
func (x *env) compose(defs []adapter.ExtensionDefinition) *engine.Engine {
	x.t.Helper()
	e := enginetest.MustNew(x.t, engine.Config{
		Store: x.store, Authorizer: x.pol, Adapters: []adapter.Factory{x.f, x.plain},
		Extensions: defs, Clock: x.clock, IDs: portstest.NewSequenceIDs(9),
	})
	x.t.Cleanup(func() { _ = e.Close(context.Background()) })
	return e
}

// step claims and executes every claimable item once with rt.
func (x *env) step(rt *runtime.Runtime) int {
	x.t.Helper()
	works, err := rt.ClaimWork(x.ctx, enginetest.TenantA, runtime.WorkerOptions{
		Owner: "w", Limit: 10, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute,
	})
	if err != nil {
		x.t.Fatal(err)
	}
	for _, w := range works {
		if err := rt.ExecuteClaimed(x.ctx, enginetest.TenantA, w); err != nil {
			x.t.Fatal(err)
		}
	}
	return len(works)
}

func (x *env) op(ref model.OperationRef) model.Operation {
	x.t.Helper()
	op, err := x.c.Operations().Get(x.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		x.t.Fatal(err)
	}
	return op
}

func (x *env) events() []model.Event {
	x.t.Helper()
	var out []model.Event
	err := x.store.View(x.ctx, enginetest.TenantA, func(r store.Reader) error {
		p, err := r.Events(x.ctx, model.Filter{}, "", 100)
		out = p.Items
		return err
	})
	if err != nil {
		x.t.Fatal(err)
	}
	return out
}

var audience = model.Audience{Visibility: model.Internal, PolicyRef: "staff-only"}

func execReq(opID model.OperationID, target model.Ref, data string) model.ExecuteExtension {
	return model.ExecuteExtension{
		Command:  model.Command{Call: model.Call{Correlation: "corr-" + model.CorrelationID(opID)}, Operation: opID},
		Target:   target,
		Audience: audience,
		Action:   model.Extension{Name: pinName, Version: 1, Data: json.RawMessage(data)},
	}
}

func queryReq(target model.Ref) model.QueryExtension {
	return model.QueryExtension{
		Call: model.Call{Correlation: "corr-q"}, Target: target, Audience: audience,
		Query: model.Extension{Name: lookupName, Version: 1, Data: json.RawMessage(`{"message":"m1","x_future":{"a":[1,2]}}`)},
	}
}

// jsonEqual reports whether a and b hold the same JSON value, numbers
// compared by their decimal text.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	decode := func(raw []byte) any {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		if err := d.Decode(&v); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		return v
	}
	return reflect.DeepEqual(decode(a), decode(b))
}
