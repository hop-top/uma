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

// A host keeps no history file of what its commands became: it lists the
// operations. Two reminders are started a minute apart, and the host reads
// them back newest first, then only those of one conversation, then only
// those written since the first one.
func ExampleOperations_List() {
	ctx := context.Background()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{textAdapter{sent: make(chan adapter.SendRequest, 2)}},
		Clock:    clock,
		IDs:      portstest.NewSequenceIDs(1),
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = e.Close(ctx) }()
	c, err := e.For(model.Principal{Tenant: "acme", Actor: "reminders"})
	if err != nil {
		panic(err)
	}
	cmd := func(op model.OperationID) model.Command {
		return model.Command{Call: model.Call{Correlation: "remind"}, Operation: op}
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

	remind := func(op model.OperationID, phone string) model.Acceptance {
		a, err := c.Conversations().Start(ctx, model.StartConversation{
			Command: cmd(op), Inbox: inbox,
			Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: phone},
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow at 10."}}},
		})
		if err != nil {
			panic(err)
		}
		return a
	}
	first := remind("remind-ada", "+15550123")
	clock.Advance(time.Minute)
	remind("remind-bob", "+15550124")
	written := clock.Now()

	name := func(op model.Operation) string {
		if op.Ref == first.Operation.Ref {
			return "ada's reminder"
		}
		return "bob's reminder"
	}
	show := func(label string, f model.Filter) {
		page, err := c.Operations().List(ctx, model.ListRequest{Call: model.Call{Correlation: "history"}, Filter: f, Page: model.PageRequest{Limit: 10}})
		if err != nil {
			panic(err)
		}
		fmt.Print(label, ":")
		for _, op := range page.Items {
			fmt.Print(" ", name(op), " ", op.State, ";")
		}
		fmt.Println(" complete:", page.Complete)
	}
	queued := []string{string(model.Queued)}
	show("queued in the inbox", model.Filter{Inboxes: []model.InboxRef{inbox}, Statuses: queued})
	adas := first.Operation.ResultMessage.Conversation
	show("in ada's conversation", model.Filter{Conversation: &adas})
	show("written since bob's", model.Filter{Inboxes: []model.InboxRef{inbox}, Since: &written})

	// Each operation carries the key its command claimed: the kind, target
	// and operation ID that Operations.Lookup takes.
	starts, err := c.Operations().List(ctx, model.ListRequest{
		Call:   model.Call{Correlation: "history"},
		Filter: model.Filter{Kinds: []string{model.ActionConversationStart}},
	})
	if err != nil {
		panic(err)
	}
	for _, op := range starts.Items {
		fmt.Println(op.Key.Kind, op.Key.ID)
	}
	// Output:
	// queued in the inbox: bob's reminder queued; ada's reminder queued; complete: true
	// in ada's conversation: ada's reminder queued; complete: true
	// written since bob's: bob's reminder queued; complete: true
	// conversation.start remind-bob
	// conversation.start remind-ada
}
