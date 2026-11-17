package ingress

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// observeIngest reports one Accept call that started at start, with its
// final result read through r and err.
func (s *Sink) observeIngest(ctx context.Context, corr model.CorrelationID, start time.Time, r *model.IngressReceipt, err *error) {
	if !s.e.Observing() {
		return
	}
	outcome := ports.OutcomeOK
	switch {
	case *err != nil:
		outcome = ports.OutcomeFailed
	case r.Duplicate:
		outcome = ports.OutcomeDuplicate
	}
	s.e.Observe(ctx, ports.Observation{
		Kind: ports.ObservationIngest, Correlation: corr, Duration: s.e.Clock().Now().Sub(start),
		Outcome: outcome, Adapter: s.channel.Type,
	})
}

// observeCommit reports one ingress store unit attempt that started at start.
func (s *Sink) observeCommit(ctx context.Context, corr model.CorrelationID, start time.Time, err error) {
	if !s.e.Observing() {
		return
	}
	outcome := ports.OutcomeOK
	switch {
	case model.IsCode(err, model.Transient):
		outcome = ports.OutcomeRetry
	case err != nil:
		outcome = ports.OutcomeFailed
	}
	s.e.Observe(ctx, ports.Observation{
		Kind: ports.ObservationCommit, Correlation: corr, Duration: s.e.Clock().Now().Sub(start),
		Outcome: outcome, Adapter: s.channel.Type,
	})
}
