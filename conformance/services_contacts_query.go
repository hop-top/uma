package conformance

import (
	"fmt"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

func (env *contactsEnv) conversation(ref model.ConversationRef) model.Conversation {
	env.t.Helper()
	var c model.Conversation
	env.view(func(r store.Reader) (err error) { c, err = r.GetConversation(env.ctx, ref); return err })
	return c
}

func (env *contactsEnv) conversationsIn(in model.InboxRef) int {
	env.t.Helper()
	var n int
	env.view(func(r store.Reader) error {
		p, err := r.Conversations(env.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 100}})
		n = len(p.Items)
		return err
	})
	return n
}

// contactsRouteAndSelectionUnchanged links bob (inbox two) and alice's
// second identity (inbox one) to alice's contact. No conversation
// changes; a reply on bob's conversation goes out on bob's route; the
// next message from each identity joins that identity's own
// conversation, never one of another identity of the contact.
func contactsRouteAndSelectionUnchanged(t *testing.T, env *contactsEnv) {
	a, ma := env.arrive(env.one, "alice")
	a2, ma2 := env.arrive(env.one, "alice-mobile")
	b, mb := env.arrive(env.two, "bob")
	convs := map[string]model.Conversation{
		"alice": env.conversation(ma.Ref.Conversation), "alice-mobile": env.conversation(ma2.Ref.Conversation),
		"bob": env.conversation(mb.Ref.Conversation),
	}
	contacts := map[model.ContactRef]model.Contact{}
	for _, id := range []model.Identity{a, a2, b} {
		c, _ := env.contact(id.Contact)
		contacts[c.Ref] = c
	}
	env.mustLink(&b, a.Contact, "")
	env.mustLink(&a2, a.Contact, "")
	for name, c := range convs {
		got := env.conversation(c.Ref)
		if got.ReplyRoute != c.ReplyRoute || !slices.Equal(got.Participants, c.Participants) || got.Revision != c.Revision {
			t.Fatalf("%s's conversation changed: %+v, was %+v", name, got, c)
		}
	}
	for _, c := range contacts {
		if got, err := env.contact(c.Ref); err != nil || !sameContact(got, c) {
			t.Fatalf("contact %s = %+v, %v; want it unchanged", c.Ref.ID, got, err)
		}
	}
	if convs["bob"].ReplyRoute != env.user("bob") {
		t.Fatalf("bob's route %v, want his sender", convs["bob"].ReplyRoute)
	}

	acc, err := env.client.Messages().Reply(env.ctx, model.Reply{
		Command: env.cmd(""), Conversation: convs["bob"].Ref,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi bob"}}},
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	if op := env.run(acc.Operation.Ref, settled); op.State != model.ProviderAccepted && op.State != model.Committed && op.State != model.Delivered {
		t.Fatalf("reply settled %+v", op)
	}
	sent := env.chat.sent()
	if len(sent) != 1 || sent[0].Route != env.user("bob") {
		t.Fatalf("sends = %+v, want one on bob's route", sent)
	}

	for _, s := range []struct {
		in     model.InboxRef
		sender string
	}{{env.two, "bob"}, {env.one, "alice-mobile"}, {env.one, "alice"}} {
		_, m := env.arrive(s.in, s.sender)
		if want := convs[s.sender].Ref; m.Ref.Conversation != want {
			t.Fatalf("next message from %s joined %v, want its own conversation %v", s.sender, m.Ref.Conversation, want)
		}
	}
	if n1, n2 := env.conversationsIn(env.one), env.conversationsIn(env.two); n1 != 2 || n2 != 1 {
		t.Fatalf("conversations = %d in inbox one, %d in inbox two; want 2 and 1", n1, n2)
	}
	for _, m := range []model.Message{ma, ma2, mb} {
		var got model.Message
		env.view(func(r store.Reader) (err error) { got, err = r.GetMessage(env.ctx, m.Ref); return err })
		if got.Author != m.Author || got.Revision != m.Revision {
			t.Fatalf("message %s rewritten", m.Ref.ID)
		}
	}
}

// contactsNoCrossInboxRead links identities of both inboxes to one
// contact and denies every read in inbox two: the link opens nothing
// there, through messages, conversations, identities, history or the
// contact list.
func contactsNoCrossInboxRead(t *testing.T, env *contactsEnv) {
	a, _ := env.arrive(env.one, "alice")
	b, mb := env.arrive(env.two, "bob")
	c, _ := env.arrive(env.two, "carol")
	left := b.Contact
	shared := env.person()
	env.mustLink(&a, shared.Ref, "")
	env.mustLink(&b, shared.Ref, "")
	inTwo := denyIn(env.two, model.ActionMessageRead, model.ActionConversationRead, model.ActionIdentityRead, model.ActionIdentityHistory)
	env.pol.set(func(x policy.Authorization) bool { return inTwo(x) || x.Action == model.ActionContactReadAll })
	_, err := env.client.Messages().Get(env.ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "c"}, Ref: mb.Ref})
	serviceCode(t, err, model.Unauthorized)
	for _, m := range drain(env, env.client.Messages().List, model.Filter{}, 10) {
		if m.Ref.Conversation.Inbox == env.two {
			t.Fatalf("message %s of inbox two listed", m.Ref.ID)
		}
	}
	for _, c := range drain(env, env.client.Conversations().List, model.Filter{}, 10) {
		if c.Ref.Inbox == env.two {
			t.Fatalf("conversation %s of inbox two listed", c.Ref.ID)
		}
	}
	if got := env.identities(model.Filter{Contact: &shared.Ref}, 10); len(got) != 1 || got[0].Ref != a.Ref {
		t.Fatalf("identities of the shared contact = %+v, want alice only", got)
	}
	for _, r := range env.history(model.Filter{Contact: &shared.Ref}, 10) {
		if r.Identity == b.Ref {
			t.Fatal("history of the shared contact exposes bob")
		}
	}
	if got := env.history(model.Filter{Identity: &b.Ref}, 10); len(got) != 0 {
		t.Fatalf("bob's history = %+v", got)
	}
	got := env.contacts(model.Filter{}, 10)
	if !slices.ContainsFunc(got, func(c model.Contact) bool { return c.Ref == shared.Ref }) {
		t.Fatal("shared contact not listed through alice")
	}
	for _, hidden := range []model.ContactRef{left, c.Contact} {
		if slices.ContainsFunc(got, func(c model.Contact) bool { return c.Ref == hidden }) {
			t.Fatalf("contact %s listed without a readable identity", hidden.ID)
		}
	}
}

