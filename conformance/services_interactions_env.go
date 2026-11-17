package conformance

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// The interaction fixture of RunServiceSuite: three channel types that
// differ only in what they declare, so one case can try the same
// interaction against an adapter that answers it, one that answers without
// follow-ups and one that cannot answer at all. Public packages only.
const (
	interactiveType  model.TypeKey = "com.example.conformance.interactive"
	respondOnlyType  model.TypeKey = "com.example.conformance.respond-only"
	noRespondType    model.TypeKey = "com.example.conformance.no-respond"
	interactionNS                  = "com.example.conformance.user"
	interactionActor model.ActorID = "responder-1"
	// interactionToken is the response token every seeded interaction
	// holds; no read, record or event may carry it.
	interactionToken = "tok-conformance-secret-0001"
)

// responderCall is one provider call as the fake observed it.
type responderCall struct {
	Method      string
	Mode        string
	FollowUp    bool
	Text        string
	Audience    model.Audience
	Interaction *adapter.InteractionContext
	Target      *model.ExternalRef
	Recipient   model.ExternalRef
}

// responderProvider records the calls of every instance of its adapter and
// answers each as applied.
type responderProvider struct {
	mu    sync.Mutex
	calls []responderCall
	seq   int
}

func (p *responderProvider) record(c responderCall) adapter.Outcome {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.calls = append(p.calls, c)
	return adapter.Outcome{State: model.ProviderAccepted}
}

func (p *responderProvider) all() []responderCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func (p *responderProvider) reset() {
	p.mu.Lock()
	p.calls = nil
	p.mu.Unlock()
}

func contentText(c model.Content) string {
	if len(c.Parts) == 0 {
		return ""
	}
	return c.Parts[0].Text
}

func cloneInteraction(ic *adapter.InteractionContext) *adapter.InteractionContext {
	if ic == nil {
		return nil
	}
	c := ic.Clone()
	return &c
}

type responderAdapter struct {
	typ  model.TypeKey
	caps []model.Capability
	p    *responderProvider
}

func (a responderAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: a.typ, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(a.typ) + "-config", Version: 1},
		Capabilities:        a.caps, Events: []string{model.EventDeliveryUpdated},
		Exchange: adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}
func (responderAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a responderAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return responderInstance{a.caps, a.p}, nil
}

type responderInstance struct {
	caps []model.Capability
	p    *responderProvider
}

func (i responderInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return available(i.caps), nil
}
func (responderInstance) Close(context.Context) error { return nil }

func (i responderInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return i.p.record(responderCall{Method: "send", Text: contentText(r.Content), Audience: r.Audience}), nil
}

func (i responderInstance) SendEphemeral(_ context.Context, r adapter.EphemeralRequest) (adapter.Outcome, error) {
	return i.p.record(responderCall{
		Method: "ephemeral", Text: contentText(r.Content), Audience: r.Audience, Recipient: r.Recipient,
		Interaction: cloneInteraction(r.Interaction),
	}), nil
}

// Respond answers with a reference to the message it posted, so a message
// mode response settles with a provider message ID.
func (i responderInstance) Respond(_ context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	out := i.p.record(responderCall{
		Method: "respond", Mode: r.Mode, FollowUp: r.FollowUp, Text: contentText(r.Content), Audience: r.Audience,
		Interaction: cloneInteraction(r.Interaction), Target: r.Target,
	})
	out.References = []model.ExternalRef{model.ProviderMessageRef(r.Route.Channel, "response-"+string(r.Attempt))}
	return out, nil
}

var (
	_ adapter.Sender               = responderInstance{}
	_ adapter.EphemeralSender      = responderInstance{}
	_ adapter.InteractionResponder = responderInstance{}
)

// interactionSite is one channel type's channel, inbox and conversation,
// with the identity of the actor of its interactions.
type interactionSite struct {
	typ   model.TypeKey
	ch    model.ChannelRef
	inbox model.InboxRef
	conv  model.Conversation
	actor model.Identity
}

// interactionEnv is an engine over the store under test with the three
// sites, a client for the responding principal and control of the clock.
type interactionEnv struct {
	t        *testing.T
	ctx      context.Context
	fx       StoreFixture
	st       store.Store
	policy   *memberAuthorizer
	engine   uma.Engine
	client   uma.Client
	provider *responderProvider
	clock    *portstest.FixedClock
	ids      *portstest.SequenceIDs
	sites    map[model.TypeKey]interactionSite
	n        int
	// queued holds the operations accepted for delivery, which a worker
	// run to settle one must settle all: stopping it with another in
	// flight would leave that one's lease held on a clock that never moves.
	queued []model.OperationRef
}

