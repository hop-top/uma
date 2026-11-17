package conformance

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Held-operation expiry cases. A proposal past its deadline becomes expired
// by a sweep that judges the deadline by the store's own clock, one
// transition per operation however many sweeps race, and a proposal still
// held a window later is marked carried over. cross is the restart: the
// next process sweeps a store reopened on what was committed.
var heldExpiryCases = []durableCase{
	{"HeldExpirySurvivesRestart", heldExpirySurvivesRestart},
	{"HeldExpiryRestartMidSweep", heldExpiryRestartMidSweep},
	{"HeldExpiryConcurrentSweepsOneTransition", heldExpiryConcurrentSweeps},
	{"HeldCarryOverSurvivesRestartAndExpiredIsRefused", heldCarryOver},
	{"HeldScopeSurvivesRestart", heldScopeSurvivesRestart},
}

const (
	heldTTL    = time.Hour
	heldWindow = 10 * time.Minute
)

// sweepEngine is one process: an engine over the env's current store,
// clocked by the store's clock, allowing every action.
func (e *env) sweepEngine(t *testing.T, ids ports.IDs) *engine.Engine {
	t.Helper()
	if ids == nil {
		ids = e.ids
	}
	eng, err := engine.New(engine.Config{
		Store: e.s, Clock: nowFunc(e.fx.Now), IDs: ids,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
	})
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	return eng
}

// heldProposal commits a held operation on its own conversation, expiring
// ttl after the store's now (never when ttl is zero).
func (e *env) heldProposal(t *testing.T, id model.OperationID, ttl time.Duration) model.OperationRef {
	t.Helper()
	rec := e.operation(t, id, []byte(id))
	rec.Key.Target = e.conversationRef(t, e.inbox).Ref()
	rec.Key.Caller = "plugin:triage"
	rec.Operation.State = model.Held
	rec.Operation.Held = &model.HeldBlock{Proposer: "plugin:triage", Reason: "r", TrustKey: "k", ProposedFingerprint: []byte(id)}
	if ttl > 0 {
		at := e.fx.Now().Add(ttl).UTC()
		rec.Operation.Held.ExpiresAt = &at
	}
	e.mustTx(t, func(tx store.Tx) error {
		_, _, err := tx.ClaimOperation(e.ctx, rec)
		return err
	})
	return rec.Operation.Ref
}

func (e *env) sweepOnce(t *testing.T, eng *engine.Engine) operations.ExpireReport {
	t.Helper()
	rep, err := operations.ExpireHeld(e.ctx, eng, tenantA, heldWindow)
	if err != nil {
		t.Fatalf("ExpireHeld: %v", err)
	}
	return rep
}

func heldExpiryState(t *testing.T, e *env, ref model.OperationRef) store.OperationRecord {
	t.Helper()
	return e.operationRecord(t, ref)
}

func heldExpirySurvivesRestart(t *testing.T, e *env, cross func(*testing.T)) {
	due, later := e.heldProposal(t, "due", heldTTL), e.heldProposal(t, "later", 3*heldTTL)
	e.advance(t, 2*heldTTL)
	cross(t)
	if rep := e.sweepOnce(t, e.sweepEngine(t, nil)); rep.Expired != 1 {
		t.Fatalf("report = %+v, want 1 expired", rep)
	}
	got := heldExpiryState(t, e, due)
	if got.Operation.State != model.Expired || got.Operation.Held.Decision != model.HeldExpired ||
		got.Operation.Held.DecidedAt == nil || !got.Operation.Held.DecidedAt.Equal(e.fx.Now().UTC().Truncate(time.Nanosecond)) {
		t.Fatalf("expired = %s %+v", got.Operation.State, got.Operation.Held)
	}
	// Past its deadline and its window at once, it expires: no carry-over.
	if got.Operation.Held.CarriedOverAt != nil {
		t.Fatalf("expired operation carried over: %+v", got.Operation.Held)
	}
	if st := heldExpiryState(t, e, later); st.Operation.State != model.Held || st.Operation.Held.CarriedOverAt == nil {
		t.Fatalf("later = %s %+v, want held and carried over", st.Operation.State, st.Operation.Held)
	}
	// The next process finds the transition committed: idempotent.
	cross(t)
	e.advance(t, time.Minute)
	if rep := e.sweepOnce(t, e.sweepEngine(t, nil)); rep.Expired != 0 {
		t.Fatalf("second sweep = %+v, want nothing", rep)
	}
	if again := heldExpiryState(t, e, due); again.Operation.Revision != got.Operation.Revision {
		t.Fatal("second sweep rewrote the expired operation")
	}
}

