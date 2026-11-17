package uma_test

import (
	"context"
	"fmt"
	"time"

	"hop.top/uma"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The contacts examples run a support desk on the memory store. Ada wrote
// once through the web chat and once by email, and Grace once by email, so
// ingress left three identities, each on its own provisional contact:
// ada-web, ada-mail and grace. newDesk writes that state directly, standing
// in for ingress.

const deskTenant model.TenantID = "acme"

var (
	supervisor = model.Principal{Tenant: deskTenant, Actor: "supervisor"}
	triage     = model.Principal{Tenant: deskTenant, Actor: "triage"}
)

type desk struct {
	ctx      context.Context
	store    store.Store
	engine   uma.Engine
	seq      *portstest.SequenceIDs
	web      model.InboxRef
	mail     model.InboxRef
	adaWeb   model.Identity
	adaMail  model.Identity
	grace    model.Identity
	contacts map[model.ContactRef]string
	ops      int
}

// deskConfig composes the engine. There is no default verifier: a host
// that links on attestation alone registers one under
// model.EvidenceKindAttestation. conformance.AttestationVerifier stands in
// for the host's own (see the examples module for one written from
// scratch).
func deskConfig(st store.Store, web model.InboxRef) uma.Config {
	return uma.Config{
		Store:      st,
		Authorizer: deskPolicy{web: web},
		IdentityVerifiers: map[string]policy.IdentityVerifier{
			model.EvidenceKindAttestation: conformance.AttestationVerifier{},
		},
		Clock: portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)),
		IDs:   portstest.NewSequenceIDs(1),
	}
}

// deskPolicy is the host authorizer. The supervisor may do anything, but
// every actor needs verified evidence to unlink. Triage works the web inbox
// only: it reads identities and their history there and links them, and
// never holds contact.read_all.
type deskPolicy struct{ web model.InboxRef }

func (p deskPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	allow := policy.Decision{Allow: true, PolicyVersion: "desk-1", ReasonCode: "desk_rule"}
	if a.Action == model.ActionIdentityUnlink && a.Evidence == nil {
		return policy.Decision{}, nil
	}
	switch a.Principal.Actor {
	case supervisor.Actor:
		return allow, nil
	case triage.Actor:
		if a.Resource.Kind != model.KindIdentity || a.Resource.Inbox != p.web.ID {
			return policy.Decision{}, nil
		}
		switch a.Action {
		case model.ActionIdentityRead, model.ActionIdentityHistory, model.ActionIdentityLink:
			return allow, nil
		}
	}
	return policy.Decision{}, nil
}

func newDesk() *desk {
	d := &desk{
		ctx: context.Background(), store: memory.New(memory.Options{}),
		seq: portstest.NewSequenceIDs(2), contacts: map[model.ContactRef]string{},
	}
	channel := model.ChannelRef{Tenant: deskTenant, ID: d.id()}
	d.web = model.InboxRef{Tenant: deskTenant, ID: d.id()}
	d.mail = model.InboxRef{Tenant: deskTenant, ID: d.id()}
	d.seed(func(tx store.Tx) error {
		if err := tx.PutChannel(d.ctx, model.Channel{Ref: channel, Type: "com.example.desk", Revision: "r1"}, ""); err != nil {
			return err
		}
		for _, in := range []model.InboxRef{d.web, d.mail} {
			if err := tx.PutInbox(d.ctx, model.Inbox{Ref: in, Channel: channel, Revision: "r1"}, ""); err != nil {
				return err
			}
		}
		return nil
	})
	d.adaWeb = d.arrive(d.web, "ada-web")
	d.adaMail = d.arrive(d.mail, "ada-mail")
	d.grace = d.arrive(d.mail, "grace")
	e, err := uma.New(deskConfig(d.store, d.web))
	if err != nil {
		panic(err)
	}
	d.engine = e
	return d
}

func (d *desk) id() model.ID {
	id, err := d.seq.New(d.ctx)
	if err != nil {
		panic(err)
	}
	return id
}

