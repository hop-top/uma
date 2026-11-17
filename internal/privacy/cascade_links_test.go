package privacy_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// linkedSeed is seedContact plus carol, a third identity, and contact
// shared, which alice and carol are linked to; bob is linked to contact
// other. Each link record carries evidence facts and a reference.
type linkedSeed struct {
	*seed
	carol         model.Identity
	shared, other model.Contact
	links         map[model.IdentityRef]model.IdentityLink
}

func seedLinked(t *testing.T, opts ...enginetest.HarnessOption) *linkedSeed {
	t.Helper()
	s := &linkedSeed{seed: seedContact(t, 1, opts...), links: map[model.IdentityRef]model.IdentityLink{}}
	h := s.h
	h.MustAccept(h.Inbound("e-carol", "pm-carol", "carol", enginetest.Dest))
	carol := h.MessageByProviderID(s.inbox, "pm-carol")
	h.View(func(r store.Reader) (err error) {
		s.carol, err = r.GetIdentity(h.Ctx, carol.Author.Identity)
		return err
	})
	s.shared, s.other = s.putContact(t), s.putContact(t)
	s.link(t, &s.alice, s.shared.Ref)
	s.link(t, &s.carol, s.shared.Ref)
	s.link(t, &s.bob, s.other.Ref)
	return s
}

func (s *linkedSeed) putContact(t *testing.T) model.Contact {
	t.Helper()
	c := model.Contact{Ref: model.ContactRef{Tenant: enginetest.TenantA, ID: s.h.ID()}, Kind: model.ContactKindPerson, Revision: "c1"}
	s.h.Tx(func(tx store.Tx) error { return tx.PutContact(s.h.Ctx, c, "") })
	return c
}

// link moves id to contact to with a link record holding evidence facts.
func (s *linkedSeed) link(t *testing.T, id *model.Identity, to model.ContactRef) {
	t.Helper()
	h := s.h
	rec := model.IdentityLink{
		ID: h.ID(), Tenant: enginetest.TenantA, Identity: id.Ref,
		IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID},
		Class:       model.LinkClassLink, From: id.Contact, To: to,
		Evidence: model.Evidence{
			Verifier: model.EvidenceKindAttestation, Reference: "ref-" + string(id.Ref.ID), Assurance: model.LinkAssuranceAsserted,
			Facts: model.Extension{Name: model.SchemaEvidenceAttestation, Version: 1, Data: []byte(`{"reason":"` + secret + `"}`)},
		},
		Assurance: model.LinkAssuranceAsserted, Actor: enginetest.Actor,
		Operation:      model.OperationRef{Tenant: enginetest.TenantA, ID: h.ID()},
		RevisionBefore: id.Revision, RevisionAfter: id.Revision + "+", RecordedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	h.Tx(func(tx store.Tx) error { return tx.LinkIdentity(h.Ctx, rec) })
	id.Contact, id.Revision = to, rec.RevisionAfter
	s.links[id.Ref] = rec
}

