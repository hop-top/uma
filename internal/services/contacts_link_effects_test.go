package services_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/model"
)

// Merge is N links: the source contact survives with no identities and
// relinking back undoes it, each record superseding the previous one.
func TestMergeByNLinksThenUndo(t *testing.T) {
	l := newLinker(t)
	source, into := l.person(), l.person()
	a, b := l.identity(l.inbox1(), source.Ref), l.identity(l.inbox2(), source.Ref)
	l.mustLink(&a, into.Ref, "", "merge-a")
	l.mustLink(&b, into.Ref, "", "merge-b")
	if _, err := l.storedContact(source.Ref); err != nil {
		t.Fatalf("merged-away contact: %v", err)
	}
	if got := l.identities(model.Filter{Contact: &source.Ref}, 10); len(got) != 0 {
		t.Fatalf("source identities = %v", identityIDs(got))
	}
	l.mustLink(&a, source.Ref, "", "undo-a")
	l.mustLink(&b, source.Ref, "", "undo-b")
	for _, id := range []model.Identity{a, b} {
		recs := l.records(id.Ref)
		if len(recs) != 2 || recs[0].Class != model.LinkClassLink || recs[1].Class != model.LinkClassRelink ||
			recs[0].Supersedes != "" || recs[1].Supersedes != recs[0].ID || recs[1].To != source.Ref {
			t.Fatalf("records of %s = %+v", id.Ref.ID, recs)
		}
	}
	if got := identityIDs(l.identities(model.Filter{Contact: &source.Ref}, 10)); !slices.Equal(got, []model.ID{a.Ref.ID, b.Ref.ID}) {
		t.Fatalf("restored identities = %v", got)
	}
}

