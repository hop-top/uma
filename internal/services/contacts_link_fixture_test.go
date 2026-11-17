package services_test

import (
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// linker is a contacts fixture with a scripted verifier registered for
// every evidence kind but challenge. Each answers with evidence bound to
// the request at its kind's maximum; the anchored kinds need an anchor set
// by the test. host_session reports a version.
type linker struct {
	*book
	vs map[string]*enginetest.ScriptedVerifier
}

const hostSessionVersion = "hs-2"

func newLinker(t *testing.T) *linker {
	t.Helper()
	vs := map[string]*enginetest.ScriptedVerifier{}
	reg := map[string]policy.IdentityVerifier{}
	for _, k := range model.EvidenceKinds() {
		if k == model.EvidenceKindChallenge {
			continue
		}
		top, _ := model.MaxLinkAssurance(k)
		vs[k] = &enginetest.ScriptedVerifier{Answer: bound(k, top, nil)}
		reg[k] = vs[k]
	}
	reg[model.EvidenceKindHostSession] = enginetest.VersionedVerifier{IdentityVerifier: vs[model.EvidenceKindHostSession], V: hostSessionVersion}
	s := newSvc(t, func(c *engine.Config) { c.IdentityVerifiers = reg })
	return &linker{book: &book{svc: s, heads: map[model.IdentityRef]model.ID{}}, vs: vs}
}

// bound answers with kind's facts bound to the request: intent, identity,
// current contact and target as core computed them, plus anchor for link
// and relink.
func bound(kind string, level model.LinkAssurance, anchor *model.IdentityRef) func(model.Principal, model.LinkRequest) (model.Evidence, error) {
	return func(p model.Principal, req model.LinkRequest) (model.Evidence, error) {
		b := model.EvidenceBinding{Intent: req.Intent, Identity: req.Identity, Current: req.Current, Target: req.Target}
		if req.Intent != model.LinkClassUnlink {
			b.Anchor = anchor
		}
		return evidenceOf(kind, level, req.Evidence.Reference, p, req, b)
	}
}

func evidenceOf(kind string, level model.LinkAssurance, ref string, p model.Principal, req model.LinkRequest, b model.EvidenceBinding) (model.Evidence, error) {
	var f model.EvidenceFacts
	switch kind {
	case model.EvidenceKindAttestation:
		code := req.Reason
		if code == "" {
			code = "same_person"
		}
		f = model.AttestationFacts{EvidenceBinding: b, Actor: p.Actor, ReasonCode: code}
	case model.EvidenceKindVerifiedAttribute:
		f = model.VerifiedAttributeFacts{EvidenceBinding: b, Attribute: "email", Value: "ada@example.com"}
	case model.EvidenceKindHostSession:
		f = model.HostSessionFacts{EvidenceBinding: b, SessionSubject: "session-1"}
	default:
		panic("fixture has no facts for " + kind)
	}
	facts, err := model.NewEvidenceFacts(f)
	return model.Evidence{Verifier: kind, Reference: ref, Facts: facts, Assurance: level}, err
}

// answer replaces kind's verifier answer.
func (l *linker) answer(kind string, fn func(model.Principal, model.LinkRequest) (model.Evidence, error)) {
	l.vs[kind].Answer = fn
}

func (l *linker) verifyCalls() int {
	n := 0
	for _, v := range l.vs {
		n += v.Calls()
	}
	return n
}

func (l *linker) person() model.Contact { return l.contact(model.ContactKindPerson) }

// provisional returns a person identity in inbox one on a fresh
// provisional contact.
func (l *linker) provisional() model.Identity {
	l.T.Helper()
	return l.identity(l.inbox1(), l.person().Ref)
}

func (l *linker) linkReq(id model.Identity, to model.ContactRef, kind, ref string, op model.OperationID) model.LinkIdentity {
	return model.LinkIdentity{
		Command: cmd(op, id.Revision), Identity: id.Ref, Contact: to,
		Evidence: model.Evidence{Verifier: kind, Reference: ref},
	}
}

func (l *linker) unlinkReq(id model.Identity, reason string, ev model.Evidence, op model.OperationID) model.UnlinkIdentity {
	return model.UnlinkIdentity{Command: cmd(op, id.Revision), Identity: id.Ref, Reason: reason, Evidence: ev}
}

func (l *linker) doLink(req model.LinkIdentity) (model.Acceptance, error) {
	return l.C.Contacts().Link(l.Ctx, req)
}

func (l *linker) doUnlink(req model.UnlinkIdentity) (model.Acceptance, error) {
	return l.C.Contacts().Unlink(l.Ctx, req)
}

// mustLink links id to `to` with attestation evidence and advances id.
func (l *linker) mustLink(id *model.Identity, to model.ContactRef, ref string, op model.OperationID) model.Acceptance {
	l.T.Helper()
	acc, err := l.doLink(l.linkReq(*id, to, model.EvidenceKindAttestation, ref, op))
	if err != nil {
		l.T.Fatalf("Link: %v", err)
	}
	*id = l.stored(id.Ref)
	return acc
}

// seed appends a link record at level directly through the store,
// bypassing the service, and advances id.
func (l *linker) seed(id *model.Identity, to model.ContactRef, level model.LinkAssurance) {
	l.T.Helper()
	head := l.head(id.Ref)
	class := model.LinkClassRelink
	if head == "" {
		class = model.LinkClassLink
	}
	rec := model.IdentityLink{
		ID: l.id(), Tenant: tenantA, Identity: id.Ref,
		IdentityKey: model.IdentityKey{Inbox: id.Ref.Inbox, Namespace: id.Namespace, SourceID: id.SourceID},
		Class:       class, From: id.Contact, To: to, Supersedes: head,
		Evidence:  model.Evidence{Verifier: model.EvidenceKindAttestation, Assurance: level},
		Assurance: level, Actor: actor, Operation: model.OperationRef{Tenant: tenantA, ID: l.id()},
		RevisionBefore: id.Revision, RevisionAfter: model.Revision(l.id()), RecordedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	l.put(func(tx store.Tx) error { return tx.LinkIdentity(l.Ctx, rec) })
	*id = l.stored(id.Ref)
}

func (l *linker) stored(ref model.IdentityRef) model.Identity {
	l.T.Helper()
	var id model.Identity
	l.view(func(r store.Reader) (err error) { id, err = r.GetIdentity(l.Ctx, ref); return err })
	return id
}

func (l *linker) storedContact(ref model.ContactRef) (model.Contact, error) {
	var c model.Contact
	err := l.E.Store().View(l.Ctx, tenantA, func(r store.Reader) (err error) { c, err = r.GetContact(l.Ctx, ref); return err })
	return c, err
}

func (l *linker) records(ref model.IdentityRef) []model.IdentityLink {
	l.T.Helper()
	var out []model.IdentityLink
	l.view(func(r store.Reader) error {
		p, err := r.IdentityLinks(l.Ctx, store.LinkFilter{Identity: &ref}, model.PageRequest{Limit: 100})
		out = p.Items
		return err
	})
	return out
}

func (l *linker) head(ref model.IdentityRef) model.ID {
	recs := l.records(ref)
	if len(recs) == 0 {
		return ""
	}
	return recs[len(recs)-1].ID
}

func (l *linker) linkedEvents() []model.Event {
	l.T.Helper()
	var out []model.Event
	l.view(func(r store.Reader) error {
		p, err := r.Events(l.Ctx, model.Filter{Kinds: []string{model.EventIdentityLinked}}, "", 100)
		out = p.Items
		return err
	})
	return out
}

func (l *linker) contactCount() int {
	l.T.Helper()
	var n int
	l.view(func(r store.Reader) error {
		p, err := r.Contacts(l.Ctx, model.ListRequest{Page: model.PageRequest{Limit: 500}})
		n = len(p.Items)
		return err
	})
	return n
}

func (l *linker) operation(kind string, target model.IdentityRef, op model.OperationID) (store.OperationRecord, error) {
	var rec store.OperationRecord
	err := l.E.Store().View(l.Ctx, tenantA, func(r store.Reader) (err error) {
		rec, err = r.OperationByKey(l.Ctx, store.OperationKey{Tenant: tenantA, Caller: actor, Kind: kind, Target: target.Ref(), ID: op})
		return err
	})
	return rec, err
}

func (l *linker) view(fn func(store.Reader) error) {
	l.T.Helper()
	if err := l.E.Store().View(l.Ctx, tenantA, fn); err != nil {
		l.T.Fatalf("view: %v", err)
	}
}

// actions lists the actions the policy was asked about, in order.
func actions(checks []policy.Authorization) []string {
	out := []string{}
	for _, c := range checks {
		out = append(out, c.Action)
	}
	return out
}

// unchanged fails unless id, its link records and the identity.linked log
// are as before.
func (l *linker) unchanged(before model.Identity, events int) {
	l.T.Helper()
	if got := l.stored(before.Ref); got.Revision != before.Revision || got.Contact != before.Contact {
		l.T.Fatalf("identity changed: %+v, was %+v", got, before)
	}
	if n := len(l.linkedEvents()); n != events {
		l.T.Fatalf("identity.linked events = %d, want %d", n, events)
	}
}
