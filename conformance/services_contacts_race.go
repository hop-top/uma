package conformance

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// barrier holds parties until n arrived, so racing calls all pass their
// checks before any commits. A party never waits past a timeout: a call
// that failed before reaching it cannot hang the others.
type barrier struct {
	n     int
	mu    sync.Mutex
	count int
	all   chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, all: make(chan struct{})} }

func (b *barrier) wait() {
	b.mu.Lock()
	b.count++
	if b.count == b.n {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
	case <-time.After(5 * time.Second):
	}
}

// holdAt makes every decision of action on id wait at a barrier of n.
func (env *contactsEnv) holdAt(action string, id model.IdentityRef, n int) {
	b := newBarrier(n)
	env.pol.hook(func(a policy.Authorization) {
		if a.Action == action && a.Resource == id.Ref() {
			b.wait()
		}
	})
}

// contactsConcurrentLinks races two Links of one provisional identity to
// different contacts, both past their checks: one commits, the other is
// model.Conflict to reconcile, with one record and one event. Each Link
// is resent while model.Transient (see settle).
func contactsConcurrentLinks(t *testing.T, env *contactsEnv) {
	for round := range 3 {
		id, _ := env.arrive(env.one, fmt.Sprint("alice-", round))
		x, y := env.person(), env.person()
		events := len(env.linkedEvents())
		env.holdAt(model.ActionIdentityLink, id.Ref, 2)
		reqs := []model.LinkIdentity{env.linkReq(id, x.Ref, ""), env.linkReq(id, y.Ref, "")}
		errs := make([]error, len(reqs))
		var wg sync.WaitGroup
		for i, req := range reqs {
			wg.Go(func() { _, errs[i] = env.linkSettled(req, time.Now().Add(settleWithin)) })
		}
		wg.Wait()
		env.pol.hook(nil)
		won := -1
		for i, err := range errs {
			switch {
			case err == nil && won < 0:
				won = i
			case err == nil:
				t.Fatalf("round %d: both links committed", round)
			default:
				requireReconcile(t, err)
			}
		}
		if won < 0 {
			t.Fatalf("round %d: no link committed: %v", round, errs)
		}
		if recs := env.records(id.Ref); len(recs) != 1 || recs[0].To != reqs[won].Contact {
			t.Fatalf("round %d: records %+v, want the winner's only", round, recs)
		}
		if got := env.identity(id.Ref); got.Contact != reqs[won].Contact {
			t.Fatalf("round %d: identity on %v, want the winner's target", round, got.Contact)
		}
		if n := len(env.linkedEvents()); n != events+1 {
			t.Fatalf("round %d: identity.linked events = %d, want %d", round, n, events+1)
		}
	}
}

// contactsConcurrentSameOperation sends one Link, and one Unlink, twice at
// once under the same operation ID: both succeed with one operation, one
// of them a replay of the other, and the change happens once. Each call is
// resent while model.Transient (see settle).
func contactsConcurrentSameOperation(t *testing.T, env *contactsEnv) {
	id, _ := env.arrive(env.one, "alice")
	target := env.person()
	race := func(action string, call func() (model.Acceptance, error)) []model.Acceptance {
		env.holdAt(action, id.Ref, 2)
		defer env.pol.hook(nil)
		accs := make([]model.Acceptance, 2)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range accs {
			wg.Go(func() { accs[i], errs[i] = settle(time.Now().Add(settleWithin), call) })
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("%s call %d: %v", action, i, err)
			}
		}
		if accs[0].Operation.Ref != accs[1].Operation.Ref || accs[0].Replayed == accs[1].Replayed {
			t.Fatalf("%s acceptances %+v and %+v, want one operation, one replayed", action, accs[0], accs[1])
		}
		return accs
	}
	link := env.linkReq(id, target.Ref, "same-op")
	accs := race(model.ActionIdentityLink, func() (model.Acceptance, error) { return env.link(link) })
	if again, err := env.link(link); err != nil || !again.Replayed || again.Operation.Ref != accs[0].Operation.Ref {
		t.Fatalf("later replay = %+v, %v", again, err)
	}
	id = env.identity(id.Ref)
	contacts := len(env.allContacts())
	un := env.unlinkReq(id, "customer_request", model.Evidence{})
	race(model.ActionIdentityUnlink, func() (model.Acceptance, error) { return env.unlink(un) })
	if recs := env.records(id.Ref); len(recs) != 2 || recs[1].Class != model.LinkClassUnlink {
		t.Fatalf("records %+v, want one link and one unlink", recs)
	}
	if n := len(env.allContacts()); n != contacts+1 {
		t.Fatalf("contacts = %d, want %d: one fresh contact", n, contacts+1)
	}
	if n := len(env.linkedEvents()); n != 2 {
		t.Fatalf("identity.linked events = %d, want 2", n)
	}
}

