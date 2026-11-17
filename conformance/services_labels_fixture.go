package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

const labelActor model.ActorID = "label-staff"

// labelPolicy allows everything except the denied actions.
type labelPolicy struct {
	mu     sync.Mutex
	denied map[string]bool
}

func (p *labelPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return policy.Decision{Allow: !p.denied[a.Action]}, nil
}

// deny replaces the denied actions.
func (p *labelPolicy) deny(actions ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.denied = map[string]bool{}
	for _, a := range actions {
		p.denied[a] = true
	}
}

// labelsEnv is an engine over the store under test with a channel, two
// inboxes on it and one open conversation in each.
type labelsEnv struct {
	t      *testing.T
	ctx    context.Context
	st     store.Store
	policy *labelPolicy
	eng    uma.Engine
	client uma.Client
	ids    *portstest.SequenceIDs
	n      int
	one    model.Conversation
	two    model.Conversation
}

func newLabelsEnv(t *testing.T, factory StoreFactory) *labelsEnv {
	return newLabelsEnvHeld(t, factory, uma.HeldPolicy{})
}

// newLabelsEnvHeld is newLabelsEnv with the local writes held opts in.
func newLabelsEnvHeld(t *testing.T, factory StoreFactory, held uma.HeldPolicy) *labelsEnv {
	t.Helper()
	fx := factory(t)
	env := &labelsEnv{t: t, ctx: t.Context(), st: fx.Store, policy: &labelPolicy{}, ids: portstest.NewSequenceIDs(0x1ab0)}
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: env.policy, Clock: portstest.NewFixedClock(fx.Now()), IDs: portstest.NewSequenceIDs(0x1ab1),
		Adapters: []adapter.Factory{serviceAdapter{}}, Held: held,
	})
	if err != nil {
		t.Fatalf("uma.New: %v", err)
	}
	t.Cleanup(func() { _ = e.Close(context.Background()) })
	env.eng = e
	if env.client, err = e.For(model.Principal{Tenant: serviceTenant, Actor: labelActor}); err != nil {
		t.Fatal(err)
	}
	ca, err := env.client.Configuration().ConfigureChannel(env.ctx, model.ConfigureChannel{
		Command: env.cmd(""),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: serviceTenant}, Type: serviceType, Configuration: model.Extension{Name: serviceSchema, Version: 1}},
	})
	if err != nil {
		t.Fatalf("ConfigureChannel: %v", err)
	}
	ch, err := ca.Operation.Result.AsChannel()
	if err != nil {
		t.Fatal(err)
	}
	env.one = env.conversation(env.inbox(ch, "support"))
	env.two = env.conversation(env.inbox(ch, "sales"))
	return env
}

func (env *labelsEnv) inbox(ch model.ChannelRef, dest string) model.InboxRef {
	env.t.Helper()
	a, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
		Command: env.cmd(""),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: serviceTenant}, Channel: ch, Name: dest, Destinations: []string{dest},
			Selection: model.Selection{Mode: model.ModePerRoom},
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

func (env *labelsEnv) conversation(in model.InboxRef) model.Conversation {
	env.t.Helper()
	id, err := env.ids.New(env.ctx)
	if err != nil {
		env.t.Fatal(err)
	}
	c := model.Conversation{Ref: model.ConversationRef{Inbox: in, ID: id}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Revision: "cv1"}
	if err := env.st.Transact(env.ctx, serviceTenant, func(tx store.Tx) error { return tx.PutConversation(env.ctx, c, "") }); err != nil {
		env.t.Fatalf("seed: %v", err)
	}
	return c
}

func (env *labelsEnv) cmd(rev model.Revision) model.Command {
	env.n++
	return model.Command{Call: model.Call{Correlation: "corr-label"}, Operation: model.OperationID(fmt.Sprintf("op-label-%d", env.n)), ExpectedRevision: rev}
}

func (env *labelsEnv) call() model.Call { return model.Call{Correlation: "corr-label"} }

// label creates a label named name.
func (env *labelsEnv) label(name string) model.Label {
	env.t.Helper()
	a, err := env.client.Labels().Configure(env.ctx, model.ConfigureLabel{
		Command: env.cmd(""), Label: model.Label{Ref: model.LabelRef{Tenant: serviceTenant}, Name: name},
	})
	if err != nil {
		env.t.Fatalf("Configure %q: %v", name, err)
	}
	ref, err := a.Operation.Result.AsLabel()
	if err != nil {
		env.t.Fatal(err)
	}
	l, err := env.client.Labels().Get(env.ctx, model.Lookup[model.LabelRef]{Call: env.call(), Ref: ref})
	if err != nil {
		env.t.Fatalf("Labels.Get: %v", err)
	}
	return l
}

func (env *labelsEnv) add(conv model.ConversationRef, labels ...model.LabelRef) (model.Acceptance, error) {
	return env.client.Conversations().AddLabels(env.ctx, model.ConversationLabels{Command: env.cmd(""), Conversation: conv, Labels: labels})
}

func (env *labelsEnv) remove(conv model.ConversationRef, labels ...model.LabelRef) (model.Acceptance, error) {
	return env.client.Conversations().RemoveLabels(env.ctx, model.ConversationLabels{Command: env.cmd(""), Conversation: conv, Labels: labels})
}

func (env *labelsEnv) get(conv model.ConversationRef) model.Conversation {
	env.t.Helper()
	c, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: env.call(), Ref: conv})
	if err != nil {
		env.t.Fatalf("Conversations.Get: %v", err)
	}
	return c
}

// labelEvent is one stored label event and its payload.
type labelEvent struct {
	model.Event
	payload model.ConversationLabelPayload
}

// events returns the label events stored in the tenant, in log order.
func (env *labelsEnv) events() []labelEvent {
	env.t.Helper()
	var out []labelEvent
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		var after model.Cursor
		for {
			p, err := r.Events(env.ctx, model.Filter{}, after, 100)
			if err != nil {
				return err
			}
			for _, ev := range p.Items {
				if ev.Kind != model.EventConversationLabelAdded && ev.Kind != model.EventConversationLabelRemoved {
					continue
				}
				le := labelEvent{Event: ev}
				if err := json.Unmarshal(ev.Payload.Data, &le.payload); err != nil {
					return err
				}
				out = append(out, le)
			}
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			after = p.Next
		}
	})
	if err != nil {
		env.t.Fatalf("events: %v", err)
	}
	return out
}