// contactsListHidesUnreadableInboxes: with identity.read denied in inbox
// two and no contact.read_all, a contact is listed through a readable
// identity only; zero-identity contacts and contacts of hidden identities
// are left out, with no total. contact.read_all lists every contact,
// zero-identity ones included, and Kinds narrows them. A page whose scan
// held only hidden items is empty, incomplete and carries a cursor.
func contactsListHidesUnreadableInboxes(t *testing.T, env *contactsEnv) {
	a, _ := env.arrive(env.one, "alice")
	b, _ := env.arrive(env.two, "bob")
	c, _ := env.arrive(env.two, "carol")
	shared := env.person()
	env.mustLink(&a, shared.Ref, "")
	env.mustLink(&b, shared.Ref, "")
	bot := model.Contact{Ref: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, Kind: model.ContactKindBot, Revision: "c1"}
	env.put(func(tx store.Tx) error { return tx.PutContact(env.ctx, bot, "") })
	member := func(x policy.Authorization) bool {
		return x.Action == model.ActionContactReadAll || (x.Action == model.ActionIdentityRead && x.Resource.Inbox == env.two.ID)
	}
	env.pol.set(member)
	if got := refs(env.contacts(model.Filter{}, 1)); !slices.Equal(got, []model.ContactRef{shared.Ref}) {
		t.Fatalf("member lists %v, want only the contact of alice", got)
	}
	if got := env.identities(model.Filter{Contact: &shared.Ref}, 1); len(got) != 1 || got[0].Ref != a.Ref {
		t.Fatalf("member sees identities %+v of the shared contact", got)
	}
	if got := env.identities(model.Filter{Contact: &shared.Ref, Inboxes: []model.InboxRef{env.two}}, 1); len(got) != 0 {
		t.Fatalf("member sees %+v in inbox two", got)
	}
	env.pol.set(func(x policy.Authorization) bool {
		return x.Action == model.ActionContactReadAll && x.Resource.ID == bot.Ref.ID
	})
	if got := refs(env.contacts(model.Filter{Kinds: []string{model.ContactKindBot}}, 1)); len(got) != 0 {
		t.Fatalf("zero-identity bot contact listed without contact.read_all: %v", got)
	}
	env.pol.set(nil)
	all := refs(env.contacts(model.Filter{}, 2))
	want := []model.ContactRef{a.Contact, b.Contact, c.Contact, shared.Ref, bot.Ref}
	for _, w := range want {
		if !slices.Contains(all, w) {
			t.Fatalf("read_all lists %v, missing %v", all, w)
		}
	}
	if len(all) != len(want) {
		t.Fatalf("read_all lists %d contacts, want %d", len(all), len(want))
	}
	if got := refs(env.contacts(model.Filter{Kinds: []string{model.ContactKindBot}}, 2)); !slices.Equal(got, []model.ContactRef{bot.Ref}) {
		t.Fatalf("Kinds bot = %v", got)
	}
	if got := env.identities(model.Filter{Contact: &shared.Ref, Inboxes: []model.InboxRef{env.two}}, 1); len(got) != 1 || got[0].Ref != b.Ref {
		t.Fatalf("identities of the shared contact in inbox two = %+v", got)
	}

	// More hidden identities than one call scans, then a visible one.
	const hidden = 510
	var hiddenContacts []model.ContactRef
	for i := 0; i < hidden; i += 100 {
		env.put(func(tx store.Tx) error {
			for j := i; j < min(i+100, hidden); j++ {
				ct := model.Contact{Ref: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, Kind: model.ContactKindPerson, Revision: "c1"}
				src := fmt.Sprint("hidden-", j)
				id := model.Identity{
					Ref: model.IdentityRef{Inbox: env.two, ID: env.id()}, Contact: ct.Ref, Namespace: chatNS, SourceID: src,
					Kind: model.ContactKindPerson, LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
				}
				if _, _, err := tx.ClaimIdentity(env.ctx, model.IdentityKey{Inbox: env.two, Namespace: chatNS, SourceID: src}, id); err != nil {
					return err
				}
				if err := tx.PutContact(env.ctx, ct, ""); err != nil {
					return err
				}
				hiddenContacts = append(hiddenContacts, ct.Ref)
			}
			return nil
		})
	}
	v, _ := env.arrive(env.one, "visible")
	env.pol.set(member)
	p, err := env.client.Contacts().Identities(env.ctx, model.ListRequest{
		Call: model.Call{Correlation: "c"}, Filter: model.Filter{Inboxes: []model.InboxRef{env.two}}, Page: model.PageRequest{Limit: 10},
	})
	if err != nil || len(p.Items) != 0 || p.Complete || p.Next == "" {
		t.Fatalf("page over hidden identities = %d items, complete %v, next %q, %v; want empty, incomplete, with a cursor", len(p.Items), p.Complete, p.Next, err)
	}
	requireNoTotals(t, p)
	if got := env.identities(model.Filter{}, 10); len(got) != 2 || got[0].Ref != a.Ref || got[1].Ref != v.Ref {
		t.Fatalf("member identities = %+v, want alice and the visible one", got)
	}
	if got := refs(env.contacts(model.Filter{}, 10)); !slices.Equal(got, []model.ContactRef{shared.Ref, v.Contact}) {
		t.Fatalf("member contacts = %v", got)
	}
}

