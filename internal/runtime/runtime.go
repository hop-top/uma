// Package runtime runs an engine's background work: the Run executor for
// outbound operations with retry and reconciliation, persistent receivers
// under a channel lease, webhook bindings, and the adapter open path they
// share.
package runtime

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Call carries the caller of one runtime method. Its fields mirror the
// public RuntimeCall one for one; the public API converts.
type Call struct {
	Principal model.Principal
	Call      model.Call
}

// WorkerOptions configures one Run call. Its fields mirror the public
// WorkerOptions one for one; the public API converts.
type WorkerOptions struct {
	Owner string
	Limit int
	Kinds []string
	Lease time.Duration
	Poll  time.Duration
	// SweepEvery is the cadence of the held-operation expiry sweep Run
	// interleaves with claiming: zero is DefaultSweepEvery, negative turns
	// it off. It is also the window after which a still-held operation is
	// marked carried over.
	SweepEvery time.Duration
}

// Runtime is one engine's runtime: worker runs, persistent receivers and
// webhook bindings. New registers it with the engine, so Engine.Close halts
// its runs and retires its bindings.
type Runtime struct {
	e         *engine.Engine
	effectors effectors
	run       runtimeState
	hooks     webhooks
	receivers receiverStates
}

// New returns the runtime of e; its executor runs the built-in replies,
// starts, notes, refetches, edits, deletes and reactions plus every extra
// Effector's operations, matched by stored intent name.
func New(e *engine.Engine, extra ...Effector) *Runtime {
	rt := &Runtime{e: e, effectors: newEffectors(e, extra)}
	e.OnStop(rt.stop)
	return rt
}

// stop halts every run and retires every webhook binding; Engine.Close
// calls it.
func (rt *Runtime) stop() {
	rt.run.stopAll()
	rt.hooks.closeAll()
}

// SetJitter replaces the backoff jitter source; nil restores the uniform
// default. Tests use it to make backoff deterministic.
func (rt *Runtime) SetJitter(fn func(max time.Duration) time.Duration) {
	rt.run.mu.Lock()
	rt.run.jitter = fn
	rt.run.mu.Unlock()
}

// Worker option defaults.
const (
	defaultLease = time.Minute
	defaultPoll  = time.Second
	// DefaultSweepEvery is one minute: a proposal past its deadline waits at
	// most that long, plus a poll, to be expired, at one cheap indexed scan
	// of the held operations per minute and process.
	DefaultSweepEvery = time.Minute
)

// runtimeState tracks Runtime.Run calls so Drain and Close can stop their
// claims. The zero value is ready.
type runtimeState struct {
	mu       sync.Mutex
	stopped  bool
	draining map[model.TenantID]bool
	runs     map[*runHandle]struct{}
	// jitter overrides engine.UniformJitter in tests.
	jitter func(max time.Duration) time.Duration
}

// runHandle is one active Runtime.Run call.
type runHandle struct {
	tenant model.TenantID
	stop   context.CancelFunc // cancels the idle wait, never an execution
	halted chan struct{}      // closed when stop is called
	once   sync.Once
	done   chan struct{} // closed when Run returns
}

func (h *runHandle) halt() {
	h.once.Do(func() {
		close(h.halted)
		h.stop()
	})
}

func (h *runHandle) isHalted() bool {
	select {
	case <-h.halted:
		return true
	default:
		return false
	}
}

func errDraining() error {
	return &model.Error{Code: model.Unavailable, SafeMessage: "runtime draining", Retry: model.NoRetry}
}

func (s *runtimeState) register(tenant model.TenantID, stop context.CancelFunc) (*runHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, engine.ErrClosed()
	}
	if s.draining[tenant] {
		return nil, errDraining()
	}
	h := &runHandle{tenant: tenant, stop: stop, halted: make(chan struct{}), done: make(chan struct{})}
	if s.runs == nil {
		s.runs = map[*runHandle]struct{}{}
	}
	s.runs[h] = struct{}{}
	return h, nil
}

func (s *runtimeState) unregister(h *runHandle) {
	s.mu.Lock()
	delete(s.runs, h)
	s.mu.Unlock()
	close(h.done)
}

// drain stops new claims for tenant, now and for later Run calls, and
// returns the active runs to wait for.
func (s *runtimeState) drain(tenant model.TenantID) []*runHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining == nil {
		s.draining = map[model.TenantID]bool{}
	}
	s.draining[tenant] = true
	var out []*runHandle
	for h := range s.runs {
		if h.tenant == tenant {
			h.halt()
			out = append(out, h)
		}
	}
	return out
}

// stopAll halts every run; Engine.Close calls it.
func (s *runtimeState) stopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	for h := range s.runs {
		h.halt()
	}
}

func (s *runtimeState) jitterFunc() func(time.Duration) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jitter != nil {
		return s.jitter
	}
	return engine.UniformJitter
}

// timerWaiter is the default Waiter: a stdlib timer that honours
// cancellation. It starts no goroutine and stops its timer on return.
type timerWaiter struct{}

func (timerWaiter) Wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runtimeKinds are the work kinds Runtime.Run executes.
func runtimeKinds() []string { return []string{operations.WorkKindOperation} }

