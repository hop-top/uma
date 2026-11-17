package retention_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/retention"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// The contacts horizon is required like every other; a negative one is a
// composition error.
func TestPurgeRequiresContactsHorizon(t *testing.T) {
	clock := portstest.NewFixedClock(time.Unix(enginetest.TestEpochS, 0))
	compose := func(p engine.RetentionPolicy) (*engine.Engine, error) {
		return engine.New(engine.Config{
			Store: memory.New(memory.Options{Clock: clock}), Authorizer: &enginetest.Allow{},
			Clock: clock, IDs: portstest.NewSequenceIDs(1), Retention: p,
		})
	}
	p := every
	p.Contacts = 0
	e, err := compose(p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = retention.Purge(t.Context(), e, principal())
	enginetest.RequireCode(t, err, model.Validation, "Retention.Contacts")
	p.Contacts = -1
	_, err = compose(p)
	enginetest.RequireCode(t, err, model.Validation, "Retention.Contacts: must not be negative")
}

// contacts seeds identities and contacts straight into a harness store.
type contacts struct {
	h     *enginetest.Harness
	inbox model.InboxRef
}

func newContacts(t *testing.T) *contacts {
	t.Helper()
	h := enginetest.NewHarness(t, enginetest.ChatType, func(c *engine.Config) { c.Retention = every })
	return &contacts{h: h, inbox: h.Configure(enginetest.PerContact(false), enginetest.Dest).Ref}
}

func (c *contacts) put() model.ContactRef {
	ref := model.ContactRef{Tenant: enginetest.TenantA, ID: c.h.ID()}
	c.h.Tx(func(tx store.Tx) error {
		return tx.PutContact(c.h.Ctx, model.Contact{Ref: ref, Kind: model.ContactKindPerson, Revision: "c1"}, "")
	})
	return ref
}

// provisional claims an identity on a provisional contact of its own.
func (c *contacts) provisional() model.Identity {
	home := c.put()
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: c.inbox, ID: c.h.ID()}, Contact: home, Namespace: "test.user", SourceID: "U-" + string(c.h.ID()),
		Kind: model.ContactKindPerson, LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
	}
	c.h.Tx(func(tx store.Tx) error {
		_, _, err := tx.ClaimIdentity(c.h.Ctx, model.IdentityKey{Inbox: c.inbox, Namespace: id.Namespace, SourceID: id.SourceID}, id)
		return err
	})
	return id
}

// move appends a link record moving id to contact to.
func (c *contacts) move(id *model.Identity, to model.ContactRef, supersedes model.ID) model.ID {
	rec := model.IdentityLink{
		ID: c.h.ID(), Tenant: enginetest.TenantA, Identity: id.Ref,
		IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID},
		Class:       model.LinkClassLink, From: id.Contact, To: to, Supersedes: supersedes,
		Evidence: model.Evidence{
			Verifier: model.EvidenceKindAttestation, Assurance: model.LinkAssuranceAsserted,
			Facts: model.Extension{Name: model.SchemaEvidenceAttestation, Version: 1, Data: []byte(`{}`)},
		},
		Assurance: model.LinkAssuranceAsserted, Actor: enginetest.Actor,
		Operation:      model.OperationRef{Tenant: enginetest.TenantA, ID: c.h.ID()},
		RevisionBefore: id.Revision, RevisionAfter: id.Revision + "+", RecordedAt: c.h.Clock.Now(),
	}
	if supersedes != "" {
		rec.Class = model.LinkClassRelink
	}
	c.h.Tx(func(tx store.Tx) error { return tx.LinkIdentity(c.h.Ctx, rec) })
	id.Contact, id.Revision = to, rec.RevisionAfter
	return rec.ID
}

func (c *contacts) exists(t *testing.T, ref model.ContactRef) bool {
	t.Helper()
	err := c.h.St.View(c.h.Ctx, enginetest.TenantA, func(r store.Reader) error {
		_, err := r.GetContact(c.h.Ctx, ref)
		return err
	})
	if err != nil && !model.IsCode(err, model.NotFound) {
		t.Fatal(err)
	}
	return err == nil
}

func (c *contacts) links(t *testing.T, id model.IdentityRef) int {
	t.Helper()
	var n int
	c.h.View(func(r store.Reader) error {
		p, err := r.IdentityLinks(c.h.Ctx, store.LinkFilter{Identity: &id}, model.PageRequest{Limit: 10})
		n = len(p.Items)
		return err
	})
	return n
}

// Purge deletes a contact no identity points at once the latest link off
// it is past the contacts horizon, counts it in the report, and keeps the
// link history naming it.
func TestPurgeDeletesOrphanContactsAndReports(t *testing.T) {
	c := newContacts(t)
	x := c.provisional()
	left, target := x.Contact, c.put()
	c.move(&x, target, "")
	c.h.Clock.Advance(2 * horizon)

	rep, err := retention.Purge(c.h.Ctx, c.h.E, principal())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if rep.Contacts != 1 {
		t.Fatalf("report = %+v, want 1 contact purged", rep)
	}
	if c.exists(t, left) {
		t.Error("orphaned contact survived the purge")
	}
	if !c.exists(t, target) {
		t.Error("linked contact purged")
	}
	if n := c.links(t, x.Ref); n != 1 {
		t.Errorf("link history = %d records, want it kept", n)
	}
	if rep2, err := retention.Purge(c.h.Ctx, c.h.E, principal()); err != nil || rep2.Contacts != 0 {
		t.Fatalf("second purge = %+v, %v; want no contact", rep2, err)
	}
}

// Purge keeps, whatever their age, a provisional contact no link ever
// left, a contact an identity points at again, and an orphan whose latest
// link off it is inside the horizon.
func TestPurgeKeepsLinkedAndProvisionalContacts(t *testing.T) {
	c := newContacts(t)
	prov := c.provisional()
	back := c.provisional()
	home, away := back.Contact, c.put()
	head := c.move(&back, away, "")
	c.move(&back, home, head)
	fresh := c.provisional()
	c.h.Clock.Advance(2 * horizon)
	freshHome := fresh.Contact
	c.move(&fresh, c.put(), "")

	rep, err := retention.Purge(c.h.Ctx, c.h.E, principal())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	// away was left before the horizon and holds no identity: the only
	// contact purged.
	if rep.Contacts != 1 || c.exists(t, away) {
		t.Fatalf("report = %+v, want only the contact back left", rep)
	}
	for name, ref := range map[string]model.ContactRef{"provisional": prov.Contact, "relinked": home, "fresh orphan": freshHome} {
		if !c.exists(t, ref) {
			t.Errorf("%s contact purged", name)
		}
	}
}
