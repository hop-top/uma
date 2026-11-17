package conformance

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
)

// spreadContact links one identity per sender, each arriving with one
// message in its inbox, to a new contact.
type spreadContact struct {
	contact model.Contact
	ids     []model.Identity
	msgs    []model.Message
	links   [][]model.IdentityLink
}

// arrival is a sender arriving in an inbox.
type arrival struct {
	in     model.InboxRef
	sender string
}

func at(in model.InboxRef, sender string) arrival { return arrival{in, sender} }

func (env *contactsEnv) spread(arrivals ...arrival) *spreadContact {
	env.t.Helper()
	sc := &spreadContact{contact: env.person()}
	for _, a := range arrivals {
		id, m := env.arrive(a.in, a.sender)
		env.mustLink(&id, sc.contact.Ref, "ref-"+a.sender)
		sc.ids, sc.msgs = append(sc.ids, id), append(sc.msgs, m)
		sc.links = append(sc.links, env.history(model.Filter{Identity: &id.Ref}, 10))
	}
	c, err := env.contact(sc.contact.Ref)
	if err != nil {
		env.t.Fatal(err)
	}
	sc.contact = c
	return sc
}

// requireCascaded requires the identities at the indexes in redacted
// tombstoned with their messages redacted, and every other one untouched.
func (env *contactsEnv) requireCascaded(sc *spreadContact, redacted ...int) {
	env.t.Helper()
	for i, id := range sc.ids {
		done := slices.Contains(redacted, i)
		env.requireTombstones(id.Ref, sc.links[i], done)
		got := env.message(sc.msgs[i].Ref)
		if done != got.Redacted || (!done && got.Revision != sc.msgs[i].Revision) {
			env.t.Fatalf("message of %s: redacted %v at %s, want redacted %v", id.SourceID, got.Redacted, got.Revision, done)
		}
	}
}

// requireRefusedCascade requires err to refuse req with nothing claimed
// and nothing of sc touched.
func (env *contactsEnv) requireRefusedCascade(sc *spreadContact, req model.Redact, err error) {
	env.t.Helper()
	serviceCode(env.t, err, model.Unauthorized)
	if env.claimed(model.ActionPrivacyRedactCascade, sc.contact.Ref.Ref(), req.Operation) {
		env.t.Fatal("refused cascade claimed its operation")
	}
	env.requireCascaded(sc)
}

// requirePartialCascade requires acc accepted, Partial exactly when
// partial, and settled committed with the identities at redacted done.
func (env *contactsEnv) requirePartialCascade(sc *spreadContact, acc model.Acceptance, err error, partial bool, redacted ...int) {
	env.t.Helper()
	if err != nil {
		env.t.Fatalf("contact cascade: %v", err)
	}
	if p := acc.Operation.Progress; (p != nil && p.Partial) != partial {
		env.t.Fatalf("accepted progress %+v, want partial %v", p, partial)
	}
	op := env.run(acc.Operation.Ref, settled)
	if op.State != model.Committed || op.Progress == nil || op.Progress.Partial != partial || op.Progress.Done != int64(len(redacted)) {
		env.t.Fatalf("contact cascade settled %+v progress %+v, want committed, %d done, partial %v", op, op.Progress, len(redacted), partial)
	}
	env.requireCascaded(sc, redacted...)
}

