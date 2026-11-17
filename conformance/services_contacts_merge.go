package conformance

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// mergeReq merges from into into, reviewing the identities now on from.
func (env *contactsEnv) mergeReq(from, into model.Contact) model.MergeContacts {
	env.t.Helper()
	var held []model.IdentityRef
	for _, id := range env.identities(model.Filter{Contact: &from.Ref}, 10) {
		held = append(held, id.Ref)
	}
	f, i := env.mustContact(from.Ref), env.mustContact(into.Ref)
	return model.MergeContacts{
		Command: env.cmd(f.Revision), From: from.Ref, Into: into.Ref, ExpectedIntoRevision: i.Revision,
		Identities: held, Reason: "duplicate_contact",
	}
}

func (env *contactsEnv) mustContact(ref model.ContactRef) model.Contact {
	env.t.Helper()
	c, err := env.contact(ref)
	if err != nil {
		env.t.Fatalf("contact %s: %v", ref.ID, err)
	}
	return c
}

func (env *contactsEnv) merge(req model.MergeContacts) (model.Acceptance, error) {
	return env.client.Contacts().Merge(env.ctx, req)
}

func (env *contactsEnv) mergeSettled(req model.MergeContacts, deadline time.Time) (model.Acceptance, error) {
	return settle(deadline, func() (model.Acceptance, error) { return env.merge(req) })
}

// mergedEvents reads the contact.merged log.
func (env *contactsEnv) mergedEvents() []model.Event {
	env.t.Helper()
	var out []model.Event
	env.view(func(r store.Reader) error {
		after, err := r.EventBase(env.ctx)
		if err != nil {
			return err
		}
		for {
			p, err := r.Events(env.ctx, model.Filter{Kinds: []string{model.EventContactMerged}}, after, 100)
			if err != nil {
				return err
			}
			out = append(out, p.Items...)
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			after = p.Next
		}
	})
	return out
}

