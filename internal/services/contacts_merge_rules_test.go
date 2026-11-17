package services_test

import (
	"reflect"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Merge is explicit: contact.merge on the contact merged, with the contact
// merged into as destination at the contact's revision, and relink on
// every identity at assurance asserted. One denial refuses everything.
func TestMergeNeedsEveryAuthorization(t *testing.T) {
	m := newMerger(t)
	req := m.req("op-1")
	for name, deny := range map[string]func(policy.Authorization) bool{
		"merge gate": func(a policy.Authorization) bool { return a.Action != model.ActionContactMerge },
		"relink of the second identity": func(a policy.Authorization) bool {
			return a.Action != model.ActionIdentityRelink || a.Resource != m.b.Ref.Ref()
		},
		"everything": denyAll,
	} {
		m.pol.set(deny)
		_, err := m.merge(req)
		requireCode(t, err, model.Unauthorized, "")
		m.untouched()
		if m.claimedMerge(req) {
			t.Fatalf("%s: denied merge claimed its operation", name)
		}
	}
	m.pol.set(nil)
	if _, err := m.merge(req); err != nil {
		t.Fatal(err)
	}
	into := m.into.Ref.Ref()
	gate := policy.Authorization{
		Principal: principalA(), Action: model.ActionContactMerge, Resource: m.from.Ref.Ref(), Destination: &into,
		Revision: req.ExpectedRevision,
	}
	relink := func(id model.Identity) policy.Authorization {
		return policy.Authorization{
			Principal: principalA(), Action: model.ActionIdentityRelink, Resource: id.Ref.Ref(), Destination: &into,
			Revision: id.Revision, Assurance: model.LinkAssuranceAsserted,
		}
	}
	got := m.pol.seen()
	if len(got) != 3 || !sameAuthorization(got[0], gate) || !sameAuthorization(got[1], relink(m.a)) || !sameAuthorization(got[2], relink(m.b)) {
		t.Fatalf("authorizations = %+v", got)
	}
}

// A merge never lowers a stronger link without the override action, as a
// relink at the asserted level would not.
func TestMergeNeedsOverrideBelowCurrentAssurance(t *testing.T) {
	m := newMerger(t)
	proven := m.identity(m.inbox1(), m.person().Ref)
	m.seed(&proven, m.from.Ref, model.LinkAssuranceProven)
	m.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionIdentityLinkOverride })
	req := m.req("op-1")
	req.Identities = append(req.Identities, proven.Ref)
	_, err := m.merge(req)
	requireCode(t, err, model.Unauthorized, "")
	if got := m.stored(proven.Ref); got.Contact != m.from.Ref || got.LinkAssurance != model.LinkAssuranceProven {
		t.Fatalf("proven link lowered without override: %+v", got)
	}
	m.pol.set(nil)
	if _, err := m.merge(req); err != nil {
		t.Fatal(err)
	}
	asked := actions(m.pol.seen())
	if !slices.Contains(asked, model.ActionIdentityLinkOverride) {
		t.Fatalf("actions = %v, want an override decision", asked)
	}
	if got := m.stored(proven.Ref); got.LinkAssurance != model.LinkAssuranceAsserted || got.Contact != m.into.Ref {
		t.Fatalf("after merge %+v", got)
	}
}

// Both contacts' revisions and the reviewed identity set are compared: a
// stale one is a conflict that writes nothing.
func TestMergeRefusesStaleRevisionsAndIdentities(t *testing.T) {
	m := newMerger(t)
	for name, mutate := range map[string]func(*model.MergeContacts){
		"stale from":       func(r *model.MergeContacts) { r.ExpectedRevision = "old" },
		"stale into":       func(r *model.MergeContacts) { r.ExpectedIntoRevision = "old" },
		"missing identity": func(r *model.MergeContacts) { r.Identities = r.Identities[:1] },
		"foreign identity": func(r *model.MergeContacts) { r.Identities = append(r.Identities, m.c.Ref) },
		"swapped identity": func(r *model.MergeContacts) { r.Identities = []model.IdentityRef{m.a.Ref, m.c.Ref} },
		"unknown identity": func(r *model.MergeContacts) {
			r.Identities = []model.IdentityRef{m.a.Ref, {Inbox: m.inbox2(), ID: m.id()}}
		},
	} {
		req := m.req("op-" + model.OperationID(name))
		mutate(&req)
		_, err := m.merge(req)
		requireCode(t, err, model.Conflict, "")
		m.untouched()
		_ = name
	}
	if len(m.pol.seen()) != 0 {
		t.Fatal("a stale merge reached the authorizer")
	}
}

