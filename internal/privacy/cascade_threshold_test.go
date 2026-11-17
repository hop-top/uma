package privacy_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// denySet allows every decision but privacy.redact on the resources it
// holds, and privacy.redact_cascade when cascade is set.
type denySet struct {
	refs    map[model.Ref]bool
	cascade bool
}

func (d *denySet) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	switch a.Action {
	case model.ActionPrivacyRedact:
		return policy.Decision{Allow: !d.refs[a.Resource]}, nil
	case model.ActionPrivacyRedactCascade:
		return policy.Decision{Allow: !d.cascade}, nil
	}
	return policy.Decision{Allow: true}, nil
}

// thresholds answers per inbox, else per tenant, else
// policy.CascadeNeverRefuse, and records every question.
type thresholds struct {
	inbox  map[model.InboxRef]int
	tenant map[model.TenantID]int
	err    error
	asked  []model.InboxRef
	as     []model.Principal
}

func (l *thresholds) ContactCascadeThreshold(_ context.Context, p model.Principal, in model.InboxRef) (int, error) {
	l.asked, l.as = append(l.asked, in), append(l.as, p)
	if l.err != nil {
		return 0, l.err
	}
	if n, ok := l.inbox[in]; ok {
		return n, nil
	}
	if n, ok := l.tenant[p.Tenant]; ok {
		return n, nil
	}
	return policy.CascadeNeverRefuse, nil
}

// spread holds contact shared, with alice and carol in inbox one and bob
// and dora in inbox two, and contact solo, with erin and fred in inbox one
// only. Each identity authored one message and has one link record with
// evidence facts.
type spread struct {
	*linkedSeed
	one, two      model.InboxRef
	ids           map[string]model.Identity
	msgs          map[string]model.Message
	shared2, solo model.Contact
	deny          *denySet
}

var (
	sharedNames = []string{"alice", "carol", "bob", "dora"}
	soloNames   = []string{"erin", "fred"}
)

// seedSpread composes the engine with limits as its CascadePolicy; nil
// leaves it unset.
func seedSpread(t *testing.T, limits policy.CascadePolicy) *spread {
	t.Helper()
	deny := &denySet{refs: map[model.Ref]bool{}}
	h := enginetest.NewHarness(t, enginetest.ChatType, func(c *engine.Config) {
		c.Blobs, c.Authorizer, c.CascadePolicy = &portstest.MemoryBlobs{}, deny, limits
	})
	s := &spread{
		linkedSeed: &linkedSeed{seed: &seed{h: h}, links: map[model.IdentityRef]model.IdentityLink{}},
		ids:        map[string]model.Identity{}, msgs: map[string]model.Message{}, deny: deny,
	}
	s.one = h.Configure(enginetest.PerContact(false), enginetest.Dest).Ref
	s.two = h.Configure(enginetest.PerContact(false), "sales").Ref
	s.shared2, s.solo = s.putContact(t), s.putContact(t)
	for _, name := range append(slices.Clone(sharedNames), soloNames...) {
		in, dest, to := s.one, enginetest.Dest, s.shared2.Ref
		if name == "bob" || name == "dora" {
			in, dest = s.two, "sales"
		}
		if slices.Contains(soloNames, name) {
			to = s.solo.Ref
		}
		h.MustAccept(h.Inbound("e-"+name, "pm-"+name, name, dest))
		m := h.MessageByProviderID(in, "pm-"+name)
		var id model.Identity
		h.View(func(r store.Reader) (err error) { id, err = r.GetIdentity(h.Ctx, m.Author.Identity); return err })
		s.link(t, &id, to)
		s.ids[name], s.msgs[name] = id, m
	}
	return s
}

func (s *spread) denyRedact(names ...string) {
	s.deny.refs = map[model.Ref]bool{}
	for _, n := range names {
		s.deny.refs[s.ids[n].Ref.Ref()] = true
	}
}

// cascadeOn asks a contact cascade of c at its current revision.
func (s *spread) cascadeOn(c model.Contact, op model.OperationID) (model.Acceptance, error) {
	h := s.h
	h.View(func(r store.Reader) (err error) { c, err = r.GetContact(h.Ctx, c.Ref); return err })
	return h.Client.Privacy().Redact(h.Ctx, cascade(c.Ref.Ref(), op, c.Revision))
}

// requireUntouched requires the identities' link records intact and their
// messages unredacted at their seeded revision.
func (s *spread) requireUntouched(t *testing.T, names ...string) {
	t.Helper()
	for _, n := range names {
		s.requireLink(t, s.ids[n].Ref, false)
		if m := s.message(t, s.msgs[n].Ref); m.Redacted || m.Revision != s.msgs[n].Revision {
			t.Fatalf("%s's message touched: redacted %v, revision %s", n, m.Redacted, m.Revision)
		}
	}
}

