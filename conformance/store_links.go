package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var linkCases = []storeCase{
	{"LinkMovesPointerOnly", func(t *testing.T, e *env) {
		id, target := e.linkSetup(t)
		rec := e.linkRecord(t, id, target.Ref, "", "ref-1")
		e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, rec) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Identity(e.ctx, e.identityKey(id.SourceID))
			if err != nil {
				return err
			}
			if got.Contact != target.Ref || got.Revision != rec.RevisionAfter || got.LinkAssurance != rec.Assurance {
				t.Errorf("identity after link = contact %v rev %q assurance %q", got.Contact, got.Revision, got.LinkAssurance)
			}
			for _, c := range []model.ContactRef{id.Contact, target.Ref} {
				cur, err := r.GetContact(e.ctx, c)
				if err != nil {
					return err
				}
				if cur.Revision != "c1" {
					t.Errorf("contact %s revision = %q; link must not change contacts", c.ID, cur.Revision)
				}
			}
			return nil
		})
	}},
	{"LinkComparesIdentityRevision", func(t *testing.T, e *env) {
		id, target := e.linkSetup(t)
		rec := e.linkRecord(t, id, target.Ref, "", "")
		rec.RevisionBefore = "stale"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, rec) }), model.Conflict)
		// Two links decided on the same revision: the second loses.
		first := e.linkRecord(t, id, target.Ref, "", "")
		second := e.linkRecord(t, id, target.Ref, "", "")
		e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, first) })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, second) }), model.Conflict)
	}},
	{"LinkEvidenceIsSingleUse", func(t *testing.T, e *env) {
		id, target := e.linkSetup(t)
		first := e.linkRecord(t, id, target.Ref, "", "challenge-1")
		e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, first) })
		id.Revision, id.Contact = first.RevisionAfter, target.Ref
		back := e.linkRecord(t, id, first.From, first.ID, "challenge-1")
		back.Class = model.LinkClassRelink
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, back) }), model.Conflict)
	}},
	{"LinkHistoryReads", func(t *testing.T, e *env) {
		id, target := e.linkSetup(t)
		first := e.linkRecord(t, id, target.Ref, "", "")
		e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, first) })
		id.Revision, id.Contact = first.RevisionAfter, target.Ref
		undo := e.linkRecord(t, id, first.From, first.ID, "")
		undo.Class = model.LinkClassRelink
		e.mustTx(t, func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, undo) })
		// Supersedes must name the identity's latest record.
		id.Revision, id.Contact = undo.RevisionAfter, first.From
		wrong := e.linkRecord(t, id, target.Ref, first.ID, "")
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.LinkIdentity(e.ctx, wrong) }), model.Validation)

		e.mustView(t, func(r store.Reader) error {
			p, err := r.IdentityLinks(e.ctx, store.LinkFilter{Identity: &id.Ref}, page(10))
			if err != nil {
				return err
			}
			if len(p.Items) != 2 || p.Items[0].ID != first.ID || p.Items[1].ID != undo.ID || !p.Complete {
				t.Errorf("history = %+v, want [first, undo] complete", p.Items)
			}
			byContact, err := r.IdentityLinks(e.ctx, store.LinkFilter{Contact: &target.Ref}, page(1))
			if err != nil {
				return err
			}
			if len(byContact.Items) != 1 || byContact.Complete {
				t.Errorf("first page by contact = %d items complete=%v, want 1 and more", len(byContact.Items), byContact.Complete)
			}
			_, err = r.IdentityLinks(e.ctx, store.LinkFilter{}, page(10))
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
}

// linkSetup claims an identity with its provisional contact and creates a
// second contact to link it to.
func (e *env) linkSetup(t *testing.T) (model.Identity, model.Contact) {
	t.Helper()
	key := e.identityKey("U-" + string(e.id(t)))
	id := e.identity(t, key)
	target := model.Contact{Ref: model.ContactRef{Tenant: tenantA, ID: e.id(t)}, Kind: model.ContactKindPerson, Revision: "c1"}
	e.mustTx(t, func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(e.ctx, key, id); err != nil {
			return err
		}
		if err := tx.PutContact(e.ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "c1"}, ""); err != nil {
			return err
		}
		return tx.PutContact(e.ctx, target, "")
	})
	return id, target
}

func (e *env) linkRecord(t *testing.T, id model.Identity, to model.ContactRef, supersedes model.ID, evidenceRef string) store.LinkRecord {
	t.Helper()
	return store.LinkRecord{
		ID:          e.id(t),
		Tenant:      tenantA,
		Identity:    id.Ref,
		IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID},
		Class:       model.LinkClassLink,
		From:        id.Contact,
		To:          to,
		Supersedes:  supersedes,
		Evidence: model.Evidence{
			Verifier: "attestation", Reference: evidenceRef, Assurance: model.LinkAssuranceAsserted,
			Facts: model.Extension{Name: "conformance.attestation", Version: 1, Data: []byte(`{}`)},
		},
		Assurance:      model.LinkAssuranceAsserted,
		Actor:          "conformance-actor",
		Operation:      model.OperationRef{Tenant: tenantA, ID: e.id(t)},
		RevisionBefore: id.Revision,
		RevisionAfter:  id.Revision + "+",
		RecordedAt:     time.Unix(1_700_000_000, 0).UTC(),
	}
}
