package uma_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

// textType is a text messaging channel that can start conversations: its
// sends reach any number, one that never wrote in included.
const textType model.TypeKey = "com.example.text"

const textNS = "com.example.text.number"

type textAdapter struct{ sent chan adapter.SendRequest }

func (textAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: textType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: "com.example.text-config", Version: 1},
		Capabilities:        []model.Capability{model.CapabilitySend, model.CapabilityStartConversation},
		IdentityNamespaces:  []string{textNS},
		Exchange:            adapter.Exchange{MessageIDs: adapter.MessageIDsProvider},
	}
}
func (textAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a textAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return textInstance(a), nil
}

type textInstance textAdapter

func (textInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilitySend, Available: true}, {Name: model.CapabilityStartConversation, Available: true},
	}, nil
}
func (textInstance) Close(context.Context) error { return nil }
func (i textInstance) Send(_ context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	i.sent <- r
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(r.Route.Channel, "sms-1"),
	}}, nil
}

// A host reminds a customer who never wrote in: Start claims the
// customer's identity, creates the conversation routed to their number and
// queues the first message; a worker delivers it on that route.
func ExampleConversations_Start() {
	ctx := context.Background()
	text := textAdapter{sent: make(chan adapter.SendRequest, 1)}
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{text},
		Clock:    clock,
		IDs:      portstest.NewSequenceIDs(1),
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = e.Close(ctx) }()
	host := model.Principal{Tenant: "acme", Actor: "reminders"}
	c, err := e.For(host)
	if err != nil {
		panic(err)
	}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "remind"}, Operation: model.OperationID(op)}
	}
	ch, err := c.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{Command: cmd("channel"), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: "acme"}, Type: textType, Configuration: model.Extension{Name: "com.example.text-config", Version: 1},
	}})
	if err != nil {
		panic(err)
	}
	channel, _ := ch.Operation.Result.AsChannel()
	lock := true
	ib, err := c.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{Command: cmd("inbox"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: "acme"}, Channel: channel, Name: "texts", Destinations: []string{"+15550001"},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		panic(err)
	}
	inbox, _ := ib.Operation.Result.AsInbox()

	customer := model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: "+15550123"}
	acc, err := c.Conversations().Start(ctx, model.StartConversation{
		Command: cmd("remind-ada-2026-10-01"), Inbox: inbox, Recipient: customer,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow at 10."}}},
	})
	if err != nil {
		panic(err)
	}
	conv, err := c.Conversations().Get(ctx, model.Lookup[model.ConversationRef]{
		Call: model.Call{Correlation: "remind"}, Ref: acc.Operation.ResultMessage.Conversation,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(acc.Operation.State, conv.Status, conv.ReplyRoute.ID)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- e.Runtime().Run(runCtx, uma.RuntimeCall{Principal: host, Call: model.Call{Correlation: "worker"}},
			uma.WorkerOptions{Owner: "worker-1", Poll: time.Millisecond})
	}()
	for {
		op, err := c.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "remind"}, Ref: acc.Operation.Ref})
		if err != nil {
			panic(err)
		}
		if op.State != model.Queued && op.State != model.RetryPending {
			fmt.Println(op.State)
			break
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		panic(err)
	}
	sent := <-text.sent
	fmt.Println(sent.Route.ID, sent.Content.Parts[0].Text, sent.Thread == nil)
	// Output:
	// queued open +15550123
	// provider_accepted
	// +15550123 Your appointment is tomorrow at 10. true
}
