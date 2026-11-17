package conformance

import (
	"fmt"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// cascade asks a cascade redaction of target at rev.
func (env *contactsEnv) cascade(target model.Ref, rev model.Revision) model.Redact {
	return model.Redact{Command: env.cmd(rev), Target: target, Cascade: true, Reason: "erasure"}
}

func (env *contactsEnv) redact(req model.Redact) (model.Acceptance, error) {
	return env.client.Privacy().Redact(env.ctx, req)
}

func (env *contactsEnv) message(ref model.MessageRef) model.Message {
	env.t.Helper()
	var m model.Message
	env.view(func(r store.Reader) (err error) { m, err = r.GetMessage(env.ctx, ref); return err })
	return m
}

// requireTombstones fails unless the history of id is want, every record
// redacted when redacted is set: facts gone, every other field kept, the
// reference included.
func (env *contactsEnv) requireTombstones(id model.IdentityRef, want []model.IdentityLink, redacted bool) {
	env.t.Helper()
	got := env.history(model.Filter{Identity: &id}, 10)
	if len(got) != len(want) {
		env.t.Fatalf("history of %s = %d records, want %d", id.ID, len(got), len(want))
	}
	for i, w := range want {
		w = w.Clone()
		if redacted {
			w.Evidence.Facts, w.Redacted = model.Extension{}, true
		}
		g := got[i]
		if g.Redacted != w.Redacted || g.Evidence.Facts.Name != w.Evidence.Facts.Name || len(g.Evidence.Facts.Data) != len(w.Evidence.Facts.Data) ||
			g.Evidence.Verifier != w.Evidence.Verifier || g.Evidence.Reference != w.Evidence.Reference || g.Evidence.Assurance != w.Evidence.Assurance ||
			g.ID != w.ID || g.Class != w.Class || g.From != w.From || g.To != w.To || g.Supersedes != w.Supersedes || g.Reason != w.Reason ||
			g.Assurance != w.Assurance || g.Operation != w.Operation || g.Actor != w.Actor || g.RevisionAfter != w.RevisionAfter ||
			g.PolicyVersion != w.PolicyVersion || g.ReasonCode != w.ReasonCode || !g.RecordedAt.Equal(w.RecordedAt) {
			env.t.Fatalf("history of %s record %d: redacted %v, facts %q (%d bytes), %s %s -> %s, ref %q; want redacted %v, facts %q (%d bytes), %s %s -> %s, ref %q",
				id.ID, i, g.Redacted, g.Evidence.Facts.Name, len(g.Evidence.Facts.Data), g.Class, g.From.ID, g.To.ID, g.Evidence.Reference,
				w.Redacted, w.Evidence.Facts.Name, len(w.Evidence.Facts.Data), w.Class, w.From.ID, w.To.ID, w.Evidence.Reference)
		}
	}
}

// contactsCascadeTombstones: identity and contact cascades tombstone the
// link history of the identity, or of each identity on the contact, in
// their submission unit: facts gone, every other field kept, other
// identities untouched. A tombstoned reference stays consumed, and a
// retried cascade replays without touching records written since.
func contactsCascadeTombstones(t *testing.T, env *contactsEnv) {
	a, _ := env.arrive(env.one, "alice")
	b, _ := env.arrive(env.two, "bob")
	c, _ := env.arrive(env.one, "carol")
	shared, elsewhere := env.person(), env.person()
	env.mustLink(&a, shared.Ref, "ref-a")
	env.mustLink(&b, shared.Ref, "ref-b")
	env.mustLink(&c, elsewhere.Ref, "ref-c")
	before := map[model.IdentityRef][]model.IdentityLink{}
	for _, id := range []model.Identity{a, b, c} {
		before[id.Ref] = env.history(model.Filter{Identity: &id.Ref}, 10)
	}

	req := env.cascade(c.Ref.Ref(), c.Revision)
	acc, err := env.redact(req)
	if err != nil {
		t.Fatalf("identity cascade: %v", err)
	}
	env.requireTombstones(c.Ref, before[c.Ref], true)
	env.requireTombstones(a.Ref, before[a.Ref], false)
	env.requireTombstones(b.Ref, before[b.Ref], false)

	// The tombstone keeps the reference consumed.
	c = env.identity(c.Ref)
	d, _ := env.arrive(env.two, "dora")
	_, err = env.link(env.linkReq(d, elsewhere.Ref, "ref-c"))
	serviceCode(t, err, model.Conflict)
	_, err = env.link(env.linkReq(c, env.person().Ref, "ref-c"))
	serviceCode(t, err, model.Conflict)

	// A record written after the cascade survives its retry.
	env.mustLink(&c, env.person().Ref, "ref-c2")
	again, err := env.redact(req)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("retried cascade = %+v, %v; want a replay", again, err)
	}
	recs := env.history(model.Filter{Identity: &c.Ref}, 10)
	if len(recs) != 2 || !recs[0].Redacted || recs[1].Redacted || len(recs[1].Evidence.Facts.Data) == 0 {
		t.Fatalf("history after the retry = %+v, want the new record intact", recs)
	}
	if op := env.run(acc.Operation.Ref, settled); op.State != model.Committed {
		t.Fatalf("identity cascade settled %+v", op)
	}

	contact, err := env.contact(shared.Ref)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := env.redact(env.cascade(shared.Ref.Ref(), contact.Revision))
	if err != nil {
		t.Fatalf("contact cascade: %v", err)
	}
	env.requireTombstones(a.Ref, before[a.Ref], true)
	env.requireTombstones(b.Ref, before[b.Ref], true)
	if op := env.run(ca.Operation.Ref, settled); op.State != model.Committed {
		t.Fatalf("contact cascade settled %+v", op)
	}
}