// onMerged returns every identity whose contact is merged away: a merge
// and a link must never leave one.
func (env *contactsEnv) onMerged() []model.IdentityRef {
	env.t.Helper()
	var out []model.IdentityRef
	env.view(func(r store.Reader) error {
		req := model.ListRequest{Page: model.PageRequest{Limit: 500}}
		for {
			p, err := r.Identities(env.ctx, req)
			if err != nil {
				return err
			}
			for _, id := range p.Items {
				c, err := r.GetContact(env.ctx, id.Contact)
				if err == nil && c.MergedInto != nil {
					out = append(out, id.Ref)
				} else if err != nil && !model.IsCode(err, model.NotFound) {
					return err
				}
			}
			if p.Complete || len(p.Items) == 0 {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	return out
}

// contactsMergeThroughLedger previews and merges one contact into another:
// every identity moves with a merge record chained onto its history, the
// contact merged reads as redirected, a replay returns the stored result,
// a Link back is refused, History shows both sides and, past the
// retention horizon, a purge removes the contact merged and its redirect.
func contactsMergeThroughLedger(t *testing.T, env *contactsEnv) {
	a, _ := env.arrive(env.one, "ada-chat")
	b, _ := env.arrive(env.two, "ada-mail")
	c, _ := env.arrive(env.one, "ada-phone")
	from, into := env.mustContact(a.Contact), env.person()
	env.mustLink(&b, from.Ref, "ref-b")
	env.mustLink(&c, into.Ref, "ref-c")
	if env.identity(a.Ref).Contact != from.Ref {
		t.Fatal("seed: a left its contact")
	}

	pv, err := env.client.Contacts().PreviewMerge(env.ctx, model.PreviewMerge{Call: model.Call{Correlation: "c"}, From: from.Ref, Into: into.Ref})
	if err != nil {
		t.Fatalf("PreviewMerge: %v", err)
	}
	if pv.FromRevision != from.Revision || pv.IntoRevision != into.Revision || len(pv.Identities) != 2 ||
		!slices.Contains(pv.Identities, a.Ref) || !slices.Contains(pv.Identities, b.Ref) {
		t.Fatalf("preview %+v", pv)
	}
	events := len(env.linkedEvents())
	req := model.MergeContacts{
		Command: env.cmd(pv.FromRevision), From: pv.From, Into: pv.Into, ExpectedIntoRevision: pv.IntoRevision,
		Identities: pv.Identities, Reason: "duplicate_contact",
	}
	acc, err := env.merge(req)
	if err != nil || acc.Replayed || acc.Operation.State != model.Committed || *acc.Operation.Result != into.Ref.Ref() {
		t.Fatalf("Merge = %+v, %v", acc, err)
	}
	for _, id := range []model.Identity{a, b} {
		now := env.identity(id.Ref)
		recs := env.records(id.Ref)
		rec := recs[len(recs)-1]
		if now.Contact != into.Ref || now.LinkAssurance != model.LinkAssuranceAsserted ||
			rec.Class != model.LinkClassMerge || rec.From != from.Ref || rec.To != into.Ref || rec.Reason != "duplicate_contact" ||
			rec.Actor != contactsActor || rec.Operation != acc.Operation.Ref || rec.RevisionAfter != now.Revision ||
			rec.RecordedAt.IsZero() || (len(recs) > 1 && rec.Supersedes != recs[len(recs)-2].ID) {
			t.Fatalf("identity %+v record %+v", now, rec)
		}
	}
	merged, survivor := env.mustContact(from.Ref), env.mustContact(into.Ref)
	if merged.MergedInto == nil || *merged.MergedInto != into.Ref || merged.Revision == from.Revision ||
		survivor.MergedInto != nil || survivor.Revision == into.Revision {
		t.Fatalf("contacts after merge: %+v and %+v", merged, survivor)
	}
	if n := len(env.linkedEvents()); n != events+2 {
		t.Fatalf("identity.linked events = %d, want %d", n, events+2)
	}
	evs := env.mergedEvents()
	if len(evs) != 1 || evs[0].Resource != into.Ref.Ref() || evs[0].Audience.Visibility != model.Internal ||
		evs[0].Payload.Name != model.SchemaContactMerged || evs[0].Revision != survivor.Revision {
		t.Fatalf("contact.merged events %+v", evs)
	}
	if h := env.history(model.Filter{Contact: &from.Ref}, 100); len(h) < 3 {
		t.Fatalf("history of the contact merged = %d records", len(h))
	}
	if got := env.identities(model.Filter{Contact: &into.Ref}, 10); len(got) != 3 {
		t.Fatalf("identities of the contact merged into = %d, want 3", len(got))
	}

	if again, err := env.merge(req); err != nil || !again.Replayed || again.Operation.Ref != acc.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if n := len(env.mergedEvents()); n != 1 {
		t.Fatalf("replay wrote: %d contact.merged events", n)
	}
	fresh := b
	fresh.Revision = env.identity(b.Ref).Revision
	_, err = env.link(env.linkReq(fresh, from.Ref, "back"))
	serviceCode(t, err, model.Conflict)
	next := env.mergeReq(from, env.person())
	_, err = env.merge(next)
	serviceCode(t, err, model.Conflict)
	if dangling := env.onMerged(); len(dangling) != 0 {
		t.Fatalf("identities on merged contacts: %v", dangling)
	}

	if env.fx.Advance == nil {
		return
	}
	env.pastHorizon()
	if rep := env.purge(); rep.Contacts == 0 {
		t.Fatalf("purge report %+v, want the merged contact", rep)
	}
	if _, err := env.contact(from.Ref); !isNotFound(err) {
		t.Fatalf("merged contact after the purge: %v", err)
	}
	if h := env.history(model.Filter{Identity: &b.Ref}, 100); len(h) < 2 {
		t.Fatalf("history after the purge = %+v", h)
	}
}

func isNotFound(err error) bool { return model.IsCode(err, model.NotFound) }

// hold makes every decision matching m wait at a barrier of n.
func (env *contactsEnv) hold(n int, m func(policy.Authorization) bool) {
	b := newBarrier(n)
	env.pol.hook(func(a policy.Authorization) {
		if m(a) {
			b.wait()
		}
	})
}

func isMerge(refs ...model.ContactRef) func(policy.Authorization) bool {
	return func(a policy.Authorization) bool {
		if a.Action != model.ActionContactMerge {
			return false
		}
		for _, r := range refs {
			if a.Resource == r.Ref() {
				return true
			}
		}
		return false
	}
}

// contactsConcurrentMerges races merges that touch one contact, both past
// their checks: the contact merged twice, a contact merged while it is
// merged into, and a merge against a Link of one of its identities or onto
// its target. Exactly one commits; the other is model.Conflict. No
// identity is left on a merged contact. Each call is resent while
// model.Transient (see settle).
func contactsConcurrentMerges(t *testing.T, env *contactsEnv) {
	pair := func(round int, name string) (model.Contact, model.Identity) {
		id, _ := env.arrive(env.one, fmt.Sprint(name, "-", round))
		return env.mustContact(id.Contact), id
	}
	run := func(calls ...func() error) []error {
		errs := make([]error, len(calls))
		var wg sync.WaitGroup
		for i, call := range calls {
			wg.Go(func() { errs[i] = call() })
		}
		wg.Wait()
		env.pol.hook(nil)
		return errs
	}
	exactlyOne := func(round int, label string, errs []error) int {
		won := -1
		for i, err := range errs {
			switch {
			case err == nil && won < 0:
				won = i
			case err == nil:
				t.Fatalf("round %d %s: both committed", round, label)
			default:
				serviceCode(t, err, model.Conflict)
			}
		}
		if won < 0 {
			t.Fatalf("round %d %s: none committed: %v", round, label, errs)
		}
		return won
	}
	for round := range 3 {
		deadline := time.Now().Add(settleWithin)
		events := len(env.mergedEvents())

		// One contact merged into two others.
		from, _ := pair(round, "twice")
		x, y := env.person(), env.person()
		env.hold(2, isMerge(from.Ref))
		reqs := []model.MergeContacts{env.mergeReq(from, x), env.mergeReq(from, y)}
		errs := run(
			func() (err error) { _, err = env.mergeSettled(reqs[0], deadline); return },
			func() (err error) { _, err = env.mergeSettled(reqs[1], deadline); return })
		won := exactlyOne(round, "twice", errs)
		if got := env.mustContact(from.Ref); got.MergedInto == nil || *got.MergedInto != reqs[won].Into {
			t.Fatalf("round %d twice: redirect %+v, want the winner's target", round, got.MergedInto)
		}
		lost := reqs[1-won].Into
		if got := env.mustContact(lost); got.MergedInto != nil || len(env.identities(model.Filter{Contact: &lost}, 10)) != 0 {
			t.Fatalf("round %d twice: the loser's target changed: %+v", round, got)
		}
		if n := len(env.mergedEvents()); n != events+1 {
			t.Fatalf("round %d twice: contact.merged events = %d, want %d", round, n, events+1)
		}

		// A contact merged while another is merged into it.
		mid, _ := pair(round, "chain-mid")
		top := env.person()
		leaf, _ := pair(round, "chain-leaf")
		env.hold(2, isMerge(leaf.Ref, mid.Ref))
		reqs = []model.MergeContacts{env.mergeReq(leaf, mid), env.mergeReq(mid, top)}
		errs = run(
			func() (err error) { _, err = env.mergeSettled(reqs[0], deadline); return },
			func() (err error) { _, err = env.mergeSettled(reqs[1], deadline); return })
		exactlyOne(round, "chain", errs)

		// A merge against a Link of one of its identities.
		src, id := pair(round, "link-away")
		dst, away := env.person(), env.person()
		env.hold(2, func(a policy.Authorization) bool {
			return isMerge(src.Ref)(a) || (a.Action == model.ActionIdentityLink || a.Action == model.ActionIdentityRelink) && a.Resource == id.Ref.Ref()
		})
		merge := env.mergeReq(src, dst)
		link := env.linkReq(id, away.Ref, "")
		errs = run(
			func() (err error) { _, err = env.mergeSettled(merge, deadline); return },
			func() (err error) { _, err = env.linkSettled(link, deadline); return })
		exactlyOne(round, "link away", errs)

		// A merge against a Link of an identity onto its target.
		src2, _ := pair(round, "link-onto")
		tgt := env.person()
		other, _ := env.arrive(env.two, fmt.Sprint("onto-", round))
		env.hold(2, func(a policy.Authorization) bool {
			return isMerge(tgt.Ref)(a) || a.Action == model.ActionIdentityLink && a.Resource == other.Ref.Ref() ||
				a.Action == model.ActionIdentityRelink && a.Resource == other.Ref.Ref()
		})
		merge = env.mergeReq(tgt, src2)
		link = env.linkReq(other, tgt.Ref, "")
		errs = run(
			func() (err error) { _, err = env.mergeSettled(merge, deadline); return },
			func() (err error) { _, err = env.linkSettled(link, deadline); return })
		exactlyOne(round, "link onto", errs)

		if dangling := env.onMerged(); len(dangling) != 0 {
			t.Fatalf("round %d: identities on merged contacts: %v", round, dangling)
		}
		if dangling := env.dangling(); len(dangling) != 0 {
			t.Fatalf("round %d: identities on missing contacts: %v", round, dangling)
		}
	}
}
