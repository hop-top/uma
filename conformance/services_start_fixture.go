package conformance

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// Outbound-first channel types: one with email exchange semantics
// (sender-chosen message IDs, header threading) and one with messaging
// semantics (provider-assigned message IDs).
const (
	startMailType  model.TypeKey = "com.example.conformance.start-mail"
	startChatType  model.TypeKey = "com.example.conformance.start-chat"
	startNS                      = "com.example.conformance.start-user"
	startRouteKind               = "address"
	startActor     model.ActorID = "start-staff"
)

// startAdapter is a channel that can start conversations: its webhook
// instance hands the suite the engine-bound sink and records every send.
// A mail adapter answers a send with the email Message-ID it composed.
type startAdapter struct {
	typ  model.TypeKey
	mail bool

	mu    sync.Mutex
	sink  adapter.Sink
	sends []adapter.SendRequest
}

func (a *startAdapter) Describe() adapter.Descriptor {
	ex := adapter.Exchange{MessageIDs: adapter.MessageIDsProvider}
	if a.mail {
		ex = adapter.Exchange{MessageIDs: adapter.MessageIDsSender, Threading: adapter.ThreadingEmail}
	}
	return adapter.Descriptor{
		Type: a.typ, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(a.typ) + "-config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityStartConversation},
		Events:              []string{model.EventMessageReceived, model.EventDeliveryUpdated},
		IdentityNamespaces:  []string{startNS},
		Exchange:            ex,
	}
}
func (*startAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *startAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return startInstance{a}, nil
}

func (a *startAdapter) sent() []adapter.SendRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.sends)
}

// sentID is the provider message ID the adapter reports for a send.
func (a *startAdapter) sentID(r adapter.SendRequest) string {
	if a.mail {
		return "<start-" + string(r.Attempt) + "@mail.example>"
	}
	return "sent-" + string(r.Attempt)
}

type startInstance struct{ a *startAdapter }

func (i startInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range i.a.Describe().Capabilities {
		out = append(out, model.CapabilityStatus{Name: c, Available: true})
	}
	return out, nil
}
func (startInstance) Close(context.Context) error { return nil }
func (i startInstance) Handler(s adapter.Sink) http.Handler {
	i.a.mu.Lock()
	i.a.sink = s
	i.a.mu.Unlock()
	return http.NotFoundHandler()
}

func (i startInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	i.a.mu.Lock()
	i.a.sends = append(i.a.sends, r)
	i.a.mu.Unlock()
	ref := model.ProviderMessageRef(r.Route.Channel, i.a.sentID(r))
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{ref}}, nil
}

var _ adapter.Sender = startInstance{}

// startPolicy allows every decision unless deny refuses it and records the
// decisions the staff principal's calls ask.
type startPolicy struct {
	mu   sync.Mutex
	deny func(policy.Authorization) bool
	log  []policy.Authorization
}

func (p *startPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.Principal.Actor == startActor {
		p.log = append(p.log, a)
	}
	if p.deny != nil && p.deny(a) {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true}, nil
}

func (p *startPolicy) set(deny func(policy.Authorization) bool) {
	p.mu.Lock()
	p.deny, p.log = deny, nil
	p.mu.Unlock()
}

func (p *startPolicy) take() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.log
	p.log = nil
	return out
}

// startEnv is one engine over a fresh store with a mail and a chat
// channel that can start conversations, their sinks bound.
type startEnv struct {
	t      *testing.T
	ctx    context.Context
	st     store.Store
	clock  *portstest.FixedClock
	engine uma.Engine
	client uma.Client
	pol    *startPolicy
	mail   *startAdapter
	chat   *startAdapter
	chans  map[*startAdapter]model.ChannelRef
	n      atomic.Int64
	events atomic.Int64
}

func newStartEnv(t *testing.T, factory StoreFactory) *startEnv {
	return newStartEnvHeld(t, factory, uma.HeldPolicy{})
}

// newStartEnvHeld is newStartEnv with the local writes held opts in.
func newStartEnvHeld(t *testing.T, factory StoreFactory, held uma.HeldPolicy) *startEnv {
	t.Helper()
	fx := factory(t)
	env := &startEnv{
		t: t, ctx: t.Context(), st: fx.Store, clock: portstest.NewFixedClock(fx.Now()), pol: &startPolicy{},
		mail: &startAdapter{typ: startMailType, mail: true}, chat: &startAdapter{typ: startChatType},
		chans: map[*startAdapter]model.ChannelRef{},
	}
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: env.pol, Clock: env.clock, IDs: portstest.NewSequenceIDs(0x5a7),
		Adapters: []adapter.Factory{env.mail, env.chat}, Held: held,
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	env.engine = e
	if env.client, err = e.For(model.Principal{Tenant: serviceTenant, Actor: startActor}); err != nil {
		t.Fatal(err)
	}
	service := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "start-service"}, Call: model.Call{Correlation: "corr"}}
	for _, a := range []*startAdapter{env.mail, env.chat} {
		ca, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
			Command: env.cmd(),
			Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: a.typ, Configuration: model.Extension{Name: string(a.typ) + "-config", Version: 1}},
		})
		if err != nil {
			t.Fatalf("ConfigureChannel: %v", err)
		}
		ch, err := ca.Operation.Result.AsChannel()
		if err != nil {
			t.Fatal(err)
		}
		env.chans[a] = ch
		if _, err := e.Runtime().Webhook(env.ctx, service, ch); err != nil {
			t.Fatalf("Webhook: %v", err)
		}
		a.mu.Lock()
		bound := a.sink != nil
		a.mu.Unlock()
		if !bound {
			t.Fatal("webhook binding did not hand the instance a sink")
		}
	}
	return env
}

