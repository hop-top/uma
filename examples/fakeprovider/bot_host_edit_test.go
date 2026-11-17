package fakeprovider_test

import (
	"slices"
	"testing"

	"hop.top/uma/companions/bot"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
)

// A bot corrects its own reply: the edit goes through the worker the host
// already runs, reaches the provider message the reply created, and the
// stored reply changes once the provider applied it.
func TestBotEditsItsReply(t *testing.T) {
	h := newBotHost(t)
	h.start()
	b := h.newBot(h.client(botActor))
	h.say(botChanA, "@helper hi")
	h.handled(b, bot.Handled, bot.RouteMention)
	h.delivered(botChanA, "watching")

	// The provider call returns before the send settles: wait for the reply
	// to carry its provider reference, which also fixes its revision.
	var reply model.Message
	inEventually(t, "reply settled", func() bool {
		page, err := h.client(botActor).Messages().List(h.ctx, model.ListRequest{
			Call: model.Call{Correlation: "bot-list"}, Filter: model.Filter{Inboxes: []model.InboxRef{botInboxA}},
			Page: model.PageRequest{Limit: 50},
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Items {
			if m.Origin != nil && len(m.External) > 0 {
				reply = m
				return true
			}
		}
		return false
	})
	a, err := h.client(botActor).Messages().Edit(h.ctx, model.EditMessage{
		Command: model.Command{Call: model.Call{Correlation: "bot-edit"}, Operation: "bot-edit", ExpectedRevision: reply.Revision},
		Message: reply.Ref, Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "watching, corrected"}}},
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	inEventually(t, "edit settled", func() bool {
		op, err := h.client(botActor).Operations().Get(h.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "bot-get"}, Ref: a.Operation.Ref})
		return err == nil && op.State == model.ProviderAccepted
	})
	var edits []fakeprovider.Call
	for _, c := range h.factory.Script(botChanA).Calls() {
		if c.Method == fakeprovider.MethodEdit {
			edits = append(edits, c)
		}
	}
	if len(edits) != 1 || !edits[0].Applied || edits[0].Effect.Target == nil || !slices.Contains(reply.External, *edits[0].Effect.Target) || bot.Text(edits[0].Content) != "watching, corrected" {
		t.Fatalf("provider edits = %+v, want one applied edit of the reply", edits)
	}
	got, err := h.client(botActor).Messages().Get(h.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "bot-get"}, Ref: reply.Ref})
	if err != nil || bot.Text(got.Content) != "watching, corrected" || got.LastEdit == nil || got.LastEdit.Actor != botActor.Actor {
		t.Fatalf("reply = %+v, %v; want it corrected by the bot", got, err)
	}
	h.drain()
}
