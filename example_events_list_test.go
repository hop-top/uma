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

// A host catches up on the event log without a subscription: it lists the
// retained history in pages, then opens a subscription at the last page's
// Next, which delivers exactly what was committed after the list. The
// list holds no cursor or lease of its own: a crash after a page just
// lists that page again.
func ExampleEvents_List() {
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
	c, err := e.For(model.Principal{Tenant: "acme", Actor: "auditor"})
	if err != nil {
		panic(err)
	}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "audit"}, Operation: model.OperationID(op)}
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
	remind := func(op, phone string) {
		_, err := c.Conversations().Start(ctx, model.StartConversation{
			Command: cmd(op), Inbox: inbox,
			Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: phone},
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow at 10."}}},
		})
		if err != nil {
			panic(err)
		}
	}
	remind("remind-ada", "+15550123")

	// Catch up: one scanned event per page, from the retained beginning,
	// then from each page's Next. Complete reports that the page reached
	// the head.
	req := model.ListEvents{Call: model.Call{Correlation: "audit"}, Start: model.StartBeginning, Limit: 1}
	var next model.Cursor
	for {
		page, err := c.Events().List(ctx, req)
		if err != nil {
			panic(err)
		}
		for _, ev := range page.Items {
			fmt.Println("listed", ev.Kind)
		}
		next = page.Next
		if page.Complete {
			break
		}
		req.Start, req.After = model.StartCheckpoint, next
	}

	// Hand off: the subscription starts at the position the list ended at,
	// so it delivers the reminder sent after the list and nothing twice.
	sub, err := c.Events().Open(ctx, model.OpenSubscription{
		Command: cmd("auditor-sub"), Consumer: "auditor", Start: model.StartCheckpoint, After: next,
		PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		panic(err)
	}
	remind("remind-grace", "+15550456")
	batch, err := c.Events().Poll(ctx, model.Poll{Call: model.Call{Correlation: "audit"}, Subscription: sub.Subscription.Ref})
	if err != nil {
		panic(err)
	}
	for _, ev := range batch.Events {
		fmt.Println("delivered", ev.Kind)
	}
	// Output:
	// listed conversation.changed
	// listed message.created
	// delivered conversation.changed
	// delivered message.created
}