// cancelIDs lets n IDs through (two per operation: revision and event), then cancels the sweep: the process stops
// between two operations' units.
type cancelIDs struct {
	ports.IDs
	mu     sync.Mutex
	left   int
	cancel context.CancelFunc
}

func (c *cancelIDs) New(ctx context.Context) (model.ID, error) {
	c.mu.Lock()
	c.left--
	stop := c.left < 0
	c.mu.Unlock()
	if stop {
		c.cancel()
		return "", context.Canceled
	}
	return c.IDs.New(ctx)
}

func heldExpiryRestartMidSweep(t *testing.T, e *env, cross func(*testing.T)) {
	refs := []model.OperationRef{e.heldProposal(t, "a", heldTTL), e.heldProposal(t, "b", heldTTL), e.heldProposal(t, "c", heldTTL)}
	e.advance(t, 2*heldTTL)
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	eng := e.sweepEngine(t, &cancelIDs{IDs: e.ids, left: 2, cancel: cancel})
	rep, err := operations.ExpireHeld(ctx, eng, tenantA, heldWindow)
	if err == nil || rep.Expired != 1 {
		t.Fatalf("interrupted sweep = %+v, %v; want one expired and an error", rep, err)
	}
	cross(t)
	expired := 0
	revs := map[model.OperationRef]model.Revision{}
	for _, ref := range refs {
		rec := heldExpiryState(t, e, ref)
		if rec.Operation.State == model.Expired {
			expired++
			revs[ref] = rec.Operation.Revision
		}
	}
	if expired != 1 {
		t.Fatalf("after the crash %d expired, want exactly the committed one", expired)
	}
	if rep := e.sweepOnce(t, e.sweepEngine(t, nil)); rep.Expired != 2 {
		t.Fatalf("resumed sweep = %+v, want the 2 left", rep)
	}
	for _, ref := range refs {
		rec := heldExpiryState(t, e, ref)
		if rec.Operation.State != model.Expired {
			t.Fatalf("%s = %s, want expired", ref.ID, rec.Operation.State)
		}
		if rev, ok := revs[ref]; ok && rev != rec.Operation.Revision {
			t.Fatalf("%s rewritten by the resumed sweep", ref.ID)
		}
	}
}

// rendezvousIDs holds every sweeper after its scan and before its first
// unit, so all of them work from the same stale scan.
type rendezvousIDs struct {
	ports.IDs
	meet *rendezvous
	once sync.Once
}

func (r *rendezvousIDs) New(ctx context.Context) (model.ID, error) {
	r.once.Do(r.meet.wait)
	return r.IDs.New(ctx)
}

func heldExpiryConcurrentSweeps(t *testing.T, e *env, cross func(*testing.T)) {
	ref := e.heldProposal(t, "race", heldTTL)
	e.advance(t, 2*heldTTL)
	cross(t)
	const sweepers = 6
	meet := newRendezvous(sweepers)
	reps := make([]operations.ExpireReport, sweepers)
	errs := make([]error, sweepers)
	engines := make([]*engine.Engine, sweepers)
	for i := range engines {
		engines[i] = e.sweepEngine(t, &rendezvousIDs{IDs: e.ids, meet: meet})
	}
	var wg sync.WaitGroup
	for i := range sweepers {
		wg.Go(func() { reps[i], errs[i] = operations.ExpireHeld(e.ctx, engines[i], tenantA, heldWindow) })
	}
	wg.Wait()
	total := 0
	for i := range reps {
		if errs[i] != nil {
			t.Fatalf("sweeper %d: %v", i, errs[i])
		}
		total += reps[i].Expired
	}
	if total != 1 {
		t.Fatalf("transitions = %d (%+v), want exactly one", total, reps)
	}
	if st := heldExpiryState(t, e, ref).Operation.State; st != model.Expired {
		t.Fatalf("state = %s", st)
	}
}

