package conformance

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

const (
	historyType   model.TypeKey = "com.example.conformance.history"
	historySchema               = "com.example.conformance.history-config"
	historyNS                   = "com.example.conformance.history-user"
	historyRoomNS               = "com.example.conformance.history-room"
	historyDest                 = "history"
	historyDest2                = "history-2"
	historyStaff  model.ActorID = "staff-1"
)

// historyProvider is the provider side of the history adapter: the
// message history it holds, served oldest first in pages of pageSize, and
// the requests it was asked.
type historyProvider struct {
	mu       sync.Mutex
	held     []model.Envelope
	pageSize int
	gapAt    int
	// overfill makes pages ignore the request's limit, as a provider with
	// a fixed page size does.
	overfill bool
	// endless serves empty pages whose cursor always advances; stall
	// serves pages whose cursor never does.
	endless, stall bool
	// failAt makes the read of the page at that cursor fail with failErr.
	failAt  string
	failErr error
	asked   []adapter.HistoryRequest
}

func (p *historyProvider) history(r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, r)
	if p.endless {
		n, _ := strconv.Atoi(r.Cursor)
		return adapter.HistoryPage{Next: strconv.Itoa(n + 1)}, nil
	}
	if p.failErr != nil && r.Cursor == p.failAt {
		return adapter.HistoryPage{}, p.failErr
	}
	var window []model.Envelope
	for _, env := range p.held {
		if env.OccurredAt.Before(r.Since) || (!r.Until.IsZero() && !env.OccurredAt.Before(r.Until)) {
			continue
		}
		if r.Route != nil && !slices.ContainsFunc(env.References, func(x model.ExternalRef) bool { return x == *r.Route }) {
			continue
		}
		window = append(window, env.Clone())
	}
	start := 0
	if r.Cursor != "" {
		n, err := strconv.Atoi(r.Cursor)
		if err != nil || n < 0 || n > len(window) {
			return adapter.HistoryPage{}, model.NewError(model.Validation, "cursor: unknown")
		}
		start = n
	}
	size := min(p.pageSize, r.Limit)
	if p.overfill {
		size = p.pageSize
	}
	end := min(start+size, len(window))
	page := adapter.HistoryPage{Envelopes: window[start:end], Gap: p.gapAt > 0 && start < p.gapAt && p.gapAt <= end}
	if end < len(window) {
		page.Next = strconv.Itoa(end)
	}
	if p.stall && r.Cursor != "" {
		page.Next = r.Cursor
	}
	return page, nil
}

func (p *historyProvider) requests() []adapter.HistoryRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.asked)
}

// historyAdapter is a webhook channel that also reads history. Its
// webhook instance hands the suite the engine-bound sink, so live
// messages arrive through the engine's own selection.
type historyAdapter struct {
	p    *historyProvider
	ids  adapter.MessageIDAuthority
	mu   sync.Mutex
	sink adapter.Sink
}

func (a *historyAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: historyType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: historySchema, Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP, model.CapabilityHistory},
		Events:              []string{model.EventMessageReceived},
		IdentityNamespaces:  []string{historyNS},
		Exchange:            adapter.Exchange{MessageIDs: a.ids},
	}
}
func (*historyAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *historyAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return historyInstance{a}, nil
}

type historyInstance struct{ a *historyAdapter }

func (historyInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return available([]model.Capability{model.CapabilityReceiveHTTP, model.CapabilityHistory}), nil
}
func (historyInstance) Close(context.Context) error { return nil }
func (i historyInstance) Handler(s adapter.Sink) http.Handler {
	i.a.mu.Lock()
	i.a.sink = s
	i.a.mu.Unlock()
	return http.NotFoundHandler()
}
func (i historyInstance) History(_ context.Context, r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	return i.a.p.history(r)
}

// historyPolicy allows everything except the actions denied to an actor.
type historyPolicy struct {
	mu   sync.Mutex
	deny map[model.ActorID]string
}

func (p *historyPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return policy.Decision{Allow: p.deny[a.Principal.Actor] != a.Action}, nil
}

func (p *historyPolicy) denyTo(actor model.ActorID, action string) {
	p.mu.Lock()
	p.deny[actor] = action
	p.mu.Unlock()
}

// historyEnv is one engine with a per-room inbox on a history channel, the
// sink its webhook bound, and conversation room-1 opened by a live
// message, pm-live.
type historyEnv struct {
	t        *testing.T
	ctx      context.Context
	st       store.Store
	engine   uma.Engine
	client   uma.Client
	provider *historyProvider
	policy   *historyPolicy
	sink     adapter.Sink
	channel  model.ChannelRef
	inbox    model.InboxRef
	conv     model.Conversation
	start    time.Time
	service  uma.RuntimeCall
	clock    *portstest.FixedClock
	fx       StoreFixture
}

func newHistoryEnv(t *testing.T, factory StoreFactory) *historyEnv {
	t.Helper()
	return newHistoryEnvIDs(t, factory, adapter.MessageIDsProvider)
}

