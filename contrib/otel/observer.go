package otel

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// ScopeName is the instrumentation scope of every tracer and meter this
// package creates.
const ScopeName = "hop.top/uma/contrib/otel"

// Attribute keys, the only ones the observer emits. Each has a closed value
// set; see the package documentation.
const (
	// AttrStage is the stage of a stage duration: [ports.ObservationIngest],
	// [ports.ObservationCommit] or [ports.ObservationDispatch].
	AttrStage = attribute.Key("uma.stage")
	// AttrOutcome is [ports.Observation.Outcome]: one of the ports Outcome
	// constants, or [OtherValue].
	AttrOutcome = attribute.Key("uma.outcome")
	// AttrAdapter is [ports.Observation.Adapter]: a type key passed to
	// [WithAdapterTypes], or [OtherValue].
	AttrAdapter = attribute.Key("uma.adapter")
)

// OtherValue replaces an outcome or adapter value outside its closed set.
const OtherValue = "other"

// outcomes is the closed AttrOutcome value set, [OtherValue] aside.
var outcomes = map[ports.Outcome]string{
	ports.OutcomeOK:        string(ports.OutcomeOK),
	ports.OutcomeDuplicate: string(ports.OutcomeDuplicate),
	ports.OutcomeRetry:     string(ports.OutcomeRetry),
	ports.OutcomeFailed:    string(ports.OutcomeFailed),
	ports.OutcomeUnknown:   string(ports.OutcomeUnknown),
}

// stageBounds are histogram bucket boundaries in seconds.
var stageBounds = []float64{0.005, 0.01, 0.025, 0.05, 0.075, 0.1, 0.25, 0.5, 0.75, 1, 2.5, 5, 7.5, 10, 30, 60}

// stage holds the precomputed span name and stage attribute of a stage kind.
type stage struct {
	span string
	attr attribute.KeyValue
}

func newStage(kind string) stage {
	return stage{span: "uma." + kind, attr: AttrStage.String(kind)}
}

var stages = map[string]stage{
	ports.ObservationIngest:   newStage(ports.ObservationIngest),
	ports.ObservationCommit:   newStage(ports.ObservationCommit),
	ports.ObservationDispatch: newStage(ports.ObservationDispatch),
}

// Option configures [New].
type Option func(*config)

type config struct {
	tp       trace.TracerProvider
	mp       metric.MeterProvider
	adapters map[model.TypeKey]string
}

// WithTracerProvider sets the provider of the observer's tracer. Nil keeps
// the no-op default.
func WithTracerProvider(tp trace.TracerProvider) Option {
	return func(c *config) {
		if tp != nil {
			c.tp = tp
		}
	}
}

// WithMeterProvider sets the provider of the observer's meter. Nil keeps the
// no-op default.
func WithMeterProvider(mp metric.MeterProvider) Option {
	return func(c *config) {
		if mp != nil {
			c.mp = mp
		}
	}
}

// WithAdapterTypes declares the adapter type keys AttrAdapter may carry,
// typically every registered factory's descriptor type. Any other non-empty
// [ports.Observation.Adapter] records as [OtherValue]. Repeated options add
// to the set.
func WithAdapterTypes(keys ...model.TypeKey) Option {
	return func(c *config) {
		for _, k := range keys {
			if k != "" && k != OtherValue {
				c.adapters[k] = string(k)
			}
		}
	}
}

// Observer records observations as spans and metrics. It is safe for
// concurrent use and owns no resources: the host shuts its providers down.
type Observer struct {
	tracer       trace.Tracer
	adapters     map[model.TypeKey]string
	stage        metric.Float64Histogram
	unknown      metric.Int64Counter
	retries      metric.Int64Counter
	saturated    metric.Int64Counter
	backlog      metric.Int64Gauge
	unrecognized metric.Int64Counter
	now          func() time.Time
}

var _ ports.Observer = (*Observer)(nil)