func (d *desk) seed(fn func(store.Tx) error) {
	if err := d.store.Transact(d.ctx, deskTenant, fn); err != nil {
		panic(err)
	}
}

// arrive stores what ingress stores for a first message: a person identity
// in inbox on a new provisional contact, named name.
func (d *desk) arrive(in model.InboxRef, name string) model.Identity {
	c := model.Contact{Ref: model.ContactRef{Tenant: deskTenant, ID: d.id()}, Kind: model.ContactKindPerson, Revision: "r1"}
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: in, ID: d.id()}, Contact: c.Ref,
		Namespace: "com.example.desk-user", SourceID: name, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	d.seed(func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(d.ctx, model.IdentityKey{Inbox: in, Namespace: id.Namespace, SourceID: name}, id); err != nil {
			return err
		}
		return tx.PutContact(d.ctx, c, "")
	})
	d.contacts[c.Ref] = name
	return id
}

func (d *desk) client(p model.Principal) uma.Client {
	c, err := d.engine.For(p)
	if err != nil {
		panic(err)
	}
	return c
}

// command is a fresh operation expected at the identity's revision.
func (d *desk) command(id model.Identity) model.Command {
	d.ops++
	return model.Command{
		Call: model.Call{Correlation: "desk"}, Operation: model.OperationID(fmt.Sprintf("op-%d", d.ops)),
		ExpectedRevision: id.Revision,
	}
}

// attestation is the evidence a caller passes: the verifier key and a
// single-use reference. The verifier, not the caller, returns the facts and
// the assurance.
func attestation(ref string) model.Evidence {
	return model.Evidence{Verifier: model.EvidenceKindAttestation, Reference: ref}
}

// identity reads ref back through Contacts.Identities as the supervisor.
func (d *desk) identity(ref model.IdentityRef) model.Identity {
	p, err := d.client(supervisor).Contacts().Identities(d.ctx, model.ListRequest{
		Call: model.Call{Correlation: "desk"}, Filter: model.Filter{Inboxes: []model.InboxRef{ref.Inbox}},
	})
	if err != nil {
		panic(err)
	}
	for _, id := range p.Items {
		if id.Ref == ref {
			return id
		}
	}
	panic("identity not listed")
}

// mustLink links id to the contact to as the supervisor and returns id as
// it now stands.
func (d *desk) mustLink(id model.Identity, to model.ContactRef, ref string) model.Identity {
	_, err := d.client(supervisor).Contacts().Link(d.ctx, model.LinkIdentity{
		Command: d.command(id), Identity: id.Ref, Contact: to, Evidence: attestation(ref),
	})
	if err != nil {
		panic(err)
	}
	return d.identity(id.Ref)
}

func (d *desk) name(c model.ContactRef) string {
	if n, ok := d.contacts[c]; ok {
		return n
	}
	return "a fresh contact"
}

// Link moves Ada's email identity onto her web chat contact. The verifier
// registered for the evidence kind vouches for the evidence; the
// authorizer then decides identity.link on the identity with the target
// contact as destination. ExpectedRevision is the identity's revision:
// replaying the same operation returns the stored result, and a new
// operation at a revision that has moved on is model.Conflict.
func ExampleContacts_Link() {
	d := newDesk()
	contacts := d.client(supervisor).Contacts()

	req := model.LinkIdentity{
		Command:  d.command(d.adaMail),
		Identity: d.adaMail.Ref,
		Contact:  d.adaWeb.Contact,
		Evidence: attestation("ticket-4711"),
	}
	acc, err := contacts.Link(d.ctx, req)
	if err != nil {
		panic(err)
	}
	fmt.Println("link:", acc.Operation.State)
	fmt.Println("result is the identity:", *acc.Operation.Result == d.adaMail.Ref.Ref())

	again, err := contacts.Link(d.ctx, req)
	if err != nil {
		panic(err)
	}
	fmt.Println("same operation replayed:", again.Replayed)

	now := d.identity(d.adaMail.Ref)
	fmt.Println("email identity on:", d.name(now.Contact), "at", now.LinkAssurance)

	stale := req
	stale.Command = d.command(d.adaMail) // new operation, old revision
	_, err = contacts.Link(d.ctx, stale)
	fmt.Println("stale revision:", model.CodeOf(err))
	// Output:
	// link: committed
	// result is the identity: true
	// same operation replayed: true
	// email identity on: ada-web at asserted
	// stale revision: conflict
}