// history reads an identity's link history through Contacts.History.
func (s *linkedSeed) history(t *testing.T, id model.IdentityRef) []model.IdentityLink {
	t.Helper()
	p, err := s.h.Client.Contacts().History(s.h.Ctx, model.ListRequest{
		Call: model.Call{Correlation: "c"}, Filter: model.Filter{Identity: &id}, Page: model.PageRequest{Limit: 10},
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	return p.Items
}

// requireLink requires id's single link record to be the seeded one, as a
// tombstone when redacted: facts gone, every other field kept.
func (s *linkedSeed) requireLink(t *testing.T, id model.IdentityRef, redacted bool) {
	t.Helper()
	want := s.links[id].Clone()
	if redacted {
		want.Evidence.Facts, want.Redacted = model.Extension{}, true
	}
	got := s.history(t, id)
	if len(got) != 1 {
		t.Fatalf("history of %s = %d records, want 1", id.ID, len(got))
	}
	if g := got[0]; g.Redacted != want.Redacted || len(g.Evidence.Facts.Data) != len(want.Evidence.Facts.Data) ||
		g.Evidence.Facts.Name != want.Evidence.Facts.Name || g.Evidence.Verifier != want.Evidence.Verifier ||
		g.Evidence.Reference != want.Evidence.Reference || g.Class != want.Class || g.From != want.From || g.To != want.To ||
		g.Reason != want.Reason || g.Assurance != want.Assurance {
		t.Fatalf("history of %s = %+v, want %+v", id.ID, g, want)
	}
}

// An identity cascade tombstones that identity's link records in its
// submission unit, before any worker runs: History shows them Redacted,
// facts cleared, verifier, reference and class kept. Other identities'
// records keep their facts.
func TestCascadeOnIdentityTombstonesLinkFacts(t *testing.T) {
	s := seedLinked(t)
	_, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.alice.Ref.Ref(), "op-cascade", s.alice.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	s.requireLink(t, s.alice.Ref, true)
	s.requireLink(t, s.carol.Ref, false)
	s.requireLink(t, s.bob.Ref, false)
	again, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.alice.Ref.Ref(), "op-cascade", s.alice.Revision))
	if err != nil || !again.Replayed {
		t.Fatalf("retry = %+v, %v; want a replay", again, err)
	}
	s.requireLink(t, s.alice.Ref, true)
}

// A contact cascade tombstones the link records of every identity on the
// contact at submission, and none of another contact's identities.
func TestCascadeOnContactTombstonesLinkedIdentitiesFacts(t *testing.T) {
	s := seedLinked(t)
	_, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(s.shared.Ref.Ref(), "op-cascade", s.shared.Revision))
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	s.requireLink(t, s.alice.Ref, true)
	s.requireLink(t, s.carol.Ref, true)
	s.requireLink(t, s.bob.Ref, false)
}

// Message redaction and a conversation cascade never touch link records.
func TestMessageRedactLeavesLinkHistory(t *testing.T) {
	s := seedLinked(t)
	h := s.h
	m := s.message(t, s.messages[0])
	if _, err := h.Client.Privacy().Redact(h.Ctx, redactMessage(m, "op-message")); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	var conv model.Conversation
	h.View(func(r store.Reader) (err error) {
		conv, err = r.GetConversation(h.Ctx, s.bobs.Ref.Conversation)
		return err
	})
	if _, err := h.Client.Privacy().Redact(h.Ctx, cascade(conv.Ref.Ref(), "op-conv", conv.Revision)); err != nil {
		t.Fatalf("conversation cascade: %v", err)
	}
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	for _, id := range []model.IdentityRef{s.alice.Ref, s.carol.Ref, s.bob.Ref} {
		s.requireLink(t, id, false)
	}
}

// A contact cascade decides model.ActionPrivacyRedact on each identity on
// the contact before anything is written. With no threshold policy a
// denied identity is skipped: the cascade is accepted as Partial, the
// permitted identities' link records are tombstoned and the denied one's
// stay whole. An identity cascade needs only its own authorization.
func TestContactCascadeSkipsAnIdentityThatMayNotBeRedacted(t *testing.T) {
	pol := &refusePolicy{}
	s := seedLinked(t, func(c *engine.Config) { c.Authorizer = pol })
	h := s.h
	pol.resource = s.carol.Ref.Ref()
	a, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.shared.Ref.Ref(), "op-cascade", s.shared.Revision))
	if err != nil || a.Operation.Progress == nil || !a.Operation.Progress.Partial {
		t.Fatalf("cascade = %+v, %v; want accepted as partial", a, err)
	}
	s.requireLink(t, s.alice.Ref, true)
	s.requireLink(t, s.carol.Ref, false)
	drive(t, h.Ctx, h.RT, enginetest.TenantA)
	if m := s.message(t, s.messages[0]); !m.Redacted {
		t.Fatal("partial cascade left alice's message")
	}

	if _, err := h.Client.Privacy().Redact(h.Ctx, cascade(s.carol.Ref.Ref(), "op-carol", s.carol.Revision)); err != nil {
		t.Fatalf("identity cascade: %v", err)
	}
	s.requireLink(t, s.carol.Ref, true)
}