// The verifier sees the intent core computed, the identity's current
// contact and revision, the target and the caller's evidence.
func TestVerifierSeesIntentCurrentAndRevision(t *testing.T) {
	l := newLinker(t)
	target, next := l.person(), l.person()
	id := l.provisional()
	ev := model.Evidence{Verifier: attest, Reference: "r1", Facts: model.Extension{Name: "host.proof", Version: 1, Data: []byte(`{"p":1}`)}}
	first := l.linkReq(id, target.Ref, attest, "", "op-1")
	first.Evidence = ev
	provisional := id.Contact
	if _, err := l.doLink(first); err != nil {
		t.Fatal(err)
	}
	id = l.stored(id.Ref)
	l.mustLink(&id, next.Ref, "r2", "op-2")
	att := model.Evidence{Verifier: attest, Reference: "r3"}
	if _, err := l.doUnlink(l.unlinkReq(id, "customer_request", att, "op-3")); err != nil {
		t.Fatal(err)
	}
	reqs := l.vs[attest].Requests()
	if len(reqs) != 3 {
		t.Fatalf("verify calls = %d", len(reqs))
	}
	want0 := model.LinkRequest{Intent: model.LinkClassLink, Identity: id.Ref, Current: provisional, ExpectedRevision: "i1", Target: &target.Ref, Evidence: ev}
	if !jsonEqual(reqs[0], want0) {
		t.Fatalf("link request = %+v", reqs[0])
	}
	if reqs[1].Intent != model.LinkClassRelink || reqs[1].Current != target.Ref || *reqs[1].Target != next.Ref {
		t.Fatalf("relink request = %+v", reqs[1])
	}
	if reqs[2].Intent != model.LinkClassUnlink || reqs[2].Current != next.Ref || reqs[2].Target != nil ||
		reqs[2].Reason != "customer_request" || reqs[2].ExpectedRevision != id.Revision {
		t.Fatalf("unlink request = %+v", reqs[2])
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Linking changes the identity's revision only; neither contact changes.
func TestLinkBumpsIdentityRevisionOnly(t *testing.T) {
	l := newLinker(t)
	target := l.person()
	id := l.provisional()
	from, _ := l.storedContact(id.Contact)
	l.mustLink(&id, target.Ref, "", "op-1")
	recs := l.records(id.Ref)
	if id.Revision == "i1" || id.Revision != recs[0].RevisionAfter || recs[0].RevisionBefore != "i1" {
		t.Fatalf("identity revision %s, record %s -> %s", id.Revision, recs[0].RevisionBefore, recs[0].RevisionAfter)
	}
	for _, c := range []model.Contact{from, target} {
		if got, _ := l.storedContact(c.Ref); !reflect.DeepEqual(got, c) {
			t.Fatalf("contact changed: %+v, was %+v", got, c)
		}
	}
}

// Linking to the current contact commits without verification, record or
// event, authorized as identity.read with no evidence context.
func TestLinkNoOpHasNoRecordOrEvent(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	acc, err := l.doLink(l.linkReq(id, id.Contact, attest, "unused", "op-1"))
	if err != nil || acc.Operation.State != model.Committed || acc.Replayed || *acc.Operation.Result != id.Ref.Ref() {
		t.Fatalf("no-op = %+v, %v", acc, err)
	}
	if l.verifyCalls() != 0 {
		t.Fatal("no-op verified")
	}
	checks := l.pol.seen()
	if got := actions(checks); !slices.Equal(got, []string{model.ActionIdentityRead}) {
		t.Fatalf("no-op actions = %v", got)
	}
	if checks[0].Assurance != "" || checks[0].Evidence != nil || checks[0].Destination != nil {
		t.Fatalf("no-op authorization carries link context: %+v", checks[0])
	}
	l.unchanged(id, 0)
	if len(l.records(id.Ref)) != 0 {
		t.Fatal("no-op wrote a record")
	}
	if _, err := l.operation(model.ActionIdentityLink, id.Ref, "op-1"); err != nil {
		t.Fatalf("no-op ledger row: %v", err)
	}
	again, err := l.doLink(l.linkReq(id, id.Contact, attest, "unused", "op-1"))
	if err != nil || !again.Replayed {
		t.Fatalf("no-op replay = %+v, %v", again, err)
	}
	if got := actions(l.pol.seen()); !slices.Equal(got, []string{model.ActionIdentityRead, model.ActionIdentityRead}) {
		t.Fatalf("no-op replay actions = %v", got)
	}
}

func TestLinkEmptyContactIDIsValidation(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	_, err := l.doLink(l.linkReq(id, model.ContactRef{Tenant: tenantA}, attest, "", "op-1"))
	requireCode(t, err, model.Validation, "contact")
	if len(l.pol.seen()) != 0 || l.verifyCalls() != 0 {
		t.Fatal("invalid link verified or decided")
	}
}

// There is no default verifier: a kind nobody registered, or a key that
// is no kind, is unsupported before any decision.
func TestLinkUnregisteredVerifierIsUnsupported(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	for _, key := range []string{model.EvidenceKindChallenge, "sms_code"} {
		_, err := l.doLink(l.linkReq(id, l.person().Ref, key, "r", model.OperationID("op-"+key)))
		requireCode(t, err, model.Unsupported, "verifier")
	}
	if len(l.pol.seen()) != 0 {
		t.Fatal("unregistered verifier reached the authorizer")
	}
	l.unchanged(id, 0)
}

// A verifier returning more than its kind may carry is not believed.
func TestLinkAssuranceAboveKindCapIsValidation(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	l.answer(attest, bound(attest, model.LinkAssuranceProven, nil))
	_, err := l.doLink(l.linkReq(id, l.person().Ref, attest, "", "op-1"))
	requireCode(t, err, model.Validation, "assurance")
	if len(l.pol.seen()) != 0 {
		t.Fatal("over-cap evidence reached the authorizer")
	}
	l.unchanged(id, 0)
}

// A relink at lower assurance than the current link needs the override
// action as well; the record takes the override decision's provenance.
func TestLinkDowngradeNeedsOverride(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	l.seed(&id, l.person().Ref, model.LinkAssuranceHostAuthenticated)
	to := l.person()
	l.mustLink(&id, to.Ref, "", "op-1")
	if got := actions(l.pol.seen()); !slices.Equal(got, []string{model.ActionIdentityRelink, model.ActionIdentityLinkOverride}) {
		t.Fatalf("downgrade actions = %v", got)
	}
	rec := l.records(id.Ref)[1]
	if rec.Assurance != model.LinkAssuranceAsserted || id.LinkAssurance != model.LinkAssuranceAsserted ||
		rec.ReasonCode != "allow_"+model.ActionIdentityLinkOverride {
		t.Fatalf("downgrade record %+v, identity %s", rec, id.LinkAssurance)
	}
	l.pol.set(nil)
	l.mustLink(&id, l.person().Ref, "", "op-2")
	if got := actions(l.pol.seen()); !slices.Equal(got, []string{model.ActionIdentityRelink}) {
		t.Fatalf("equal-assurance relink actions = %v", got)
	}
}

// identity.linked carries the record summary only: no facts, no other
// identities.
func TestLinkEmitsIdentityLinkedWithoutFacts(t *testing.T) {
	l := newLinker(t)
	anchor := l.provisional()
	target := l.person()
	l.seed(&anchor, target.Ref, model.LinkAssuranceAsserted)
	const va = model.EvidenceKindVerifiedAttribute
	l.answer(va, bound(va, model.LinkAssuranceAttributeMatch, &anchor.Ref))
	id := l.provisional()
	from := id.Contact
	if _, err := l.doLink(l.linkReq(id, target.Ref, va, "", "op-1")); err != nil {
		t.Fatal(err)
	}
	rec := l.records(id.Ref)[0]
	evs := l.linkedEvents()
	if len(evs) != 1 {
		t.Fatalf("identity.linked events = %d", len(evs))
	}
	ev := evs[0]
	if ev.Resource != id.Ref.Ref() || ev.Audience.Visibility != model.Internal || ev.Payload.Name != model.SchemaIdentityLinked ||
		ev.Payload.Version != model.PayloadVersion1 || ev.Revision != rec.RevisionAfter || ev.Correlation != "corr-svc" || ev.Actor != actor {
		t.Fatalf("event %+v", ev)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(raw)); !slices.Equal(keys, []string{"assurance", "class", "from", "link", "to"}) {
		t.Fatalf("payload keys = %v", keys)
	}
	if strings.Contains(string(ev.Payload.Data), "ada@example.com") || strings.Contains(string(ev.Payload.Data), string(anchor.Ref.ID)) {
		t.Fatal("payload leaks facts or the anchor")
	}
	var p model.IdentityLinkedPayload
	_ = json.Unmarshal(ev.Payload.Data, &p)
	if p != (model.IdentityLinkedPayload{Link: rec.ID, Class: model.LinkClassLink, From: from, To: target.Ref, Assurance: model.LinkAssuranceAttributeMatch}) {
		t.Fatalf("payload %+v", p)
	}
}

// The fingerprint covers the evidence and, for Unlink, the reason; not the
// correlation.
func TestLinkFingerprintCoversEvidenceAndReason(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	target := l.person()
	req := l.linkReq(id, target.Ref, attest, "r1", "op-1")
	if _, err := l.doLink(req); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*model.LinkIdentity){
		"reference": func(r *model.LinkIdentity) { r.Evidence.Reference = "r2" },
		"facts": func(r *model.LinkIdentity) {
			r.Evidence.Facts = model.Extension{Name: "host.proof", Version: 1, Data: []byte(`{}`)}
		},
		"verifier": func(r *model.LinkIdentity) { r.Evidence.Verifier = model.EvidenceKindImport },
		"contact":  func(r *model.LinkIdentity) { r.Contact = l.person().Ref },
		"revision": func(r *model.LinkIdentity) { r.ExpectedRevision = "i0" },
	} {
		changed := req
		change(&changed)
		if _, err := l.doLink(changed); !model.IsCode(err, model.Conflict) {
			t.Fatalf("%s change replayed: %v", name, err)
		}
	}
	same := req
	same.Correlation = "corr-other"
	if acc, err := l.doLink(same); err != nil || !acc.Replayed {
		t.Fatalf("correlation change = %+v, %v", acc, err)
	}

	id = l.stored(id.Ref)
	un := l.unlinkReq(id, "customer_request", model.Evidence{}, "op-2")
	if _, err := l.doUnlink(un); err != nil {
		t.Fatal(err)
	}
	un.Reason = "duplicate"
	_, err := l.doUnlink(un)
	requireCode(t, err, model.Conflict, "different payload")
	un.Reason, un.Evidence = "customer_request", model.Evidence{Verifier: attest, Reference: "r9"}
	_, err = l.doUnlink(un)
	requireCode(t, err, model.Conflict, "different payload")
}

// The record carries the deciding policy's provenance, the verifier
// version, the actor, the operation and the correlation.
func TestLinkRecordsDecisionProvenance(t *testing.T) {
	l := newLinker(t)
	anchor := l.provisional()
	target := l.person()
	l.seed(&anchor, target.Ref, model.LinkAssuranceAsserted)
	const hs = model.EvidenceKindHostSession
	l.answer(hs, bound(hs, model.LinkAssuranceHostAuthenticated, &anchor.Ref))
	id := l.provisional()
	acc, err := l.doLink(l.linkReq(id, target.Ref, hs, "sess-ref", "op-1"))
	if err != nil {
		t.Fatal(err)
	}
	rec := l.records(id.Ref)[0]
	key := model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID}
	if rec.PolicyVersion != "pv-1" || rec.ReasonCode != "allow_"+model.ActionIdentityLink || rec.VerifierVersion != hostSessionVersion ||
		rec.Actor != actor || rec.Operation != acc.Operation.Ref || rec.Correlation != "corr-svc" || rec.IdentityKey != key ||
		!rec.RecordedAt.Equal(l.Clock.Now()) || rec.Evidence.Verifier != hs || rec.Evidence.Reference != "sess-ref" ||
		rec.Evidence.Assurance != model.LinkAssuranceHostAuthenticated || rec.Reason != "" {
		t.Fatalf("record %+v", rec)
	}
	plain := l.provisional()
	l.mustLink(&plain, target.Ref, "", "op-2")
	if got := l.records(plain.Ref)[0].VerifierVersion; got != "" {
		t.Fatalf("unversioned verifier recorded %q", got)
	}
}
