package services_test

import (
	"errors"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

const attest = model.EvidenceKindAttestation

// Link, relink and override are distinct actions, each decided on the
// identity with the target as destination at the identity's revision and
// the verified assurance; a denial writes nothing.
func TestLinkUnauthorizedClasses(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	id := l.provisional()
	l.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionIdentityLink })
	_, err := l.doLink(l.linkReq(id, target.Ref, attest, "ref-link", "op-1"))
	requireCode(t, err, model.Unauthorized, "")
	want := policy.Authorization{
		Principal: principalA(), Action: model.ActionIdentityLink, Resource: id.Ref.Ref(),
		Destination: new(target.Ref.Ref()), Revision: id.Revision, Assurance: model.LinkAssuranceAsserted,
		Evidence: &policy.VerifiedEvidence{Verifier: attest, Reference: "ref-link", Assurance: model.LinkAssuranceAsserted},
	}
	if got := l.pol.seen(); len(got) != 1 || !sameAuthorization(got[0], want) {
		t.Fatalf("authorizations = %+v, want [%+v]", got, want)
	}
	l.unchanged(id, 0)

	l.pol.set(nil)
	l.mustLink(&id, target.Ref, "ref-1", "op-2")
	other := l.person()
	l.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionIdentityRelink })
	_, err = l.doLink(l.linkReq(id, other.Ref, attest, "ref-relink", "op-3"))
	requireCode(t, err, model.Unauthorized, "")
	if got := actions(l.pol.seen()); !slices.Equal(got, []string{model.ActionIdentityRelink}) {
		t.Fatalf("relink actions = %v", got)
	}
	l.unchanged(id, 1)

	l.seed(&id, l.person().Ref, model.LinkAssuranceHostAuthenticated)
	l.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionIdentityLinkOverride })
	_, err = l.doLink(l.linkReq(id, other.Ref, attest, "ref-down", "op-4"))
	requireCode(t, err, model.Unauthorized, "")
	checks := l.pol.seen()
	if got := actions(checks); !slices.Equal(got, []string{model.ActionIdentityRelink, model.ActionIdentityLinkOverride}) {
		t.Fatalf("override actions = %v", got)
	}
	for _, c := range checks {
		if c.Assurance != model.LinkAssuranceAsserted || c.Evidence == nil || c.Evidence.Reference != "ref-down" ||
			c.Revision != id.Revision || *c.Destination != other.Ref.Ref() {
			t.Fatalf("override decision %+v", c)
		}
	}
	l.unchanged(id, 1)
}

func sameAuthorization(a, b policy.Authorization) bool {
	de := func(x, y *model.Ref) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	ev := func(x, y *policy.VerifiedEvidence) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.Principal == b.Principal && a.Action == b.Action && a.Resource == b.Resource && a.Audience == b.Audience &&
		de(a.Destination, b.Destination) && a.Revision == b.Revision && a.Assurance == b.Assurance && ev(a.Evidence, b.Evidence)
}

// A stale expected revision is a conflict to reconcile before deciding
// again, whether stale at the call or advanced before the commit; the
// verifier is not asked about a request already stale.
func TestLinkStaleRevisionConflictsReconcileFirst(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	id := l.provisional()
	req := l.linkReq(id, target.Ref, attest, "ref-1", "op-1")
	req.ExpectedRevision = "stale"
	_, err := l.doLink(req)
	requireReconcile(t, err)
	if l.verifyCalls() != 0 || len(l.pol.seen()) != 0 {
		t.Fatal("stale request verified or decided")
	}

	// The identity moves while the policy decides.
	race := func(action string, on *model.Identity) {
		fired := false
		l.pol.set(func(a policy.Authorization) bool {
			if a.Action == action && !fired {
				fired = true
				moved := *on
				l.seed(&moved, l.person().Ref, model.LinkAssuranceAsserted)
			}
			return true
		})
	}
	race(model.ActionIdentityLink, &id)
	_, err = l.doLink(l.linkReq(id, target.Ref, attest, "ref-2", "op-2"))
	requireReconcile(t, err)
	if n := len(l.records(id.Ref)); n != 1 {
		t.Fatalf("records = %d, want only the racing one", n)
	}

	// A no-op is checked too: it has no store write to fail on.
	noop := l.provisional()
	race(model.ActionIdentityRead, &noop)
	_, err = l.doLink(l.linkReq(noop, noop.Contact, attest, "", "op-3"))
	requireReconcile(t, err)
	if _, err := l.operation(model.ActionIdentityLink, noop.Ref, "op-3"); !model.IsCode(err, model.NotFound) {
		t.Fatalf("stale no-op claimed its operation: %v", err)
	}
}

