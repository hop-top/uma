// Package otel provides a [ports.Observer] that records uma observations as
// OpenTelemetry spans and metrics.
//
// It depends on the OpenTelemetry API only. The host builds the SDK, chooses
// exporters and passes its providers with [WithTracerProvider] and
// [WithMeterProvider]. The package never reads the global providers, starts
// nothing and exports nothing: without options every instrument is a no-op.
//
// # Kinds and instruments
//
// [ports.Observation.Kind], one of the ports Observation* constants, selects
// the instrument; [ports.Observation.Duration] or [ports.Observation.Value]
// is the recorded value where the table says so. Negative durations and
// values record as zero.
//
//	Kind              Instrument                  Type             Unit           Value     Span
//	ingest            uma.stage.duration          float64 histo.   s              Duration  uma.ingest
//	commit            uma.stage.duration          float64 histo.   s              Duration  uma.commit
//	dispatch          uma.stage.duration          float64 histo.   s              Duration  uma.dispatch
//	outcome_unknown   uma.outcomes.unknown        int64 counter    {outcome}      1         none
//	retry             uma.retries                 int64 counter    {retry}        1         none
//	queue_saturated   uma.queue.saturated         int64 counter    {round}        1         none
//	backlog           uma.backlog                 int64 gauge      {item}         Value     none
//	(any other)       uma.observer.unrecognized   int64 counter    {observation}  1         none
//
// Stage durations measure one unit of work: ingest (one Sink.Accept call),
// commit (one ingress store transaction attempt) and dispatch (one outbound
// provider call). Each also produces an internal span, named as in the
// table, that ends when Observe is called and starts Duration earlier, as a
// child of any span in the context.
//
// # Attributes
//
// Only three keys exist, each with a closed value set:
//
//	Key          Values                                       Recorded on
//	uma.stage    ingest, commit, dispatch                     stage durations and spans
//	uma.outcome  ok, duplicate, retry, failed, unknown, other  every recognized kind
//	uma.adapter  the WithAdapterTypes keys, other             every recognized kind
//
// uma.outcome and uma.adapter are omitted when the observation leaves the
// field empty. A value outside the set records as "other", so a host
// declares its registered adapter type keys with [WithAdapterTypes] to see
// them; without it every adapter is "other". Values always come from the
// sets, never from the observation. The correlation ID, unrecognized kind
// strings, identities, message content and credentials are never recorded,
// and an unrecognized kind carries no attributes.
package otel
