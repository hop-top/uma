package conformance

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// searchLabel creates a tenant label named name.
func (env *serviceEnv) searchLabel(name string) model.LabelRef {
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
	return ref
}

// searchLabelled creates a label named name and assigns it to the suite
// conversation.
func (env *serviceEnv) searchLabelled(name string) model.LabelRef {
	env.t.Helper()
	ref := env.searchLabel(name)
	env.labelConversation(env.conv.Ref, ref)
	return ref
}

func (env *serviceEnv) labelConversation(conv model.ConversationRef, labels ...model.LabelRef) {
	env.t.Helper()
	if _, err := env.client.Conversations().AddLabels(env.ctx, model.ConversationLabels{Command: env.cmd(""), Conversation: conv, Labels: labels}); err != nil {
		env.t.Fatalf("AddLabels: %v", err)
	}
}

func (env *serviceEnv) unlabelConversation(conv model.ConversationRef, labels ...model.LabelRef) {
	env.t.Helper()
	if _, err := env.client.Conversations().RemoveLabels(env.ctx, model.ConversationLabels{Command: env.cmd(""), Conversation: conv, Labels: labels}); err != nil {
		env.t.Fatalf("RemoveLabels: %v", err)
	}
}

// searchMessageIn stores a public message in conv.
func (env *serviceEnv) searchMessageIn(conv model.ConversationRef, text string) model.Message {
	env.t.Helper()
	m := env.searchValue(model.Public, searchAt, text)
	m.Ref.Conversation = conv
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") })
	return m
}

// searchPage reads one page of Messages.List.
func (env *serviceEnv) searchPage(f model.Filter, limit int, after model.Cursor) model.Page[model.Message] {
	env.t.Helper()
	p, err := env.client.Messages().List(env.ctx, serviceList(f, limit, after))
	if err != nil {
		env.t.Fatalf("Messages.List: %v", err)
	}
	return p
}

// searchLabelNeedsConversationRead: a label selector needs conversation.read
// on the message's conversation. A reader who holds message.read but not
// conversation.read lists no label-matched message, and the page reads as
// one matching nothing: empty, complete, no Next. The same reader still
// lists the message without the selector.
func searchLabelNeedsConversationRead(t *testing.T, env *serviceEnv) {
	vip := env.searchLabelled("vip")
	m := env.searchMessage(model.Public, searchAt, "invoice sent")
	sel := model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}}
	textSel := model.Filter{Text: "invoice", Labels: sel.Labels}
	wantSearch(t, "labelled", env.messages(sel, 1), m)
	wantSearch(t, "labelled text", env.messages(textSel, 1), m)

	env.policy.denyOn(model.ActionConversationRead, env.conv.Ref.Ref())
	for name, f := range map[string]model.Filter{"labels": sel, "labels and text": textSel} {
		if p := env.searchPage(f, 10, ""); len(p.Items) != 0 || !p.Complete || p.Next != "" {
			t.Fatalf("%s page without conversation.read = %+v; want empty, complete, without Next", name, p)
		}
	}
	// The selector alone is gated: the message stays readable without it.
	wantSearch(t, "unfiltered", env.messages(model.Filter{Text: "invoice"}, 1), m)
}

// searchLabelSelectors: Any, All and None select messages by the labels
// their conversation holds, alone and with text, and a label held by
// no conversation selects nothing.
func searchLabelSelectors(t *testing.T, env *serviceEnv) {
	vip, urgent, unused := env.searchLabel("vip"), env.searchLabel("urgent"), env.searchLabel("unused")
	both := env.conversation(env.inbox, model.ConversationOpen)
	plain := env.conversation(env.inbox, model.ConversationOpen)
	env.labelConversation(env.conv.Ref, vip)
	env.labelConversation(both.Ref, vip, urgent)
	a := env.searchMessageIn(env.conv.Ref, "invoice")
	b := env.searchMessageIn(both.Ref, "invoice")
	c := env.searchMessageIn(plain.Ref, "invoice")
	d := env.searchMessageIn(both.Ref, "greeting")
	for _, tc := range []struct {
		name string
		f    model.Filter
		want []model.Message
	}{
		{"any vip", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}}, []model.Message{a, b, d}},
		{"any vip or urgent", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{vip, urgent}}}, []model.Message{a, b, d}},
		{"all vip urgent", model.Filter{Labels: &model.LabelSelector{All: []model.LabelRef{vip, urgent}}}, []model.Message{b, d}},
		{"none urgent", model.Filter{Labels: &model.LabelSelector{None: []model.LabelRef{urgent}}}, []model.Message{a, c}},
		{"any vip none urgent", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{vip}, None: []model.LabelRef{urgent}}}, []model.Message{a}},
		{"unused", model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{unused}}}, nil},
		{"any vip and text", model.Filter{Text: "invoice", Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}}, []model.Message{a, b}},
		{"all vip urgent and text", model.Filter{Text: "greeting", Labels: &model.LabelSelector{All: []model.LabelRef{vip, urgent}}}, []model.Message{d}},
	} {
		for _, limit := range []int{1, 50} {
			wantSearch(t, tc.name, env.messages(tc.f, limit), tc.want...)
		}
	}
}

// searchLabelChangeBetweenPages: labels join the conversation's current
// labels, never a copy on the message: a label removed after one page is
// read hides its conversation's later messages from the next page, and a
// label added after it shows them.
func searchLabelChangeBetweenPages(t *testing.T, env *serviceEnv) {
	vip := env.searchLabel("vip")
	other := env.conversation(env.inbox, model.ConversationOpen)
	third := env.conversation(env.inbox, model.ConversationOpen)
	env.labelConversation(env.conv.Ref, vip)
	env.labelConversation(other.Ref, vip)
	first := env.searchMessageIn(env.conv.Ref, "ticket")
	env.searchMessageIn(other.Ref, "ticket")
	late := env.searchMessageIn(third.Ref, "ticket")
	f := model.Filter{Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}}
	p := env.searchPage(f, 1, "")
	wantSearch(t, "first page", pageIDs(p), first)
	env.unlabelConversation(other.Ref, vip)
	env.labelConversation(third.Ref, vip)
	var got []model.ID
	for after := p.Next; ; {
		p = env.searchPage(f, 1, after)
		got = append(got, pageIDs(p)...)
		if p.Complete {
			break
		}
		after = p.Next
	}
	wantSearch(t, "later pages", got, late)
}

func pageIDs(p model.Page[model.Message]) []model.ID {
	ids := make([]model.ID, 0, len(p.Items))
	for _, m := range p.Items {
		ids = append(ids, m.Ref.ID)
	}
	return ids
}