// Run executes claimed work of the selected kinds for the caller's tenant
// until ctx ends, Drain is called for the tenant or the engine closes. It
// claims up to opts.Limit items per
// round under a lease of opts.Lease, within the policy's in-flight cap,
// runs each to its next durable state, quarantines an item whose claims
// exceed the policy's budget, and waits opts.Poll through the configured
// Waiter, or a stdlib timer without one, when a round finds nothing or is
// saturated. Work already claimed when a
// drain starts is finished; nothing new is claimed. Run returns nil after a
// drain or close and ctx's error after cancellation. Several Run calls,
// in one or many processes, may share a store: leases and fences keep each
// item with one current owner.
func (rt *Runtime) Run(ctx context.Context, call Call, opts WorkerOptions) error {
	e := rt.e
	if e.IsClosed() {
		return engine.ErrClosed()
	}
	if err := call.Principal.Validate(); err != nil {
		return err
	}
	opts, err := workerOptions(opts)
	if err != nil {
		return err
	}
	waiter := e.Waiter()
	if waiter == nil {
		waiter = timerWaiter{}
	}
	tenant := call.Principal.Tenant
	waitCtx, stop := context.WithCancel(ctx)
	defer stop()
	h, err := rt.run.register(tenant, stop)
	if err != nil {
		return err
	}
	defer rt.run.unregister(h)
	// stopped maps a failed wait to Run's result.
	stopped := func(err error) error {
		if h.isHalted() || e.IsClosed() {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	lost := 0
	var sweepAt time.Time
	for {
		if h.isHalted() || e.IsClosed() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := rt.sweepIfDue(ctx, tenant, opts.SweepEvery, &sweepAt); err != nil {
			return stopped(err)
		}
		works, err := rt.ClaimWork(ctx, tenant, opts)
		switch {
		case model.IsCode(err, model.Transient):
			// Lost to concurrent claims, which says nothing about the
			// queue: claim again shortly instead of idling a poll.
			if err := rt.pause(waitCtx, lost, opts.Poll); err != nil {
				return stopped(err)
			}
			lost++
			continue
		case err != nil && !saturated(err):
			return err
		}
		lost = 0
		for _, w := range works {
			if err := rt.ExecuteClaimed(ctx, tenant, w); err != nil {
				return err
			}
		}
		if len(works) > 0 {
			continue
		}
		if err := waiter.Wait(waitCtx, opts.Poll); err != nil {
			return stopped(err)
		}
	}
}

// ExecuteClaimed runs one claimed item as one unit of engine work, so Close
// waits for it. Run calls it for each item ClaimWork returns.
func (rt *Runtime) ExecuteClaimed(ctx context.Context, tenant model.TenantID, w store.Work) error {
	done, err := rt.e.Begin()
	if err != nil {
		// Closed: the lease expires and another owner reclaims the item.
		return nil
	}
	defer done()
	if rt.poisoned(w) {
		err = rt.quarantine(ctx, tenant, w)
	} else {
		err = rt.execute(ctx, tenant, w)
	}
	switch {
	case err == nil, errors.Is(err, errStale):
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	case model.IsCode(err, model.Transient), model.IsCode(err, model.Conflict):
		// Lost the lease, or a unit for longer than transactClaimed
		// reruns it: the item stays recoverable once its lease expires.
		return nil
	}
	return err
}

// ClaimWork leases up to opts.Limit claimable items of opts.Kinds for
// opts.Owner, as one claim round of Run, within the work policy's in-flight
// cap. A round the cap left empty fails with model.Unavailable and is
// observed as queue_saturated; every round reports its backlog. opts must
// already carry defaults.
func (rt *Runtime) ClaimWork(ctx context.Context, tenant model.TenantID, opts WorkerOptions) ([]store.Work, error) {
	e := rt.e
	policy := e.Work()
	var works []store.Work
	err := operations.Transact(ctx, e, tenant, func(tx store.Tx) error {
		var err error
		works, err = tx.ClaimWork(ctx, store.WorkClaim{
			Kinds: opts.Kinds, Limit: opts.Limit, Owner: opts.Owner,
			Until: e.Clock().Now().Add(opts.Lease), MaxInFlight: policy.MaxInFlight,
		})
		return err
	})
	rt.observeRound(ctx, tenant, opts.Kinds, err)
	if err != nil {
		// The items of a unit that did not commit were never claimed.
		return nil, err
	}
	return works, nil
}

func workerOptions(o WorkerOptions) (WorkerOptions, error) {
	if o.Owner == "" {
		return o, model.NewError(model.Validation, "worker owner: required")
	}
	if o.Limit < 0 || o.Lease < 0 || o.Poll < 0 {
		return o, model.NewError(model.Validation, "worker options: must not be negative")
	}
	if o.Limit == 0 {
		o.Limit = 1
	}
	if o.Lease == 0 {
		o.Lease = defaultLease
	}
	if o.Poll == 0 {
		o.Poll = defaultPoll
	}
	if o.SweepEvery == 0 {
		o.SweepEvery = DefaultSweepEvery
	}
	if len(o.Kinds) == 0 {
		o.Kinds = runtimeKinds()
	}
	o.Kinds = slices.Clone(o.Kinds)
	for _, k := range o.Kinds {
		if !slices.Contains(runtimeKinds(), k) {
			return o, &model.Error{Code: model.Unsupported, SafeMessage: "work kind not executed by this runtime", Retry: model.NoRetry}
		}
	}
	return o, nil
}

// Drain stops new claims for the caller's tenant: active Run calls finish
// the work they hold and return, and later Run calls for the tenant fail.
// It waits for active runs until ctx ends, then returns ctx's error;
// unfinished work keeps its lease until expiry and stays recoverable.
func (rt *Runtime) Drain(ctx context.Context, call Call) error {
	if rt.e.IsClosed() {
		return engine.ErrClosed()
	}
	if err := call.Principal.Validate(); err != nil {
		return err
	}
	for _, h := range rt.run.drain(call.Principal.Tenant) {
		select {
		case <-h.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// errStale reports that this worker no longer owns the item.
var errStale = errors.New("lease lost")
