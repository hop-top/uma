package conformance

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

const (
	capsDeskActor     model.ActorID = "caps-desk-1"
	capsExternalActor model.ActorID = "caps-external-1"
)

// capsPolicy grants the operator everything, a desk actor everything but
// channel.read, and an external participant conversation.read at the
// public audience only.
func capsPolicy(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	switch a.Principal.Actor {
	case capsDeskActor:
		return policy.Decision{Allow: a.Action != model.ActionChannelRead}, nil
	case capsExternalActor:
		public := a.Audience.Visibility == "" || a.Audience.Visibility == model.Public
		return policy.Decision{Allow: a.Action == model.ActionConversationRead && public}, nil
	}
	return policy.Decision{Allow: true}, nil
}

// serviceConversationCapabilities checks Conversations.Capabilities over
// the store: a desk actor without channel.read reads the statuses of the
// conversation's channel, equal to Configuration.Capabilities for it, an
// external participant that may read public content is refused, and a
// conversation in another inbox than the reference names is not found.
func serviceConversationCapabilities(t *testing.T, factory StoreFactory) {
	fx := factory(t)
	env := &mutationEnv{t: t, ctx: t.Context(), st: fx.Store, ids: portstest.NewSequenceIDs(0x5c0), provider: &mutationProvider{}}
	env.clock = portstest.NewFixedClock(fx.Now())
	e, err := uma.New(uma.Config{
		Store: fx.Store, Authorizer: policy.AuthorizeFunc(capsPolicy), Clock: env.clock, IDs: portstest.NewSequenceIDs(0x5c1),
		Adapters: []adapter.Factory{
			mutationAdapter{mutationType, []model.Capability{model.CapabilitySend, model.CapabilityEdit}, env.provider},
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
	msg, _ := env.seed(mutationType, "pm-caps")
	conv := msg.Ref.Conversation
	as := func(actor model.ActorID) uma.Client {
		c, err := e.For(model.Principal{Tenant: serviceTenant, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	call := model.Call{Correlation: "corr-caps"}
	t.Run("DeskWithoutChannelRead", func(t *testing.T) {
		got, err := as(capsDeskActor).Conversations().Capabilities(env.ctx, model.Lookup[model.ConversationRef]{Call: call, Ref: conv})
		if err != nil {
			t.Fatalf("Capabilities as desk: %v", err)
		}
		inbox, err := env.client.Configuration().Inboxes(env.ctx, model.ListRequest{Call: call, Filter: model.Filter{}})
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(inbox.Items, func(in model.Inbox) bool { return in.Ref == conv.Inbox })
		if i < 0 {
			t.Fatalf("seeded inbox missing from %+v", inbox.Items)
		}
		want, err := env.client.Configuration().Capabilities(env.ctx, model.Lookup[model.ChannelRef]{Call: call, Ref: inbox.Items[i].Channel})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) || len(got) != 2 {
			t.Fatalf("conversation statuses %+v, want the channel's %+v", got, want)
		}
		for k := range got {
			if got[k].Name != want[k].Name || got[k].Available != want[k].Available || got[k].Reason != want[k].Reason {
				t.Fatalf("conversation statuses %+v, want the channel's %+v", got, want)
			}
		}
	})
	t.Run("ExternalRefused", func(t *testing.T) {
		ext := as(capsExternalActor)
		if _, err := ext.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: call, Ref: conv}); err != nil {
			t.Fatalf("external Get: %v", err)
		}
		_, err := ext.Conversations().Capabilities(env.ctx, model.Lookup[model.ConversationRef]{Call: call, Ref: conv})
		if !model.IsCode(err, model.Unauthorized) {
			t.Fatalf("external Capabilities: %v, want unauthorized", err)
		}
	})
	t.Run("OtherInboxNotFound", func(t *testing.T) {
		moved := conv
		moved.Inbox.ID = env.id()
		_, err := as(capsDeskActor).Conversations().Capabilities(env.ctx, model.Lookup[model.ConversationRef]{Call: call, Ref: moved})
		if !model.IsCode(err, model.NotFound) {
			t.Fatalf("Capabilities under another inbox: %v, want not found", err)
		}
	})
}
