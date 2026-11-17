package conformance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
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

// The fake agent adapter. It serves per-agent-context inboxes the way a
// protocol adapter for remote agents does, with public packages only: the
// adapter contract, the model and the engine's own sink. That it works
// without a hook in the core is the proof that an agent protocol needs no
// core change. Remote agents are principals of the adapter's identity
// namespace; the host (the engine's client) is the only executor; and the
// adapter's whole write path is Accept and the sink's cancel request.

const agentType model.TypeKey = "com.example.conformance.agent"

const (
	agentSchema = "com.example.conformance.agent-config"
	agentNS     = "com.example.conformance.agent-principal"
	agentCtxNS  = "com.example.conformance.agent-context"
	agentDest   = "agents"
	agentDest2  = "agents-two"
	// agentService is the actor the adapter's sink runs as.
	agentService model.ActorID = "agent-service"
	// agentHost is the actor of the host executor.
	agentHost model.ActorID = "agent-host"
)

type agentAdapter struct {
	mu   sync.Mutex
	sink adapter.Sink
}

func (a *agentAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: agentType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: agentSchema, Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend},
		Events:              []string{model.EventMessageReceived, model.EventMessageCreated, model.EventTaskUpdated},
		IdentityNamespaces:  []string{agentNS},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
		SenderKinds:         []string{model.ContactKindAgent},
	}
}
func (*agentAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *agentAdapter) Open(_ context.Context, ch model.Channel, _ adapter.Dependencies) (adapter.Instance, error) {
	return agentInstance{a: a, ch: ch.Ref}, nil
}

type agentInstance struct {
	a  *agentAdapter
	ch model.ChannelRef
}

func (i agentInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	var out []model.CapabilityStatus
	for _, c := range i.a.Describe().Capabilities {
		out = append(out, model.CapabilityStatus{Name: c, Available: true})
	}
	return out, nil
}
func (agentInstance) Close(context.Context) error { return nil }
func (i agentInstance) Handler(s adapter.Sink) http.Handler {
	i.a.mu.Lock()
	i.a.sink = s
	i.a.mu.Unlock()
	return http.NotFoundHandler()
}

// Send accepts a reply without a transport: a remote agent reads replies
// through the sink, never receives a push.
func (i agentInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return adapter.Outcome{
		State:      model.ProviderAccepted,
		References: []model.ExternalRef{model.ProviderMessageRef(i.ch, string(r.Attempt))},
	}, nil
}

var _ adapter.Sender = agentInstance{}

// agentPolicy allows everything except what it is told to deny, per actor.
type agentPolicy struct {
	mu     sync.Mutex
	denied map[model.ActorID][]string
}

func (p *agentPolicy) deny(actor model.ActorID, actions ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.denied == nil {
		p.denied = map[model.ActorID][]string{}
	}
	p.denied[actor] = append(p.denied[actor], actions...)
}

func (p *agentPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.denied[a.Principal.Actor] {
		if d == a.Action {
			return policy.Decision{}, nil
		}
	}
	return policy.Decision{Allow: true}, nil
}

// agentEnv is one engine over a fresh store with an agent channel, two
// per-agent-context inboxes, the adapter's sink bound, and the host
// executor's client.
type agentEnv struct {
	t      *testing.T
	ctx    context.Context
	st     store.Store
	clock  *portstest.FixedClock
	engine uma.Engine
	host   uma.Client
	pol    *agentPolicy
	ch     model.ChannelRef
	inbox  model.InboxRef
	inbox2 model.InboxRef
	sink   adapter.Sink
	tasks  adapter.TaskSink

	ops  atomic.Int64
	ctxN atomic.Int64
}

