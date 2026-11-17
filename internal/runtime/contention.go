package runtime

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Losing to concurrent units. A store that serializes units aborts one of
// two conflicting ones with model.Transient. Under contention a unit can
// lose every rerun operations.Transact gives it, which a caller-facing
// command reports to its caller. Work claimed by a worker has no caller to
// retry it: the worker holds the item's lease, and nobody else can run the
// item until that lease expires. So the worker's own units are rerun while
// its lease lasts, and a lost claim round is claimed again at once instead
// of waiting a poll interval with work still queued.

// claimedUnitAttempts bounds how often a unit of claimed work runs while
// it keeps losing to concurrent units.
const claimedUnitAttempts = 16

// Pause bounds between reruns of a lost unit or claim round: full jitter
// over lostPauseBase doubled per consecutive loss, at most lostPauseMax
// for a unit of claimed work and the poll interval for a claim round.
const (
	lostPauseBase = time.Millisecond
	lostPauseMax  = 500 * time.Millisecond
)

// pause waits before the rerun after n consecutive losses, at most
// ceiling, through a stdlib timer: it spaces reruns apart and is not an
// idle wait.
func (rt *Runtime) pause(ctx context.Context, n int, ceiling time.Duration) error {
	d := min(lostPauseBase<<min(n, 20), ceiling)
	return timerWaiter{}.Wait(ctx, rt.run.jitterFunc()(d))
}

// transactClaimed runs fn, a unit of claimed item w, and reruns it while it
// loses to concurrent units, w's lease is live and the attempts last. fn
// must be free of side effects outside the unit.
func (rt *Runtime) transactClaimed(ctx context.Context, tenant model.TenantID, w store.Work, fn func(store.Tx) error) error {
	for n := 0; ; n++ {
		err := rt.e.Store().Transact(ctx, tenant, fn)
		switch {
		case !model.IsCode(err, model.Transient), ctx.Err() != nil, n+1 >= claimedUnitAttempts:
			return err
		case !w.Until.IsZero() && !rt.e.Clock().Now().Before(w.Until):
			// The lease is gone: another owner may hold the item now.
			return err
		}
		if err := rt.pause(ctx, n, lostPauseMax); err != nil {
			return err
		}
	}
}
