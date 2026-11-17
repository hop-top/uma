package uma_test

import (
	"context"
	"fmt"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

// reactAdapter is textAdapter that also declares reactions.
type reactAdapter struct{ textAdapter }

func (a reactAdapter) Describe() adapter.Descriptor {
	d := a.textAdapter.Describe()
	d.Capabilities = append(d.Capabilities, model.CapabilityReact)
	return d
}

// A plugin proposes a reaction on the command itself (Command.Hold) instead
// of applying it. The operation is held, with no work queued; a reviewer
// approves it under the operation's current revision and it is queued.
func ExampleOperations_Approve() {
	ctx := context.Background()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{reactAdapter{textAdapter{sent: make(chan adapter.SendRequest, 2)}}},
		Clock:    clock,
		IDs:      portstest.NewSequenceIDs(1),
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = e.Close(ctx) }()
	as := func(actor model.ActorID) uma.Client {
		c, err := e.For(model.Principal{Tenant: "acme", Actor: actor})
		if err != nil {
			panic(err)
		}
		return c
	}
	host, plugin, reviewer := as("host"), as("plugin:triage"), as("reviewer")
	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: "held"}, Operation: op}
	}
	ch, err := host.Configuration().ConfigureChannel(ctx, model.ConfigureChannel{Command: cmd("channel"), Channel: model.Channel{
		Ref: model.ChannelRef{Tenant: "acme"}, Type: textType, Configuration: model.Extension{Name: "com.example.text-config", Version: 1},
	}})
	if err != nil {
		panic(err)
	}
	channel, _ := ch.Operation.Result.AsChannel()
	lock := true
	ib, err := host.Configuration().ConfigureInbox(ctx, model.ConfigureInbox{Command: cmd("inbox"), Inbox: model.Inbox{
		Ref: model.InboxRef{Tenant: "acme"}, Channel: channel, Name: "texts", Destinations: []string{"+15550001"},
		Selection: model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1},
	}})
	if err != nil {
		panic(err)
	}
	inbox, _ := ib.Operation.Result.AsInbox()
	start, err := host.Conversations().Start(ctx, model.StartConversation{
		Command: cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: "+15550123"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow at 10."}}},
	})
	if err != nil {
		panic(err)
	}

	// The plugin's proposal: same command, plus the hold.
	react := cmd("react")
	react.Hold = &model.HoldProposal{
		OnBehalfOf: "host", Reason: "first reaction by this plugin",
		TrustKey: "triage/react", TrustLabels: map[string]string{"plugin": "triage"},
	}
	held, err := plugin.Messages().React(ctx, model.Reaction{Command: react, Message: *start.Operation.ResultMessage, Emoji: "+1"})
	if err != nil {
		panic(err)
	}
	fmt.Println("proposed:", held.Operation.State, held.Operation.Held.Reason)

	approved, err := reviewer.Operations().Approve(ctx, model.DecideHeld{
		Call: model.Call{Correlation: "review"}, Operation: held.Operation.Ref,
		ExpectedRevision: held.Operation.Revision, Note: "looks right",
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("approved:", approved.Operation.State, approved.Decision, "correction:", approved.Correction, "trust key:", approved.TrustKey)
	// Output:
	// proposed: held first reaction by this plugin
	// approved: queued approved correction: false trust key: triage/react
}