// Unlink detaches an identity to a fresh contact the engine allocates. The
// reason code is required and recorded. Evidence is optional; when given,
// it is verified for the unlink intent and reaches the authorizer as
// policy.Authorization.Evidence and Assurance. Here host policy requires
// it: unlinking without evidence is refused. Evidence shows authority to
// detach, never that the two identities are different people.
func ExampleContacts_Unlink() {
	d := newDesk()
	grace := d.mustLink(d.grace, d.adaWeb.Contact, "ticket-4712") // linked to the wrong person
	contacts := d.client(supervisor).Contacts()

	_, err := contacts.Unlink(d.ctx, model.UnlinkIdentity{
		Command: d.command(grace), Identity: grace.Ref, Reason: "wrong_person",
	})
	fmt.Println("without evidence:", model.CodeOf(err))

	acc, err := contacts.Unlink(d.ctx, model.UnlinkIdentity{
		Command: d.command(grace), Identity: grace.Ref, Reason: "wrong_person",
		Evidence: attestation("ticket-4713"),
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("with evidence:", acc.Operation.State)

	now := d.identity(grace.Ref)
	fmt.Println("now on:", d.name(now.Contact), "at", now.LinkAssurance)
	// Output:
	// without evidence: unauthorized
	// with evidence: committed
	// now on: a fresh contact at none
}

// History pages an identity's link records in append order. Here ada-mail
// is folded into ada-web with one link and linked back (see
// ExampleContacts_Merge for merging a whole contact). Records of identities in
// inboxes the principal may not read are left out without trace.
func ExampleContacts_History() {
	d := newDesk()
	merged := d.mustLink(d.adaMail, d.adaWeb.Contact, "ticket-4711")
	d.mustLink(merged, d.adaMail.Contact, "ticket-4714") // undo the merge

	byIdentity := model.Filter{Identity: &d.adaMail.Ref}
	page, err := d.client(supervisor).Contacts().History(d.ctx, model.ListRequest{
		Call: model.Call{Correlation: "desk"}, Filter: byIdentity,
	})
	if err != nil {
		panic(err)
	}
	for _, r := range page.Items {
		fmt.Printf("%s: %s -> %s (%s, %s)\n", r.Class, d.name(r.From), d.name(r.To), r.Assurance, r.Evidence.Reference)
	}

	hidden, err := d.client(triage).Contacts().History(d.ctx, model.ListRequest{
		Call: model.Call{Correlation: "desk"}, Filter: byIdentity,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("triage, web inbox only, sees:", len(hidden.Items))
	// Output:
	// link: ada-mail -> ada-web (asserted, ticket-4711)
	// relink: ada-web -> ada-mail (asserted, ticket-4714)
	// triage, web inbox only, sees: 0
}

// List returns the contacts the principal may read: through
// contact.read_all, or through identity.read on at least one of the
// contact's identities. After the merge, ada-mail has no identity left, so
// only contact.read_all lists it. A link grants no access: triage reads the
// web inbox only and never sees Grace.
func ExampleContacts_List() {
	d := newDesk()
	d.mustLink(d.adaMail, d.adaWeb.Contact, "ticket-4711")

	for _, p := range []model.Principal{supervisor, triage} {
		page, err := d.client(p).Contacts().List(d.ctx, model.ListRequest{Call: model.Call{Correlation: "desk"}})
		if err != nil {
			panic(err)
		}
		fmt.Print(p.Actor, ":")
		for _, c := range page.Items {
			fmt.Print(" ", d.name(c.Ref))
		}
		fmt.Println()
	}
	// Output:
	// supervisor: ada-web ada-mail grace
	// triage: ada-web
}
