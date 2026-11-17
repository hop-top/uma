package conformance

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

const attestKind = model.EvidenceKindAttestation

// verified is the policy view of attestation evidence under ref.
func verified(ref string) *policy.VerifiedEvidence {
	return &policy.VerifiedEvidence{Verifier: attestKind, Reference: ref, Assurance: model.LinkAssuranceAsserted}
}

// requireDecisions fails unless got are exactly the actions in order, each
// on id at its revision toward dest (nil for none) with the same
// assurance and evidence.
func requireDecisions(t *testing.T, got []policy.Authorization, id model.Identity, dest *model.ContactRef,
	assurance model.LinkAssurance, ev *policy.VerifiedEvidence, actions ...string,
) {
	t.Helper()
	if len(got) != len(actions) {
		t.Fatalf("decisions = %+v, want %v", got, actions)
	}
	for i, a := range got {
		destOK := (a.Destination == nil) == (dest == nil) && (dest == nil || *a.Destination == dest.Ref())
		evOK := (a.Evidence == nil) == (ev == nil) && (ev == nil || *a.Evidence == *ev)
		if a.Action != actions[i] || a.Resource != id.Ref.Ref() || a.Revision != id.Revision || !destOK ||
			a.Assurance != assurance || !evOK || a.Principal.Actor != contactsActor {
			t.Fatalf("decision %d = %+v, want %s on %v at %s toward %v with %s %+v", i, a, actions[i], id.Ref, id.Revision, dest, assurance, ev)
		}
	}
}

// requireLinkedEvent fails unless ev is the identity.linked event of rec:
// internal, on the identity at its new revision, its payload the record
// summary only.
func requireLinkedEvent(t *testing.T, ev model.Event, rec model.IdentityLink) {
	t.Helper()
	if ev.Kind != model.EventIdentityLinked || ev.Resource != rec.Identity.Ref() || ev.Audience.Visibility != model.Internal ||
		ev.Revision != rec.RevisionAfter || ev.Payload.Name != model.SchemaIdentityLinked || ev.Payload.Version != model.PayloadVersion1 ||
		ev.Actor != rec.Actor || ev.Correlation != rec.Correlation {
		t.Fatalf("event %+v for record %+v", ev, rec)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(raw)); !slices.Equal(keys, []string{"assurance", "class", "from", "link", "to"}) {
		t.Fatalf("payload keys = %v, want the record summary only", keys)
	}
	var p model.IdentityLinkedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p != (model.IdentityLinkedPayload{Link: rec.ID, Class: rec.Class, From: rec.From, To: rec.To, Assurance: rec.Assurance}) {
		t.Fatalf("payload %+v for record %+v", p, rec)
	}
}

