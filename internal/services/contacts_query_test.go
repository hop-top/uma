package services_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// book seeds contacts, identities and link history over a service fixture.
type book struct {
	*svc
	heads map[model.IdentityRef]model.ID
}

func newBook(t *testing.T) *book {
	t.Helper()
	return &book{svc: newSvc(t), heads: map[model.IdentityRef]model.ID{}}
}

func (b *book) inbox1() model.InboxRef { return b.Conv.Ref.Inbox }
func (b *book) inbox2() model.InboxRef { return b.svc.inbox2.Ref }

func (b *book) contact(kind string) model.Contact {
	b.T.Helper()
	c := model.Contact{Ref: model.ContactRef{Tenant: tenantA, ID: b.id()}, Kind: kind, Revision: "c1"}
	b.put(func(tx store.Tx) error { return tx.PutContact(b.Ctx, c, "") })
	return c
}

func (b *book) identity(in model.InboxRef, on model.ContactRef) model.Identity {
	b.T.Helper()
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: in, ID: b.id()}, Contact: on, Namespace: "svc.user", SourceID: "U-" + string(b.id()),
		Kind: model.ContactKindPerson, LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
	}
	key := model.IdentityKey{Inbox: in, Namespace: id.Namespace, SourceID: id.SourceID}
	b.put(func(tx store.Tx) error {
		_, _, err := tx.ClaimIdentity(b.Ctx, key, id)
		return err
	})
	return id
}

// link appends a link record moving id to contact to, and advances id.
func (b *book) link(id *model.Identity, to model.ContactRef, class model.LinkClass) model.IdentityLink {
	b.T.Helper()
	rec := model.IdentityLink{
		ID: b.id(), Tenant: tenantA, Identity: id.Ref,
		IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID},
		Class:       class, From: id.Contact, To: to, Supersedes: b.heads[id.Ref],
		Evidence: model.Evidence{
			Verifier: model.EvidenceKindAttestation, Assurance: model.LinkAssuranceAsserted,
			Facts: model.Extension{Name: model.SchemaEvidenceAttestation, Version: 1, Data: []byte(`{}`)},
		},
		Assurance: model.LinkAssuranceAsserted, Actor: actor, Operation: model.OperationRef{Tenant: tenantA, ID: b.id()},
		RevisionBefore: id.Revision, RevisionAfter: id.Revision + "+", RecordedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if class == model.LinkClassUnlink {
		rec.Reason, rec.Assurance = "customer_request", model.LinkAssuranceNone
	}
	b.put(func(tx store.Tx) error { return tx.LinkIdentity(b.Ctx, rec) })
	id.Contact, id.Revision, id.LinkAssurance = to, rec.RevisionAfter, rec.Assurance
	b.heads[id.Ref] = rec.ID
	return rec
}

func (b *book) identities(f model.Filter, limit int) []model.Identity {
	b.T.Helper()
	return drain(b.T, func(r model.ListRequest) (model.Page[model.Identity], error) {
		return b.C.Contacts().Identities(b.Ctx, r)
	}, f, limit)
}

func (b *book) contacts(f model.Filter, limit int) []model.Contact {
	b.T.Helper()
	return drain(b.T, func(r model.ListRequest) (model.Page[model.Contact], error) { return b.C.Contacts().List(b.Ctx, r) }, f, limit)
}

func (b *book) history(f model.Filter, limit int) []model.IdentityLink {
	b.T.Helper()
	return drain(b.T, func(r model.ListRequest) (model.Page[model.IdentityLink], error) {
		return b.C.Contacts().History(b.Ctx, r)
	}, f, limit)
}

func identityIDs(ids []model.Identity) []model.ID {
	out := []model.ID{}
	for _, id := range ids {
		out = append(out, id.Ref.ID)
	}
	return out
}

func contactIDs(cs []model.Contact) []model.ID {
	out := []model.ID{}
	for _, c := range cs {
		out = append(out, c.Ref.ID)
	}
	return out
}

func linkIDs(ls []model.IdentityLink) []model.ID {
	out := []model.ID{}
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}

// readsInbox allows action only on resources in inbox, and every other
// action everywhere.
func readsInbox(action string, in model.InboxRef) func(policy.Authorization) bool {
	return func(a policy.Authorization) bool {
		return a.Action != action || (a.Resource.Inbox == in.ID)
	}
}

