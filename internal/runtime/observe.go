package runtime

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// observeDispatch reports one provider call that started at start and was
// classified as a, preceded by a retry when an earlier attempt exists.
func (x *execution) observeDispatch(ctx context.Context, start time.Time, a answer) {
	if !x.e.Observing() {
		return
	}
	if x.hist.Attempts > 0 {
		x.e.Observe(ctx, ports.Observation{Kind: ports.ObservationRetry, Adapter: x.channel.Type})
	}
	outcome := ports.OutcomeUnknown
	switch a.verdict {
	case verdictApplied:
		outcome = ports.OutcomeOK
	case verdictRetry:
		outcome = ports.OutcomeRetry
	case verdictFinal:
		outcome = ports.OutcomeFailed
	}
	x.e.Observe(ctx, ports.Observation{
		Kind: ports.ObservationDispatch, Duration: x.e.Clock().Now().Sub(start),
		Outcome: outcome, Adapter: x.channel.Type,
	})
}

// observeState reports an operation that a committed unit moved from prev
// into outcome_unknown.
func (x *execution) observeState(ctx context.Context, prev, next model.DeliveryState) {
	if next == model.Unknown && prev != model.Unknown {
		x.e.Observe(ctx, ports.Observation{Kind: ports.ObservationOutcomeUnknown, Adapter: x.channel.Type})
	}
}