// contactsContactCascadeSkipsDenied: with no CascadePolicy configured, a
// contact cascade whose actor may not redact an identity in another inbox
// skips it: accepted as Partial, the readable identity tombstoned and its
// message redacted, the denied one's history and message untouched. A
// retry once the identity is permitted replays, still Partial, and
// redacts nothing more.
func contactsContactCascadeSkipsDenied(t *testing.T, env *contactsEnv) {
	sc := env.spread(at(env.one, "alice"), at(env.two, "bob"))
	e, err := uma.New(uma.Config{
		Store: env.st, Authorizer: env.pol, Clock: env.clock, IDs: portstest.NewSequenceIDs(0xc0e),
	})
	if err != nil {
		t.Fatalf("uma.New without a cascade policy: %v", err)
	}
	defer func() { _ = e.Close(context.Background()) }()
	cl, err := e.For(model.Principal{Tenant: serviceTenant, Actor: contactsActor})
	if err != nil {
		t.Fatal(err)
	}
	env.pol.set(denyIn(env.two, model.ActionPrivacyRedact))
	req := env.cascade(sc.contact.Ref.Ref(), sc.contact.Revision)
	acc, err := cl.Privacy().Redact(env.ctx, req)
	env.requirePartialCascade(sc, acc, err, true, 0)

	env.pol.set(nil)
	again, err := cl.Privacy().Redact(env.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref ||
		again.Operation.Progress == nil || !again.Operation.Progress.Partial {
		t.Fatalf("retried cascade = %+v, %v; want a partial replay", again, err)
	}
	env.run(acc.Operation.Ref, settled)
	env.requireCascaded(sc, 0)

	b := env.identity(sc.ids[1].Ref)
	if _, err := env.redact(env.cascade(b.Ref.Ref(), b.Revision)); err != nil {
		t.Fatalf("identity cascade once permitted: %v", err)
	}
	env.requireTombstones(b.Ref, sc.links[1], true)
}

// contactsContactCascadeThreshold: 0 refuses a contact cascade with no
// identity denied; N refuses once N identities are denied and otherwise
// skips them; -1 never refuses. A refusal claims nothing and touches
// nothing.
func contactsContactCascadeThreshold(t *testing.T, env *contactsEnv) {
	sc := env.spread(at(env.one, "alice"), at(env.two, "bob"), at(env.two, "carol"))
	a, b, c := sc.ids[0], sc.ids[1], sc.ids[2]
	refused := []struct {
		n    int
		deny []model.Identity
	}{
		{policy.CascadeDisabled, nil},
		{1, []model.Identity{c}},
		{2, []model.Identity{b, c}},
		{2, []model.Identity{a, b}},
	}
	for _, r := range refused {
		env.limits.set(everywhere(r.n))
		env.pol.set(denyRedactOf(r.deny...))
		req := env.cascade(sc.contact.Ref.Ref(), sc.contact.Revision)
		_, err := env.redact(req)
		env.requireRefusedCascade(sc, req, err)
	}
	// A caller refused the cascade on the contact never reaches the
	// threshold; a threshold below -1 is model.Permanent.
	env.limits.set(everywhere(policy.CascadeDisabled))
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionPrivacyRedactCascade })
	req := env.cascade(sc.contact.Ref.Ref(), sc.contact.Revision)
	_, err := env.redact(req)
	env.requireRefusedCascade(sc, req, err)
	if q := env.limits.questions(); len(q) != 0 {
		t.Fatalf("threshold asked %+v for a caller refused the cascade", q)
	}
	env.pol.set(nil)
	env.limits.set(everywhere(-2))
	req = env.cascade(sc.contact.Ref.Ref(), sc.contact.Revision)
	_, err = env.redact(req)
	serviceCode(t, err, model.Permanent)
	env.requireCascaded(sc)

	env.limits.set(everywhere(2))
	env.pol.set(denyRedactOf(b))
	acc, err := env.redact(env.cascade(sc.contact.Ref.Ref(), sc.contact.Revision))
	env.requirePartialCascade(sc, acc, err, true, 0, 2)

	all := env.spread(at(env.one, "dora"), at(env.two, "erin"))
	env.limits.set(everywhere(policy.CascadeNeverRefuse))
	env.pol.set(denyRedactOf(all.ids...))
	acc, err = env.redact(env.cascade(all.contact.Ref.Ref(), all.contact.Revision))
	env.requirePartialCascade(all, acc, err, true)
}

// contactsContactCascadeThresholdScope: thresholds vary per inbox and per
// tenant. The policy is asked once per inbox of the contact, for the
// cascade's principal; a contact spanning inboxes takes the most
// restrictive and counts denials across the contact, a contact in one
// inbox takes that inbox's alone, and another tenant's setting never
// applies.
func contactsContactCascadeThresholdScope(t *testing.T, env *contactsEnv) {
	both := env.spread(at(env.one, "alice"), at(env.two, "bob"))
	solo := env.spread(at(env.one, "carol"), at(env.one, "dora"))
	perInbox := func(_ model.Principal, in model.InboxRef) int {
		if in == env.two {
			return 1
		}
		return policy.CascadeNeverRefuse
	}

	env.limits.set(perInbox)
	env.pol.set(denyRedactOf(both.ids[0], solo.ids[0]))
	req := env.cascade(both.contact.Ref.Ref(), both.contact.Revision)
	_, err := env.redact(req)
	env.requireRefusedCascade(both, req, err)
	want := []cascadeQuestion{{model.Principal{Tenant: serviceTenant, Actor: contactsActor}, env.one}, {model.Principal{Tenant: serviceTenant, Actor: contactsActor}, env.two}}
	if got := env.limits.questions(); !slices.Equal(got, want) {
		t.Fatalf("threshold questions %+v, want %+v", got, want)
	}

	env.limits.set(perInbox)
	acc, err := env.redact(env.cascade(solo.contact.Ref.Ref(), solo.contact.Revision))
	env.requirePartialCascade(solo, acc, err, true, 1)
	if got := env.limits.questions(); !slices.Equal(got, want[:1]) {
		t.Fatalf("threshold questions %+v, want %+v", got, want[:1])
	}

	// 2 in inbox one and 3 in inbox two make 2: two denials refuse.
	env.limits.set(func(_ model.Principal, in model.InboxRef) int {
		if in == env.two {
			return 3
		}
		return 2
	})
	env.pol.set(denyRedactOf(both.ids...))
	req = env.cascade(both.contact.Ref.Ref(), both.contact.Revision)
	_, err = env.redact(req)
	env.requireRefusedCascade(both, req, err)

	env.pol.set(denyRedactOf(both.ids[0], solo.ids[0]))
	perTenant := func(p model.Principal, _ model.InboxRef) int {
		if p.Tenant == serviceTenant {
			return policy.CascadeNeverRefuse
		}
		return policy.CascadeDisabled
	}
	env.limits.set(perTenant)
	acc, err = env.redact(env.cascade(both.contact.Ref.Ref(), both.contact.Revision))
	env.requirePartialCascade(both, acc, err, true, 1)
}