// contactsMessageRedactionLeavesLinks: redacting one message, or every
// message of a conversation, leaves link history whole.
func contactsMessageRedactionLeavesLinks(t *testing.T, env *contactsEnv) {
	a, ma := env.arrive(env.one, "alice")
	_, mb := env.arrive(env.one, "alice")
	env.mustLink(&a, env.person().Ref, "ref-a")
	before := env.history(model.Filter{Identity: &a.Ref}, 10)
	ref := ma.Ref
	if _, err := env.redact(model.Redact{Command: env.cmd(ma.Revision), Target: ref.Ref(), Message: &ref, Reason: "erasure"}); err != nil {
		t.Fatalf("message redaction: %v", err)
	}
	conv := env.conversation(mb.Ref.Conversation)
	acc, err := env.redact(env.cascade(conv.Ref.Ref(), conv.Revision))
	if err != nil {
		t.Fatalf("conversation cascade: %v", err)
	}
	if op := env.run(acc.Operation.Ref, settled); op.State != model.Committed {
		t.Fatalf("conversation cascade settled %+v", op)
	}
	if !env.message(mb.Ref).Redacted {
		t.Fatal("conversation cascade left a message")
	}
	env.requireTombstones(a.Ref, before, false)
}

// contactsPurgedContactGone: contacts left by a real Link and a real
// Unlink are purged past the horizon; relinking to either is then
// model.NotFound, and history keeps naming them.
func contactsPurgedContactGone(t *testing.T, env *contactsEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	p, _ := env.arrive(env.one, "left")
	left := p.Contact
	shared := env.person()
	env.mustLink(&p, shared.Ref, "")
	u, _ := env.arrive(env.two, "unlinked")
	prov := u.Contact
	if _, err := env.unlink(env.unlinkReq(u, "customer_request", model.Evidence{})); err != nil {
		t.Fatal(err)
	}
	u = env.identity(u.Ref)
	env.pastHorizon()
	if rep := env.purge(); rep.Contacts != 2 {
		t.Fatalf("purge report %+v, want the two contacts left", rep)
	}
	for _, ref := range []model.ContactRef{left, prov} {
		if _, err := env.contact(ref); !model.IsCode(err, model.NotFound) {
			t.Fatalf("left contact %s: %v, want purged", ref.ID, err)
		}
	}
	for _, ref := range []model.ContactRef{shared.Ref, u.Contact} {
		if _, err := env.contact(ref); err != nil {
			t.Fatalf("held contact %s: %v", ref.ID, err)
		}
	}
	_, err := env.link(env.linkReq(p, left, ""))
	serviceCode(t, err, model.NotFound)
	_, err = env.link(env.linkReq(u, prov, ""))
	serviceCode(t, err, model.NotFound)
	if h := env.history(model.Filter{Identity: &p.Ref}, 10); len(h) != 1 || h[0].From != left {
		t.Fatalf("history after the purge = %+v", h)
	}
	if rep := env.purge(); rep.Contacts != 0 {
		t.Fatalf("second purge %+v", rep)
	}
}

// contactsOrphanPurgeBatches purges more orphaned contacts than one purge
// unit holds.
func contactsOrphanPurgeBatches(t *testing.T, env *contactsEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	const orphans = 520
	target := env.person()
	for i := 0; i < orphans; i += 100 {
		env.put(func(tx store.Tx) error {
			for j := i; j < min(i+100, orphans); j++ {
				if err := env.seedOrphan(tx, fmt.Sprint("orphan-", j), target.Ref); err != nil {
					return err
				}
			}
			return nil
		})
	}
	env.pastHorizon()
	if rep := env.purge(); rep.Contacts != orphans {
		t.Fatalf("purge report %+v, want %d contacts", rep, orphans)
	}
	if left := env.allContacts(); len(left) != 1 || left[0].Ref != target.Ref {
		t.Fatalf("contacts after the purge = %d, want the target only", len(left))
	}
	if rep := env.purge(); rep.Contacts != 0 {
		t.Fatalf("second purge %+v", rep)
	}
}

// seedOrphan claims an identity on a new contact and links it to target,
// leaving the new contact without identities.
func (env *contactsEnv) seedOrphan(tx store.Tx, source string, target model.ContactRef) error {
	ct := model.Contact{Ref: model.ContactRef{Tenant: serviceTenant, ID: env.id()}, Kind: model.ContactKindPerson, Revision: "c1"}
	key := model.IdentityKey{Inbox: env.one, Namespace: chatNS, SourceID: source}
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: env.one, ID: env.id()}, Contact: ct.Ref, Namespace: chatNS, SourceID: source,
		Kind: model.ContactKindPerson, LinkAssurance: model.LinkAssuranceNone, Revision: "i1",
	}
	if _, _, err := tx.ClaimIdentity(env.ctx, key, id); err != nil {
		return err
	}
	if err := tx.PutContact(env.ctx, ct, ""); err != nil {
		return err
	}
	return tx.LinkIdentity(env.ctx, store.LinkRecord{
		ID: env.id(), Tenant: serviceTenant, Identity: id.Ref, IdentityKey: key,
		Class: model.LinkClassLink, From: ct.Ref, To: target,
		Evidence: model.Evidence{
			Verifier: attestKind, Assurance: model.LinkAssuranceAsserted,
			Facts: model.Extension{Name: model.SchemaEvidenceAttestation, Version: 1, Data: []byte(`{}`)},
		},
		Assurance: model.LinkAssuranceAsserted, Actor: contactsActor,
		Operation:      model.OperationRef{Tenant: serviceTenant, ID: env.id()},
		RevisionBefore: id.Revision, RevisionAfter: "i2", RecordedAt: env.clock.Now(),
	})
}