// newHistoryEnvIDs is newHistoryEnv for an adapter whose message IDs are
// chosen by ids' authority.
func newHistoryEnvIDs(t *testing.T, factory StoreFactory, ids adapter.MessageIDAuthority) *historyEnv {
	t.Helper()
	fx := factory(t)
	env := &historyEnv{
		t: t, ctx: t.Context(), st: fx.Store, provider: &historyProvider{pageSize: 2},
		policy: &historyPolicy{deny: map[model.ActorID]string{}}, start: fx.Now().UTC(),
		clock: portstest.NewFixedClock(fx.Now()), fx: fx,
		service: uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "history-service"}, Call: model.Call{Correlation: "corr-history"}},
	}
	a := &historyAdapter{p: env.provider, ids: ids}
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: env.policy, Clock: env.clock, IDs: portstest.NewSequenceIDs(0x4157),
		Adapters: []adapter.Factory{a}, Blobs: &portstest.MemoryBlobs{},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	env.engine = e
	if env.client, err = e.For(model.Principal{Tenant: serviceTenant, Actor: historyStaff}); err != nil {
		t.Fatal(err)
	}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID(op)}
	}
	ca, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: cmd("op-channel"),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: historyType, Configuration: model.Extension{Name: historySchema, Version: 1}},
	})
	if err != nil {
		t.Fatalf("ConfigureChannel: %v", err)
	}
	if env.channel, err = ca.Operation.Result.AsChannel(); err != nil {
		t.Fatal(err)
	}
	ia, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: cmd("op-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.channel, Name: historyDest, Destinations: []string{historyDest},
			Selection: model.Selection{Mode: model.ModePerRoom},
		},
	})
	if err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
	if env.inbox, err = ia.Operation.Result.AsInbox(); err != nil {
		t.Fatal(err)
	}
	// A second inbox on the channel, whose messages no conversation of the
	// first holds.
	if _, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: cmd("op-inbox-2"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.channel, Name: historyDest2, Destinations: []string{historyDest2},
			Selection: model.Selection{Mode: model.ModePerRoom},
		},
	}); err != nil {
		t.Fatalf("ConfigureInbox: %v", err)
	}
	if _, err := e.Runtime().Webhook(env.ctx, env.service, env.channel); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	a.mu.Lock()
	env.sink = a.sink
	a.mu.Unlock()
	live := env.message("room-1", "pm-live", env.start.Add(-time.Hour))
	if _, err := env.sink.Accept(env.ctx, model.Ingress{Call: model.Call{Correlation: "corr-live"}, Envelope: live}); err != nil {
		t.Fatalf("live Accept: %v", err)
	}
	err = env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Conversations(env.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{env.inbox}}, Page: model.PageRequest{Limit: 10}})
		if err == nil && len(p.Items) != 1 {
			t.Fatalf("live message opened %d conversations, want 1", len(p.Items))
		}
		if err == nil {
			env.conv = p.Items[0]
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return env
}

func (env *historyEnv) room(id string) model.ExternalRef {
	return model.ExternalRef{Channel: env.channel, Namespace: historyRoomNS, Kind: model.RefKindRoom, ID: id}
}

// message is a message envelope of room with provider message ID msgID,
// its event ID the provider's webhook ID for it.
func (env *historyEnv) message(room, msgID string, at time.Time) model.Envelope {
	return model.Envelope{
		EventID: "event-" + msgID, MessageID: msgID, Destination: historyDest, Kind: model.EnvelopeMessage,
		Sender:    model.ExternalRef{Channel: env.channel, Namespace: historyNS, Kind: "user", ID: "user-" + room},
		Assurance: "verified", References: []model.ExternalRef{env.room(room)},
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "text of " + msgID}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: at,
	}
}

// hold gives the provider its history: each envelope as reported by the
// history API, under another event ID than its webhook copy.
func (env *historyEnv) hold(envs ...model.Envelope) {
	env.provider.mu.Lock()
	defer env.provider.mu.Unlock()
	for _, e := range envs {
		e.EventID = "history-" + e.MessageID
		env.provider.held = append(env.provider.held, e)
	}
}

func (env *historyEnv) importReq(f func(*uma.ImportRequest)) uma.ImportRequest {
	conv := env.conv.Ref
	req := uma.ImportRequest{Requester: historyStaff, Conversation: &conv, Since: env.start.Add(-24 * time.Hour)}
	if f != nil {
		f(&req)
	}
	return req
}

func (env *historyEnv) importNow(req uma.ImportRequest) (uma.ImportReport, error) {
	return env.engine.Runtime().Import(env.ctx, env.service, req)
}

// providerIDs returns the provider message IDs of the conversation's
// messages, in recording order.
func (env *historyEnv) providerIDs(conv model.ConversationRef) []string {
	env.t.Helper()
	var out []string
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Messages(env.ctx, model.ListRequest{Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 100}})
		for _, m := range p.Items {
			for _, x := range m.External {
				if x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
					out = append(out, x.ID)
				}
			}
		}
		return err
	})
	if err != nil {
		env.t.Fatal(err)
	}
	return out
}
