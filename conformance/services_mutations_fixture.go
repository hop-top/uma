package conformance

import (
	"context"
	"fmt"
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
	mutationType  model.TypeKey = "com.example.conformance.mutations"
	sendOnlyType  model.TypeKey = "com.example.conformance.send-only"
	mutationActor model.ActorID = "editor-1"
)

// mutationAdapter is a scripted adapter whose instances answer every
// effect as applied and record each mutation in its provider.
type mutationAdapter struct {
	typ  model.TypeKey
	caps []model.Capability
	p    *mutationProvider
}

func (a mutationAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: a.typ, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(a.typ) + "-config", Version: 1},
		Capabilities:        a.caps, Events: []string{model.EventDeliveryUpdated},
		Exchange: adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}
func (mutationAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a mutationAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	if a.p == nil {
		return sendOnlyInstance{a.caps}, nil
	}
	return &mutationInstance{a.caps, a.p}, nil
}

// readSwitch allows everything, except reading messages while hidden.
type readSwitch struct {
	mu     sync.Mutex
	hidden bool
	denied map[[2]string]bool
}

func (p *readSwitch) Authorize(_ context.Context, r policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.denied[[2]string{string(r.Principal.Actor), r.Action}] {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: !p.hidden || r.Action != model.ActionMessageRead}, nil
}

// deny refuses action to actor from now on.
func (p *readSwitch) deny(actor model.ActorID, action string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.denied == nil {
		p.denied = map[[2]string]bool{}
	}
	p.denied[[2]string{string(actor), action}] = true
}

func (p *readSwitch) hide(v bool) {
	p.mu.Lock()
	p.hidden = v
	p.mu.Unlock()
}

// mutationEnv is an engine over the store under test with a channel whose
// adapter declares edit, delete and react, and a send-only one, each with
// an inbox and a conversation routed on it holding one inbound message
// with its provider message reference.
type mutationEnv struct {
	t        *testing.T
	ctx      context.Context
	st       store.Store
	policy   *readSwitch
	engine   uma.Engine
	client   uma.Client
	provider *mutationProvider
	clock    *portstest.FixedClock
	ids      *portstest.SequenceIDs
	n        int
	target   model.Message
	plain    model.Message
	provRef  model.ExternalRef
}

func newMutationEnv(t *testing.T, factory StoreFactory) *mutationEnv {
	t.Helper()
	fx := factory(t)
	env := &mutationEnv{t: t, ctx: t.Context(), st: fx.Store, policy: &readSwitch{}, ids: portstest.NewSequenceIDs(0x3e7), provider: &mutationProvider{}}
	env.clock = portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: env.policy, Clock: env.clock, IDs: portstest.NewSequenceIDs(0x3e8),
		Adapters: []adapter.Factory{
			mutationAdapter{mutationType, []model.Capability{
				model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact, model.CapabilityRefetch,
				model.CapabilityReceipt, model.CapabilityMembership,
			}, env.provider},
			mutationAdapter{sendOnlyType, []model.Capability{model.CapabilitySend}, nil},
		},
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	env.engine = e
	if env.client, err = e.For(model.Principal{Tenant: serviceTenant, Actor: mutationActor}); err != nil {
		t.Fatal(err)
	}
	env.target, env.provRef = env.seed(mutationType, "pm-1")
	env.plain, _ = env.seed(sendOnlyType, "pm-2")
	return env
}

// seed configures a channel of type typ and an inbox on it, then stores a
// conversation routed on the channel and an inbound message holding
// provider message providerID there.
func (env *mutationEnv) seed(typ model.TypeKey, providerID string) (model.Message, model.ExternalRef) {
	env.t.Helper()
	ca, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: env.cmd(""),
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
		Command: env.cmd(""),
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
	conv := model.Conversation{
		Ref: model.ConversationRef{Inbox: inbox, ID: env.id()}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		ReplyRoute: model.ExternalRef{Channel: ch, Namespace: "com.example.conformance", Kind: model.RefKindRoom, ID: "room-1"},
		Revision:   "cv1",
	}
	ref := model.ProviderMessageRef(ch, providerID)
	m := model.Message{
		Ref: model.MessageRef{Conversation: conv.Ref, ID: env.id()}, Audience: model.Audience{Visibility: model.Public},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "original"}}}, External: []model.ExternalRef{ref},
		Revision: "m1",
	}
	env.put(func(tx store.Tx) error {
		if err := tx.PutConversation(env.ctx, conv, ""); err != nil {
			return err
		}
		return tx.PutMessage(env.ctx, m, "")
	})
	return m, ref
}

func (env *mutationEnv) put(fn func(store.Tx) error) {
	env.t.Helper()
	if err := env.st.Transact(env.ctx, serviceTenant, fn); err != nil {
		env.t.Fatalf("seed: %v", err)
	}
}

func (env *mutationEnv) id() model.ID {
	id, err := env.ids.New(env.ctx)
	if err != nil {
		env.t.Fatal(err)
	}
	return id
}

func (env *mutationEnv) cmd(rev model.Revision) model.Command {
	env.n++
	return model.Command{Call: model.Call{Correlation: "corr-mut"}, Operation: model.OperationID(fmt.Sprintf("op-mut-%d", env.n)), ExpectedRevision: rev}
}

func (env *mutationEnv) get(ref model.OperationRef) model.Operation {
	env.t.Helper()
	op, err := env.client.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		env.t.Fatalf("Operations.Get: %v", err)
	}
	return op
}

// settle moves an operation to state as an executor would.
func (env *mutationEnv) settle(ref model.OperationRef, state model.DeliveryState) {
	env.t.Helper()
	env.put(func(tx store.Tx) error {
		rec, err := tx.Operation(env.ctx, ref)
		if err != nil {
			return err
		}
		next := rec.Operation.Clone()
		next.State, next.Revision = state, rec.Operation.Revision+"-settled"
		return tx.UpdateOperation(env.ctx, next, rec.Operation.Revision, store.Fence{})
	})
}

func (env *mutationEnv) now() time.Time { return env.clock.Now().UTC() }
