package conformance

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

// requireUntouched fails unless id's request left nothing behind: no
// decision, no history, no claimed operation, the identity and the
// identity.linked log as before.
func (env *contactsEnv) requireUntouched(name string, id model.Identity, events int, kind string, op model.OperationID) {
	env.t.Helper()
	if got := env.pol.take(); len(got) != 0 {
		env.t.Fatalf("%s: reached the authorizer: %+v", name, got)
	}
	if got := env.history(model.Filter{Identity: &id.Ref}, 10); len(got) != 0 {
		env.t.Fatalf("%s: history %+v", name, got)
	}
	if env.claimed(kind, id.Ref.Ref(), op) {
		env.t.Fatalf("%s: operation claimed", name)
	}
	env.unchanged(id, events)
	env.pol.take() // the history read's own decisions
}

// contactsVerifierCannotBypassScope: whatever a verifier returns, core
// checks it before any decision. Evidence bound to a foreign tenant's
// contact, another identity or contact, the other intent, above the
// kind's cap, expired, under another kind or anchored off the target is
// model.Validation; a failing verifier too; an unregistered kind is
// model.Unsupported; a request declaring assurance is model.Validation
// before the verifier is asked. None reaches the authorizer or writes.
// A verifier returning one constant reference has it consumed once.
func contactsVerifierCannotBypassScope(t *testing.T, env *contactsEnv) {
	id, _ := env.arrive(env.one, "alice")
	other, _ := env.arrive(env.two, "bob")
	target := env.person()
	honest := AttestationVerifier{}
	rebind := func(fn func(*model.Evidence, *model.AttestationFacts)) func(model.Principal, model.LinkRequest) (model.Evidence, error) {
		return func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
			ev, _ := honest.Evidence(p, req)
			f := model.AttestationFacts{EvidenceBinding: Binding(req), Actor: p.Actor, ReasonCode: "same_person"}
			fn(&ev, &f)
			facts, err := model.NewEvidenceFacts(f)
			ev.Facts = facts
			return ev, err
		}
	}
	past := env.clock.Now().Add(-time.Minute)
	foreign := model.ContactRef{Tenant: "conformance-other", ID: target.Ref.ID}
	scripts := map[string]func(model.Principal, model.LinkRequest) (model.Evidence, error){
		"foreign tenant contact": rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.Target = &foreign }),
		"other identity":         rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.Identity = other.Ref }),
		"other current contact":  rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.Current = other.Contact }),
		"other target":           rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.Target = &other.Contact }),
		"other intent": rebind(func(_ *model.Evidence, f *model.AttestationFacts) {
			f.Intent, f.Target = model.LinkClassUnlink, nil
		}),
		"relink intent":      rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.Intent = model.LinkClassRelink }),
		"other actor":        rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.Actor = "someone-else" }),
		"assurance over cap": rebind(func(ev *model.Evidence, _ *model.AttestationFacts) { ev.Assurance = model.LinkAssuranceProven }),
		"assurance none":     rebind(func(ev *model.Evidence, _ *model.AttestationFacts) { ev.Assurance = model.LinkAssuranceNone }),
		"expired":            rebind(func(_ *model.Evidence, f *model.AttestationFacts) { f.ExpiresAt = &past }),
		"another verifier":   rebind(func(ev *model.Evidence, _ *model.AttestationFacts) { ev.Verifier = model.EvidenceKindImport }),
		"anchor on attestation": rebind(func(_ *model.Evidence, f *model.AttestationFacts) {
			f.Anchor = &other.Ref
		}),
		"another kind's facts": func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
			ev, err := sessionAnswer(&other.Ref, model.LinkAssuranceAsserted)(p, req)
			ev.Verifier = attestKind
			return ev, err
		},
		"verifier fails": func(model.Principal, model.LinkRequest) (model.Evidence, error) {
			return model.Evidence{}, errors.New("verifier refuses")
		},
	}
	env.pol.take()
	for name, script := range scripts {
		env.attest.Script(script)
		req := env.linkReq(id, target.Ref, "")
		_, err := env.link(req)
		if !model.IsCode(err, model.Validation) {
			t.Fatalf("%s: error = %v, want validation", name, err)
		}
		env.requireUntouched(name, id, 0, model.ActionIdentityLink, req.Operation)
	}
	env.attest.Script(honest.Evidence)

	// A host_session anchor that is not on the target.
	env.session.Script(sessionAnswer(&other.Ref, model.LinkAssuranceHostAuthenticated))
	req := env.linkReq(id, target.Ref, "")
	req.Evidence.Verifier = model.EvidenceKindHostSession
	_, err := env.link(req)
	serviceCode(t, err, model.Validation)
	env.requireUntouched("anchor off the target", id, 0, model.ActionIdentityLink, req.Operation)

	for _, kind := range []string{model.EvidenceKindChallenge, model.EvidenceKindImport, "sms_code"} {
		req := env.linkReq(id, target.Ref, "r")
		req.Evidence.Verifier = kind
		_, err := env.link(req)
		serviceCode(t, err, model.Unsupported)
		env.requireUntouched("unregistered "+kind, id, 0, model.ActionIdentityLink, req.Operation)
	}

	calls := env.attest.Calls()
	declared := env.linkReq(id, target.Ref, "")
	declared.Evidence.Assurance = model.LinkAssuranceProven
	_, err = env.link(declared)
	serviceCode(t, err, model.Validation)
	un := env.unlinkReq(id, "customer_request", model.Evidence{Verifier: attestKind, Assurance: model.LinkAssuranceNone})
	_, err = env.unlink(un)
	serviceCode(t, err, model.Validation)
	if env.attest.Calls() != calls {
		t.Fatal("a request declaring assurance reached the verifier")
	}
	env.requireUntouched("declared assurance", id, 0, model.ActionIdentityLink, declared.Operation)

	// The verifier's reference, not the caller's, is persisted and
	// consumed.
	env.attest.Script(func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
		ev, err := honest.Evidence(p, req)
		ev.Reference = "constant"
		return ev, err
	})
	env.mustLink(&id, target.Ref, "first")
	if r := env.records(id.Ref); len(r) != 1 || r[0].Evidence.Reference != "constant" {
		t.Fatalf("record evidence %+v, want the verifier's reference", r)
	}
	env.pol.take()
	second := env.linkReq(other, target.Ref, "second")
	_, err = env.link(second)
	serviceCode(t, err, model.Conflict)
	if env.claimed(model.ActionIdentityLink, other.Ref.Ref(), second.Operation) || len(env.records(other.Ref)) != 0 {
		t.Fatal("a consumed verifier reference linked again")
	}
	env.unchanged(other, 1)
}

