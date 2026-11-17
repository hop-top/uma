package runtime

import (
	"context"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Receiver lease timing: a Receive call holds the channel's lease for
// receiverLease and renews it about every receiverRenew. The wait before a
// renewal is spread so channels started together do not renew in lockstep
// (see renewWait): the first renewal falls at a per-channel offset in
// [receiverRenew/2, receiverRenew), later ones every receiverRenew +-10%.
// The longest wait is receiverRenewMax, so a renewal is always attempted at
// least receiverLease-receiverRenewMax (38s) before the lease expires, and
// two consecutive renewals may fail with the lease still held, as at the
// fixed period.
const (
	receiverLease    = time.Minute
	receiverRenew    = receiverLease / 3
	receiverRenewMax = receiverRenew + receiverRenew/10
)

// phaseOf is a stable fraction in [0, 1) of the channel, distinct per
// salt so the start offset and the lease phase are not correlated.
func phaseOf(salt string, ref model.ChannelRef) float64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(salt + "/" + string(ref.Tenant) + "/" + string(ref.ID)))
	return float64(h.Sum32()%1000) / 1000
}

// startOffset is the wait before the first connection of a Receive with
// the given StartSpread: a stable offset in [0, spread) per channel.
func startOffset(ref model.ChannelRef, spread time.Duration) time.Duration {
	return time.Duration(phaseOf("start", ref) * float64(spread))
}

// renewWait is the wait before a lease renewal. first is the wait after
// acquiring the lease: it is receiverRenew scaled by a stable fraction in
// [0.5, 1) of the channel's reference, so channels do not share a phase,
// and the same channel always starts at the same one. Later waits are
// receiverRenew +-10%, spread by jitter (a duration in [0, max], as
// Runtime.SetJitter replaces it). Neither exceeds receiverRenewMax.
func renewWait(ref model.ChannelRef, first bool, jitter func(time.Duration) time.Duration) time.Duration {
	if first {
		return receiverRenew/2 + time.Duration(phaseOf("renew", ref)*float64(receiverRenew/2))
	}
	w := receiverRenew - receiverRenew/10 + jitter(receiverRenew/5)
	return min(max(w, receiverRenew-receiverRenew/10), receiverRenewMax)
}

// maxReceiveWait caps the wait a failed connection states with a
// Retry-After: a longer one is treated as this long, so a bogus or absurd
// value cannot park a receiver for good.
const maxReceiveWait = 24 * time.Hour

// ReceiveOptions configures one Receive call. Owner names the call as the
// holder of the channel's receiver lease, as ReceiverStatus reports it; it
// follows the opaque-token rule. Empty, the call is named receiver/<id>
// with a fresh ID.
//
// StartSpread, when positive, delays the call's first connection by a
// stable offset in [0, StartSpread) taken from a hash of the channel, so
// receivers a host starts together do not open their first connections in
// phase. The same channel always gets the same offset; later connections
// (reconnects) are not delayed. Zero starts at once; negative is a
// validation error. The wait is by the engine's Waiter, ends with the call,
// and the lease is held (and renewed) through it.
type ReceiveOptions struct {
	Owner       string
	StartSpread time.Duration
}

