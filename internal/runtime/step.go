package runtime

import (
	"context"

	"hop.top/uma/internal/engine"
)

// Step runs one claim round in the caller's goroutine: it claims up to
// opts.Limit items of opts.Kinds for the caller's tenant under opts.Owner
// and a lease of opts.Lease, executes each to its next durable state, and
// returns how many it claimed. It never waits: an empty round returns 0 and
// opts.Poll is unused. The caller owns the loop, the pacing and the clock,
// so tests and recovery tools drive work without a background Run.
//
// A round the work policy's in-flight cap left empty fails with
// model.Unavailable, as a claim does; a claim lost to concurrent claims
// fails with model.Transient and the caller steps again. Step is subject to
// Drain and Close like Run: a drained tenant claims nothing and Drain waits
// for a Step in flight.
func (rt *Runtime) Step(ctx context.Context, call Call, opts WorkerOptions) (int, error) {
	e := rt.e
	if e.IsClosed() {
		return 0, engine.ErrClosed()
	}
	if err := call.Principal.Validate(); err != nil {
		return 0, err
	}
	opts, err := workerOptions(opts)
	if err != nil {
		return 0, err
	}
	tenant := call.Principal.Tenant
	// Registered like a Run, so Drain waits for this call; there is no idle
	// wait for a drain to cancel, so the stop function does nothing.
	h, err := rt.run.register(tenant, func() {})
	if err != nil {
		return 0, err
	}
	defer rt.run.unregister(h)
	if e.IsClosed() {
		return 0, engine.ErrClosed()
	}
	if h.isHalted() {
		return 0, errDraining()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	works, err := rt.ClaimWork(ctx, tenant, opts)
	if err != nil {
		return 0, err
	}
	for i, w := range works {
		if err := rt.ExecuteClaimed(ctx, tenant, w); err != nil {
			return i, err
		}
	}
	return len(works), nil
}
