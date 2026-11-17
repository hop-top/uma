package ports

import (
	"context"
	"time"

	"hop.top/uma/model"
)

// Observation kinds. Each names one operational health signal the engine
// emits.
const (
	// ObservationIngest is one Sink.Accept call: Duration from the call to
	// its result, Outcome OutcomeOK, OutcomeDuplicate or OutcomeFailed.
	ObservationIngest = "ingest"
	// ObservationCommit is one ingress store transaction attempt: Duration
	// of the unit, Outcome OutcomeOK, OutcomeRetry (a serialization conflict
	// the engine may retry) or OutcomeFailed.
	ObservationCommit = "commit"
	// ObservationDispatch is one outbound provider call: Duration of the
	// call, Outcome OutcomeOK (applied), OutcomeRetry (a retryable failure,
	// even when the retry budget is spent), OutcomeFailed or OutcomeUnknown.
	ObservationDispatch = "dispatch"
	// ObservationRetry is one provider attempt made after an earlier attempt
	// of the same operation. It counts occurrences.
	ObservationRetry = "retry"
	// ObservationOutcomeUnknown is one operation entering the
	// outcome_unknown state. It counts occurrences.
	ObservationOutcomeUnknown = "outcome_unknown"
	// ObservationBacklog is the depth of the work queue a Runtime.Run
	// claim round reads: Value counts the work items waiting to be claimed,
	// deferred retries included, as of that round. It is a gauge.
	ObservationBacklog = "backlog"
	// ObservationQueueSaturated is one Runtime.Run claim round refused
	// because every claimable item's work kind was at its in-flight cap. It
	// counts occurrences.
	ObservationQueueSaturated = "queue_saturated"
)

// Outcome is the closed, low-cardinality result of an observed unit of
// work. The zero value means the kind carries no outcome.
type Outcome string

// Observation outcomes.
const (
	// OutcomeOK: the unit completed; for dispatch, the provider applied the
	// effect.
	OutcomeOK Outcome = "ok"
	// OutcomeDuplicate: the ingress event identity was already claimed.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeRetry: the unit failed and may be retried.
	OutcomeRetry Outcome = "retry"
	// OutcomeFailed: the unit failed and is not retried.
	OutcomeFailed Outcome = "failed"
	// OutcomeUnknown: whether the provider applied the effect is unknown.
	OutcomeUnknown Outcome = "unknown"
)

// Observation is one operational health signal. It never carries resource
// or provider identities, message content or credentials: every field but
// Correlation is a closed or bounded value or a count, and Correlation is the caller's
// diagnostic token, which observers must not treat as a metric dimension.
type Observation struct {
	// Kind is one of the Observation* constants; observers count any other
	// value as unrecognized.
	Kind        string
	Correlation model.CorrelationID
	// Duration is the length of the observed unit; zero for kinds that only
	// count occurrences.
	Duration time.Duration
	// Outcome is the unit's result, for the kinds that define one.
	Outcome Outcome
	// Adapter is the registered adapter type key of the channel involved,
	// when there is one. Its values are bounded by the host's registry.
	Adapter model.TypeKey
	// Value is the measured quantity of gauge kinds, such as a queue depth;
	// zero for the other kinds.
	Value int64
}

// Observer receives observations. The engine calls Observe synchronously on
// its hot paths, possibly from several goroutines at once, so an
// implementation must be safe for concurrent use, return quickly and never
// block: buffer or drop rather than wait on I/O. It must not retain the
// context beyond the call. A nil Observer disables observation.
type Observer interface {
	Observe(context.Context, Observation)
}