// ReceiverStatus is a channel's receiver lease: the Owner of the Receive
// call holding it and Until, when the lease expires unless renewed. A
// channel no Receive owns has no Owner and a zero Until.
//
// The rest is what the process running that Receive call knows of it, read
// from its memory and empty for a call another process runs, for a lease
// that expired and for a call that returned. Phase is "" then. A crash
// loses it; the next Receive starts at running with no failures. Phase is
// what the supervisor is doing, Link what the adapter says of its provider
// link; neither affects retry or the lease.
//
//   - Phase: running while a connection is opened or open, retrying
//     between a failed connection and the next, ended while the call
//     releases its lease. Since is when the phase began.
//   - Failures: consecutive connections that accepted nothing, the count
//     Retry.MaxAttempts bounds; zero once a connection accepted ingress.
//     Reason is the code of the last of them, "" when none; no message.
//   - RetryAt: while retrying, when the next connection opens, as the
//     engine's clock reads, at the earliest: backoff or the wait the
//     failure stated, up to 24 hours.
//   - Link: while running and the adapter implements
//     adapter.ReceiverReporter, connecting or connected; "" otherwise.
//     An adapter that implements adapter.ReceiverDegradationReporter and
//     reports a degradation shows degraded instead, with its code in
//     Detail ("" when not degraded); the receiver still works, and
//     reports of connecting and connected resume at recovery.
type ReceiverStatus struct {
	Owner    string
	Until    time.Time
	Phase    ReceiverPhase
	Since    time.Time
	Failures int
	Reason   model.ErrorCode
	RetryAt  time.Time
	Link     ReceiverLink
	Detail   string
}

// ReceiverStatus reports who holds the channel's receiver lease, read from
// the store in one read-only unit (the channel, then its lease), so a host
// can see which process receives a channel without starting a Receive that
// conflicts. The channel must exist in call's tenant. Only the fields of
// the process running the call come from memory; a host that polls them
// should use ReceiverState, which reads no store.
func (rt *Runtime) ReceiverStatus(ctx context.Context, call Call, ref model.ChannelRef) (ReceiverStatus, error) {
	e := rt.e
	done, err := e.Begin()
	if err != nil {
		return ReceiverStatus{}, err
	}
	defer done()
	if err := engine.FirstErr(call.Principal.Validate(), ref.Validate()); err != nil {
		return ReceiverStatus{}, err
	}
	if ref.Tenant != call.Principal.Tenant {
		return ReceiverStatus{}, engine.ErrNotFound()
	}
	var st ReceiverStatus
	err = e.Store().View(ctx, ref.Tenant, func(r store.Reader) error {
		if _, err := r.GetChannel(ctx, ref); err != nil {
			return err
		}
		l, err := r.Lease(ctx, ref.Ref())
		switch {
		case model.IsCode(err, model.NotFound):
			return nil
		case err != nil:
			return err
		}
		st = ReceiverStatus{Owner: l.Fence.Owner, Until: l.Until}
		if rs, ok := rt.receivers.ownedBy(ref, l.Fence.Owner); ok {
			mine := rs.snapshot()
			mine.Owner, mine.Until = st.Owner, st.Until
			st = mine
		}
		return nil
	})
	if model.IsCode(err, model.NotFound) {
		return ReceiverStatus{}, engine.ErrNotFound()
	}
	return st, err
}

// ReceiverState is the state of the Receive call of ref this runtime is
// running, read from memory alone: it opens no store unit, so a host can
// poll it per channel without load on the store. It reports false when this
// runtime runs no Receive for the channel, which includes a call another
// process runs, a call that returned and an invalid call or channel.
//
// Authorization is ReceiverStatus's: call's principal must be valid and in
// the channel's tenant, and the answer is false otherwise, never another
// tenant's state. ReceiverStatus authorizes by tenant alone and reads the
// store only to learn whether the channel and lease exist; here a running
// receiver is that proof, and the snapshot holds supervisor state and the
// Owner name only, no tenant data.
//
// The status has Owner, Phase, Since, Failures, Reason, RetryAt, Link and Detail as
// ReceiverStatus reports them for the process's own call. Until is zero:
// the lease's expiry is stored, so only ReceiverStatus reads it. Unlike
// ReceiverStatus the snapshot is not cleared when the lease expired or was
// taken over; the supervisor is about to stop and the phase says what it
// is doing until then.
func (rt *Runtime) ReceiverState(call Call, ref model.ChannelRef) (ReceiverStatus, bool) {
	if rt.e.IsClosed() {
		return ReceiverStatus{}, false
	}
	if engine.FirstErr(call.Principal.Validate(), ref.Validate()) != nil || ref.Tenant != call.Principal.Tenant {
		return ReceiverStatus{}, false
	}
	s, ok := rt.receivers.get(ref)
	if !ok {
		return ReceiverStatus{}, false
	}
	st := s.snapshot()
	st.Owner = s.owner
	return st, true
}