// requireRefused requires err to refuse a cascade of shared with nothing
// queued and none of its identities touched.
func (s *spread) requireRefused(t *testing.T, err error, msg string) {
	t.Helper()
	enginetest.RequireCode(t, err, model.Unauthorized, msg)
	if steps := drive(t, s.h.Ctx, s.h.RT, enginetest.TenantA); steps != 0 {
		t.Fatalf("refused cascade queued %d items", steps)
	}
	s.requireUntouched(t, sharedNames...)
}

// requirePartial requires the accepted cascade of the identities in names
// to report Partial exactly when skipped is not empty, to commit, and to
// have redacted every identity in names but the skipped ones, which stay
// untouched.
func (s *spread) requirePartial(t *testing.T, a model.Acceptance, err error, names []string, skipped ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("cascade: %v", err)
	}
	want := len(skipped) > 0
	if p := a.Operation.Progress; (p != nil && p.Partial) != want {
		t.Fatalf("accepted progress = %+v, want partial %v", p, want)
	}
	drive(t, s.h.Ctx, s.h.RT, enginetest.TenantA)
	op := operation(t, s.h.Client, s.h.Ctx, a.Operation.Ref)
	if op.State != model.Committed || done(op) != int64(len(names)-len(skipped)) || op.Progress.Partial != want {
		t.Fatalf("cascade settled %+v progress %+v, want committed, %d done, partial %v", op, op.Progress, len(names)-len(skipped), want)
	}
	for _, n := range names {
		if slices.Contains(skipped, n) {
			s.requireUntouched(t, n)
			continue
		}
		s.requireLink(t, s.ids[n].Ref, true)
		if !s.message(t, s.msgs[n].Ref).Redacted {
			t.Fatalf("%s's message survived the cascade", n)
		}
	}
}

// With no CascadePolicy every inbox never refuses: a contact cascade
// redacts the identities its actor may redact and skips the denied ones,
// reporting Partial; with none denied it is not partial.
func TestContactCascadeDefaultSkipsDenied(t *testing.T) {
	s := seedSpread(t, nil)
	s.denyRedact("bob", "dora", "carol")
	a, err := s.cascadeOn(s.shared2, "op-shared")
	s.requirePartial(t, a, err, sharedNames, "bob", "dora", "carol")
	s.requireUntouched(t, soloNames...)

	s.denyRedact()
	a, err = s.cascadeOn(s.solo, "op-solo")
	s.requirePartial(t, a, err, soloNames)
}

// CascadeNeverRefuse never refuses, even with every identity denied: the
// cascade commits having redacted nothing.
func TestContactCascadeNeverRefuse(t *testing.T) {
	s := seedSpread(t, &thresholds{tenant: map[model.TenantID]int{enginetest.TenantA: policy.CascadeNeverRefuse}})
	s.denyRedact(sharedNames...)
	a, err := s.cascadeOn(s.shared2, "op-shared")
	s.requirePartial(t, a, err, sharedNames, sharedNames...)
}

// CascadeDisabled refuses every contact cascade, none denied, before any
// identity is decided; an identity cascade is not a contact cascade.
func TestContactCascadeDisabled(t *testing.T) {
	limits := &thresholds{tenant: map[model.TenantID]int{enginetest.TenantA: policy.CascadeDisabled}}
	s := seedSpread(t, limits)
	_, err := s.cascadeOn(s.shared2, "op-shared")
	s.requireRefused(t, err, "disabled")

	carol := s.ids["carol"]
	if _, err := s.h.Client.Privacy().Redact(s.h.Ctx, cascade(carol.Ref.Ref(), "op-carol", carol.Revision)); err != nil {
		t.Fatalf("identity cascade: %v", err)
	}
	s.requireLink(t, carol.Ref, true)
}

// A threshold N refuses once N identities are denied and otherwise skips
// the denied ones.
func TestContactCascadeThreshold(t *testing.T) {
	cases := []struct {
		n      int
		denied []string
		refuse bool
	}{
		{1, nil, false},
		{1, []string{"dora"}, true},
		{2, []string{"dora"}, false},
		{2, []string{"dora", "alice"}, true},
		{3, []string{"dora", "alice"}, false},
	}
	for _, c := range cases {
		s := seedSpread(t, &thresholds{tenant: map[model.TenantID]int{enginetest.TenantA: c.n}})
		s.denyRedact(c.denied...)
		a, err := s.cascadeOn(s.shared2, "op-shared")
		if c.refuse {
			s.requireRefused(t, err, "threshold")
			continue
		}
		s.requirePartial(t, a, err, sharedNames, c.denied...)
	}
}