// contactsConcurrentClaimAndLink races the first messages of a new sender
// with a Link of the identity they claim: one identity, every message in
// one conversation, the Link committed (one link record) or model.Conflict
// (none), and no contact left without an identity that no link moved off.
// The Link is resent while model.Transient (see settle): its unit can
// lose every rerun the engine gives it to the burst of ingress units.
func contactsConcurrentClaimAndLink(t *testing.T, env *contactsEnv) {
	target := env.person()
	for round := range 3 {
		sender := fmt.Sprint("racer-", round)
		const n = 6
		start := make(chan struct{})
		accepts := make(chan error, n)
		var linkErr error
		var wg sync.WaitGroup
		for range n {
			e := env.envelope(env.one, sender)
			wg.Go(func() { <-start; accepts <- env.accept(e) })
		}
		wg.Go(func() {
			<-start
			deadline := time.Now().Add(10 * time.Second)
			for {
				var id model.Identity
				err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) (err error) {
					id, err = r.Identity(env.ctx, model.IdentityKey{Inbox: env.one, Namespace: chatNS, SourceID: sender})
					return err
				})
				switch {
				case err == nil:
					_, linkErr = env.linkSettled(env.linkReq(id, target.Ref, ""), deadline)
					return
				case !model.IsCode(err, model.NotFound):
					linkErr = err
					return
				case time.Now().After(deadline):
					linkErr = errors.New("identity never claimed")
					return
				}
				time.Sleep(100 * time.Microsecond)
			}
		})
		close(start)
		wg.Wait()
		close(accepts)
		for err := range accepts {
			if err != nil {
				t.Fatalf("round %d: Accept: %v", round, err)
			}
		}
		if linkErr != nil && !model.IsCode(linkErr, model.Conflict) {
			t.Fatalf("round %d: Link = %v, want committed or conflict", round, linkErr)
		}
		var mine []model.Identity
		var convs = map[model.ConversationRef]int{}
		env.view(func(r store.Reader) error {
			ids, err := r.Identities(env.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{env.one}}, Page: model.PageRequest{Limit: 100}})
			for _, id := range ids.Items {
				if id.SourceID == sender {
					mine = append(mine, id)
				}
			}
			if err != nil || len(mine) != 1 {
				return err
			}
			ms, err := r.Messages(env.ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{env.one}}, Page: model.PageRequest{Limit: 100}})
			for _, m := range ms.Items {
				if m.Author.Identity == mine[0].Ref {
					convs[m.Ref.Conversation]++
				}
			}
			return err
		})
		if len(mine) != 1 {
			t.Fatalf("round %d: identities for one key = %d, want 1", round, len(mine))
		}
		if len(convs) != 1 {
			t.Fatalf("round %d: messages in %d conversations, want 1", round, len(convs))
		}
		for _, c := range convs {
			if c != n {
				t.Fatalf("round %d: %d messages, want %d", round, c, n)
			}
		}
		if linkErr == nil && mine[0].Contact != target.Ref {
			t.Fatalf("round %d: committed link left the identity on %v", round, mine[0].Contact)
		}
		want := 0
		if linkErr == nil {
			want = 1
		}
		if recs := env.records(mine[0].Ref); len(recs) != want {
			t.Fatalf("round %d: link records %+v, want %d", round, recs, want)
		}
	}
	if orphans := env.orphans(target.Ref); len(orphans) != 0 {
		t.Fatalf("contacts without identity or link history: %v", orphans)
	}
}

// settleWithin bounds how long a racing call is resent while transient.
const settleWithin = 10 * time.Second

// settle sends call until its error is not model.Transient with
// model.RetrySameOperation advice, or deadline passes. A unit that loses
// every rerun the engine gives it to concurrent units commits nothing and
// reports that code, and a host retries it by resubmitting the same call,
// a command under the same operation: a racing call settles as committed,
// replayed or refused, and a transient one is neither. Resends wait a full-jitter backoff, as a host
// would, so they do not keep the contention that lost them going.
func settle[T any](deadline time.Time, call func() (T, error)) (T, error) {
	for n := 0; ; n++ {
		v, err := call()
		var me *model.Error
		if !errors.As(err, &me) || me.Code != model.Transient || me.Retry != model.RetrySameOperation || time.Now().After(deadline) {
			return v, err
		}
		time.Sleep(rand.N(min(time.Millisecond<<min(n, 7), 100*time.Millisecond))) // #nosec G404 -- jitter, not a secret
	}
}