func newAgentEnv(t *testing.T, factory StoreFactory) *agentEnv {
	t.Helper()
	fx := factory(t)
	a := &agentAdapter{}
	pol := &agentPolicy{}
	clock := portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Clock: clock, IDs: portstest.NewSequenceIDs(0xa6e), Authorizer: pol,
		Adapters: []adapter.Factory{a},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	host, err := e.For(model.Principal{Tenant: serviceTenant, Actor: agentHost})
	if err != nil {
		t.Fatal(err)
	}
	env := &agentEnv{t: t, ctx: t.Context(), st: fx.Store, clock: clock, engine: e, host: host, pol: pol}
	ca, err := host.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: env.cmd(""),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: agentType, Configuration: model.Extension{Name: agentSchema, Version: 1}},
	})
	if err != nil {
		t.Fatalf("ConfigureChannel: %v", err)
	}
	if env.ch, err = ca.Operation.Result.AsChannel(); err != nil {
		t.Fatal(err)
	}
	env.inbox = env.configureInbox(agentDest)
	env.inbox2 = env.configureInbox(agentDest2)
	service := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: agentService}, Call: model.Call{Correlation: "agent"}}
	if _, err := e.Runtime().Webhook(env.ctx, service, env.ch); err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	a.mu.Lock()
	env.sink = a.sink
	a.mu.Unlock()
	ts, ok := env.sink.(adapter.TaskSink)
	if !ok {
		t.Fatal("the engine's sink does not implement adapter.TaskSink")
	}
	env.tasks = ts
	return env
}

func (env *agentEnv) configureInbox(dest string) model.InboxRef {
	env.t.Helper()
	a, err := env.host.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: env.cmd(""),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.ch, Name: dest, Destinations: []string{dest},
			Selection: model.Selection{Mode: model.ModePerAgentContext},
		},
	})
	if err != nil {
		env.t.Fatalf("ConfigureInbox: %v", err)
	}
	ref, err := a.Operation.Result.AsInbox()
	if err != nil {
		env.t.Fatal(err)
	}
	return ref
}

func (env *agentEnv) cmd(rev model.Revision) model.Command {
	return model.Command{
		Call:             model.Call{Correlation: "agent-corr"},
		Operation:        model.OperationID("agent-op-" + strconv.FormatInt(env.ops.Add(1), 10)),
		ExpectedRevision: rev,
	}
}

// principal is the remote agent id as the adapter reports it.
func (env *agentEnv) principal(id string) model.ExternalRef {
	return model.ExternalRef{Channel: env.ch, Namespace: agentNS, Kind: "agent", ID: id}
}

func (env *agentEnv) context(id string) model.ExternalRef {
	return model.ExternalRef{Channel: env.ch, Namespace: agentCtxNS, Kind: model.RefKindContext, ID: id}
}

// freshContext allocates a new opaque context reference, as the adapter
// does for every request that names none.
func (env *agentEnv) freshContext() model.ExternalRef {
	return env.context("ctx-" + strconv.FormatInt(env.ctxN.Add(1), 10))
}

func (env *agentEnv) taskRef(id model.ID) model.ExternalRef { return model.TaskExternalRef(env.ch, id) }

// clientMessageID derives the engine message ID from the authenticated
// principal and the client's message ID, length-prefixed and hashed, so a
// message ID chosen by one agent never collides with another's.
func clientMessageID(p model.ExternalRef, clientID string) string {
	h := sha256.New()
	for _, s := range []string{p.Namespace, p.ID, clientID} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(s), s)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// sendOpts describes one remote request.
type sendOpts struct {
	text string
	// dest overrides the destination.
	dest string
	// context names the context to send under; task a task to continue
	// (by core task ID); open asks for a task to be opened with the
	// message.
	context string
	task    model.ID
	open    bool
}

func (o sendOpts) body() string {
	if o.text == "" {
		return "hello"
	}
	return o.text
}

