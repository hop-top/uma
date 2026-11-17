package services_test

import (
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// History authorizes every record with identity.history on its identity at
// the record's resulting revision; records of identities in unreadable
// inboxes vanish from contact-filtered pages.
func TestHistoryAuthorizesPerRecord(t *testing.T) {
	b := newBook(t)
	shared := b.contact(model.ContactKindPerson)
	a := b.identity(b.inbox1(), b.contact(model.ContactKindPerson).Ref)
	hidden := b.identity(b.inbox2(), b.contact(model.ContactKindPerson).Ref)
	ra := b.link(&a, shared.Ref, model.LinkClassLink)
	b.link(&hidden, shared.Ref, model.LinkClassLink)
	b.pol.set(readsInbox(model.ActionIdentityHistory, b.inbox1()))
	f := model.Filter{Contact: &shared.Ref}
	if got := linkIDs(b.history(f, 1)); !slices.Equal(got, []model.ID{ra.ID}) {
		t.Fatalf("History(contact) = %v, want [%s]", got, ra.ID)
	}
	checks := b.pol.seen()
	if len(checks) != 2 {
		t.Fatalf("authorizations = %d, want one per record", len(checks))
	}
	for _, c := range checks {
		if c.Action != model.ActionIdentityHistory || c.Resource.Kind != model.KindIdentity || !strings.HasSuffix(string(c.Revision), "+") {
			t.Fatalf("authorization %+v", c)
		}
	}
	if checks[0].Resource != a.Ref.Ref() || checks[0].Revision != ra.RevisionAfter {
		t.Fatalf("first authorization %+v, want identity %v at %s", checks[0], a.Ref, ra.RevisionAfter)
	}
	p, err := b.C.Contacts().History(b.Ctx, listReq(f, 10, ""))
	if err != nil {
		t.Fatal(err)
	}
	requireNoTotals(t, p)
	if got := b.history(model.Filter{Identity: &hidden.Ref}, 10); len(got) != 0 {
		t.Fatalf("hidden identity history = %v", linkIDs(got))
	}
	b.pol.set(denyAll)
	if got := b.history(f, 10); len(got) != 0 {
		t.Fatalf("denied History = %v", linkIDs(got))
	}
}

// History needs an identity or a contact to select records.
func TestHistoryRequiresIdentityOrContact(t *testing.T) {
	b := newBook(t)
	_, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{}, 10, ""))
	requireCode(t, err, model.Validation, "identity or filter.contact")
	if len(b.pol.seen()) != 0 {
		t.Fatal("policy consulted for an invalid request")
	}
	foreign := model.IdentityRef{Inbox: model.InboxRef{Tenant: tenantB, ID: inboxOne}, ID: b.id()}
	_, err = b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &foreign}, 10, ""))
	requireCode(t, err, model.NotFound, "")
}

// History pages in append order; each record supersedes the identity's
// previous one; Identity and Contact together select both.
func TestHistoryOrderAndSupersedesChain(t *testing.T) {
	b := newBook(t)
	start, mid, end := b.contact(model.ContactKindPerson), b.contact(model.ContactKindPerson), b.contact(model.ContactKindPerson)
	a := b.identity(b.inbox1(), start.Ref)
	other := b.identity(b.inbox2(), start.Ref)
	r1 := b.link(&a, mid.Ref, model.LinkClassLink)
	o1 := b.link(&other, mid.Ref, model.LinkClassLink)
	r2 := b.link(&a, end.Ref, model.LinkClassRelink)
	r3 := b.link(&a, b.contact(model.ContactKindPerson).Ref, model.LinkClassUnlink)

	got := b.history(model.Filter{Identity: &a.Ref}, 1)
	if !slices.Equal(linkIDs(got), []model.ID{r1.ID, r2.ID, r3.ID}) {
		t.Fatalf("History(identity) = %v", linkIDs(got))
	}
	if got[0].Supersedes != "" || got[1].Supersedes != r1.ID || got[2].Supersedes != r2.ID {
		t.Fatalf("supersedes chain = %q %q %q", got[0].Supersedes, got[1].Supersedes, got[2].Supersedes)
	}
	if got[2].Class != model.LinkClassUnlink || got[2].Reason != "customer_request" || got[2].From != end.Ref {
		t.Fatalf("unlink record %+v", got[2])
	}
	if ids := linkIDs(b.history(model.Filter{Contact: &mid.Ref}, 2)); !slices.Equal(ids, []model.ID{r1.ID, o1.ID, r2.ID}) {
		t.Fatalf("History(contact mid) = %v", ids)
	}
	if ids := linkIDs(b.history(model.Filter{Identity: &a.Ref, Contact: &mid.Ref}, 10)); !slices.Equal(ids, []model.ID{r1.ID, r2.ID}) {
		t.Fatalf("History(identity, contact mid) = %v", ids)
	}
	// Results are snapshots.
	got[0].Evidence.Facts.Data[0] = 'x'
	if again := b.history(model.Filter{Identity: &a.Ref}, 10); string(again[0].Evidence.Facts.Data) != "{}" {
		t.Fatal("History returned stored memory")
	}
}