// New returns an observer using the given providers. It fails only when the
// meter provider rejects an instrument.
func New(opts ...Option) (*Observer, error) {
	c := config{tp: tracenoop.NewTracerProvider(), mp: metricnoop.NewMeterProvider(), adapters: map[model.TypeKey]string{}}
	for _, opt := range opts {
		if opt != nil {
			opt(&c)
		}
	}
	m := c.mp.Meter(ScopeName)
	o := &Observer{tracer: c.tp.Tracer(ScopeName), adapters: c.adapters, now: time.Now}
	var err error
	if o.stage, err = m.Float64Histogram("uma.stage.duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of an ingest, commit or dispatch unit of work."),
		metric.WithExplicitBucketBoundaries(stageBounds...)); err != nil {
		return nil, err
	}
	if o.unknown, err = m.Int64Counter("uma.outcomes.unknown",
		metric.WithUnit("{outcome}"),
		metric.WithDescription("Operations whose provider outcome is unknown.")); err != nil {
		return nil, err
	}
	if o.retries, err = m.Int64Counter("uma.retries",
		metric.WithUnit("{retry}"),
		metric.WithDescription("Retried attempts.")); err != nil {
		return nil, err
	}
	if o.saturated, err = m.Int64Counter("uma.queue.saturated",
		metric.WithUnit("{round}"),
		metric.WithDescription("Claim rounds refused because their work kinds were at the in-flight cap.")); err != nil {
		return nil, err
	}
	if o.backlog, err = m.Int64Gauge("uma.backlog",
		metric.WithUnit("{item}"),
		metric.WithDescription("Work items waiting to be claimed, as of the latest claim round.")); err != nil {
		return nil, err
	}
	if o.unrecognized, err = m.Int64Counter("uma.observer.unrecognized",
		metric.WithUnit("{observation}"),
		metric.WithDescription("Observations of a kind this observer does not map.")); err != nil {
		return nil, err
	}
	return o, nil
}

// Observe records ob. It reads Kind, Duration, Value, Outcome and Adapter;
// Outcome and Adapter only through their closed value sets.
func (o *Observer) Observe(ctx context.Context, ob ports.Observation) {
	d := max(ob.Duration, 0)
	s, isStage := stages[ob.Kind]
	if !isStage {
		if _, known := kinds[ob.Kind]; !known {
			o.unrecognized.Add(ctx, 1)
			return
		}
	}
	kv := o.attributes(ob, s, isStage)
	attrs := metric.WithAttributes(kv...)
	switch ob.Kind {
	case ports.ObservationIngest, ports.ObservationCommit, ports.ObservationDispatch:
		o.stage.Record(ctx, d.Seconds(), attrs)
		end := o.now()
		_, span := o.tracer.Start(ctx, s.span,
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithTimestamp(end.Add(-d)),
			trace.WithAttributes(kv...))
		span.End(trace.WithTimestamp(end))
	case ports.ObservationOutcomeUnknown:
		o.unknown.Add(ctx, 1, attrs)
	case ports.ObservationRetry:
		o.retries.Add(ctx, 1, attrs)
	case ports.ObservationQueueSaturated:
		o.saturated.Add(ctx, 1, attrs)
	case ports.ObservationBacklog:
		o.backlog.Record(ctx, max(ob.Value, 0), attrs)
	}
}

// kinds are the recognized observation kinds.
var kinds = map[string]struct{}{
	ports.ObservationIngest: {}, ports.ObservationCommit: {}, ports.ObservationDispatch: {},
	ports.ObservationOutcomeUnknown: {}, ports.ObservationRetry: {},
	ports.ObservationQueueSaturated: {}, ports.ObservationBacklog: {},
}

// attributes maps ob onto the allow-listed keys. Values come from the
// closed sets, never from ob: an unlisted outcome or adapter becomes
// [OtherValue] and an empty one is omitted.
func (o *Observer) attributes(ob ports.Observation, s stage, isStage bool) []attribute.KeyValue {
	kv := make([]attribute.KeyValue, 0, 3)
	if isStage {
		kv = append(kv, s.attr)
	}
	if ob.Outcome != "" {
		v, ok := outcomes[ob.Outcome]
		if !ok {
			v = OtherValue
		}
		kv = append(kv, AttrOutcome.String(v))
	}
	if ob.Adapter != "" {
		v, ok := o.adapters[ob.Adapter]
		if !ok {
			v = OtherValue
		}
		kv = append(kv, AttrAdapter.String(v))
	}
	return kv
}