// contactsEvidenceIntentNotTransferable: evidence a verifier issued for a
// link cannot authorize an unlink, nor unlink evidence a link; both are
// model.Validation, write nothing and leave the reference unconsumed.
func contactsEvidenceIntentNotTransferable(t *testing.T, env *contactsEnv) {
	id, _ := env.arrive(env.one, "alice")
	target := env.person()
	honest := AttestationVerifier{}
	var issued model.LinkRequest
	capture := func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
		issued = req
		return honest.Evidence(p, req)
	}
	// The link evidence is issued, then denied, so nothing commits.
	env.attest.Script(capture)
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionIdentityLink })
	_, err := env.link(env.linkReq(id, target.Ref, "ref-x"))
	serviceCode(t, err, model.Unauthorized)
	env.pol.set(nil)
	linkEv, err := honest.Evidence(model.Principal{Tenant: serviceTenant, Actor: contactsActor}, issued)
	if err != nil {
		t.Fatal(err)
	}
	unlinkReq := issued
	unlinkReq.Intent, unlinkReq.Target, unlinkReq.Reason = model.LinkClassUnlink, nil, "customer_request"
	unlinkEv, err := honest.Evidence(model.Principal{Tenant: serviceTenant, Actor: contactsActor}, unlinkReq)
	if err != nil {
		t.Fatal(err)
	}

	env.attest.Script(func(model.Principal, model.LinkRequest) (model.Evidence, error) { return linkEv, nil })
	un := env.unlinkReq(id, "customer_request", model.Evidence{Verifier: attestKind, Reference: "ref-x"})
	_, err = env.unlink(un)
	serviceCode(t, err, model.Validation)
	env.requireUntouched("link evidence on unlink", id, 0, model.ActionIdentityUnlink, un.Operation)

	env.attest.Script(func(model.Principal, model.LinkRequest) (model.Evidence, error) { return unlinkEv, nil })
	req := env.linkReq(id, target.Ref, "ref-x")
	_, err = env.link(req)
	serviceCode(t, err, model.Validation)
	env.requireUntouched("unlink evidence on link", id, 0, model.ActionIdentityLink, req.Operation)

	// Link evidence does not carry over to a relink either.
	env.attest.Script(honest.Evidence)
	env.mustLink(&id, target.Ref, "ref-x")
	next := env.person()
	env.attest.Script(func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
		req.Intent = model.LinkClassLink
		return honest.Evidence(p, req)
	})
	env.pol.take()
	relink := env.linkReq(id, next.Ref, "ref-y")
	_, err = env.link(relink)
	serviceCode(t, err, model.Validation)
	if got := env.pol.take(); len(got) != 0 {
		t.Fatalf("link-intent evidence reached the relink decision: %+v", got)
	}
	env.unchanged(id, 1)
	env.attest.Script(honest.Evidence)
	env.mustLink(&id, next.Ref, "ref-y")
}

