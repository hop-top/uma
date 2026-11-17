package services_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

const mergeReason = "duplicate_contact"

// merger is a linker with two person contacts, from holding identities in
// both inboxes, and into holding one.
type merger struct {
	*linker
	from, into model.Contact
	a, b, c    model.Identity // a and b on from, c on into
}

func newMerger(t *testing.T) *merger {
	t.Helper()
	l := newLinker(t)
	m := &merger{linker: l, from: l.person(), into: l.person()}
	m.a = l.identity(l.inbox1(), m.from.Ref)
	m.b = l.identity(l.inbox2(), m.from.Ref)
	m.c = l.identity(l.inbox1(), m.into.Ref)
	return m
}

// req merges from into into, reviewing the identities now on from.
func (m *merger) req(op model.OperationID) model.MergeContacts {
	m.T.Helper()
	return model.MergeContacts{
		Command: cmd(op, m.stored2(m.from.Ref).Revision), From: m.from.Ref, Into: m.into.Ref,
		ExpectedIntoRevision: m.stored2(m.into.Ref).Revision, Identities: []model.IdentityRef{m.a.Ref, m.b.Ref},
		Reason: mergeReason,
	}
}

func (m *merger) stored2(ref model.ContactRef) model.Contact {
	m.T.Helper()
	c, err := m.storedContact(ref)
	if err != nil {
		m.T.Fatal(err)
	}
	return c
}

func (m *merger) merge(req model.MergeContacts) (model.Acceptance, error) {
	return m.C.Contacts().Merge(m.Ctx, req)
}

func (m *merger) preview() (model.MergePreview, error) {
	return m.C.Contacts().PreviewMerge(m.Ctx, model.PreviewMerge{Call: call(), From: m.from.Ref, Into: m.into.Ref})
}

func (m *merger) events(kind string) []model.Event {
	m.T.Helper()
	var out []model.Event
	m.view(func(r store.Reader) error {
		p, err := r.Events(m.Ctx, model.Filter{Kinds: []string{kind}}, "", 100)
		out = p.Items
		return err
	})
	return out
}

// untouched fails unless the contacts, the three identities and the event
// logs are as they were seeded: a refused merge writes nothing.
func (m *merger) untouched() {
	m.T.Helper()
	for _, c := range []model.Contact{m.from, m.into} {
		if got := m.stored2(c.Ref); !reflect.DeepEqual(got, c) {
			m.T.Fatalf("contact changed: %+v, was %+v", got, c)
		}
	}
	for _, id := range []model.Identity{m.a, m.b, m.c} {
		if got := m.stored(id.Ref); got.Contact != id.Contact || got.Revision != id.Revision {
			m.T.Fatalf("identity moved or advanced: %+v, was %+v", got, id)
		}
	}
	if n := len(m.events(model.EventContactMerged)) + len(m.linkedEvents()); n != 0 {
		m.T.Fatalf("%d merge or link events", n)
	}
}