// What moves between the decisions and the commit is caught in the unit:
// an identity arriving on the contact, an identity advancing, or either
// contact changing.
func TestMergeRefusesChangesBetweenDecisionAndCommit(t *testing.T) {
	for name, move := range map[string]func(m *merger){
		"identity linked in": func(m *merger) {
			late := m.identity(m.inbox1(), m.person().Ref)
			m.seed(&late, m.from.Ref, model.LinkAssuranceAsserted)
		},
		"identity linked out": func(m *merger) { m.seed(&m.b, m.person().Ref, model.LinkAssuranceAsserted) },
		"identity advanced": func(m *merger) {
			m.seed(&m.b, m.person().Ref, model.LinkAssuranceAsserted)
			m.seed(&m.b, m.from.Ref, model.LinkAssuranceAsserted)
		},
		"into merged away": func(m *merger) {
			// Written straight to the store: the policy is mid-decision.
			into := m.stored2(m.into.Ref)
			redirected := into
			redirected.MergedInto, redirected.Revision = new(m.person().Ref), "c2"
			m.put(func(tx store.Tx) error { return tx.PutContact(m.Ctx, redirected, into.Revision) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := newMerger(t)
			req := m.req("op-1")
			fired := false
			m.pol.set(func(a policy.Authorization) bool {
				if !fired && a.Action == model.ActionIdentityRelink {
					fired = true
					move(m)
				}
				return true
			})
			_, err := m.merge(req)
			requireCode(t, err, model.Conflict, "")
			if got := m.stored(m.a.Ref); got.Contact != m.from.Ref {
				t.Fatalf("identity a moved: %+v", got)
			}
			if got := m.stored2(m.from.Ref); got.MergedInto != nil {
				t.Fatalf("contact redirected: %+v", got)
			}
			if len(m.events(model.EventContactMerged)) != 0 || m.claimedMerge(req) {
				t.Fatal("refused merge left an event or an operation")
			}
		})
	}
}

func (m *merger) operation2(kind string, target model.Ref, op model.OperationID) (store.OperationRecord, error) {
	var rec store.OperationRecord
	err := m.E.Store().View(m.Ctx, tenantA, func(r store.Reader) (err error) {
		rec, err = r.OperationByKey(m.Ctx, store.OperationKey{Tenant: tenantA, Caller: actor, Kind: kind, Target: target, ID: op})
		return err
	})
	return rec, err
}

func (m *merger) claimedMerge(req model.MergeContacts) bool {
	m.T.Helper()
	_, err := m.operation2(model.ActionContactMerge, req.From.Ref(), req.Operation)
	return err == nil
}

// A contact merges once and never into itself; a merged contact is not a
// target; kinds match.
func TestMergeRefusesMergedSelfAndKindMismatch(t *testing.T) {
	m := newMerger(t)
	self := m.req("op-self")
	self.Into = self.From
	_, err := m.merge(self)
	requireCode(t, err, model.Validation, "into")

	if _, err := m.merge(m.req("op-1")); err != nil {
		t.Fatal(err)
	}
	// from is merged: refused with a fresh operation and fresh revisions.
	again := model.MergeContacts{
		Command: cmd("op-2", m.stored2(m.from.Ref).Revision), From: m.from.Ref, Into: m.into.Ref,
		ExpectedIntoRevision: m.stored2(m.into.Ref).Revision, Reason: mergeReason,
	}
	_, err = m.merge(again)
	requireCode(t, err, model.Conflict, "")
	third := m.person()
	back := model.MergeContacts{
		Command: cmd("op-3", third.Revision), From: third.Ref, Into: m.from.Ref,
		ExpectedIntoRevision: m.stored2(m.from.Ref).Revision, Reason: mergeReason,
	}
	_, err = m.merge(back)
	requireCode(t, err, model.Conflict, "")

	team := m.contact(model.ContactKindBot)
	mismatch := model.MergeContacts{
		Command: cmd("op-4", third.Revision), From: third.Ref, Into: team.Ref, ExpectedIntoRevision: team.Revision, Reason: mergeReason,
	}
	_, err = m.merge(mismatch)
	requireCode(t, err, model.Validation, "kind")
	if got := m.stored2(third.Ref); !reflect.DeepEqual(got, third) {
		t.Fatalf("refused merge changed the contact: %+v", got)
	}
}

// A bot identity never joins a person: a person contact holding one (a
// store seeded by hand) still cannot merge into a person.
func TestMergeRefusesIdentityOfAnotherKind(t *testing.T) {
	m := newMerger(t)
	bot := m.botIdentity()
	m.seed(&bot, m.from.Ref, model.LinkAssuranceAsserted)
	req := m.req("op-1")
	req.Identities = append(req.Identities, bot.Ref)
	_, err := m.merge(req)
	requireCode(t, err, model.Validation, "kind")
	if got := m.stored(m.a.Ref); got.Contact != m.from.Ref {
		t.Fatal("identity moved despite the refusal")
	}
}

// The same operation replays; a different payload under it conflicts; a
// replay is authorized again before it reveals anything.
func TestMergeReplay(t *testing.T) {
	m := newMerger(t)
	req := m.req("op-1")
	first, err := m.merge(req)
	if err != nil {
		t.Fatal(err)
	}
	records := len(m.history(model.Filter{Contact: &m.from.Ref}, 10))
	m.pol.set(nil)
	again, err := m.merge(req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if k := again.Operation.Key; k == nil || first.Operation.Key == nil || *k != *first.Operation.Key || k.Kind != model.ActionContactMerge {
		t.Fatalf("replayed key = %+v, first %+v; want the contact.merge key twice", k, first.Operation.Key)
	}
	if got := actions(m.pol.seen()); !slices.Equal(got, []string{model.ActionContactMerge}) {
		t.Fatalf("replay decided %v, want only the merge gate", got)
	}
	if n := len(m.history(model.Filter{Contact: &m.from.Ref}, 10)); n != records || len(m.events(model.EventContactMerged)) != 1 {
		t.Fatal("replay wrote again")
	}
	other := req
	other.Reason = "other_reason"
	_, err = m.merge(other)
	requireCode(t, err, model.Conflict, "")
	m.pol.set(denyAll)
	_, err = m.merge(req)
	requireCode(t, err, model.Unauthorized, "")
}

// References into another tenant are not found, and nothing is decided.
func TestMergeCrossTenantIsNotFound(t *testing.T) {
	m := newMerger(t)
	foreign := func(c model.ContactRef) model.ContactRef { c.Tenant = tenantB; return c }
	req := m.req("op-1")
	req.Into = foreign(req.Into)
	_, err := m.merge(req)
	requireCode(t, err, model.NotFound, "")
	req = m.req("op-2")
	req.From = foreign(req.From)
	_, err = m.merge(req)
	requireCode(t, err, model.NotFound, "")
	req = m.req("op-3")
	req.Identities[0].Inbox.Tenant = tenantB
	_, err = m.merge(req)
	requireCode(t, err, model.NotFound, "")
	req = m.req("op-4")
	req.Into = model.ContactRef{Tenant: tenantA, ID: m.id()}
	_, err = m.merge(req)
	requireCode(t, err, model.NotFound, "")
	if len(m.pol.seen()) != 0 {
		t.Fatal("out-of-scope merge was decided")
	}
	m.untouched()
}

// Merge moves at most model.MaxMergeIdentities.
func TestMergeRefusesTooManyIdentities(t *testing.T) {
	m := newMerger(t)
	for range model.MaxMergeIdentities - 1 {
		m.identity(m.inbox1(), m.from.Ref)
	}
	// 2 held + 499 = 501.
	_, err := m.preview()
	requireCode(t, err, model.Validation, "")
	req := m.req("op-1")
	_, err = m.merge(req)
	requireCode(t, err, model.Validation, "")
}

// Link to a merged contact is a conflict: it holds no identity any more.
func TestLinkToMergedContactConflicts(t *testing.T) {
	m := newMerger(t)
	if _, err := m.merge(m.req("op-1")); err != nil {
		t.Fatal(err)
	}
	id := m.provisional()
	_, err := m.doLink(m.linkReq(id, m.from.Ref, attest, "back-1", "op-2"))
	requireCode(t, err, model.Conflict, "")
	m.unchanged(id, 2)
}