// contactsDecisionsCarryVerifiedEvidence: a no-op link is decided as
// identity.read with no evidence context and writes no record or event;
// missing and verified-none evidence reach the authorizer differently; an
// authorizer that never reads Assurance or Evidence still composes and
// links.
func contactsDecisionsCarryVerifiedEvidence(t *testing.T, env *contactsEnv) {
	id, _ := env.arrive(env.one, "alice")
	calls := env.attest.Calls()
	env.pol.take()
	req := env.linkReq(id, id.Contact, "unused")
	acc, err := env.link(req)
	if err != nil || acc.Operation.State != model.Committed || acc.Replayed || *acc.Operation.Result != id.Ref.Ref() {
		t.Fatalf("no-op = %+v, %v", acc, err)
	}
	requireDecisions(t, env.pol.take(), id, nil, "", nil, model.ActionIdentityRead)
	if env.attest.Calls() != calls || len(env.records(id.Ref)) != 0 || !env.claimed(model.ActionIdentityLink, id.Ref.Ref(), req.Operation) {
		t.Fatal("no-op verified, wrote a record or claimed nothing")
	}
	env.unchanged(id, 0)
	again, err := env.link(req)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("no-op replay = %+v, %v", again, err)
	}
	requireDecisions(t, env.pol.take(), id, nil, "", nil, model.ActionIdentityRead)

	a, _ := env.arrive(env.one, "missing")
	b, _ := env.arrive(env.one, "verified-none")
	if _, err := env.unlink(env.unlinkReq(a, "customer_request", model.Evidence{})); err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, env.pol.take(), a, nil, model.LinkAssuranceNone, nil, model.ActionIdentityUnlink)
	env.attest.Script(func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
		ev, err := AttestationVerifier{}.Evidence(p, req)
		ev.Assurance = model.LinkAssuranceNone
		return ev, err
	})
	if _, err := env.unlink(env.unlinkReq(b, "customer_request", model.Evidence{Verifier: attestKind, Reference: "none-1"})); err != nil {
		t.Fatal(err)
	}
	requireDecisions(t, env.pol.take(), b, nil, model.LinkAssuranceNone,
		&policy.VerifiedEvidence{Verifier: attestKind, Reference: "none-1", Assurance: model.LinkAssuranceNone}, model.ActionIdentityUnlink)

	// An authorizer that knows nothing of evidence context.
	var ignoring policy.Authorizer = policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
		return policy.Decision{Allow: a.Action != ""}, nil
	})
	e, err := uma.New(uma.Config{
		Store: env.st, Authorizer: ignoring, Clock: env.clock, IDs: portstest.NewSequenceIDs(0xc0d),
		IdentityVerifiers: map[string]policy.IdentityVerifier{attestKind: AttestationVerifier{}},
	})
	if err != nil {
		t.Fatalf("uma.New with an evidence-blind authorizer: %v", err)
	}
	defer func() { _ = e.Close(env.ctx) }()
	cl, err := e.For(model.Principal{Tenant: serviceTenant, Actor: contactsActor})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := env.arrive(env.two, "carol")
	if _, err := cl.Contacts().Link(env.ctx, env.linkReq(c, env.person().Ref, "blind-1")); err != nil {
		t.Fatalf("Link under an evidence-blind authorizer: %v", err)
	}
}