// linkSettled sends req through settle.
func (env *contactsEnv) linkSettled(req model.LinkIdentity, deadline time.Time) (model.Acceptance, error) {
	return settle(deadline, func() (model.Acceptance, error) { return env.link(req) })
}

// orphans returns every contact, but those in except, that holds no
// identity and that no link moved an identity off: a contact a claim
// created and then abandoned.
func (env *contactsEnv) orphans(except ...model.ContactRef) []model.ContactRef {
	env.t.Helper()
	var out []model.ContactRef
	for _, c := range env.allContacts() {
		ref := c.Ref
		if containsRef(except, ref) {
			continue
		}
		env.view(func(r store.Reader) error {
			ids, err := r.Identities(env.ctx, model.ListRequest{Filter: model.Filter{Contact: &ref}, Page: model.PageRequest{Limit: 1}})
			if err != nil || len(ids.Items) > 0 {
				return err
			}
			links, err := r.IdentityLinks(env.ctx, store.LinkFilter{Contact: &ref}, model.PageRequest{Limit: 100})
			for _, l := range links.Items {
				if l.From == ref {
					return err
				}
			}
			out = append(out, ref)
			return err
		})
	}
	return out
}

func containsRef(refs []model.ContactRef, ref model.ContactRef) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

// contactsAnchorRelinkedBeforeCommit moves the anchor of host_session
// evidence off the target after verification: the link commit refuses
// with model.Conflict and writes nothing. With the anchor back, the link
// commits and records it. Raced at once, the link commits only with a
// record naming the anchor, or is model.Conflict; both racing Links are
// resent while model.Transient (see settle).
func contactsAnchorRelinkedBeforeCommit(t *testing.T, env *contactsEnv) {
	anchor, _ := env.arrive(env.two, "anchor")
	target := anchor.Contact
	id, _ := env.arrive(env.one, "alice")
	away := env.person()
	env.session.Script(sessionAnswer(&anchor.Ref, model.LinkAssuranceHostAuthenticated))
	sessionReq := func(id model.Identity) model.LinkIdentity {
		req := env.linkReq(id, target, "")
		req.Evidence.Verifier = model.EvidenceKindHostSession
		return req
	}
	env.pol.hook(func(a policy.Authorization) {
		if a.Action != model.ActionIdentityLink || a.Resource != id.Ref.Ref() {
			return
		}
		env.pol.hook(nil)
		moved := env.identity(anchor.Ref)
		env.mustLink(&moved, away.Ref, "")
	})
	req := sessionReq(id)
	_, err := env.link(req)
	serviceCode(t, err, model.Conflict)
	env.pol.hook(nil)
	env.unchanged(id, 1)
	if env.claimed(model.ActionIdentityLink, id.Ref.Ref(), req.Operation) {
		t.Fatal("refused anchored link claimed its operation")
	}
	anchor = env.identity(anchor.Ref)
	env.mustLink(&anchor, target, "")
	if _, err := env.link(sessionReq(id)); err != nil {
		t.Fatalf("anchored link with the anchor back: %v", err)
	}
	if recs := env.records(id.Ref); len(recs) != 1 || recs[0].Anchor == nil || *recs[0].Anchor != anchor.Ref {
		t.Fatalf("anchored record %+v", recs)
	}

	for round := range 3 {
		anchor = env.identity(anchor.Ref)
		if anchor.Contact != target {
			env.mustLink(&anchor, target, "")
		}
		racer, _ := env.arrive(env.one, fmt.Sprint("racer-", round))
		start := make(chan struct{})
		var linkErr, moveErr error
		var wg sync.WaitGroup
		link, move := sessionReq(racer), env.linkReq(anchor, away.Ref, "")
		deadline := time.Now().Add(settleWithin)
		wg.Go(func() { <-start; _, linkErr = env.linkSettled(link, deadline) })
		wg.Go(func() { <-start; _, moveErr = env.linkSettled(move, deadline) })
		close(start)
		wg.Wait()
		if moveErr != nil {
			t.Fatalf("round %d: anchor move: %v", round, moveErr)
		}
		recs := env.records(racer.Ref)
		switch {
		case linkErr == nil && len(recs) == 1 && recs[0].Anchor != nil && *recs[0].Anchor == anchor.Ref:
		case model.IsCode(linkErr, model.Conflict) || model.IsCode(linkErr, model.Validation):
			// Verified after the move (validation) or caught at commit.
			if len(recs) != 0 {
				t.Fatalf("round %d: refused link wrote %+v", round, recs)
			}
		default:
			t.Fatalf("round %d: Link = %v with records %+v", round, linkErr, recs)
		}
	}
}

