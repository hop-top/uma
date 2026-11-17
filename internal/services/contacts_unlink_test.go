package services_test

import (
	"reflect"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

const reason = "customer_request"

// attested is unlink evidence asking the attestation verifier.
var attested = model.Evidence{Verifier: attest, Reference: "att-1"}

// Unlink detaches to a fresh contact of the identity's kind; the record
// names the previous contact and the reason, and the link assurance
// becomes none.
func TestUnlinkCreatesFreshContact(t *testing.T) {
	l := newLinker(t)
	bot := l.botIdentity()
	team := l.contact(model.ContactKindBot)
	l.mustLink(&bot, team.Ref, "", "op-1")
	contacts := l.contactCount()
	acc, err := l.doUnlink(l.unlinkReq(bot, reason, model.Evidence{}, "op-2"))
	if err != nil || acc.Operation.State != model.Committed || *acc.Operation.Result != bot.Ref.Ref() {
		t.Fatalf("Unlink = %+v, %v", acc, err)
	}
	after := l.stored(bot.Ref)
	fresh, err := l.storedContact(after.Contact)
	if err != nil || after.Contact == team.Ref || fresh.Kind != model.ContactKindBot || fresh.DisplayName != "" {
		t.Fatalf("fresh contact %+v, %v", fresh, err)
	}
	if l.contactCount() != contacts+1 {
		t.Fatal("unlink did not add exactly one contact")
	}
	recs := l.records(bot.Ref)
	rec := recs[len(recs)-1]
	if rec.Class != model.LinkClassUnlink || rec.From != team.Ref || rec.To != fresh.Ref || rec.Reason != reason ||
		rec.Assurance != model.LinkAssuranceNone || after.LinkAssurance != model.LinkAssuranceNone ||
		rec.Supersedes != recs[0].ID || !rec.Evidence.IsZero() || rec.ReasonCode != "allow_"+model.ActionIdentityUnlink {
		t.Fatalf("unlink record %+v", rec)
	}
	if got, _ := l.storedContact(team.Ref); !reflect.DeepEqual(got, team) {
		t.Fatal("previous contact changed")
	}
	evs := l.linkedEvents()
	if len(evs) != 2 || evs[1].Resource != bot.Ref.Ref() {
		t.Fatalf("events %+v", evs)
	}
}

func TestUnlinkRequiresReason(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	for _, r := range []string{"", "Free text reason"} {
		_, err := l.doUnlink(l.unlinkReq(id, r, model.Evidence{}, "op-1"))
		requireCode(t, err, model.Validation, "reason")
	}
	if len(l.pol.seen()) != 0 {
		t.Fatal("reasonless unlink decided")
	}
	l.unchanged(id, 0)
}

// Without evidence Unlink is decided as identity.unlink with no
// destination, assurance none and no evidence.
func TestUnlinkEvidenceOptionalAndAssuranceNone(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	if _, err := l.doUnlink(l.unlinkReq(id, reason, model.Evidence{}, "op-1")); err != nil {
		t.Fatal(err)
	}
	want := policy.Authorization{
		Principal: principalA(), Action: model.ActionIdentityUnlink, Resource: id.Ref.Ref(),
		Revision: id.Revision, Assurance: model.LinkAssuranceNone,
	}
	if got := l.pol.seen(); len(got) != 1 || !sameAuthorization(got[0], want) {
		t.Fatalf("authorizations = %+v, want [%+v]", got, want)
	}
	if l.verifyCalls() != 0 {
		t.Fatal("unlink without evidence called a verifier")
	}
}

// requireEvidence denies identity.unlink unless verified evidence reaches
// the authorizer.
func requireEvidence(a policy.Authorization) bool {
	return a.Action != model.ActionIdentityUnlink || a.Evidence != nil
}

// Host policy can require verified evidence to unlink: it denies when none
// was supplied and admits attested evidence, which the record keeps.
func TestUnlinkPolicyMayRequireEvidence(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	l.pol.set(requireEvidence)
	_, err := l.doUnlink(l.unlinkReq(id, reason, model.Evidence{}, "op-1"))
	requireCode(t, err, model.Unauthorized, "")
	if _, err := l.doUnlink(l.unlinkReq(id, reason, attested, "op-2")); err != nil {
		t.Fatalf("attested unlink: %v", err)
	}
	checks := l.pol.seen()
	last := checks[len(checks)-1]
	if last.Evidence == nil || *last.Evidence != (policy.VerifiedEvidence{Verifier: attest, Reference: "att-1", Assurance: model.LinkAssuranceAsserted}) ||
		last.Assurance != model.LinkAssuranceAsserted {
		t.Fatalf("attested authorization %+v", last)
	}
	rec := l.records(id.Ref)[0]
	if rec.Evidence.Verifier != attest || rec.Evidence.Reference != "att-1" || rec.Evidence.Assurance != model.LinkAssuranceAsserted ||
		len(rec.Evidence.Facts.Data) == 0 || rec.Assurance != model.LinkAssuranceNone {
		t.Fatalf("attested unlink record %+v", rec)
	}
}

// A denial for missing evidence happens before any write: no operation
// claimed, no contact created, identity and event log unchanged.
func TestUnlinkRequiringEvidenceDeniesBeforeAnyWrite(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	contacts := l.contactCount()
	var head model.Cursor
	l.view(func(r store.Reader) (err error) { head, err = r.EventHead(l.Ctx); return err })
	l.pol.set(requireEvidence)
	_, err := l.doUnlink(l.unlinkReq(id, reason, model.Evidence{}, "op-1"))
	requireCode(t, err, model.Unauthorized, "")
	if _, err := l.operation(model.ActionIdentityUnlink, id.Ref, "op-1"); !model.IsCode(err, model.NotFound) {
		t.Fatalf("denied unlink claimed its operation: %v", err)
	}
	if l.contactCount() != contacts {
		t.Fatal("denied unlink created a contact")
	}
	var now model.Cursor
	l.view(func(r store.Reader) (err error) { now, err = r.EventHead(l.Ctx); return err })
	if now != head {
		t.Fatal("denied unlink appended events")
	}
	l.unchanged(id, 0)
}

// Assurance comes from the verifier only: a request declaring one is
// invalid before the verifier or the policy is asked.
func TestRequestAssuranceIsRejected(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	req := l.linkReq(id, l.person().Ref, attest, "", "op-1")
	req.Evidence.Assurance = model.LinkAssuranceProven
	_, err := l.doLink(req)
	requireCode(t, err, model.Validation, "assurance")
	ev := attested
	ev.Assurance = model.LinkAssuranceNone
	_, err = l.doUnlink(l.unlinkReq(id, reason, ev, "op-2"))
	requireCode(t, err, model.Validation, "assurance")
	if l.verifyCalls() != 0 || len(l.pol.seen()) != 0 {
		t.Fatal("declared assurance verified or decided")
	}
	l.unchanged(id, 0)
}

// Evidence the verifier rejects is validation; the authorizer is never
// asked and nothing is written.
func TestUnverifiedEvidenceNeverReachesAuthorizer(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	l.answer(attest, nil)
	_, err := l.doLink(l.linkReq(id, l.person().Ref, attest, "", "op-1"))
	requireCode(t, err, model.Validation, "evidence")
	_, err = l.doUnlink(l.unlinkReq(id, reason, attested, "op-2"))
	requireCode(t, err, model.Validation, "evidence")
	if len(l.pol.seen()) != 0 {
		t.Fatal("unverified evidence reached the authorizer")
	}
	if l.vs[attest].Calls() != 2 {
		t.Fatalf("verify calls = %d", l.vs[attest].Calls())
	}
	l.unchanged(id, 0)
	if _, err := l.operation(model.ActionIdentityLink, id.Ref, "op-1"); !model.IsCode(err, model.NotFound) {
		t.Fatal("unverified link claimed its operation")
	}
}

// No evidence and evidence verified at none are different: the first has
// no Evidence, the second Evidence at none.
func TestAuthorizerDistinguishesMissingFromVerifiedNone(t *testing.T) {
	l := newLinker(t)
	a, b := l.provisional(), l.provisional()
	l.answer(attest, bound(attest, model.LinkAssuranceNone, nil))
	if _, err := l.doUnlink(l.unlinkReq(a, reason, model.Evidence{}, "op-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.doUnlink(l.unlinkReq(b, reason, attested, "op-2")); err != nil {
		t.Fatal(err)
	}
	checks := l.pol.seen()
	if len(checks) != 2 {
		t.Fatalf("authorizations = %d", len(checks))
	}
	if checks[0].Evidence != nil || checks[0].Assurance != model.LinkAssuranceNone {
		t.Fatalf("missing evidence authorization %+v", checks[0])
	}
	if checks[1].Evidence == nil || checks[1].Evidence.Assurance != model.LinkAssuranceNone || checks[1].Assurance != model.LinkAssuranceNone {
		t.Fatalf("verified-none authorization %+v", checks[1])
	}
}

// intentAnswer returns attestation facts declaring intent, whatever core
// asked for.
func intentAnswer(intent model.LinkClass, target *model.ContactRef) func(model.Principal, model.LinkRequest) (model.Evidence, error) {
	return func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
		b := model.EvidenceBinding{Intent: intent, Identity: req.Identity, Current: req.Current, Target: target}
		return evidenceOf(attest, model.LinkAssuranceAsserted, req.Evidence.Reference, p, req, b)
	}
}

func TestUnlinkEvidenceMustCarryUnlinkIntent(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	l.answer(attest, intentAnswer(model.LinkClassLink, nil))
	_, err := l.doUnlink(l.unlinkReq(id, reason, attested, "op-1"))
	requireCode(t, err, model.Validation, "intent")
	if len(l.pol.seen()) != 0 {
		t.Fatal("link-intent evidence reached the unlink decision")
	}
	l.unchanged(id, 0)
}

func TestLinkRejectsUnlinkIntentEvidence(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	target := l.person()
	l.answer(attest, intentAnswer(model.LinkClassUnlink, &target.Ref))
	_, err := l.doLink(l.linkReq(id, target.Ref, attest, "", "op-1"))
	requireCode(t, err, model.Validation, "intent")
	if len(l.pol.seen()) != 0 {
		t.Fatal("unlink-intent evidence reached the link decision")
	}
	l.unchanged(id, 0)
}

// Unlinking rewrites no message: authors keep their identity reference.
func TestUnlinkKeepsMessageAuthorIdentity(t *testing.T) {
	l := newLinker(t)
	id := l.provisional()
	shared := l.person()
	l.mustLink(&id, shared.Ref, "", "op-1")
	msg := model.Message{
		Ref: model.MessageRef{Conversation: l.Conv.Ref, ID: l.id()}, Audience: model.Audience{Visibility: model.Public},
		Author:     model.Author{Identity: id.Ref, Kind: model.ContactKindPerson},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi"}}},
		External:   []model.ExternalRef{{Channel: l.Ch.Ref, Namespace: routeNSpace, Kind: "message", ID: string(l.id())}},
		RecordedAt: l.Clock.Now(), Revision: "m1",
	}
	l.put(func(tx store.Tx) error { return tx.PutMessage(l.Ctx, msg, "") })
	if _, err := l.doUnlink(l.unlinkReq(id, reason, model.Evidence{}, "op-2")); err != nil {
		t.Fatal(err)
	}
	var got model.Message
	l.view(func(r store.Reader) (err error) { got, err = r.GetMessage(l.Ctx, msg.Ref); return err })
	if got.Author != msg.Author || got.Revision != msg.Revision {
		t.Fatalf("message rewritten: %+v", got.Author)
	}
	conv := l.Conv
	var after model.Conversation
	l.view(func(r store.Reader) (err error) { after, err = r.GetConversation(l.Ctx, conv.Ref); return err })
	if after.ReplyRoute != conv.ReplyRoute || !slices.Equal(after.Participants, conv.Participants) {
		t.Fatal("unlink changed the conversation's route or participants")
	}
}