// requireNoTotals fails when a page carries anything beyond its items,
// cursor and completeness: no count can leak hidden items.
func requireNoTotals[T any](t *testing.T, p model.Page[T]) {
	t.Helper()
	if n := reflect.TypeFor[model.Page[T]]().NumField(); n != 3 {
		t.Fatalf("Page has %d fields; a new field may expose a total", n)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	for k := range keys {
		if k != "items" && k != "next" && k != "complete" {
			t.Fatalf("page exposes %q", k)
		}
	}
}

// Identities(Filter.Contact) returns only identities in inboxes the
// principal may read; the hidden ones leave no trace on any page.
func TestIdentitiesHideUnreadableInboxesWithoutTotals(t *testing.T) {
	b := newBook(t)
	c := b.contact(model.ContactKindPerson)
	seen := b.identity(b.inbox1(), c.Ref)
	b.identity(b.inbox2(), c.Ref)
	b.pol.set(readsInbox(model.ActionIdentityRead, b.inbox1()))
	f := model.Filter{Contact: &c.Ref}
	if got := identityIDs(b.identities(f, 1)); !slices.Equal(got, []model.ID{seen.Ref.ID}) {
		t.Fatalf("Identities = %v, want only %s", got, seen.Ref.ID)
	}
	var after model.Cursor
	for range 10 {
		p, err := b.C.Contacts().Identities(b.Ctx, listReq(f, 1, after))
		if err != nil {
			t.Fatal(err)
		}
		requireNoTotals(t, p)
		for _, id := range p.Items {
			if id.Ref.Inbox != b.inbox1() {
				t.Fatalf("hidden identity %v exposed", id.Ref)
			}
		}
		if p.Complete {
			break
		}
		after = p.Next
	}
	for _, a := range b.pol.seen() {
		if a.Action != model.ActionIdentityRead || a.Resource.Kind != model.KindIdentity || a.Revision != "i1" {
			t.Fatalf("authorization %+v", a)
		}
	}
	b.pol.set(denyAll)
	if got := b.identities(f, 10); len(got) != 0 {
		t.Fatalf("denied Identities = %v", identityIDs(got))
	}
}

// Identities honours Contact and Inboxes together.
func TestIdentitiesFilterByContactAndInboxes(t *testing.T) {
	b := newBook(t)
	one, two := b.contact(model.ContactKindPerson), b.contact(model.ContactKindPerson)
	a1 := b.identity(b.inbox1(), one.Ref)
	a2 := b.identity(b.inbox2(), one.Ref)
	b.identity(b.inbox2(), two.Ref)
	in2 := []model.InboxRef{b.inbox2()}
	if got := identityIDs(b.identities(model.Filter{Contact: &one.Ref, Inboxes: in2}, 10)); !slices.Equal(got, []model.ID{a2.Ref.ID}) {
		t.Fatalf("contact one in inbox two = %v", got)
	}
	if got := identityIDs(b.identities(model.Filter{Contact: &one.Ref}, 1)); !slices.Equal(got, []model.ID{a1.Ref.ID, a2.Ref.ID}) {
		t.Fatalf("contact one = %v", got)
	}
	if got := b.identities(model.Filter{Inboxes: in2}, 10); len(got) != 2 {
		t.Fatalf("inbox two = %v", identityIDs(got))
	}
	if got := b.identities(model.Filter{}, 10); len(got) != 3 {
		t.Fatalf("all = %v", identityIDs(got))
	}
	foreign := one.Ref
	foreign.Tenant = tenantB
	_, err := b.C.Contacts().Identities(b.Ctx, listReq(model.Filter{Contact: &foreign}, 10, ""))
	requireCode(t, err, model.NotFound, "")
}

// List admits a contact under contact.read_all on it, else under
// identity.read on at least one of its identities.
func TestContactsListAdmitsByIdentityOrReadAll(t *testing.T) {
	b := newBook(t)
	byIdentity := b.contact(model.ContactKindPerson)
	b.identity(b.inbox2(), byIdentity.Ref)
	b.identity(b.inbox1(), byIdentity.Ref)
	hidden := b.contact(model.ContactKindPerson)
	b.identity(b.inbox2(), hidden.Ref)
	byReadAll := b.contact(model.ContactKindBot)
	b.identity(b.inbox2(), byReadAll.Ref)
	b.pol.set(func(a policy.Authorization) bool {
		switch a.Action {
		case model.ActionContactReadAll:
			return a.Resource == byReadAll.Ref.Ref()
		case model.ActionIdentityRead:
			return a.Resource.Inbox == b.inbox1().ID
		}
		return false
	})
	if got := contactIDs(b.contacts(model.Filter{}, 1)); !slices.Equal(got, []model.ID{byIdentity.Ref.ID, byReadAll.Ref.ID}) {
		t.Fatalf("List = %v, want [%s %s]", got, byIdentity.Ref.ID, byReadAll.Ref.ID)
	}
	for _, a := range b.pol.seen() {
		if a.Action == model.ActionContactRead {
			t.Fatalf("List asked contact.read: %+v", a)
		}
		if a.Action == model.ActionContactReadAll && a.Revision != "c1" {
			t.Fatalf("read_all authorization %+v", a)
		}
	}
	if got := contactIDs(b.contacts(model.Filter{Kinds: []string{model.ContactKindBot}}, 10)); !slices.Equal(got, []model.ID{byReadAll.Ref.ID}) {
		t.Fatalf("List bots = %v", got)
	}
	p, err := b.C.Contacts().List(b.Ctx, listReq(model.Filter{}, 10, ""))
	if err != nil {
		t.Fatal(err)
	}
	requireNoTotals(t, p)
	b.pol.set(denyAll)
	if got := b.contacts(model.Filter{}, 10); len(got) != 0 {
		t.Fatalf("denied List = %v", contactIDs(got))
	}
}

// A contact without identities is listed only under contact.read_all.
func TestContactsListZeroIdentityOnlyWithReadAll(t *testing.T) {
	b := newBook(t)
	empty := b.contact(model.ContactKindPerson)
	b.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionContactReadAll })
	if got := b.contacts(model.Filter{}, 10); len(got) != 0 {
		t.Fatalf("zero-identity contact listed without read_all: %v", contactIDs(got))
	}
	b.pol.set(nil)
	if got := contactIDs(b.contacts(model.Filter{}, 10)); !slices.Equal(got, []model.ID{empty.Ref.ID}) {
		t.Fatalf("List under read_all = %v", got)
	}
}