// contactsLinkRacesOrphanPurge: a retention purge removing the target
// between a Link's checks and its commit leaves the Link model.NotFound
// with nothing written. Raced at once, either the link commits and the
// contact is kept, or the purge removes it and the link is not found. The
// racing Link and Purge are resent while model.Transient (see settle).
func contactsLinkRacesOrphanPurge(t *testing.T, env *contactsEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	shared := env.person()
	p, _ := env.arrive(env.one, "left")
	orphan := p.Contact
	env.mustLink(&p, shared.Ref, "")
	env.pastHorizon()
	q, _ := env.arrive(env.one, "late")
	env.pol.hook(func(a policy.Authorization) {
		if a.Action != model.ActionIdentityLink || a.Resource != q.Ref.Ref() {
			return
		}
		env.pol.hook(nil)
		if rep := env.purge(); rep.Contacts != 1 {
			t.Errorf("purge report %+v, want the orphan", rep)
		}
	})
	req := env.linkReq(q, orphan, "")
	_, err := env.link(req)
	serviceCode(t, err, model.NotFound)
	env.pol.hook(nil)
	if got := env.identity(q.Ref); got.Contact != q.Contact || got.Revision != q.Revision || len(env.records(q.Ref)) != 0 {
		t.Fatalf("identity after the refused link %+v", got)
	}
	if env.claimed(model.ActionIdentityLink, q.Ref.Ref(), req.Operation) {
		t.Fatal("link to a purged contact claimed its operation")
	}
	if _, err := env.contact(orphan); !model.IsCode(err, model.NotFound) {
		t.Fatalf("purged contact: %v", err)
	}

	call := uma.RuntimeCall{Principal: model.Principal{Tenant: serviceTenant, Actor: contactsActor}, Call: model.Call{Correlation: "purge"}}
	for round := range 4 {
		p, _ := env.arrive(env.one, fmt.Sprint("left-", round))
		orphan := p.Contact
		env.mustLink(&p, shared.Ref, "")
		env.pastHorizon()
		q, _ := env.arrive(env.one, fmt.Sprint("late-", round))
		start := make(chan struct{})
		var linkErr, purgeErr error
		var wg sync.WaitGroup
		req := env.linkReq(q, orphan, "")
		deadline := time.Now().Add(settleWithin)
		wg.Go(func() { <-start; _, linkErr = env.linkSettled(req, deadline) })
		wg.Go(func() {
			<-start
			_, purgeErr = settle(deadline, func() (uma.PurgeReport, error) { return env.engine.Runtime().Purge(env.ctx, call) })
		})
		close(start)
		wg.Wait()
		if purgeErr != nil {
			t.Fatalf("round %d: Purge: %v", round, purgeErr)
		}
		_, gone := env.contact(orphan)
		now := env.identity(q.Ref)
		switch {
		case linkErr == nil:
			if gone != nil || now.Contact != orphan {
				t.Fatalf("round %d: link committed but contact %v, identity on %v", round, gone, now.Contact)
			}
		case model.IsCode(linkErr, model.NotFound):
			if !model.IsCode(gone, model.NotFound) || now.Contact != q.Contact {
				t.Fatalf("round %d: link not found but contact %v, identity on %v", round, gone, now.Contact)
			}
		default:
			t.Fatalf("round %d: Link = %v, want committed or not found", round, linkErr)
		}
		// Whichever won, no identity points at a missing contact.
		if dangling := env.dangling(); len(dangling) != 0 {
			t.Fatalf("round %d: identities on missing contacts: %v", round, dangling)
		}
	}
}

// dangling returns every identity whose contact does not exist.
func (env *contactsEnv) dangling() []model.IdentityRef {
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
				if _, err := r.GetContact(env.ctx, id.Contact); model.IsCode(err, model.NotFound) {
					out = append(out, id.Ref)
				} else if err != nil {
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