func (env *startEnv) cmd() model.Command {
	return model.Command{Call: model.Call{Correlation: "corr-start"}, Operation: model.OperationID(fmt.Sprintf("op-%d", env.n.Add(1)))}
}

func startPerContact(lock bool) model.Selection {
	return model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
}

// inbox configures an inbox with selection sel on a's channel, its one
// destination dest.
func (env *startEnv) inbox(a *startAdapter, dest string, sel model.Selection) model.InboxRef {
	env.t.Helper()
	acc, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: env.cmd(),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.chans[a], Name: dest, Destinations: []string{dest},
			Selection: sel,
		},
	})
	if err != nil {
		env.t.Fatalf("ConfigureInbox: %v", err)
	}
	ref, err := acc.Operation.Result.AsInbox()
	if err != nil {
		env.t.Fatal(err)
	}
	return ref
}

// recipient is the route of address id on a's channel.
func (env *startEnv) recipient(a *startAdapter, id string) model.ExternalRef {
	return model.ExternalRef{Channel: env.chans[a], Namespace: startNS, Kind: startRouteKind, ID: id}
}

func (env *startEnv) startReq(op model.OperationID, in model.InboxRef, to model.ExternalRef, text string) model.StartConversation {
	return model.StartConversation{
		Command: model.Command{Call: model.Call{Correlation: "corr-start"}, Operation: op},
		Inbox:   in, Recipient: to,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

// start starts a conversation with to and fails the test on error.
func (env *startEnv) start(in model.InboxRef, to model.ExternalRef, text string) model.Acceptance {
	env.t.Helper()
	acc, err := env.client.Conversations().Start(env.ctx, env.startReq(env.cmd().Operation, in, to, text))
	if err != nil {
		env.t.Fatalf("Start: %v", err)
	}
	if acc.Operation.ResultMessage == nil {
		env.t.Fatalf("Start result names no message: %+v", acc.Operation)
	}
	return acc
}

// inbound is a message from address from to destination dest of a's
// channel, with email threading headers when inReplyTo is set.
func (env *startEnv) inbound(a *startAdapter, dest, from, inReplyTo string) model.Envelope {
	n := env.events.Add(1)
	e := model.Envelope{
		EventID: fmt.Sprintf("in-%d", n), MessageID: fmt.Sprintf("m-%d", n), Destination: dest,
		Kind: model.EnvelopeMessage, Sender: env.recipient(a, from), Assurance: "verified",
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "answer"}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: env.clock.Now(),
	}
	if a.mail {
		e.MessageID = fmt.Sprintf("<in-%d@client.example>", n)
		e.References = model.EmailHeaders{MessageID: e.MessageID, InReplyTo: inReplyTo}.Normalize(env.chans[a]).References
	}
	return e
}

func (env *startEnv) accept(a *startAdapter, e model.Envelope) error {
	a.mu.Lock()
	sink := a.sink
	a.mu.Unlock()
	_, err := sink.Accept(env.ctx, model.Ingress{Call: model.Call{Correlation: model.CorrelationID("corr-" + e.EventID)}, Envelope: e})
	return err
}

// deliver runs Runtime.Run workers until the operation settles.
func (env *startEnv) deliver(ref model.OperationRef) model.Operation {
	env.t.Helper()
	ctx, cancel := context.WithCancel(env.ctx)
	defer cancel()
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: "worker"}, Call: model.Call{Correlation: "run"}}
	ended := make(chan error, 1)
	go func() {
		ended <- env.engine.Runtime().Run(ctx, call, uma.WorkerOptions{Owner: "worker", Limit: 4, Poll: time.Millisecond})
	}()
	deadline := time.Now().Add(time.Minute)
	for {
		op, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "c"}, Ref: ref})
		if err != nil {
			env.t.Fatalf("Operations.Get: %v", err)
		}
		if settled(op) {
			cancel()
			<-ended
			return op
		}
		if time.Now().After(deadline) {
			env.t.Fatalf("operation stuck at %+v", op)
		}
		select {
		case err := <-ended:
			env.t.Fatalf("Run returned early: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// read pages list under f to the end in the suite's tenant.
func startRead[T any](env *startEnv, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error), f model.Filter) []T {
	env.t.Helper()
	var out []T
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		req := model.ListRequest{Filter: f, Page: model.PageRequest{Limit: 100}}
		for {
			p, err := list(r, env.ctx, req)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	if err != nil {
		env.t.Fatalf("View: %v", err)
	}
	return out
}

func (env *startEnv) conversations(in model.InboxRef) []model.Conversation {
	return startRead(env, store.Reader.Conversations, model.Filter{Inboxes: []model.InboxRef{in}})
}

func (env *startEnv) messages(conv model.ConversationRef) []model.Message {
	return startRead(env, store.Reader.Messages, model.Filter{Conversation: &conv})
}

func (env *startEnv) identities(in model.InboxRef) []model.Identity {
	return startRead(env, store.Reader.Identities, model.Filter{Inboxes: []model.InboxRef{in}})
}

// eventsFor returns the committed events on resource res, in order.
func (env *startEnv) eventsFor(res model.Ref) []model.Event {
	env.t.Helper()
	var out []model.Event
	for _, e := range startRead(env, func(r store.Reader, ctx context.Context, req model.ListRequest) (model.Page[model.Event], error) {
		return r.Events(ctx, model.Filter{}, req.Page.After, req.Page.Limit)
	}, model.Filter{}) {
		if e.Resource == res {
			out = append(out, e)
		}
	}
	return out
}