func requireReconcile(t *testing.T, err error) {
	t.Helper()
	requireCode(t, err, model.Conflict, "")
	var me *model.Error
	if !errors.As(err, &me) || me.Retry != model.ReconcileFirst {
		t.Fatalf("retry advice = %+v, want reconcile_first", err)
	}
}

// Replay comes first: after the identity moved on, the same command
// returns its stored result without verifying again, authorized with the
// stored action and verified evidence before anything is revealed.
func TestLinkReplayAfterRevisionAdvance(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	id := l.provisional()
	req := l.linkReq(id, target.Ref, attest, "ref-1", "op-1")
	first, err := l.doLink(req)
	if err != nil {
		t.Fatal(err)
	}
	moved := l.stored(id.Ref)
	l.mustLink(&moved, l.person().Ref, "ref-2", "op-2")
	calls := l.vs[attest].Calls()

	l.pol.set(nil)
	again, err := l.doLink(req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if k := again.Operation.Key; k == nil || first.Operation.Key == nil || *k != *first.Operation.Key || k.Kind != model.ActionIdentityLink {
		t.Fatalf("replayed key = %+v, first %+v; want the identity.link key twice", k, first.Operation.Key)
	}
	if *again.Operation.Result != id.Ref.Ref() {
		t.Fatalf("result = %v, want the identity", again.Operation.Result)
	}
	if l.vs[attest].Calls() != calls {
		t.Fatal("replay verified again")
	}
	want := policy.Authorization{
		Principal: principalA(), Action: model.ActionIdentityLink, Resource: id.Ref.Ref(),
		Destination: new(target.Ref.Ref()), Revision: id.Revision, Assurance: model.LinkAssuranceAsserted,
		Evidence: &policy.VerifiedEvidence{Verifier: attest, Reference: "ref-1", Assurance: model.LinkAssuranceAsserted},
	}
	if got := l.pol.seen(); len(got) != 1 || !sameAuthorization(got[0], want) {
		t.Fatalf("replay authorization = %+v, want %+v", got, want)
	}

	l.pol.set(denyAll)
	if acc, err := l.doLink(req); !model.IsCode(err, model.Unauthorized) || acc.Operation.Ref.ID != "" {
		t.Fatalf("denied replay = %+v, %v", acc, err)
	}
	l.pol.set(nil)
	changed := req
	changed.Evidence.Reference = "ref-other"
	_, err = l.doLink(changed)
	requireCode(t, err, model.Conflict, "different payload")
	if l.vs[attest].Calls() != calls || len(l.pol.seen()) != 0 {
		t.Fatal("conflicting replay verified or decided")
	}
}

// Evidence bound to another identity or another contact is not evidence
// for this request: validation, nothing decided or written.
func TestLinkEvidenceBoundToOtherIdentityOrContact(t *testing.T) {
	l := newLinker(t)
	target, elsewhere := l.person(), l.person()
	id, other := l.provisional(), l.provisional()
	for name, rebind := range map[string]func(*model.EvidenceBinding){
		"identity": func(b *model.EvidenceBinding) { b.Identity = other.Ref },
		"current":  func(b *model.EvidenceBinding) { b.Current = other.Contact },
		"target":   func(b *model.EvidenceBinding) { b.Target = &elsewhere.Ref },
	} {
		l.answer(attest, func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
			b := model.EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current, Target: req.Target}
			rebind(&b)
			return evidenceOf(attest, model.LinkAssuranceAsserted, "", p, req, b)
		})
		_, err := l.doLink(l.linkReq(id, target.Ref, attest, "", model.OperationID("op-"+name)))
		requireCode(t, err, model.Validation, "evidence.facts."+name)
		if len(l.pol.seen()) != 0 {
			t.Fatalf("%s: unbound evidence reached the authorizer", name)
		}
		l.unchanged(id, 0)
	}
}

// Import facts must name this identity's full key, not only its inbox.
func TestLinkImportFactsNameTheIdentityKey(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	id := l.provisional()
	answer := func(source string) {
		l.vs[model.EvidenceKindImport].Answer = func(_ model.Principal, req model.LinkRequest) (model.Evidence, error) {
			f := model.ImportFacts{
				EvidenceBinding: model.EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current, Target: req.Target},
				SourceSystem:    "crm", SourceRecord: "rec-1",
				IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: source},
			}
			facts, err := model.NewEvidenceFacts(f)
			return model.Evidence{Verifier: model.EvidenceKindImport, Facts: facts, Assurance: model.LinkAssuranceImported}, err
		}
	}
	answer("someone-else")
	_, err := l.doLink(l.linkReq(id, target.Ref, model.EvidenceKindImport, "", "op-1"))
	requireCode(t, err, model.Validation, "identity_key")
	l.unchanged(id, 0)
	answer(id.SourceID)
	if _, err := l.doLink(l.linkReq(id, target.Ref, model.EvidenceKindImport, "", "op-2")); err != nil {
		t.Fatal(err)
	}
	if got := l.stored(id.Ref).LinkAssurance; got != model.LinkAssuranceImported {
		t.Fatalf("link assurance = %s", got)
	}
}

