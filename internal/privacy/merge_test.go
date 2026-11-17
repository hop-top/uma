package privacy_test

import (
	"slices"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// merged is a linkedSeed after Contacts.Merge of its other contact (bob)
// into its shared contact (alice and carol), and then of shared into top:
// the redirect chain other -> shared -> top.
type merged struct {
	*linkedSeed
	top model.Contact
}

func (s *merged) contactNow(t *testing.T, ref model.ContactRef) model.Contact {
	t.Helper()
	var c model.Contact
	s.h.View(func(r store.Reader) (err error) { c, err = r.GetContact(s.h.Ctx, ref); return err })
	return c
}

// mergeInto merges from into into, reviewing every identity on from.
func (s *linkedSeed) mergeInto(t *testing.T, from, into model.ContactRef, op model.OperationID) {
	t.Helper()
	var held []model.IdentityRef
	s.h.View(func(r store.Reader) error {
		p, err := r.Identities(s.h.Ctx, model.ListRequest{Filter: model.Filter{Contact: &from}, Page: model.PageRequest{Limit: 100}})
		for _, id := range p.Items {
			held = append(held, id.Ref)
		}
		return err
	})
	var f, i model.Contact
	s.h.View(func(r store.Reader) (err error) {
		if f, err = r.GetContact(s.h.Ctx, from); err != nil {
			return err
		}
		i, err = r.GetContact(s.h.Ctx, into)
		return err
	})
	_, err := s.h.Client.Contacts().Merge(s.h.Ctx, model.MergeContacts{
		Command: cmd(op, f.Revision), From: from, Into: into, ExpectedIntoRevision: i.Revision,
		Identities: held, Reason: "duplicate_contact",
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
}

func seedMerged(t *testing.T) *merged {
	t.Helper()
	s := &merged{linkedSeed: seedLinked(t)}
	s.top = s.putContact(t)
	s.mergeInto(t, s.other.Ref, s.shared.Ref, "op-merge-1")
	s.mergeInto(t, s.shared.Ref, s.top.Ref, "op-merge-2")
	return s
}

// An export of a merged contact follows the redirect to the contact its
// identities went to: the surviving contact, all its identities (the
// merged ones among them) and their messages. The merged contact itself,
// holding nothing, is not exported.
func TestExportOfMergedContactFollowsTheRedirect(t *testing.T) {
	s := seedMerged(t)
	h := s.h
	_, x, lines := export(t, h.Client, h.Ctx, exportReq("op-export", model.Filter{Contact: &s.other.Ref}))
	if x.Filter.Contact == nil || *x.Filter.Contact != s.other.Ref {
		t.Fatalf("export filter = %+v, want the contact asked for", x.Filter)
	}
	if cs := ofType[model.Contact](t, lines, model.ExportLineContact); len(cs) != 1 || cs[0].Ref != s.top.Ref {
		t.Errorf("contacts = %+v, want the surviving contact only", cs)
	}
	var got []model.IdentityRef
	for _, id := range ofType[model.Identity](t, lines, model.ExportLineIdentity) {
		got = append(got, id.Ref)
	}
	slices.SortFunc(got, func(a, b model.IdentityRef) int { return compareIDs(a.ID, b.ID) })
	want := []model.IdentityRef{s.alice.Ref, s.bob.Ref, s.carol.Ref}
	slices.SortFunc(want, func(a, b model.IdentityRef) int { return compareIDs(a.ID, b.ID) })
	if !slices.Equal(got, want) {
		t.Errorf("identities = %v, want alice, bob and carol", got)
	}
	authors := map[model.IdentityRef]bool{}
	for _, m := range ofType[model.Message](t, lines, model.ExportLineMessage) {
		authors[m.Author.Identity] = true
	}
	if len(authors) != 3 {
		t.Errorf("message authors = %v, want all three identities", authors)
	}
}

func compareIDs(a, b model.ID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// A cascade on a merged contact erases what its identities went to, as
// the surviving contact's cascade would: every message of every identity
// on it, and their link records' facts.
func TestCascadeOnMergedContactFollowsTheRedirect(t *testing.T) {
	s := seedMerged(t)
	h := s.h
	// Every contact on the redirect chain carries a name.
	chain := []model.ContactRef{s.other.Ref, s.shared.Ref, s.top.Ref}
	for _, ref := range chain {
		c := s.contactNow(t, ref)
		named := c
		named.DisplayName, named.Revision = "Ada", c.Revision+"-named"
		h.Tx(func(tx store.Tx) error { return tx.PutContact(h.Ctx, named, c.Revision) })
	}
	other := s.contactNow(t, s.other.Ref)
	if other.MergedInto == nil {
		t.Fatalf("seed: %+v is not merged", other)
	}
	if _, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.other.Ref.Ref(), "op-cascade", other.Revision)); err != nil {
		t.Fatalf("cascade: %v", err)
	}
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	for _, ref := range []model.MessageRef{s.messages[0], s.bobs.Ref} {
		if m := s.message(t, ref); !m.Redacted {
			t.Errorf("message %s survived the cascade", ref.ID)
		}
	}
	carol := h.MessageByProviderID(s.inbox, "pm-carol")
	if m := s.message(t, carol.Ref); !m.Redacted {
		t.Error("carol's message survived the cascade")
	}
	for i, ref := range chain {
		c := s.contactNow(t, ref)
		if c.DisplayName != "" {
			t.Errorf("contact %s kept its name after the cascade", ref.ID)
		}
		if (c.MergedInto == nil) != (i == len(chain)-1) {
			t.Errorf("contact %s redirect after the cascade = %v", ref.ID, c.MergedInto)
		}
	}
	for _, id := range []model.IdentityRef{s.alice.Ref, s.carol.Ref, s.bob.Ref} {
		for _, rec := range s.history(t, id) {
			if !rec.Redacted {
				t.Errorf("link record %s of %s kept its facts", rec.ID, id.ID)
			}
		}
	}
}