func heldCarryOver(t *testing.T, e *env, cross func(*testing.T)) {
	carried := e.heldProposal(t, "carried", 4*heldTTL)
	dying := e.heldProposal(t, "dying", heldTTL)
	e.advance(t, heldWindow)
	if rep := e.sweepOnce(t, e.sweepEngine(t, nil)); rep.CarriedOver != 2 || rep.Expired != 0 {
		t.Fatalf("report = %+v, want both carried over", rep)
	}
	first := heldExpiryState(t, e, carried)
	if first.Operation.State != model.Held || first.Operation.Held.CarriedOverAt == nil {
		t.Fatalf("carried = %s %+v", first.Operation.State, first.Operation.Held)
	}
	cross(t)
	// The marker is set once and survives a later sweep, which also expires
	// the one past its deadline.
	e.advance(t, 2*heldTTL)
	if rep := e.sweepOnce(t, e.sweepEngine(t, nil)); rep.Expired != 1 || rep.CarriedOver != 0 {
		t.Fatalf("report = %+v, want one expired and no new marker", rep)
	}
	if again := heldExpiryState(t, e, carried); again.Operation.Revision != first.Operation.Revision ||
		!again.Operation.Held.CarriedOverAt.Equal(*first.Operation.Held.CarriedOverAt) {
		t.Fatal("carry-over marker rewritten")
	}
	// An expired operation refuses approve and reject, and is never
	// carried over again.
	exp := heldExpiryState(t, e, dying)
	if exp.Operation.State != model.Expired {
		t.Fatalf("dying = %s", exp.Operation.State)
	}
	eng := e.sweepEngine(t, nil)
	c, err := eng.For(model.Principal{Tenant: tenantA, Actor: "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	req := model.DecideHeld{Call: model.Call{Correlation: "d"}, Operation: dying, ExpectedRevision: exp.Operation.Revision}
	svc := operations.New(c)
	if _, err := svc.Approve(e.ctx, req); !model.IsCode(err, model.Conflict) {
		t.Fatalf("Approve of an expired operation = %v, want Conflict", err)
	}
	if _, err := svc.Reject(e.ctx, req); !model.IsCode(err, model.Conflict) {
		t.Fatalf("Reject of an expired operation = %v, want Conflict", err)
	}
	e.advance(t, heldWindow)
	if rep := e.sweepOnce(t, eng); rep != (operations.ExpireReport{}) {
		t.Fatalf("sweep after expiry = %+v, want nothing", rep)
	}
	// The marker it earned while held stays as history.
	if got := heldExpiryState(t, e, dying); got.Operation.Revision != exp.Operation.Revision || got.Operation.Held.CarriedOverAt == nil {
		t.Fatalf("expired operation changed: %+v", got.Operation.Held)
	}
}

// A held operation's duplicate-guard scope is committed with it: two
// proposals of one kind and target under different scopes are both held,
// and each keeps its scope through a restart and a held listing.
func heldScopeSurvivesRestart(t *testing.T, e *env, cross func(*testing.T)) {
	target := e.conversationRef(t, e.inbox).Ref()
	propose := func(id model.OperationID, scope string) model.OperationRef {
		rec := e.operation(t, id, []byte(id))
		rec.Key.Target, rec.Key.Caller = target, "plugin:triage"
		rec.Operation.State = model.Held
		rec.Operation.Held = &model.HeldBlock{
			Proposer: "plugin:triage", Reason: "r", TrustKey: "k", ProposedFingerprint: []byte(id), Scope: scope,
		}
		e.mustTx(t, func(tx store.Tx) error {
			_, created, err := tx.ClaimOperation(e.ctx, rec)
			if err == nil && !created {
				t.Fatalf("%s not created", id)
			}
			return err
		})
		return rec.Operation.Ref
	}
	want := map[model.OperationRef]string{propose("s-a", "alice"): "alice", propose("s-b", "bob"): "bob", propose("s-c", ""): ""}
	cross(t)
	for ref, scope := range want {
		if got := e.operationRecord(t, ref).Operation.Held.Scope; got != scope {
			t.Fatalf("%s scope = %q, want %q", ref.ID, got, scope)
		}
	}
	e.mustTx(t, func(tx store.Tx) error {
		p, err := tx.Operations(e.ctx, model.ListRequest{
			Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: 10},
		})
		if err != nil {
			return err
		}
		if len(p.Items) != len(want) {
			t.Fatalf("held listing = %d operations, want %d", len(p.Items), len(want))
		}
		for _, rec := range p.Items {
			if scope, ok := want[rec.Operation.Ref]; !ok || rec.Operation.Held.Scope != scope {
				t.Fatalf("listed %s scope = %q, want %q", rec.Operation.Ref.ID, rec.Operation.Held.Scope, scope)
			}
		}
		return nil
	})
}
