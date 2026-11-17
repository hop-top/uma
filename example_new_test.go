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

// A host composes an engine: a store, an authorizer, the adapters it
// registers, a clock and an ID source. New validates the composition and
// performs no I/O; Describe lists what the registered adapters declare, and
// ValidateChannel checks a channel configuration without creating anything.
func ExampleNew() {
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

	for _, d := range e.Describe() {
		fmt.Println(d.Type, d.Capabilities)
	}
	issues := e.ValidateChannel("com.example.unknown", model.Extension{Name: "com.example.text-config", Version: 1})
	fmt.Println(len(issues) > 0)
	// Output:
	// com.example.text [send start_conversation]
	// true
}

// A host replies in a conversation. The reply is queued as a durable
// operation and a worker (Runtime.Run) delivers it on the conversation's
// route; InReplyTo records which message it answers.
func ExampleMessages_Reply() {
	ctx := context.Background()
	text := textAdapter{sent: make(chan adapter.SendRequest, 2)}
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
	host := model.Principal{Tenant: "acme", Actor: "support"}
	c, err := e.For(host)
	if err != nil {
		panic(err)
	}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "reply"}, Operation: model.OperationID(op)}
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
	first, err := c.Conversations().Start(ctx, model.StartConversation{
		Command: cmd("start"), Inbox: inbox,
		Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: "+15550123"},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Your appointment is tomorrow at 10."}}},
	})
	if err != nil {
		panic(err)
	}

	acc, err := c.Messages().Reply(ctx, model.Reply{
		Command:      cmd("reply"),
		Conversation: first.Operation.ResultMessage.Conversation,
		InReplyTo:    first.Operation.ResultMessage,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "See you then."}}},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(acc.Operation.State)

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- e.Runtime().Run(runCtx, uma.RuntimeCall{Principal: host, Call: model.Call{Correlation: "worker"}},
			uma.WorkerOptions{Owner: "worker-1", Poll: time.Millisecond})
	}()
	var texts []string
	for len(texts) < 2 {
		texts = append(texts, (<-text.sent).Content.Parts[0].Text)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		panic(err)
	}
	fmt.Println(texts)
	// Output:
	// queued
	// [Your appointment is tomorrow at 10. See you then.]
}