// contactsStaleAndSingleUse: a stale expected revision is model.Conflict
// with reconcile_first before verification, and so is an identity that
// moved while the policy decided, caught by the commit's revision check;
// an evidence reference is consumed once, by Link or Unlink, and a refused
// reuse claims nothing.
func contactsStaleAndSingleUse(t *testing.T, env *contactsEnv) {
	id, _ := env.arrive(env.one, "alice")
	target := env.person()
	calls := env.attest.Calls()
	env.pol.take()
	stale := env.linkReq(id, target.Ref, "s-1")
	stale.ExpectedRevision = "stale"
	_, err := env.link(stale)
	requireReconcile(t, err)
	if env.attest.Calls() != calls || len(env.pol.take()) != 0 {
		t.Fatal("stale request verified or decided")
	}

	// The identity moves between the decision and the commit.
	moved := env.person()
	env.pol.hook(func(a policy.Authorization) {
		if a.Action != model.ActionIdentityLink || a.Evidence == nil || a.Evidence.Reference != "s-2" {
			return
		}
		env.pol.hook(nil)
		racer := env.identity(id.Ref)
		if _, err := env.link(env.linkReq(racer, moved.Ref, "racer")); err != nil {
			t.Errorf("racing link: %v", err)
		}
	})
	req := env.linkReq(id, target.Ref, "s-2")
	_, err = env.link(req)
	requireReconcile(t, err)
	env.pol.hook(nil)
	if recs := env.records(id.Ref); len(recs) != 1 || recs[0].To != moved.Ref {
		t.Fatalf("records = %+v, want only the racing link", recs)
	}
	if env.claimed(model.ActionIdentityLink, id.Ref.Ref(), req.Operation) {
		t.Fatal("stale commit claimed its operation")
	}
	id = env.identity(id.Ref)

	// Single use across identities and across Link and Unlink.
	b, _ := env.arrive(env.two, "bob")
	c, _ := env.arrive(env.two, "carol")
	reuse := env.linkReq(b, target.Ref, "racer")
	_, err = env.link(reuse)
	serviceCode(t, err, model.Conflict)
	un := env.unlinkReq(c, "customer_request", model.Evidence{Verifier: attestKind, Reference: "racer"})
	_, err = env.unlink(un)
	serviceCode(t, err, model.Conflict)
	if env.claimed(model.ActionIdentityLink, b.Ref.Ref(), reuse.Operation) || env.claimed(model.ActionIdentityUnlink, c.Ref.Ref(), un.Operation) {
		t.Fatal("a refused reuse claimed its operation")
	}
	env.unchanged(b, 1)
	env.unchanged(c, 1)
	// alice's, bob's and carol's provisional contacts, target and moved:
	// the refused unlink left no fresh contact.
	if n := len(env.allContacts()); n != 5 {
		t.Fatalf("contacts = %d, want 5", n)
	}
}

// contactsReplayAfterPurge: once the operation is purged past its horizon
// the same command is a new one, judged on the identity as it is now.
func contactsReplayAfterPurge(t *testing.T, env *contactsEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	a, _ := env.arrive(env.one, "alice")
	b, _ := env.arrive(env.one, "bob")
	link := env.linkReq(a, env.person().Ref, "p-1")
	if _, err := env.link(link); err != nil {
		t.Fatal(err)
	}
	noop := env.linkReq(b, b.Contact, "")
	first, err := env.link(noop)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := env.link(link); err != nil || !again.Replayed {
		t.Fatalf("replay before the purge = %+v, %v", again, err)
	}
	env.pastHorizon()
	if rep := env.purge(); rep.Operations == 0 {
		t.Fatalf("purge report %+v, want operations purged", rep)
	}
	if env.claimed(model.ActionIdentityLink, a.Ref.Ref(), link.Operation) {
		t.Fatal("operation survived the purge")
	}
	_, err = env.link(link)
	requireReconcile(t, err)
	acc, err := env.link(noop)
	if err != nil || acc.Replayed || acc.Operation.Ref == first.Operation.Ref {
		t.Fatalf("no-op after the purge = %+v, %v; want a new command", acc, err)
	}
	if n := len(env.records(a.Ref)); n != 1 {
		t.Fatalf("records = %d, want 1", n)
	}
}