// An evidence (verifier, reference) pair is single-use across links.
func TestLinkReusedEvidenceReferenceConflicts(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	a, b := l.provisional(), l.provisional()
	l.mustLink(&a, target.Ref, "challenge-7", "op-1")
	_, err := l.doLink(l.linkReq(b, target.Ref, attest, "challenge-7", "op-2"))
	requireCode(t, err, model.Conflict, "")
	l.unchanged(b, 1)
}

// The anchor must still be on the target when the link commits; an anchor
// never on the target is invalid evidence.
func TestLinkAnchorRelinkedBetweenVerifyAndCommit(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	anchor := l.provisional()
	l.seed(&anchor, target.Ref, model.LinkAssuranceAsserted)
	const hs = model.EvidenceKindHostSession
	l.answer(hs, bound(hs, model.LinkAssuranceHostAuthenticated, &anchor.Ref))

	stray := l.provisional()
	_, err := l.doLink(l.linkReq(stray, l.person().Ref, hs, "", "op-0"))
	requireCode(t, err, model.Validation, "anchor")
	if len(l.pol.seen()) != 0 {
		t.Fatal("anchor off the target reached the authorizer")
	}

	id := l.provisional()
	fired := false
	l.pol.set(func(a policy.Authorization) bool {
		if !fired {
			fired = true
			l.seed(&anchor, l.person().Ref, model.LinkAssuranceAsserted)
		}
		return true
	})
	_, err = l.doLink(l.linkReq(id, target.Ref, hs, "", "op-1"))
	requireCode(t, err, model.Conflict, "anchor")
	l.unchanged(id, 0)

	l.seed(&anchor, target.Ref, model.LinkAssuranceAsserted)
	l.pol.set(nil)
	if _, err := l.doLink(l.linkReq(id, target.Ref, hs, "", "op-2")); err != nil {
		t.Fatal(err)
	}
	recs := l.records(id.Ref)
	if len(recs) != 1 || recs[0].Anchor == nil || *recs[0].Anchor != anchor.Ref ||
		recs[0].Assurance != model.LinkAssuranceHostAuthenticated || recs[0].VerifierVersion != hostSessionVersion {
		t.Fatalf("anchored record %+v", recs)
	}
}

// A bot identity is never linked to a person contact, and no override
// exists for it.
func TestLinkKindMismatchHasNoOverride(t *testing.T) {
	l := newLinker(t)
	bot := l.botIdentity()
	_, err := l.doLink(l.linkReq(bot, l.person().Ref, attest, "", "op-1"))
	requireCode(t, err, model.Validation, "kind")
	if len(l.pol.seen()) != 0 || l.verifyCalls() != 0 {
		t.Fatal("kind mismatch verified or decided")
	}
	l.unchanged(bot, 0)
	if _, err := l.doLink(l.linkReq(bot, l.contact(model.ContactKindBot).Ref, attest, "", "op-2")); err != nil {
		t.Fatalf("same-kind link: %v", err)
	}
}

// botIdentity claims a bot identity on a bot provisional contact.
func (l *linker) botIdentity() model.Identity {
	l.T.Helper()
	c := l.contact(model.ContactKindBot)
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: l.inbox1(), ID: l.id()}, Contact: c.Ref, Namespace: "svc.bot", SourceID: "B-" + string(l.id()),
		Kind: model.ContactKindBot, LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
	}
	key := model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID}
	l.put(func(tx store.Tx) error { _, _, err := tx.ClaimIdentity(l.Ctx, key, id); return err })
	return id
}

// A reference into another tenant is not found, without asking the
// verifier or the policy.
func TestLinkCrossTenantIsNotFound(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	target := l.person()
	foreignID := id
	foreignID.Ref.Inbox.Tenant = tenantB
	_, err := l.doLink(l.linkReq(foreignID, target.Ref, attest, "", "op-1"))
	requireCode(t, err, model.NotFound, "")
	_, err = l.doLink(l.linkReq(id, model.ContactRef{Tenant: tenantB, ID: target.Ref.ID}, attest, "", "op-2"))
	requireCode(t, err, model.NotFound, "")
	_, err = l.doLink(l.linkReq(id, model.ContactRef{Tenant: tenantA, ID: l.id()}, attest, "", "op-3"))
	requireCode(t, err, model.NotFound, "")
	_, err = l.doUnlink(l.unlinkReq(foreignID, "customer_request", model.Evidence{}, "op-4"))
	requireCode(t, err, model.NotFound, "")
	if len(l.pol.seen()) != 0 || l.verifyCalls() != 0 {
		t.Fatal("out-of-scope link verified or decided")
	}
	l.unchanged(id, 0)
}
