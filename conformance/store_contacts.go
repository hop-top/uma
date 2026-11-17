package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// contactPurgeCases cover Purge of FamilyContacts: a contact goes once it
// holds no identity and the latest link record moving an identity off it
// was written before the horizon, judged by the store clock. A contact
// never left by a link, such as a provisional one, never goes.
var contactPurgeCases = []storeCase{
	{"ContactsPurgeDeletesOrphanPastHorizon", func(t *testing.T, e *env) {
		e.needClock(t)
		x := e.provisional(t)
		target := e.putContact(t)
		left := x.id.Contact
		rec := e.move(t, &x, target)
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)

		got := e.purge(t, store.FamilyContacts, horizon, 10)
		if got.Purged != 1 || !got.Complete {
			t.Fatalf("purge = %+v, want the orphaned contact, complete", got)
		}
		e.wantContact(t, left, false)
		e.wantContact(t, target, true)
		if again := e.purge(t, store.FamilyContacts, horizon, 10); again.Purged != 0 || !again.Complete {
			t.Errorf("repeated purge = %+v, want nothing", again)
		}
		// Link history is never purged and keeps naming the purged contact.
		e.mustView(t, func(r store.Reader) error {
			p, err := r.IdentityLinks(e.ctx, store.LinkFilter{Contact: &left}, page(10))
			if err == nil && (len(p.Items) != 1 || p.Items[0].ID != rec.ID || p.Items[0].From != left) {
				t.Errorf("history of the purged contact = %+v, want the link off it", p.Items)
			}
			return err
		})
	}},
	{"ContactsPurgeSkipsLinkedContacts", func(t *testing.T, e *env) {
		e.needClock(t)
		x := e.provisional(t)
		home, target := x.id.Contact, e.putContact(t)
		e.move(t, &x, target)
		e.move(t, &x, home) // home was left once and holds x again
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)

		got := e.purge(t, store.FamilyContacts, horizon, 10)
		if got.Purged != 1 || !got.Complete {
			t.Fatalf("purge = %+v, want only the contact x left", got)
		}
		e.wantContact(t, home, true)
		e.wantContact(t, target, false)
	}},
	{"ContactsPurgeKeepsFreshOrphan", func(t *testing.T, e *env) {
		e.needClock(t)
		target := e.putContact(t)
		old, fresh, back := e.provisional(t), e.provisional(t), e.provisional(t)
		oldContact, freshContact, backContact := old.id.Contact, fresh.id.Contact, back.id.Contact
		e.move(t, &old, target)
		e.move(t, &back, target)
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		e.move(t, &fresh, target)
		// back's contact was first left before the horizon, but its latest
		// link off it is recent.
		e.move(t, &back, backContact)
		e.move(t, &back, target)

		got := e.purge(t, store.FamilyContacts, horizon, 10)
		if got.Purged != 1 || !got.Complete {
			t.Fatalf("purge = %+v, want only the orphan left before the horizon", got)
		}
		e.wantContact(t, oldContact, false)
		e.wantContact(t, freshContact, true)
		e.wantContact(t, backContact, true)
		e.wantContact(t, target, true)
	}},
	{"ContactsPurgeNeverTouchesProvisional", func(t *testing.T, e *env) {
		e.needClock(t)
		x := e.provisional(t)
		bare := e.putContact(t)
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)

		if got := e.purge(t, store.FamilyContacts, horizon, 10); got.Purged != 0 || !got.Complete {
			t.Fatalf("purge = %+v, want nothing: no contact was ever left by a link", got)
		}
		e.wantContact(t, x.id.Contact, true)
		e.wantContact(t, bare, true)
	}},
	{"ContactsPurgeIsBounded", func(t *testing.T, e *env) {
		e.needClock(t)
		target := e.putContact(t)
		var left []model.ContactRef
		for range 3 {
			x := e.provisional(t)
			left = append(left, x.id.Contact)
			e.move(t, &x, target)
		}
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)

		if got := e.purge(t, store.FamilyContacts, horizon, 2); got.Purged != 2 || got.Complete {
			t.Fatalf("bounded purge = %+v, want 2 purged and more left", got)
		}
		if got := e.purge(t, store.FamilyContacts, horizon, 2); got.Purged != 1 || !got.Complete {
			t.Fatalf("second purge = %+v, want the last one, complete", got)
		}
		for _, c := range left {
			e.wantContact(t, c, false)
		}
		e.wantContact(t, target, true)
	}},
}

