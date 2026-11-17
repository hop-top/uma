package runtime

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Backpressure: every claim round is bounded by the engine's work policy.
// The store caps each kind's claimed items at Work.MaxInFlight across all
// workers; a round the cap left empty fails with model.Unavailable and is
// observed as queue_saturated, so saturation is never a silent empty
// round. Run waits out a saturated round like an idle one. With an
// Observer, each round also reports the waiting depth of its kinds as
// backlog.

// saturated reports whether err is a claim round refused at the in-flight
// cap.
func saturated(err error) bool { return model.IsCode(err, model.Unavailable) }

// observeRound reports one claim round: saturation when err says so, and
// the backlog of kinds, read in its own view after the round.
func (rt *Runtime) observeRound(ctx context.Context, tenant model.TenantID, kinds []string, err error) {
	e := rt.e
	if !e.Observing() {
		return
	}
	if saturated(err) {
		e.Observe(ctx, ports.Observation{Kind: ports.ObservationQueueSaturated})
	}
	var waiting int64
	viewErr := e.Store().View(ctx, tenant, func(r store.Reader) error {
		for _, k := range kinds {
			d, err := r.WorkDepth(ctx, k)
			if err != nil {
				return err
			}
			waiting += int64(d.Pending)
		}
		return nil
	})
	if viewErr == nil {
		e.Observe(ctx, ports.Observation{Kind: ports.ObservationBacklog, Value: waiting})
	}
}