// A filter field a contacts query does not honour is rejected.
func TestContactsFiltersRejected(t *testing.T) {
	b := newBook(t)
	c := b.contact(model.ContactKindPerson)
	id := b.identity(b.inbox1(), c.Ref)
	conv := b.Conv.Ref
	cases := map[string]struct {
		run   func() error
		field string
	}{
		"identities statuses": {func() error {
			_, err := b.C.Contacts().Identities(b.Ctx, listReq(model.Filter{Statuses: []string{"open"}}, 1, ""))
			return err
		}, "filter.statuses"},
		"identities kinds": {func() error {
			_, err := b.C.Contacts().Identities(b.Ctx, listReq(model.Filter{Kinds: []string{model.ContactKindBot}}, 1, ""))
			return err
		}, "filter.kinds"},
		"identities identity": {func() error {
			_, err := b.C.Contacts().Identities(b.Ctx, listReq(model.Filter{Identity: &id.Ref}, 1, ""))
			return err
		}, "filter.identity"},
		"list conversation": {func() error {
			_, err := b.C.Contacts().List(b.Ctx, listReq(model.Filter{Conversation: &conv}, 1, ""))
			return err
		}, "filter.conversation"},
		"list contact": {func() error {
			_, err := b.C.Contacts().List(b.Ctx, listReq(model.Filter{Contact: &c.Ref}, 1, ""))
			return err
		}, "filter.contact"},
		"list inboxes": {func() error {
			_, err := b.C.Contacts().List(b.Ctx, listReq(model.Filter{Inboxes: []model.InboxRef{b.inbox1()}}, 1, ""))
			return err
		}, "filter.inboxes"},
		"list identity": {func() error {
			_, err := b.C.Contacts().List(b.Ctx, listReq(model.Filter{Identity: &id.Ref}, 1, ""))
			return err
		}, "filter.identity"},
		"history kinds": {func() error {
			_, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{Contact: &c.Ref, Kinds: []string{"x"}}, 1, ""))
			return err
		}, "filter.kinds"},
		"history inboxes": {func() error {
			_, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{Contact: &c.Ref, Inboxes: []model.InboxRef{b.inbox1()}}, 1, ""))
			return err
		}, "filter.inboxes"},
		"history statuses": {func() error {
			_, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &id.Ref, Statuses: []string{"open"}}, 1, ""))
			return err
		}, "filter.statuses"},
		"history conversation": {func() error {
			_, err := b.C.Contacts().History(b.Ctx, listReq(model.Filter{Identity: &id.Ref, Conversation: &conv}, 1, ""))
			return err
		}, "filter.conversation"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { requireCode(t, tc.run(), model.Validation, tc.field) })
	}
}
