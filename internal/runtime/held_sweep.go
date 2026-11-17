package runtime

import (
	"context"
	"time"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// SweepHeld runs one expiry sweep of the tenant's held operations: those
// past their deadline become expired, those still held after window are
// marked carried over. See operations.ExpireHeld. Run does this on its own
// cadence (WorkerOptions.SweepEvery); a caller that owns its loop, such as
// one driving Step, calls it directly.
func (rt *Runtime) SweepHeld(ctx context.Context, tenant model.TenantID, window time.Duration) (operations.ExpireReport, error) {
	return operations.ExpireHeld(ctx, rt.e, tenant, window)
}

// SweepSnoozes runs one wake sweep of the tenant's conversations: a snooze
// that ended (Until passed, or a live inbound message after it was set) is
// cleared and announced with conversation.snooze_ended. See operations.WakeSnoozes.
// Run does this beside SweepHeld, on the same cadence.
func (rt *Runtime) SweepSnoozes(ctx context.Context, tenant model.TenantID) (operations.WakeReport, error) {
	return operations.WakeSnoozes(ctx, rt.e, tenant)
}

// sweepIfDue runs both sweeps when every has passed since *at on the engine clock,
// the same clock a decision judges deadlines by, and moves *at on. every < 0
// never sweeps. A sweep that fails to commit for contention (another
// replica won) is retried at the next cadence, never fails the run.
func (rt *Runtime) sweepIfDue(ctx context.Context, tenant model.TenantID, every time.Duration, at *time.Time) error {
	if every < 0 {
		return nil
	}
	now := rt.e.Clock().Now()
	if !at.IsZero() && now.Before(at.Add(every)) {
		return nil
	}
	*at = now
	_, err := rt.SweepHeld(ctx, tenant, every)
	if err == nil {
		_, err = rt.SweepSnoozes(ctx, tenant)
	}
	if model.IsCode(err, model.Transient) || model.IsCode(err, model.Conflict) {
		return nil
	}
	return err
}