func refs(cs []model.Contact) []model.ContactRef {
	out := []model.ContactRef{}
	for _, c := range cs {
		out = append(out, c.Ref)
	}
	return out
}

// contactsQueriesFilterAndPage: unsupported filters are model.Validation,
// History needs an identity or a contact and another tenant's identity is
// not found; paging a contact's identities while one relinks away skips it
// without error or duplicate.
func contactsQueriesFilterAndPage(t *testing.T, env *contactsEnv) {
	req := func(f model.Filter, limit int, after model.Cursor) model.ListRequest {
		return model.ListRequest{Call: model.Call{Correlation: "c"}, Filter: f, Page: model.PageRequest{Limit: limit, After: after}}
	}
	a, _ := env.arrive(env.one, "alice")
	conv := model.ConversationRef{Inbox: env.one, ID: env.id()}
	_, err := env.client.Contacts().List(env.ctx, req(model.Filter{Contact: &a.Contact}, 10, ""))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().Identities(env.ctx, req(model.Filter{Identity: &a.Ref}, 10, ""))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().Identities(env.ctx, req(model.Filter{Conversation: &conv}, 10, ""))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().History(env.ctx, req(model.Filter{}, 10, ""))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &a.Ref, Kinds: []string{"x"}}, 10, ""))
	serviceCode(t, err, model.Validation)
	foreign := model.IdentityRef{Inbox: model.InboxRef{Tenant: "conformance-other", ID: env.one.ID}, ID: a.Ref.ID}
	_, err = env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &foreign}, 10, ""))
	serviceCode(t, err, model.NotFound)

	shared := env.person()
	var ids []model.Identity
	for _, s := range []string{"m-1", "m-2", "m-3"} {
		id, _ := env.arrive(env.one, s)
		env.mustLink(&id, shared.Ref, "")
		ids = append(ids, id)
	}
	f := model.Filter{Contact: &shared.Ref}
	p, err := env.client.Contacts().Identities(env.ctx, req(f, 1, ""))
	if err != nil || len(p.Items) != 1 || p.Items[0].Ref != ids[0].Ref || p.Complete {
		t.Fatalf("first page %+v, %v", p, err)
	}
	moving := ids[1]
	env.mustLink(&moving, env.person().Ref, "")
	var rest []model.Identity
	for after := p.Next; ; {
		p, err := env.client.Contacts().Identities(env.ctx, req(f, 1, after))
		if err != nil {
			t.Fatalf("page after the move: %v", err)
		}
		rest = append(rest, p.Items...)
		if p.Complete {
			break
		}
		after = p.Next
	}
	if len(rest) != 1 || rest[0].Ref != ids[2].Ref {
		t.Fatalf("pages after the move = %+v, want only the identity still on the contact", rest)
	}
	if got := env.history(model.Filter{Contact: &shared.Ref}, 2); len(got) != 4 || got[3].Identity != moving.Ref || got[3].From != shared.Ref {
		t.Fatalf("history of the contact = %+v, want three links and the move away", got)
	}
}