// envelope is the message envelope of a request from principal p with
// client message ID clientID, to context ctx (zero for a continuation).
func (env *agentEnv) envelope(p model.ExternalRef, clientID string, ctx model.ExternalRef, o sendOpts) model.Envelope {
	dest := o.dest
	if dest == "" {
		dest = agentDest
	}
	e := model.Envelope{
		MessageID: clientMessageID(p, clientID), Destination: dest, Kind: model.EnvelopeMessage,
		Sender: p, SenderKind: model.ContactKindAgent, Assurance: "verified",
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: o.body()}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: env.clock.Now().UTC(),
	}
	if !ctx.IsZero() {
		e.References = []model.ExternalRef{ctx}
	}
	switch {
	case o.task != "":
		e.Task = &model.TaskBinding{External: env.taskRef(o.task)}
	case o.open:
		e.Task = &model.TaskBinding{Open: true}
	}
	return e
}

// accept hands an envelope to the sink as it is.
func (env *agentEnv) accept(e model.Envelope) (model.IngressReceipt, error) {
	return env.sink.Accept(env.ctx, model.Ingress{Call: model.Call{Correlation: "agent-rx"}, Envelope: e})
}

func (env *agentEnv) mustAccept(e model.Envelope) model.IngressReceipt {
	env.t.Helper()
	r, err := env.accept(e)
	if err != nil {
		env.t.Fatalf("Accept: %v", err)
	}
	if !r.Committed {
		env.t.Fatalf("receipt = %+v, want committed", r)
	}
	return r
}

// send is one request of a remote agent, handled as a protocol adapter
// handles it: ownership first, through the sink, then Accept. A context
// the principal does not own, a task it does not own, a task of another
// context and a task that ended are refused before anything is accepted.
func (env *agentEnv) send(p model.ExternalRef, clientID string, o sendOpts) (model.IngressReceipt, error) {
	env.t.Helper()
	var ctx model.ExternalRef
	switch {
	case o.task != "":
		t, err := env.tasks.Task(env.ctx, p, env.taskRef(o.task))
		if err != nil {
			return model.IngressReceipt{}, err
		}
		if o.context != "" && o.context != t.Context.ID {
			return model.IngressReceipt{}, model.NewError(model.Validation, "context is not the task's")
		}
		if model.IsTerminalTaskState(t.State) {
			return model.IngressReceipt{}, model.NewError(model.Conflict, "task is "+t.State)
		}
	case o.context != "":
		ctx = env.context(o.context)
		if _, err := env.tasks.Context(env.ctx, p, ctx); err != nil {
			return model.IngressReceipt{}, err
		}
	default:
		ctx = env.freshContext()
	}
	return env.accept(env.envelope(p, clientID, ctx, o))
}

func (env *agentEnv) mustSend(p model.ExternalRef, clientID string, o sendOpts) model.IngressReceipt {
	env.t.Helper()
	r, err := env.send(p, clientID, o)
	if err != nil {
		env.t.Fatalf("send %s: %v", clientID, err)
	}
	if !r.Committed || r.Duplicate || r.Message == nil || r.Context == nil || r.Conversation == nil {
		env.t.Fatalf("receipt of %s = %+v, want a committed one carrying its references", clientID, r)
	}
	return r
}

// await polls what answers the message every few milliseconds until it
// answers or d passes, as a blocked SendMessage does. It reports whether
// the wait ran out; it opens, cancels and writes nothing.
func (env *agentEnv) await(p model.ExternalRef, m model.MessageRef, d time.Duration) (adapter.Responses, bool) {
	env.t.Helper()
	deadline := time.Now().Add(d)
	for {
		r, err := env.tasks.Responses(env.ctx, p, m)
		if err != nil {
			env.t.Fatalf("Responses: %v", err)
		}
		if r.Task != nil && (model.IsTerminalTaskState(r.Task.State) || model.IsInterruptedTaskState(r.Task.State)) || len(r.Replies) > 0 {
			return r, false
		}
		if time.Now().After(deadline) {
			return r, true
		}
		time.Sleep(5 * time.Millisecond)
	}
}