// Merge moves every identity, one merge record each that chains onto the
// identity's history, redirects the contact merged and bumps both
// contacts, and reports the move as one contact.merged and one
// identity.linked per identity.
func TestMergeMovesIdentitiesWithHistoryAndRedirect(t *testing.T) {
	m := newMerger(t)
	elsewhere := m.person()
	m.seed(&m.a, elsewhere.Ref, model.LinkAssuranceAsserted)
	m.seed(&m.a, m.from.Ref, model.LinkAssuranceAsserted)
	earlier := m.records(m.a.Ref)
	beforeEvents := len(m.linkedEvents())
	req := m.req("op-merge")
	fromBefore, intoBefore := m.stored2(m.from.Ref), m.stored2(m.into.Ref)

	acc, err := m.merge(req)
	if err != nil || acc.Replayed || acc.Operation.State != model.Committed || *acc.Operation.Result != m.into.Ref.Ref() {
		t.Fatalf("Merge = %+v, %v", acc, err)
	}
	for _, id := range []model.Identity{m.a, m.b} {
		if got := m.stored(id.Ref); got.Contact != m.into.Ref || got.Revision == id.Revision || got.LinkAssurance != model.LinkAssuranceAsserted {
			t.Fatalf("identity after merge %+v, was %+v", got, id)
		}
	}
	from, into := m.stored2(m.from.Ref), m.stored2(m.into.Ref)
	if from.MergedInto == nil || *from.MergedInto != m.into.Ref || from.Revision == fromBefore.Revision ||
		from.Kind != fromBefore.Kind || from.DisplayName != fromBefore.DisplayName {
		t.Fatalf("merged contact %+v, was %+v", from, fromBefore)
	}
	if into.MergedInto != nil || into.Revision == intoBefore.Revision || into.Kind != intoBefore.Kind {
		t.Fatalf("contact merged into %+v, was %+v", into, intoBefore)
	}

	// History of both sides survives: earlier records unchanged, the merge
	// records chain onto them.
	recs := m.records(m.a.Ref)
	if len(recs) != len(earlier)+1 || !slices.EqualFunc(recs[:len(earlier)], earlier, func(x, y model.IdentityLink) bool { return x.ID == y.ID }) {
		t.Fatalf("records of a %+v", recs)
	}
	for _, id := range []model.Identity{m.a, m.b} {
		recs := m.records(id.Ref)
		rec := recs[len(recs)-1]
		if rec.Class != model.LinkClassMerge || rec.From != m.from.Ref || rec.To != m.into.Ref || rec.Reason != mergeReason ||
			rec.Actor != actor || rec.Assurance != model.LinkAssuranceAsserted || rec.RecordedAt.IsZero() ||
			rec.Operation != acc.Operation.Ref || rec.RevisionBefore != id.Revision ||
			rec.RevisionAfter != m.stored(id.Ref).Revision || !rec.Evidence.IsZero() || rec.ReasonCode != "allow_"+model.ActionIdentityRelink ||
			(len(recs) > 1 && rec.Supersedes != recs[len(recs)-2].ID) {
			t.Fatalf("merge record %+v", rec)
		}
	}
	if byFrom := m.history(model.Filter{Contact: &m.from.Ref}, 10); len(byFrom) != 4 {
		t.Fatalf("history of the contact merged = %+v", byFrom)
	}
	if byInto := m.history(model.Filter{Contact: &m.into.Ref}, 10); len(byInto) < 2 {
		t.Fatalf("history of the contact merged into = %+v", byInto)
	}

	if evs := m.linkedEvents(); len(evs) != beforeEvents+2 {
		t.Fatalf("identity.linked events = %d, want %d", len(evs), beforeEvents+2)
	}
	evs := m.events(model.EventContactMerged)
	if len(evs) != 1 {
		t.Fatalf("contact.merged events = %d", len(evs))
	}
	ev := evs[0]
	var p model.ContactMergedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if ev.Resource != m.into.Ref.Ref() || ev.Audience.Visibility != model.Internal || ev.Revision != into.Revision ||
		ev.Payload.Name != model.SchemaContactMerged || p.From != m.from.Ref || p.Into != m.into.Ref ||
		!slices.Equal(p.Identities, []model.IdentityRef{m.a.Ref, m.b.Ref}) || len(p.Links) != 2 || p.Operation != acc.Operation.Ref {
		t.Fatalf("contact.merged %+v payload %+v", ev, p)
	}
	// A merged contact reads as redirected; the contact merged into does not.
	var listed []model.Contact
	for _, c := range m.contacts(model.Filter{}, 50) {
		if c.Ref == m.from.Ref || c.Ref == m.into.Ref {
			listed = append(listed, c)
		}
	}
	if len(listed) != 2 || listed[0].MergedInto == nil || listed[1].MergedInto != nil {
		t.Fatalf("List = %+v", listed)
	}
	if got := m.identities(model.Filter{Contact: &m.into.Ref}, 10); len(got) != 3 {
		t.Fatalf("identities of the contact merged into = %d", len(got))
	}
	if got := m.identities(model.Filter{Contact: &m.from.Ref}, 10); len(got) != 0 {
		t.Fatalf("identities left on the contact merged = %d", len(got))
	}
}