// Linking identities of two inboxes to one contact grants no read across
// them: messages, identities and history of the other inbox stay hidden.
func TestLinkedContactGrantsNoCrossInboxMessageRead(t *testing.T) {
	b := newBook(t)
	shared := b.contact(model.ContactKindPerson)
	mine := b.identity(b.inbox1(), b.contact(model.ContactKindPerson).Ref)
	theirs := b.identity(b.inbox2(), b.contact(model.ContactKindPerson).Ref)
	b.link(&mine, shared.Ref, model.LinkClassLink)
	b.link(&theirs, shared.Ref, model.LinkClassLink)
	conv := b.conversation(b.inbox2(), model.ConversationOpen)
	msg := model.Message{
		Ref: model.MessageRef{Conversation: conv.Ref, ID: b.id()}, Audience: model.Audience{Visibility: model.Public},
		Author:     model.Author{Identity: theirs.Ref, Kind: model.ContactKindPerson},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi"}}},
		External:   []model.ExternalRef{{Channel: b.Ch.Ref, Namespace: routeNSpace, Kind: "message", ID: string(b.id())}},
		RecordedAt: b.Clock.Now(), Revision: "m1",
	}
	b.put(func(tx store.Tx) error { return tx.PutMessage(b.Ctx, msg, "") })
	b.pol.set(func(a policy.Authorization) bool { return a.Resource.Inbox != b.inbox2().ID })

	_, err := b.C.Messages().Get(b.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: msg.Ref})
	requireCode(t, err, model.Unauthorized, "")
	msgs := drain(t, func(r model.ListRequest) (model.Page[model.Message], error) { return b.C.Messages().List(b.Ctx, r) }, model.Filter{}, 10)
	if slices.Contains(msgIDs(msgs), msg.Ref.ID) {
		t.Fatal("linked contact exposed a message of another inbox")
	}
	if got := identityIDs(b.identities(model.Filter{Contact: &shared.Ref}, 10)); !slices.Equal(got, []model.ID{mine.Ref.ID}) {
		t.Fatalf("Identities(shared) = %v", got)
	}
	for _, l := range b.history(model.Filter{Contact: &shared.Ref}, 10) {
		if l.Identity == theirs.Ref {
			t.Fatal("history exposed another inbox's identity")
		}
	}
}

// A History cursor is bound to its identity filter; filters without an
// identity encode exactly as before it existed, so issued cursors of every
// other query stay valid.
func TestContactsCursorsBindIdentityFilter(t *testing.T) {
	b := newBook(t)
	c := b.contact(model.ContactKindPerson)
	a := b.identity(b.inbox1(), c.Ref)
	o := b.identity(b.inbox1(), c.Ref)
	for _, to := range []model.ContactRef{b.contact(model.ContactKindPerson).Ref, c.Ref} {
		b.link(&a, to, model.LinkClassRelink)
		b.link(&o, to, model.LinkClassRelink)
	}
	p, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &a.Ref}, 1, ""))
	if err != nil || p.Complete || p.Next == "" {
		t.Fatalf("first page %+v, %v", p, err)
	}
	_, err = b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &o.Ref}, 1, p.Next))
	requireCode(t, err, model.Validation, "cursor")
	_, err = b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &a.Ref, Contact: &c.Ref}, 1, p.Next))
	requireCode(t, err, model.Validation, "cursor")
	if _, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &a.Ref}, 1, p.Next)); err != nil {
		t.Fatalf("same filter: %v", err)
	}
	ip, err := b.C.Contacts().Identities(b.Ctx, listReq(model.Filter{Contact: &c.Ref}, 1, ""))
	if err != nil || ip.Next == "" {
		t.Fatalf("identities page %+v, %v", ip, err)
	}
	_, err = b.C.Contacts().History(b.Ctx, listReq(model.Filter{Contact: &c.Ref}, 1, ip.Next))
	requireCode(t, err, model.Validation, "cursor")

	in := model.InboxRef{Tenant: tenantA, ID: "inbox-1"}
	conv := model.ConversationRef{Inbox: in, ID: "conv-1"}
	ct := model.ContactRef{Tenant: tenantA, ID: "contact-1"}
	full := model.Filter{Inboxes: []model.InboxRef{in}, Conversation: &conv, Contact: &ct, Kinds: []string{"k"}, Statuses: []string{"open"}}
	golden := map[string]struct {
		f    model.Filter
		want string
	}{
		"empty": {model.Filter{}, "01300130013001300130"},
		"full": {full, "013107696e626f782d3101310874656e616e742d610c636f6e766572736174696f6e07696e626f782d3106636f6e762d31" +
			"01310874656e616e742d6107636f6e746163740009636f6e746163742d310131016b0131046f70656e"},
	}
	for name, g := range golden {
		if got := hex.EncodeToString(services.CanonicalFilter(g.f)); got != g.want {
			t.Fatalf("%s filter encoding changed: %s", name, got)
		}
	}
	who := model.IdentityRef{Inbox: in, ID: "who"}
	withID := full
	withID.Identity = &who
	enc, base := services.CanonicalFilter(withID), services.CanonicalFilter(full)
	if !strings.HasPrefix(string(enc), string(base)) || len(enc) == len(base) {
		t.Fatal("identity not appended last")
	}
	other := model.IdentityRef{Inbox: in, ID: "whom"}
	withID.Identity = &other
	if string(services.CanonicalFilter(withID)) == string(enc) {
		t.Fatal("identity not bound")
	}
}