func newInteractionEnv(t *testing.T, factory StoreFactory) *interactionEnv {
	t.Helper()
	fx := factory(t)
	env := &interactionEnv{
		t: t, ctx: t.Context(), fx: fx, st: fx.Store, policy: &memberAuthorizer{}, provider: &responderProvider{},
		ids: portstest.NewSequenceIDs(0x1a7), sites: map[model.TypeKey]interactionSite{},
		clock: portstest.NewFixedClock(fx.Now()),
	}
	both := []model.Capability{model.CapabilitySend, model.CapabilitySendEphemeral}
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: env.policy, Clock: env.clock, IDs: portstest.NewSequenceIDs(0x1a8),
		Adapters: []adapter.Factory{
			responderAdapter{interactiveType, append(slices.Clone(both), model.CapabilityRespond, model.CapabilityRespondFollowUp), env.provider},
			responderAdapter{respondOnlyType, append(slices.Clone(both), model.CapabilityRespond), env.provider},
			responderAdapter{noRespondType, both, env.provider},
		},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	env.engine = e
	if env.client, err = e.For(model.Principal{Tenant: serviceTenant, Actor: interactionActor}); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []model.TypeKey{interactiveType, respondOnlyType, noRespondType} {
		env.sites[typ] = env.site(typ)
	}
	return env
}

func (env *interactionEnv) id() model.ID {
	id, err := env.ids.New(env.ctx)
	if err != nil {
		env.t.Fatal(err)
	}
	return id
}

func (env *interactionEnv) cmd() model.Command {
	env.n++
	return model.Command{Call: model.Call{Correlation: "corr-ix"}, Operation: model.OperationID(fmt.Sprintf("op-ix-%d", env.n))}
}

func (env *interactionEnv) put(fn func(store.Tx) error) {
	env.t.Helper()
	if err := env.st.Transact(env.ctx, serviceTenant, fn); err != nil {
		env.t.Fatalf("seed: %v", err)
	}
}

// site configures a channel of type typ and an inbox on it, then stores a
// conversation routed on the channel with the actor as its participant.
func (env *interactionEnv) site(typ model.TypeKey) interactionSite {
	env.t.Helper()
	ca, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: env.cmd(),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: typ, Configuration: model.Extension{Name: string(typ) + "-config", Version: 1}},
	})
	if err != nil {
		env.t.Fatalf("ConfigureChannel: %v", err)
	}
	ch, err := ca.Operation.Result.AsChannel()
	if err != nil {
		env.t.Fatal(err)
	}
	ia, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: env.cmd(),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: ch, Name: string(typ), Destinations: []string{string(typ)},
			Selection: model.Selection{Mode: model.ModePerRoom},
		},
	})
	if err != nil {
		env.t.Fatalf("ConfigureInbox: %v", err)
	}
	inbox, err := ia.Operation.Result.AsInbox()
	if err != nil {
		env.t.Fatal(err)
	}
	actorID := env.id()
	actor := model.Identity{
		Ref:     model.IdentityRef{Inbox: inbox, ID: actorID},
		Contact: model.ContactRef{Tenant: serviceTenant, ID: actorID}, SourceID: "user-" + string(typ),
		Namespace: interactionNS, Kind: model.ContactKindPerson, Revision: "id1",
	}
	conv := model.Conversation{
		Ref: model.ConversationRef{Inbox: inbox, ID: env.id()}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		Participants: []model.Participant{{Identity: actor.Ref, Role: "participant"}},
		ReplyRoute:   model.ExternalRef{Channel: ch, Namespace: "com.example.conformance", Kind: model.RefKindRoom, ID: "room-1"},
		CreatedAt:    env.now(), Revision: "cv1",
	}
	env.put(func(tx store.Tx) error {
		key := model.IdentityKey{Inbox: inbox, Namespace: actor.Namespace, SourceID: actor.SourceID}
		if _, _, err := tx.ClaimIdentity(env.ctx, key, actor); err != nil {
			return err
		}
		return tx.PutConversation(env.ctx, conv, "")
	})
	return interactionSite{typ: typ, ch: ch, inbox: inbox, conv: conv, actor: actor}
}

