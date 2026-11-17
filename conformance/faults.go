package conformance

import (
	"context"
	"errors"
	"slices"
	"sync"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Fault injection. A crashStore is one process's handle on the store under
// test. Rules fire at a point of a unit of work whose writes they match:
// before it commits, or after it committed but before its caller learns the
// result. A rule that crashes the process fails that unit, so a unit
// crashed before commit never commits, and every later call on the handle
// fails, as calls of a dead process would. A rule may also block, holding
// the unit at that point while the case drives other work.

// errCrashed fails every call on a crashed process's store handle.
var errCrashed = errors.New("conformance: process crashed")

type faultPoint int

const (
	// beforeCommit: the unit's writes are done and it has not committed.
	beforeCommit faultPoint = iota + 1
	// afterCommit: the unit committed and its caller has not seen it.
	afterCommit
)

// unitLog records the writes of one unit that rules match on.
type unitLog struct {
	evidence []store.AttemptEvidence
	updates  []model.Operation
	complete []store.Work
	ingress  []store.IngressRecord
	advances []store.CheckpointAdvance
}

// begins reports a unit recording an attempt before its provider call.
func (u *unitLog) begins() bool {
	return len(u.evidence) == 1 && u.evidence[0].State == model.Unknown && len(u.updates) == 1
}

// answers reports a unit recording a provider answer as attempt evidence.
func (u *unitLog) answers() bool {
	return len(u.evidence) == 1 && u.evidence[0].State != model.Unknown && len(u.updates) == 0
}

// settles reports a unit writing an operation state and completing its work.
func (u *unitLog) settles() bool {
	return len(u.evidence) == 0 && len(u.updates) == 1 && len(u.complete) == 1
}

// claimsIngress reports an ingress unit.
func (u *unitLog) claimsIngress() bool { return len(u.ingress) > 0 }

// acks reports a unit advancing a subscription checkpoint.
func (u *unitLog) acks() bool { return len(u.advances) > 0 }

type faultRule struct {
	point faultPoint
	match func(*unitLog) bool
	act   func() error
	fired bool
}

type crashStore struct {
	inner store.Store
	// onCrash cancels the process's work.
	onCrash func()

	mu      sync.Mutex
	rules   []*faultRule
	dead    bool
	onClaim func(store.IngressRecord)
	// candidates lists the groups whose candidates units read, in order.
	candidates []store.SelectionKey
}

// candidateReads returns the candidate groups units on the handle read.
func (s *crashStore) candidateReads() []store.SelectionKey {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.candidates)
}

// at adds a rule that fires once, at the first unit matching match at p.
func (s *crashStore) at(p faultPoint, match func(*unitLog) bool, act func() error) {
	s.mu.Lock()
	s.rules = append(s.rules, &faultRule{point: p, match: match, act: act})
	s.mu.Unlock()
}

// crash kills the process: its work is cancelled and every later call on
// the handle fails. As a rule's action it fails the unit in flight too.
func (s *crashStore) crash() error {
	s.mu.Lock()
	s.dead = true
	f := s.onCrash
	s.mu.Unlock()
	if f != nil {
		f()
	}
	return errCrashed
}

// onIngressClaim calls fn before each ingress claim reaches the store.
func (s *crashStore) onIngressClaim(fn func(store.IngressRecord)) {
	s.mu.Lock()
	s.onClaim = fn
	s.mu.Unlock()
}

func (s *crashStore) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func (s *crashStore) fire(p faultPoint, u *unitLog) error {
	s.mu.Lock()
	var hit *faultRule
	for _, r := range s.rules {
		if !r.fired && r.point == p && r.match(u) {
			r.fired, hit = true, r
			break
		}
	}
	s.mu.Unlock()
	if hit == nil {
		return nil
	}
	return hit.act()
}

func (s *crashStore) Properties() store.Properties { return s.inner.Properties() }

func (s *crashStore) View(ctx context.Context, tenant model.TenantID, fn func(store.Reader) error) error {
	if s.isDead() {
		return errCrashed
	}
	return s.inner.View(ctx, tenant, fn)
}

func (s *crashStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	if s.isDead() {
		return errCrashed
	}
	var u *unitLog
	err := s.inner.Transact(ctx, tenant, func(tx store.Tx) error {
		u = &unitLog{}
		if err := fn(&faultTx{Tx: tx, s: s, u: u}); err != nil {
			return err
		}
		return s.fire(beforeCommit, u)
	})
	if err != nil {
		return err
	}
	return s.fire(afterCommit, u)
}

// faultTx records the writes rules match on.
type faultTx struct {
	store.Tx
	s *crashStore
	u *unitLog
}

func (t *faultTx) AppendAttemptEvidence(ctx context.Context, ev store.AttemptEvidence) error {
	t.u.evidence = append(t.u.evidence, ev)
	return t.Tx.AppendAttemptEvidence(ctx, ev)
}

func (t *faultTx) UpdateOperation(ctx context.Context, op model.Operation, expected model.Revision, fence store.Fence) error {
	t.u.updates = append(t.u.updates, op)
	return t.Tx.UpdateOperation(ctx, op, expected, fence)
}

func (t *faultTx) CompleteWork(ctx context.Context, w store.Work) error {
	t.u.complete = append(t.u.complete, w)
	return t.Tx.CompleteWork(ctx, w)
}

func (t *faultTx) ClaimIngress(ctx context.Context, rec store.IngressRecord) (store.IngressRecord, bool, error) {
	t.u.ingress = append(t.u.ingress, rec)
	t.s.mu.Lock()
	fn := t.s.onClaim
	t.s.mu.Unlock()
	if fn != nil {
		fn(rec)
	}
	return t.Tx.ClaimIngress(ctx, rec)
}

func (t *faultTx) Candidates(ctx context.Context, key store.SelectionKey, limit int) ([]model.Conversation, error) {
	t.s.mu.Lock()
	t.s.candidates = append(t.s.candidates, key)
	t.s.mu.Unlock()
	return t.Tx.Candidates(ctx, key, limit)
}

func (t *faultTx) AdvanceCheckpoint(ctx context.Context, a store.CheckpointAdvance) error {
	t.u.advances = append(t.u.advances, a)
	return t.Tx.AdvanceCheckpoint(ctx, a)
}
