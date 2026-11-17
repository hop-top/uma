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

// A host sends a reminder, but the connection drops before the acceptance
// arrives: the command may or may not have been accepted, and the
// operation ref is unknown. Lookup by the key the command claimed (kind
// and target from Conversations.Start's "Operation key" line, and the
// command's operation ID) recovers the operation; a key never claimed is
// model.NotFound, so the command can be sent again under it.
func ExampleOperations_Lookup() {
	ctx := context.Background()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: memory.New(memory.Options{Clock: clock}),
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters: []adapter.Factory{textAdapter{sent: make(chan adapter.SendRequest, 1)}},
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

	// The host persists the operation ID before sending; the acceptance
	// is lost.
	const sent model.OperationID = "remind-ada-2026-10-01"
	_, _ = c.Conversations().Start(ctx, model.StartConversation{
		Command: cmd(sent), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: "+15550123"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow at 10."}}},
	})

	find := func(id model.OperationID) {
		op, err := c.Operations().Lookup(ctx, model.OperationLookup{
			Call: model.Call{Correlation: "recover"}, Kind: model.ActionConversationStart, Target: inbox.Ref(), ID: id,
		})
		switch {
		case model.IsCode(err, model.NotFound):
			fmt.Println(id, "not found: send the command again under", id)
		case err != nil:
			panic(err)
		default:
			fmt.Println(id, op.State, op.ResultMessage.Conversation.Inbox == inbox)
		}
	}
	find(sent)
	find("remind-ada-2026-10-02")
	// Output:
	// remind-ada-2026-10-01 queued true
	// remind-ada-2026-10-02 not found: send the command again under remind-ada-2026-10-02
}
