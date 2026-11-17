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

// A host finds the messages that mention a word. Text matches whole,
// case-folded words of the message text, all of them; Since and Before
// bound the message's own time as [Since, Before); Attachments selects by
// attachment parts. The search narrows Messages.List and changes nothing
// else: the order is the recording order, and a page that completes the
// search carries no Next.
func ExampleMessages_List() {
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
	c, err := e.For(model.Principal{Tenant: "acme", Actor: "support"})
	if err != nil {
		panic(err)
	}
	cmd := func(op string) model.Command {
		return model.Command{Call: model.Call{Correlation: "search"}, Operation: model.OperationID(op)}
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
	for i, text := range []string{"Your appointment is tomorrow at 10.", "Your invoice is ready."} {
		_, err := c.Conversations().Start(ctx, model.StartConversation{
			Command: cmd(fmt.Sprintf("start-%d", i)), Inbox: inbox,
			Recipient: model.ExternalRef{Channel: channel, Namespace: textNS, Kind: "phone", ID: fmt.Sprintf("+1555012%d", i)},
			Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
		})
		if err != nil {
			panic(err)
		}
	}

	// find pages the search to its end: a page can be short or empty
	// and still carry Next, so loop until Complete.
	find := func(label string, f model.Filter) {
		req := model.ListRequest{Call: model.Call{Correlation: "search"}, Filter: f}
		for {
			page, err := c.Messages().List(ctx, req)
			if err != nil {
				panic(err)
			}
			for _, m := range page.Items {
				fmt.Printf("%s: %s\n", label, m.Content.Parts[0].Text)
			}
			if page.Complete {
				return
			}
			req.Page.After = page.Next
		}
	}
	noAttachments := false
	later := clock.Now().Add(time.Hour)
	find("text", model.Filter{Text: "APPOINTMENT tomorrow"})
	find("prefix", model.Filter{Text: "appoint"})
	find("future", model.Filter{Text: "invoice", Since: &later})
	find("no attachment", model.Filter{Text: "ready", Attachments: &model.AttachmentFilter{Has: &noAttachments}})
	// Output:
	// text: Your appointment is tomorrow at 10.
	// no attachment: Your invoice is ready.
}