// contactsLinkThroughLedger links an identity through Contacts.Link: one
// class decision with the verified evidence, the identity moved with a new
// revision and link assurance, neither contact touched, one history record
// with the verifier's evidence and the deciding provenance, one
// identity.linked delivered to an identity.read subscriber, and a replay
// that verifies nothing and is authorized again before revealing the
// result.
func contactsLinkThroughLedger(t *testing.T, env *contactsEnv) {
	sub := env.subscribe("linked")
	a, _ := env.arrive(env.one, "alice")
	b, _ := env.arrive(env.two, "bob")
	from, _ := env.contact(a.Contact)
	to, _ := env.contact(b.Contact)
	env.pol.take()
	req := env.linkReq(a, to.Ref, "att-1")
	acc, err := env.link(req)
	if err != nil || acc.Operation.State != model.Committed || acc.Replayed || acc.Operation.Result == nil || *acc.Operation.Result != a.Ref.Ref() {
		t.Fatalf("Link = %+v, %v; want committed with the identity as result", acc, err)
	}
	requireDecisions(t, env.pol.take(), a, &to.Ref, model.LinkAssuranceAsserted, verified("att-1"), model.ActionIdentityLink)
	after := env.identity(a.Ref)
	if after.Contact != to.Ref || after.Revision == a.Revision || after.LinkAssurance != model.LinkAssuranceAsserted {
		t.Fatalf("identity after link %+v", after)
	}
	for _, c := range []model.Contact{from, to} {
		if got, err := env.contact(c.Ref); err != nil || !sameContact(got, c) {
			t.Fatalf("contact %s = %+v, %v; want it unchanged", c.Ref.ID, got, err)
		}
	}
	hist := env.history(model.Filter{Identity: &a.Ref}, 10)
	if len(hist) != 1 {
		t.Fatalf("history = %+v, want one record", hist)
	}
	rec := hist[0]
	key := model.IdentityKey{Inbox: env.one, Namespace: chatNS, SourceID: "alice"}
	if rec.Class != model.LinkClassLink || rec.From != from.Ref || rec.To != to.Ref || rec.Supersedes != "" ||
		rec.IdentityKey != key || rec.Operation != acc.Operation.Ref || rec.Actor != contactsActor || rec.Correlation != "corr-contacts" ||
		rec.PolicyVersion != "pv-1" || rec.ReasonCode != "allow_"+model.ActionIdentityLink ||
		rec.RevisionBefore != a.Revision || rec.RevisionAfter != after.Revision || rec.Assurance != model.LinkAssuranceAsserted ||
		!rec.RecordedAt.Equal(env.clock.Now()) || rec.Reason != "" || rec.Redacted || rec.Anchor != nil || rec.VerifierVersion != "" ||
		rec.Evidence.Verifier != attestKind || rec.Evidence.Reference != "att-1" || rec.Evidence.Assurance != model.LinkAssuranceAsserted ||
		rec.Evidence.Facts.Name != model.SchemaEvidenceAttestation || len(rec.Evidence.Facts.Data) == 0 {
		t.Fatalf("record %+v", rec)
	}
	evs := env.linkedEvents()
	if len(evs) != 1 {
		t.Fatalf("identity.linked events = %d, want 1", len(evs))
	}
	requireLinkedEvent(t, evs[0], rec)
	if got := env.poll(sub); !slices.ContainsFunc(got, func(ev model.Event) bool { return ev.ID == evs[0].ID }) {
		t.Fatalf("identity.read subscriber got %d events without identity.linked", len(got))
	}
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionIdentityRead })
	if got := env.poll(env.subscribe("denied")); slices.ContainsFunc(got, func(ev model.Event) bool { return ev.Kind == model.EventIdentityLinked }) {
		t.Fatal("identity.linked delivered to a subscriber denied identity.read")
	}

	// Replay: stored result, no verification, authorized again with the
	// stored action and evidence; without a fresh allow nothing is revealed.
	calls := env.attest.Calls()
	env.pol.set(nil)
	again, err := env.link(req)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref || *again.Operation.Result != a.Ref.Ref() {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	requireDecisions(t, env.pol.take(), a, &to.Ref, model.LinkAssuranceAsserted, verified("att-1"), model.ActionIdentityLink)
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionIdentityLink })
	denied, err := env.link(req)
	serviceCode(t, err, model.Unauthorized)
	if denied.Operation.Ref.ID != "" || denied.Operation.Result != nil {
		t.Fatalf("denied replay revealed %+v", denied)
	}
	env.pol.set(nil)
	changed := req
	changed.Evidence.Reference = "att-other"
	_, err = env.link(changed)
	serviceCode(t, err, model.Conflict)
	if env.attest.Calls() != calls || len(env.pol.take()) != 0 {
		t.Fatal("replay or conflicting replay verified or decided anew")
	}
	if n := len(env.records(a.Ref)); n != 1 || len(env.linkedEvents()) != 1 {
		t.Fatalf("replays wrote: records %d", n)
	}
}