// Receive runs the channel's persistent receiver under ctx with the
// engine-bound sink of call's service principal, and blocks until ctx ends,
// Drain is called for the tenant, the engine closes, the receiver fails for
// good or ownership is lost. The adapter must declare receive.
//
// The principal needs model.ActionChannelReceive: on the channel to start
// and on every connection, and into the resolved inbox on every accept. A
// denial at start returns model.Unauthorized at once; an accept denied
// later ends the connection with it, which is not reconnected, so Receive
// returns it. A denied ingress commits nothing, cursor included, so a
// later Receive resumes before it.
//
// One Receive call owns a channel instance at a time, across every process
// sharing the store: the call holds a store lease on the channel under
// opts.Owner, renewed while it runs and released on return. A Receive for a
// channel another call owns is model.Conflict; ReceiverStatus names the
// owner. If the lease is lost, for example after a pause
// long enough for it to expire and another owner to take it, the receiver
// stops and Receive returns model.Conflict. Overlap before a lost lease is
// detected is safe: ingress deduplicates per channel.
//
// Each connection opens a fresh instance through the engine's Open path,
// rereading the channel. A retryable failure, an untyped error or a
// receiver that returns without being stopped reconnects after the retry
// policy's backoff, paced by the configured Waiter. A wait the failure
// states (RetryAfter) replaces the backoff when longer, from the first
// failure, up to 24 hours. Retry.MaxAttempts consecutive connections that
// accept nothing end the call with the last failure. A connection that
// accepted ingress resets the count. Any other failure returns at once.
// Receive returns nil after a drain or close and ctx's error after
// cancellation.
//
// While the call runs, ReceiverStatus of this runtime reports its phase,
// failures, the code of the last failure, the time of the next connection
// and, when the instance implements adapter.ReceiverReporter, the
// adapter's link. The state is advisory and kept in memory only.
func (rt *Runtime) Receive(ctx context.Context, call Call, ref model.ChannelRef, opts ReceiveOptions) error {
	e := rt.e
	if e.IsClosed() {
		return engine.ErrClosed()
	}
	if err := call.Principal.Validate(); err != nil {
		return err
	}
	if opts.StartSpread < 0 {
		return model.NewError(model.Validation, "start spread: must not be negative")
	}
	if opts.Owner != "" {
		if err := model.ActorID(opts.Owner).Validate(); err != nil {
			return model.NewError(model.Validation, "owner: must follow the opaque-token rule")
		}
	}
	snk, err := ingress.Bind(ctx, e, call.Principal, ref)
	if err != nil {
		return err
	}
	if err := declares(snk.Descriptor(), model.CapabilityReceive); err != nil {
		return err
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	h, err := rt.run.register(call.Principal.Tenant, stop)
	if err != nil {
		return err
	}
	defer rt.run.unregister(h)
	own, err := acquireReceiver(ctx, e, snk.Channel().Ref, opts.Owner, rt.run.jitterFunc(), stop)
	if err != nil {
		return err
	}
	state := newReceiverState(own.owner, e.Clock().Now())
	rt.receivers.add(ref, state)
	defer rt.receivers.remove(ref, state) // after the release below
	defer own.release()
	r := &receiver{rt: rt, e: e, principal: call.Principal, ref: ref, h: h, own: own, state: state, ctx: ctx, run: runCtx, startSpread: opts.StartSpread}
	return r.supervise()
}

// receiver is one Receive call's supervision state.
type receiver struct {
	rt        *Runtime
	e         *engine.Engine
	principal model.Principal
	ref       model.ChannelRef
	h         *runHandle
	own       *receiverOwnership
	state     *receiverState
	ctx       context.Context // the host's
	run       context.Context // cancelled on halt or lost ownership

	startSpread time.Duration // ReceiveOptions.StartSpread
}

func (r *receiver) supervise() (err error) {
	waiter := waiterOf(r.e)
	jitter := r.rt.run.jitterFunc()
	failures := 0
	defer func() { r.state.ended(failures, err, r.e.Clock().Now()) }()
	if r.startSpread > 0 {
		_ = waiter.Wait(r.run, startOffset(r.ref, r.startSpread))
	}
	for {
		if done, err := r.stopped(); done {
			return err
		}
		r.state.running(r.e.Clock().Now())
		accepted, err := r.connect()
		if done, err := r.stopped(); done {
			return err
		}
		if accepted {
			failures = 0
		}
		if !reconnectable(err) {
			return err
		}
		failures++
		if failures >= r.e.Retry().MaxAttempts {
			return err
		}
		wait := r.e.Retry().Delay(failures, statedWait(err), jitter)
		r.state.retrying(failures, err, r.e.Clock().Now(), wait)
		_ = waiter.Wait(r.run, wait)
	}
}

// statedWait returns the wait a connection failure asks for, capped at
// maxReceiveWait; zero when it states none.
func statedWait(err error) time.Duration {
	me, ok := err.(*model.Error)
	if !ok {
		return 0
	}
	return min(max(me.RetryAfter, 0), maxReceiveWait)
}

// stopped reports whether supervision ends regardless of the last
// connection, and with what.
func (r *receiver) stopped() (bool, error) {
	switch {
	case r.h.isHalted(), r.e.IsClosed():
		return true, nil
	case r.own.isLost():
		return true, errReceiverLost()
	case r.ctx.Err() != nil:
		return true, r.ctx.Err()
	}
	return false, nil
}

// connect runs the receiver over one freshly opened instance. It reports
// whether any ingress was accepted and the safe form of the failure that
// ended the connection; a return without failure is a disconnect.
func (r *receiver) connect() (bool, error) {
	snk, err := ingress.Bind(r.run, r.e, r.principal, r.ref)
	if err != nil {
		return false, err
	}
	inst, err := openInbound(r.run, r.e, snk, model.CapabilityReceive)
	if err != nil {
		return false, err
	}
	defer func() { _ = inst.Close(context.WithoutCancel(r.run)) }()
	counted := &countingSink{Sink: snk, onFirst: r.state.accepted}
	r.state.bind(inst)
	// checkInstance guarantees the interface for a declared receive.
	err = inst.(adapter.Receiver).Run(r.run, counted)
	r.state.unbind()
	if err == nil {
		err = model.NewError(model.Unavailable, "receiver disconnected")
	}
	return counted.accepted.Load(), safeAdapterError(r.run, err, "receiver failed")
}

// reconnectable reports whether a connection failure is worth another
// connection: a retryable typed failure. safeAdapterError has already
// turned untyped failures and disconnects into unavailable.
func reconnectable(err error) bool {
	me, ok := err.(*model.Error)
	return ok && engine.Retryable(me)
}

// countingSink records whether any Accept succeeded. It embeds the engine
// sink, so the receiver sees every interface the sink implements.
type countingSink struct {
	*ingress.Sink
	accepted atomic.Bool
	onFirst  func() // called once, on the first success
}

func (c *countingSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	r, err := c.Sink.Accept(ctx, in)
	if err == nil && c.accepted.CompareAndSwap(false, true) {
		c.onFirst()
	}
	return r, err
}

func errReceiverOwned() error {
	return &model.Error{Code: model.Conflict, SafeMessage: "channel receiver is owned by another Receive", Retry: model.NoRetry}
}

func errReceiverLost() error {
	return &model.Error{Code: model.Conflict, SafeMessage: "channel receiver ownership lost", Retry: model.NoRetry}
}

// waiterOf returns the configured Waiter or the stdlib timer.
func waiterOf(e *engine.Engine) ports.Waiter {
	if w := e.Waiter(); w != nil {
		return w
	}
	return timerWaiter{}
}

// receiverOwnership is a held receiver lease and the goroutine renewing it.
type receiverOwnership struct {
	e      *engine.Engine
	tenant model.TenantID
	owner  string

	mu    sync.Mutex
	lease store.Lease
	lost  bool

	stopRenew context.CancelFunc
	renewed   chan struct{} // closed when the renewal goroutine returns
}

// acquireReceiver takes the channel's receiver lease for owner, or a
// generated receiver/<id> when empty, and starts renewing it. Losing it
// calls onLost.
func acquireReceiver(ctx context.Context, e *engine.Engine, ch model.ChannelRef, owner string, jitter func(time.Duration) time.Duration, onLost context.CancelFunc) (*receiverOwnership, error) {
	if owner == "" {
		id, err := operations.NewID(ctx, e)
		if err != nil {
			return nil, err
		}
		owner = "receiver/" + string(id)
	}
	var lease store.Lease
	err := operations.Transact(ctx, e, ch.Tenant, func(tx store.Tx) error {
		var err error
		lease, err = tx.AcquireLease(ctx, store.LeaseRequest{
			Resource: ch.Ref(), Owner: owner, Until: e.Clock().Now().Add(receiverLease),
		})
		return err
	})
	switch {
	case model.IsCode(err, model.Conflict):
		return nil, errReceiverOwned()
	case err != nil:
		return nil, err
	}
	renewCtx, stopRenew := context.WithCancel(context.WithoutCancel(ctx))
	o := &receiverOwnership{e: e, tenant: ch.Tenant, owner: owner, lease: lease, stopRenew: stopRenew, renewed: make(chan struct{})}
	go o.keep(renewCtx, ch, jitter, onLost)
	return o, nil
}

// keep renews the lease about every receiverRenew (renewWait) until stopped. A renewal the
// store refuses as a conflict means another owner holds the channel: the
// ownership is lost and onLost stops the receiver. Other renewal failures
// are retried on the next tick; the lease outlives several of them.
func (o *receiverOwnership) keep(ctx context.Context, ch model.ChannelRef, jitter func(time.Duration) time.Duration, onLost context.CancelFunc) {
	defer close(o.renewed)
	waiter := waiterOf(o.e)
	for first := true; ; first = false {
		if err := waiter.Wait(ctx, renewWait(ch, first, jitter)); err != nil || ctx.Err() != nil {
			return
		}
		o.mu.Lock()
		next := o.lease
		o.mu.Unlock()
		next.Until = o.e.Clock().Now().Add(receiverLease)
		var got store.Lease
		err := operations.Transact(ctx, o.e, o.tenant, func(tx store.Tx) error {
			var err error
			got, err = tx.RenewLease(ctx, next)
			return err
		})
		switch {
		case err == nil:
			o.mu.Lock()
			o.lease = got
			o.mu.Unlock()
		case model.IsCode(err, model.Conflict):
			o.mu.Lock()
			o.lost = true
			o.mu.Unlock()
			onLost()
			return
		}
	}
}

func (o *receiverOwnership) isLost() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.lost
}

// release stops renewal and drops the lease unless it was lost. A failed
// release leaves the lease to expire.
func (o *receiverOwnership) release() {
	o.stopRenew()
	<-o.renewed
	o.mu.Lock()
	lease, lost := o.lease, o.lost
	o.mu.Unlock()
	if lost {
		return
	}
	ctx := context.Background()
	_ = operations.Transact(ctx, o.e, o.tenant, func(tx store.Tx) error { return tx.ReleaseLease(ctx, lease) })
}