// Thresholds vary per inbox. A contact spanning inboxes takes the most
// restrictive of its inboxes' thresholds and counts denials across the
// contact; a contact in one inbox takes that inbox's alone.
func TestContactCascadeThresholdPerInbox(t *testing.T) {
	limits := &thresholds{inbox: map[model.InboxRef]int{}}
	s := seedSpread(t, limits)
	limits.inbox[s.one], limits.inbox[s.two] = policy.CascadeNeverRefuse, 1

	// Inbox two's 1 governs shared: alice, denied in inbox one, refuses.
	s.denyRedact("alice", "erin")
	_, err := s.cascadeOn(s.shared2, "op-shared")
	s.requireRefused(t, err, "threshold")
	if want := []model.InboxRef{s.one, s.two}; !slices.Equal(limits.asked, want) {
		t.Fatalf("asked %v, want each of shared's inboxes once: %v", limits.asked, want)
	}

	// solo sits in inbox one only, which never refuses.
	limits.asked = nil
	a, err := s.cascadeOn(s.solo, "op-solo")
	s.requirePartial(t, a, err, soloNames, "erin")
	if want := []model.InboxRef{s.one}; !slices.Equal(limits.asked, want) {
		t.Fatalf("asked %v, want %v", limits.asked, want)
	}

	// Inbox two disabled refuses shared even with nothing denied; 2 in
	// inbox one and 3 in inbox two make 2 for shared.
	s.denyRedact()
	limits.inbox[s.two] = policy.CascadeDisabled
	_, err = s.cascadeOn(s.shared2, "op-shared-2")
	s.requireRefused(t, err, "disabled")
	limits.inbox[s.one], limits.inbox[s.two] = 2, 3
	s.denyRedact("bob", "dora")
	_, err = s.cascadeOn(s.shared2, "op-shared-3")
	s.requireRefused(t, err, "threshold")
	s.denyRedact("bob")
	a, err = s.cascadeOn(s.shared2, "op-shared-4")
	s.requirePartial(t, a, err, sharedNames, "bob")
}

// Thresholds vary per tenant: the policy is asked for the cascade's own
// principal, and another tenant's setting never applies.
func TestContactCascadeThresholdPerTenant(t *testing.T) {
	limits := &thresholds{tenant: map[model.TenantID]int{enginetest.TenantB: policy.CascadeDisabled}}
	s := seedSpread(t, limits)
	s.denyRedact("bob")
	a, err := s.cascadeOn(s.solo, "op-solo")
	s.requirePartial(t, a, err, soloNames)
	for _, p := range limits.as {
		if p != enginetest.PrincipalA() {
			t.Fatalf("asked for %+v, want the cascade's principal", p)
		}
	}

	limits.tenant = map[model.TenantID]int{enginetest.TenantA: 1, enginetest.TenantB: policy.CascadeNeverRefuse}
	_, err = s.cascadeOn(s.shared2, "op-shared")
	s.requireRefused(t, err, "threshold")
}

// A retried partial cascade replays its stored operation, still Partial,
// and never redacts an identity skipped at submission, even once the actor
// may redact it.
func TestPartialContactCascadeReplays(t *testing.T) {
	s := seedSpread(t, &thresholds{tenant: map[model.TenantID]int{enginetest.TenantA: 2}})
	s.denyRedact("bob")
	a, err := s.cascadeOn(s.shared2, "op-shared")
	s.requirePartial(t, a, err, sharedNames, "bob")

	s.denyRedact()
	again, err := s.cascadeOn(s.shared2, "op-shared")
	if err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref ||
		again.Operation.Progress == nil || !again.Operation.Progress.Partial {
		t.Fatalf("retry = %+v, %v; want a partial replay", again, err)
	}
	if steps := drive(t, s.h.Ctx, s.h.RT, enginetest.TenantA); steps != 0 {
		t.Fatalf("replay queued %d items", steps)
	}
	s.requireUntouched(t, "bob")
}

// A caller refused the cascade on the contact learns nothing of its
// inboxes: the threshold policy is never asked. A failing policy, or one
// out of range, refuses with nothing written.
func TestContactCascadeThresholdPolicyFailures(t *testing.T) {
	limits := &thresholds{tenant: map[model.TenantID]int{enginetest.TenantA: policy.CascadeDisabled}}
	s := seedSpread(t, limits)
	s.deny.cascade = true
	_, err := s.cascadeOn(s.shared2, "op-1")
	s.requireRefused(t, err, "")
	if len(limits.asked) != 0 {
		t.Fatalf("threshold asked %v for a caller refused the cascade", limits.asked)
	}
	s.deny.cascade = false

	limits.err = errors.New("policy store down")
	_, err = s.cascadeOn(s.shared2, "op-2")
	enginetest.RequireCode(t, err, model.Unavailable, "")
	limits.err = nil
	limits.tenant[enginetest.TenantA] = -2
	_, err = s.cascadeOn(s.shared2, "op-3")
	enginetest.RequireCode(t, err, model.Permanent, "")
	if steps := drive(t, s.h.Ctx, s.h.RT, enginetest.TenantA); steps != 0 {
		t.Fatalf("failed cascades queued %d items", steps)
	}
	s.requireUntouched(t, sharedNames...)
}