// subscribe opens an event subscription from the beginning.
func (env *contactsEnv) subscribe(consumer string) model.SubscriptionRef {
	env.t.Helper()
	res, err := env.client.Events().Open(env.ctx, model.OpenSubscription{
		Command: env.cmd(""), Consumer: consumer, Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		env.t.Fatalf("Open(%s): %v", consumer, err)
	}
	return res.Subscription.Ref
}

// poll returns the next batch of sub.
func (env *contactsEnv) poll(sub model.SubscriptionRef) []model.Event {
	env.t.Helper()
	b, err := env.client.Events().Poll(env.ctx, model.Poll{Call: model.Call{Correlation: "c"}, Subscription: sub, Limit: 100})
	if err != nil {
		env.t.Fatalf("Poll: %v", err)
	}
	return b.Events
}

// contactsUnlinkThenMergeBack merges two identities onto one contact,
// unlinks one to a fresh contact and links it back: the merged-away
// contact stays addressable, the fresh contact has the identity's kind and
// no fields, and each record supersedes the previous one.
func contactsUnlinkThenMergeBack(t *testing.T, env *contactsEnv) {
	a, _ := env.arrive(env.one, "alice")
	b, msg := env.arrive(env.two, "bob")
	home, away := a.Contact, b.Contact
	env.mustLink(&b, home, "merge-b")
	if _, err := env.contact(away); err != nil {
		t.Fatalf("merged-away contact: %v, want it kept", err)
	}
	if got := env.identities(model.Filter{Contact: &away}, 10); len(got) != 0 {
		t.Fatalf("identities of the merged-away contact = %d", len(got))
	}
	if got := env.identities(model.Filter{Contact: &home}, 1); len(got) != 2 || got[0].Ref != a.Ref || got[1].Ref != b.Ref {
		t.Fatalf("identities of the merged contact = %+v", got)
	}
	acc, err := env.unlink(env.unlinkReq(b, "customer_request", model.Evidence{}))
	if err != nil || acc.Operation.State != model.Committed || *acc.Operation.Result != b.Ref.Ref() {
		t.Fatalf("Unlink = %+v, %v", acc, err)
	}
	b = env.identity(b.Ref)
	fresh, err := env.contact(b.Contact)
	if err != nil || b.Contact == home || b.Contact == away || fresh.Kind != model.ContactKindPerson || fresh.DisplayName != "" ||
		b.LinkAssurance != model.LinkAssuranceNone {
		t.Fatalf("after unlink: identity %+v, contact %+v, %v", b, fresh, err)
	}
	env.mustLink(&b, home, "merge-back")
	recs := env.history(model.Filter{Identity: &b.Ref}, 1)
	if len(recs) != 3 {
		t.Fatalf("history = %+v, want link, unlink, relink", recs)
	}
	for i, want := range []struct {
		class    model.LinkClass
		from, to model.ContactRef
	}{
		{model.LinkClassLink, away, home}, {model.LinkClassUnlink, home, fresh.Ref}, {model.LinkClassRelink, fresh.Ref, home},
	} {
		r := recs[i]
		sup := model.ID("")
		if i > 0 {
			sup = recs[i-1].ID
		}
		if r.Class != want.class || r.From != want.from || r.To != want.to || r.Supersedes != sup {
			t.Fatalf("record %d = %+v, want %s %s -> %s", i, r, want.class, want.from.ID, want.to.ID)
		}
	}
	if recs[1].Reason != "customer_request" || recs[1].Assurance != model.LinkAssuranceNone || !recs[1].Evidence.IsZero() {
		t.Fatalf("unlink record %+v", recs[1])
	}
	// One identity.linked per record, link, unlink and relink alike.
	var mine []model.Event
	for _, ev := range env.linkedEvents() {
		if ev.Resource == b.Ref.Ref() {
			mine = append(mine, ev)
		}
	}
	if len(mine) != len(recs) {
		t.Fatalf("identity.linked events of bob = %d, want one per record (%d)", len(mine), len(recs))
	}
	for i, ev := range mine {
		requireLinkedEvent(t, ev, recs[i])
	}
	if got := env.history(model.Filter{Contact: &fresh.Ref}, 10); len(got) != 2 || got[0].ID != recs[1].ID || got[1].ID != recs[2].ID {
		t.Fatalf("history of the fresh contact = %+v", got)
	}
	var m model.Message
	env.view(func(r store.Reader) (err error) { m, err = r.GetMessage(env.ctx, msg.Ref); return err })
	if m.Author != msg.Author || m.Revision != msg.Revision {
		t.Fatalf("message rewritten: %+v, was %+v", m.Author, msg.Author)
	}
}

// contactsUnlinkRules: a reason code is required; without evidence the
// unlink is decided with assurance none and no evidence; a policy that
// requires evidence denies that before any write and admits an attested
// one, which core verified and the record keeps.
func contactsUnlinkRules(t *testing.T, env *contactsEnv) {
	a, msg := env.arrive(env.one, "alice")
	shared := env.person()
	env.mustLink(&a, shared.Ref, "")
	env.pol.take()
	for _, reason := range []string{"", "Free text reason"} {
		_, err := env.unlink(env.unlinkReq(a, reason, model.Evidence{}))
		serviceCode(t, err, model.Validation)
	}
	if len(env.pol.take()) != 0 {
		t.Fatal("unlink without a reason code was decided")
	}
	env.unchanged(a, 1)

	contacts := len(env.allContacts())
	head := env.eventHead()
	env.pol.set(func(x policy.Authorization) bool { return x.Action == model.ActionIdentityUnlink && x.Evidence == nil })
	req := env.unlinkReq(a, "customer_request", model.Evidence{})
	_, err := env.unlink(req)
	serviceCode(t, err, model.Unauthorized)
	requireDecisions(t, env.pol.take(), a, nil, model.LinkAssuranceNone, nil, model.ActionIdentityUnlink)
	if env.claimed(model.ActionIdentityUnlink, a.Ref.Ref(), req.Operation) {
		t.Fatal("denied unlink claimed its operation")
	}
	if len(env.allContacts()) != contacts || env.eventHead() != head {
		t.Fatal("denied unlink wrote a contact or an event")
	}
	env.unchanged(a, 1)

	attested := env.unlinkReq(a, "customer_request", model.Evidence{Verifier: attestKind, Reference: "att-unlink"})
	if _, err := env.unlink(attested); err != nil {
		t.Fatalf("attested unlink: %v", err)
	}
	requireDecisions(t, env.pol.take(), a, nil, model.LinkAssuranceAsserted, verified("att-unlink"), model.ActionIdentityUnlink)
	recs := env.history(model.Filter{Identity: &a.Ref}, 10)
	rec := recs[len(recs)-1]
	if rec.Class != model.LinkClassUnlink || rec.From != shared.Ref || rec.Reason != "customer_request" || rec.Assurance != model.LinkAssuranceNone ||
		rec.Evidence.Verifier != attestKind || rec.Evidence.Reference != "att-unlink" || rec.Evidence.Assurance != model.LinkAssuranceAsserted ||
		len(rec.Evidence.Facts.Data) == 0 || rec.ReasonCode != "allow_"+model.ActionIdentityUnlink {
		t.Fatalf("attested unlink record %+v", rec)
	}

	// Without a requiring policy, no evidence commits with none.
	env.pol.set(nil)
	b, _ := env.arrive(env.two, "bob")
	calls := env.attest.Calls()
	if _, err := env.unlink(env.unlinkReq(b, "duplicate", model.Evidence{})); err != nil {
		t.Fatalf("unlink without evidence: %v", err)
	}
	requireDecisions(t, env.pol.take(), b, nil, model.LinkAssuranceNone, nil, model.ActionIdentityUnlink)
	if env.attest.Calls() != calls {
		t.Fatal("unlink without evidence asked a verifier")
	}
	if r := env.history(model.Filter{Identity: &b.Ref}, 10); len(r) != 1 || !r[0].Evidence.IsZero() || r[0].Assurance != model.LinkAssuranceNone {
		t.Fatalf("unlink record without evidence %+v", r)
	}
	var m model.Message
	env.view(func(r store.Reader) (err error) { m, err = r.GetMessage(env.ctx, msg.Ref); return err })
	if m.Author.Identity != a.Ref {
		t.Fatalf("message author %v, want %v kept", m.Author.Identity, a.Ref)
	}
}

// allContacts lists every stored contact.
func (env *contactsEnv) allContacts() []model.Contact {
	env.t.Helper()
	var out []model.Contact
	env.view(func(r store.Reader) error {
		req := model.ListRequest{Page: model.PageRequest{Limit: 500}}
		for {
			p, err := r.Contacts(env.ctx, req)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	return out
}

// sessionAnswer answers host_session evidence bound to the request with
// anchor, for link and relink, at level.
func sessionAnswer(anchor *model.IdentityRef, level model.LinkAssurance) func(model.Principal, model.LinkRequest) (model.Evidence, error) {
	return func(_ model.Principal, req model.LinkRequest) (model.Evidence, error) {
		b := Binding(req)
		if req.Intent != model.LinkClassUnlink {
			b.Anchor = anchor
		}
		facts, err := model.NewEvidenceFacts(model.HostSessionFacts{EvidenceBinding: b, SessionSubject: "session-1"})
		return model.Evidence{Verifier: model.EvidenceKindHostSession, Reference: req.Evidence.Reference, Facts: facts, Assurance: level}, err
	}
}

// contactsOverrideRequiresDistinctAction: a relink below the identity's
// current link assurance needs identity.link_override besides
// identity.relink, both asked with the same verified evidence; denying
// the override alone refuses the downgrade.
func contactsOverrideRequiresDistinctAction(t *testing.T, env *contactsEnv) {
	anchor, _ := env.arrive(env.two, "anchor")
	target := anchor.Contact
	id, _ := env.arrive(env.one, "alice")
	env.session.Script(sessionAnswer(&anchor.Ref, model.LinkAssuranceHostAuthenticated))
	req := env.linkReq(id, target, "sess-1")
	req.Evidence.Verifier = model.EvidenceKindHostSession
	if _, err := env.link(req); err != nil {
		t.Fatalf("host_session link: %v", err)
	}
	id = env.identity(id.Ref)
	rec := env.records(id.Ref)[0]
	if id.LinkAssurance != model.LinkAssuranceHostAuthenticated || rec.Anchor == nil || *rec.Anchor != anchor.Ref ||
		rec.VerifierVersion != sessionVersion || rec.Assurance != model.LinkAssuranceHostAuthenticated {
		t.Fatalf("anchored link: identity %+v, record %+v", id, rec)
	}
	other := env.person()
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionIdentityLinkOverride })
	_, err := env.link(env.linkReq(id, other.Ref, "down-1"))
	serviceCode(t, err, model.Unauthorized)
	requireDecisions(t, env.pol.take(), id, &other.Ref, model.LinkAssuranceAsserted, verified("down-1"),
		model.ActionIdentityRelink, model.ActionIdentityLinkOverride)
	env.unchanged(id, 1)
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionIdentityRelink })
	_, err = env.link(env.linkReq(id, other.Ref, "down-2"))
	serviceCode(t, err, model.Unauthorized)
	requireDecisions(t, env.pol.take(), id, &other.Ref, model.LinkAssuranceAsserted, verified("down-2"), model.ActionIdentityRelink)
	env.unchanged(id, 1)

	env.pol.set(nil)
	before := id
	env.mustLink(&id, other.Ref, "down-3")
	requireDecisions(t, env.pol.take(), before, &other.Ref, model.LinkAssuranceAsserted, verified("down-3"),
		model.ActionIdentityRelink, model.ActionIdentityLinkOverride)
	recs := env.records(id.Ref)
	if r := recs[len(recs)-1]; r.ReasonCode != "allow_"+model.ActionIdentityLinkOverride || r.Assurance != model.LinkAssuranceAsserted ||
		id.LinkAssurance != model.LinkAssuranceAsserted {
		t.Fatalf("downgrade record %+v, identity %+v", r, id)
	}
	before = id
	env.mustLink(&id, env.person().Ref, "level-1")
	if got := env.pol.take(); len(got) != 1 || got[0].Action != model.ActionIdentityRelink || got[0].Revision != before.Revision {
		t.Fatalf("equal-assurance relink decisions = %+v", got)
	}
}