// contactsHistoryChainAndCursors pages History by identity, by contact as
// From or To, and by both, in append order with each record superseding
// the identity's previous one through link, relink and unlink. A cursor
// is bound to its query and filter, the identity included.
func contactsHistoryChainAndCursors(t *testing.T, env *contactsEnv) {
	a, _ := env.arrive(env.one, "alice")
	o, _ := env.arrive(env.two, "other")
	mid, end := env.person(), env.person()
	start := a.Contact
	env.mustLink(&a, mid.Ref, "h-1")
	env.mustLink(&o, mid.Ref, "h-2")
	env.mustLink(&a, end.Ref, "h-3")
	if _, err := env.unlink(env.unlinkReq(a, "customer_request", model.Evidence{})); err != nil {
		t.Fatal(err)
	}
	got := env.history(model.Filter{Identity: &a.Ref}, 1)
	if len(got) != 3 || got[0].Class != model.LinkClassLink || got[1].Class != model.LinkClassRelink || got[2].Class != model.LinkClassUnlink ||
		got[0].Supersedes != "" || got[1].Supersedes != got[0].ID || got[2].Supersedes != got[1].ID ||
		got[0].From != start || got[1].From != mid.Ref || got[2].From != end.Ref || got[2].Reason != "customer_request" {
		t.Fatalf("history of alice = %+v", got)
	}
	ohist := env.history(model.Filter{Identity: &o.Ref}, 10)
	if ids := linkIDs(env.history(model.Filter{Contact: &mid.Ref}, 2)); !slices.Equal(ids, []model.ID{got[0].ID, ohist[0].ID, got[1].ID}) {
		t.Fatalf("history of mid = %v", ids)
	}
	if ids := linkIDs(env.history(model.Filter{Identity: &a.Ref, Contact: &mid.Ref}, 10)); !slices.Equal(ids, []model.ID{got[0].ID, got[1].ID}) {
		t.Fatalf("history of alice on mid = %v", ids)
	}
	if ids := linkIDs(env.history(model.Filter{Identity: &a.Ref, Contact: &end.Ref}, 10)); !slices.Equal(ids, []model.ID{got[1].ID, got[2].ID}) {
		t.Fatalf("history of alice on end = %v", ids)
	}

	req := func(f model.Filter, after model.Cursor) model.ListRequest {
		return model.ListRequest{Call: model.Call{Correlation: "c"}, Filter: f, Page: model.PageRequest{Limit: 1, After: after}}
	}
	p, err := env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &a.Ref}, ""))
	if err != nil || p.Complete || p.Next == "" {
		t.Fatalf("first history page %+v, %v", p, err)
	}
	_, err = env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &o.Ref}, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &a.Ref, Contact: &mid.Ref}, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().Identities(env.ctx, req(model.Filter{}, p.Next))
	serviceCode(t, err, model.Validation)
	_, err = env.client.Contacts().List(env.ctx, req(model.Filter{}, p.Next))
	serviceCode(t, err, model.Validation)
	if _, err := env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &a.Ref}, p.Next)); err != nil {
		t.Fatalf("same filter: %v", err)
	}
	ip, err := env.client.Contacts().Identities(env.ctx, req(model.Filter{}, ""))
	if err != nil || ip.Next == "" {
		t.Fatalf("identities page %+v, %v", ip, err)
	}
	_, err = env.client.Contacts().History(env.ctx, req(model.Filter{Identity: &a.Ref}, ip.Next))
	serviceCode(t, err, model.Validation)
}

func linkIDs(ls []model.IdentityLink) []model.ID {
	out := []model.ID{}
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}