// linkTombstoneCases cover RedactLinks: an identity's link records lose
// their evidence facts and keep everything else, the evidence reference
// included, so it stays single-use.
var linkTombstoneCases = []storeCase{
	{"LinkFactsTombstoneKeepsReference", func(t *testing.T, e *env) {
		x := e.provisional(t)
		home, target := x.id.Contact, e.putContact(t)
		first := e.moveWith(t, &x, target, "challenge-1")
		second := e.moveWith(t, &x, home, "challenge-2")
		y := e.provisional(t)
		other := e.moveWith(t, &y, target, "challenge-3")

		// A rolled-back tombstone leaves the facts.
		_ = e.txErr(func(tx store.Tx) error {
			if _, err := tx.RedactLinks(e.ctx, x.id.Ref); err != nil {
				return err
			}
			return errRollback
		})
		e.wantLinks(t, x.id.Ref, first, second)

		var n int
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			n, err = tx.RedactLinks(e.ctx, x.id.Ref)
			return err
		})
		if n != 2 {
			t.Errorf("RedactLinks = %d, want 2", n)
		}
		e.wantLinks(t, x.id.Ref, linkTombstone(first), linkTombstone(second))
		e.wantLinks(t, y.id.Ref, other)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetIdentity(e.ctx, x.id.Ref)
			if err == nil && (got.Revision != x.id.Revision || got.Contact != home) {
				t.Errorf("identity after tombstone = rev %q contact %v, want it unchanged", got.Revision, got.Contact)
			}
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			again, err := tx.RedactLinks(e.ctx, x.id.Ref)
			if err == nil && again != 0 {
				t.Errorf("repeated RedactLinks = %d, want 0", again)
			}
			return err
		})
		// The tombstoned reference is still consumed.
		reuse := e.linkRecord(t, x.id, target, x.head, "challenge-1")
		reuse.Class = model.LinkClassRelink
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, reuse) }), model.Conflict)
	}},
	{"LinkTombstoneUnknownIdentityIsNotFound", func(t *testing.T, e *env) {
		x := e.provisional(t)
		redact := func(tenant model.TenantID, ref model.IdentityRef) error {
			return e.s.Transact(e.ctx, tenant, func(tx store.Tx) error {
				_, err := tx.RedactLinks(e.ctx, ref)
				return err
			})
		}
		wantCode(t, redact(tenantA, model.IdentityRef{Inbox: e.inbox, ID: e.id(t)}), model.NotFound)
		wantCode(t, redact(tenantB, x.id.Ref), model.NotFound)
		wantCode(t, redact(tenantA, model.IdentityRef{Inbox: model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}, ID: x.id.Ref.ID}), model.NotFound)
		wantCode(t, redact(tenantA, model.IdentityRef{Inbox: e.inbox}), model.Validation)
		// An identity never linked has nothing to tombstone.
		e.mustTx(t, func(tx store.Tx) error {
			n, err := tx.RedactLinks(e.ctx, x.id.Ref)
			if err == nil && n != 0 {
				t.Errorf("RedactLinks of an unlinked identity = %d, want 0", n)
			}
			return err
		})
	}},
	{"MessageRedactLeavesLinks", func(t *testing.T, e *env) {
		x := e.provisional(t)
		rec := e.moveWith(t, &x, e.putContact(t), "challenge-1")
		ref := model.MessageRef{Conversation: e.putConversation(t), ID: e.id(t)}
		msg := e.message(ref, "m1")
		msg.Author = model.Author{Identity: x.id.Ref, External: e.externalRef("user", "u-1"), Kind: model.ContactKindPerson}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, msg, "") })
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Message: &ref, Expected: "m1", Revision: "m2"})
			return err
		})
		e.wantLinks(t, x.id.Ref, rec)
	}},
}

// linked is an identity and its latest link record, for chaining links.
type linked struct {
	id   model.Identity
	head model.ID
}

// provisional claims an identity on its own provisional contact.
func (e *env) provisional(t *testing.T) linked {
	t.Helper()
	key := e.identityKey("U-" + string(e.id(t)))
	id := e.identity(t, key)
	e.mustTx(t, func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(e.ctx, key, id); err != nil {
			return err
		}
		return tx.PutContact(e.ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "c1"}, "")
	})
	return linked{id: id}
}

// putContact stores a contact no identity points at.
func (e *env) putContact(t *testing.T) model.ContactRef {
	t.Helper()
	ref := model.ContactRef{Tenant: tenantA, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error {
		return tx.PutContact(e.ctx, model.Contact{Ref: ref, Kind: model.ContactKindPerson, Revision: "c1"}, "")
	})
	return ref
}

// move links l's identity to contact without evidence reference.
func (e *env) move(t *testing.T, l *linked, to model.ContactRef) store.LinkRecord {
	t.Helper()
	return e.moveWith(t, l, to, "")
}

// moveWith links l's identity to contact in its own unit, with distinct
// evidence facts, and advances l.
func (e *env) moveWith(t *testing.T, l *linked, to model.ContactRef, evidenceRef string) store.LinkRecord {
	t.Helper()
	rec := e.linkRecord(t, l.id, to, l.head, evidenceRef)
	rec.Evidence.Facts.Data = []byte(`{"code":"` + string(rec.ID) + `"}`)
	if l.head != "" {
		rec.Class = model.LinkClassRelink
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, rec) })
	l.id.Contact, l.id.Revision, l.head = to, rec.RevisionAfter, rec.ID
	return rec
}

// linkTombstone is rec as RedactLinks leaves it.
func linkTombstone(rec store.LinkRecord) store.LinkRecord {
	rec = rec.Clone()
	rec.Evidence.Facts = model.Extension{}
	rec.Redacted = true
	return rec
}

// wantLinks requires identity's link history to be exactly want.
func (e *env) wantLinks(t *testing.T, identity model.IdentityRef, want ...store.LinkRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		p, err := r.IdentityLinks(e.ctx, store.LinkFilter{Identity: &identity}, page(10))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(p.Items, want) {
			t.Errorf("link history = %+v, want %+v", p.Items, want)
		}
		return nil
	})
}

// wantContact requires the contact to exist or to be gone.
func (e *env) wantContact(t *testing.T, ref model.ContactRef, exists bool) {
	t.Helper()
	err := e.viewErr(func(r store.Reader) error {
		_, err := r.GetContact(e.ctx, ref)
		return err
	})
	switch {
	case exists && err != nil:
		t.Errorf("contact %s: %v, want it kept", ref.ID, err)
	case !exists && !model.IsCode(err, model.NotFound):
		t.Errorf("contact %s: %v, want it purged", ref.ID, err)
	}
}