func (env *interactionEnv) now() time.Time { return env.clock.Now().UTC() }

// advance moves the engine's clock and the store's together, and reports
// false when the store cannot move its own.
func (env *interactionEnv) advance(d time.Duration) bool {
	if env.fx.Advance == nil {
		return false
	}
	env.fx.Advance(d)
	env.clock.Advance(d)
	return true
}

// seed stores an action interaction of s's conversation by its actor, public,
// with a response token and a respond window of an hour; mod adjusts the
// record before it is stored.
func (env *interactionEnv) seed(s interactionSite, mod ...func(*store.InteractionRecord)) model.Interaction {
	env.t.Helper()
	id := env.id()
	until := env.now().Add(time.Hour)
	msg := model.ProviderMessageRef(s.ch, "pm-"+string(id))
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Ref:  model.InteractionRef{Conversation: s.conv.Ref, ID: id},
			Kind: model.InteractionAction, Action: "approve",
			Actor: model.Author{
				Identity: s.actor.Ref, Kind: s.actor.Kind,
				External: model.ExternalRef{Channel: s.ch, Namespace: s.actor.Namespace, Kind: string(model.KindIdentity), ID: s.actor.SourceID},
			},
			MessageExternal: &msg,
			External:        []model.ExternalRef{model.ProviderInteractionRef(s.ch, "pi-"+string(id))},
			Audience:        model.Audience{Visibility: model.Public}, OccurredAt: env.now(), RecordedAt: env.now(),
			Deadline: env.now().Add(3 * time.Second), RespondUntil: until, Revision: "ix1", Source: s.typ,
		},
		Ingress: env.id(), ResponseToken: interactionToken, TokenExpiresAt: until,
	}
	for _, m := range mod {
		m(&rec)
	}
	env.put(func(tx store.Tx) error { return tx.PutInteraction(env.ctx, rec) })
	return rec.Interaction
}

// stored returns the interaction as the store holds it now.
func (env *interactionEnv) stored(ref model.InteractionRef) model.Interaction {
	env.t.Helper()
	var in model.Interaction
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) {
		in, err = r.Interaction(env.ctx, ref)
		return err
	})
	if err != nil {
		env.t.Fatalf("Interaction: %v", err)
	}
	return in
}

// request answers in with mode: ephemeral to the actor only, any other
// mode to everyone, for half an hour.
func (env *interactionEnv) request(in model.Interaction, mode string) model.Respond {
	audience := model.Audience{Visibility: model.Public}
	if mode == model.ResponseModeEphemeral {
		audience = model.Audience{Visibility: model.Restricted}
	}
	return model.Respond{
		Command: env.cmd(), Interaction: in.Ref, Mode: mode, Audience: audience,
		Content: textContent("answer"), ExpiresAt: env.now().Add(30 * time.Minute),
	}
}

func (env *interactionEnv) respond(req model.Respond) model.Acceptance {
	env.t.Helper()
	a, err := env.client.Interactions().Respond(env.ctx, req)
	if err != nil {
		env.t.Fatalf("Respond: %v", err)
	}
	env.queued = append(env.queued, a.Operation.Ref)
	return a
}

func (env *interactionEnv) op(ref model.OperationRef) model.Operation {
	env.t.Helper()
	op, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		env.t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// settledAll reports whether every operation accepted so far has settled.
func (env *interactionEnv) settledAll() bool {
	for _, ref := range env.queued {
		if !settled(env.op(ref)) {
			return false
		}
	}
	return true
}

// deliver runs workers until the operation and every other accepted one
// have settled.
func (env *interactionEnv) deliver(ref model.OperationRef) model.Operation {
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
		op := env.op(ref)
		if settled(op) && env.settledAll() {
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

// messages returns the messages stored in s's inbox.
func (env *interactionEnv) messages(s interactionSite) []model.Message {
	env.t.Helper()
	var out []model.Message
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		req := model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{s.inbox}}, Page: model.PageRequest{Limit: 100}}
		for {
			p, err := r.Messages(env.ctx, req)
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
		env.t.Fatalf("Messages: %v", err)
	}
	return out
}

// events returns the committed events of the given kinds, in commit order.
func (env *interactionEnv) events(kinds ...string) []model.Event {
	env.t.Helper()
	var out []model.Event
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		var after model.Cursor
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: kinds}, after, 100)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete || len(p.Items) == 0 && p.Next == after {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		env.t.Fatalf("Events: %v", err)
	}
	return out
}
