package otel

import (
	"context"
	"strings"
	"testing"
	"time"

	otelglobal "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/metric/metricdata/metricdatatest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// testAdapter is the adapter type key the test observers declare.
const testAdapter model.TypeKey = "com.example.chat"

// allowed is the attribute allow-list: key to permitted values.
var allowed = map[attribute.Key]map[string]bool{
	AttrStage: {ports.ObservationIngest: true, ports.ObservationCommit: true, ports.ObservationDispatch: true},
	AttrOutcome: {
		string(ports.OutcomeOK): true, string(ports.OutcomeDuplicate): true, string(ports.OutcomeRetry): true,
		string(ports.OutcomeFailed): true, string(ports.OutcomeUnknown): true, OtherValue: true,
	},
	AttrAdapter: {string(testAdapter): true, OtherValue: true},
}

var allKinds = []string{
	ports.ObservationIngest, ports.ObservationCommit, ports.ObservationDispatch,
	ports.ObservationOutcomeUnknown, ports.ObservationRetry,
	ports.ObservationBacklog, ports.ObservationQueueSaturated,
}

type harness struct {
	obs    *Observer
	spans  *tracetest.SpanRecorder
	reader *sdkmetric.ManualReader
	tp     *sdktrace.TracerProvider
	now    time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		spans:  tracetest.NewSpanRecorder(),
		reader: sdkmetric.NewManualReader(),
		now:    time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	h.tp = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(h.spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	t.Cleanup(func() {
		_ = h.tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	o, err := New(WithTracerProvider(h.tp), WithMeterProvider(mp), WithAdapterTypes(testAdapter))
	if err != nil {
		t.Fatal(err)
	}
	o.now = func() time.Time { return h.now }
	h.obs = o
	return h
}

func (h *harness) collect(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	return rm
}

func (h *harness) metric(t *testing.T, name string) metricdata.Metrics {
	t.Helper()
	for _, sm := range h.collect(t).ScopeMetrics {
		if sm.Scope.Name != ScopeName {
			t.Fatalf("scope %q, want %q", sm.Scope.Name, ScopeName)
		}
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %q not recorded", name)
	return metricdata.Metrics{}
}

func obs(kind string, d time.Duration) ports.Observation {
	return ports.Observation{Kind: kind, Correlation: "corr-1", Duration: d}
}

func TestStageSpans(t *testing.T) {
	h := newHarness(t)
	parentCtx, parent := h.tp.Tracer("host").Start(context.Background(), "host")
	for _, k := range []string{ports.ObservationIngest, ports.ObservationCommit, ports.ObservationDispatch} {
		h.obs.Observe(parentCtx, obs(k, 250*time.Millisecond))
	}
	parent.End()

	ended := h.spans.Ended()
	if len(ended) != 4 {
		t.Fatalf("got %d spans, want 4", len(ended))
	}
	for i, k := range []string{ports.ObservationIngest, ports.ObservationCommit, ports.ObservationDispatch} {
		s := ended[i]
		if s.Name() != "uma."+k {
			t.Errorf("span %d name %q, want uma.%s", i, s.Name(), k)
		}
		if s.SpanKind() != trace.SpanKindInternal {
			t.Errorf("%s kind %v, want internal", k, s.SpanKind())
		}
		if !s.EndTime().Equal(h.now) || s.EndTime().Sub(s.StartTime()) != 250*time.Millisecond {
			t.Errorf("%s span %v..%v, want 250ms ending at %v", k, s.StartTime(), s.EndTime(), h.now)
		}
		if s.Parent().SpanID() != parent.SpanContext().SpanID() {
			t.Errorf("%s span not a child of the context span", k)
		}
		if s.InstrumentationScope().Name != ScopeName {
			t.Errorf("%s scope %q", k, s.InstrumentationScope().Name)
		}
		want := []attribute.KeyValue{AttrStage.String(k)}
		if got := s.Attributes(); len(got) != 1 || got[0] != want[0] {
			t.Errorf("%s attributes %v, want %v", k, got, want)
		}
	}
}

func TestStageDurationMetric(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.obs.Observe(ctx, obs(ports.ObservationIngest, 20*time.Millisecond))
	h.obs.Observe(ctx, obs(ports.ObservationIngest, 2*time.Second))
	h.obs.Observe(ctx, obs(ports.ObservationDispatch, 3*time.Millisecond))

	want := metricdata.Metrics{
		Name:        "uma.stage.duration",
		Description: "Duration of an ingest, commit or dispatch unit of work.",
		Unit:        "s",
		Data: metricdata.Histogram[float64]{
			Temporality: metricdata.CumulativeTemporality,
			DataPoints: []metricdata.HistogramDataPoint[float64]{
				{
					Attributes: attribute.NewSet(AttrStage.String(ports.ObservationIngest)),
					Count:      2, Sum: 2.02,
					Bounds:       stageBounds,
					BucketCounts: bucketCounts(0.02, 2),
					Min:          metricdata.NewExtrema(0.02), Max: metricdata.NewExtrema(2.0),
				},
				{
					Attributes: attribute.NewSet(AttrStage.String(ports.ObservationDispatch)),
					Count:      1, Sum: 0.003,
					Bounds:       stageBounds,
					BucketCounts: bucketCounts(0.003),
					Min:          metricdata.NewExtrema(0.003), Max: metricdata.NewExtrema(0.003),
				},
			},
		},
	}
	metricdatatest.AssertEqual(t, want, h.metric(t, "uma.stage.duration"),
		metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())
}

// bucketCounts places vs into stageBounds buckets (upper bound inclusive).
func bucketCounts(vs ...float64) []uint64 {
	c := make([]uint64, len(stageBounds)+1)
	for _, v := range vs {
		i := 0
		for i < len(stageBounds) && v > stageBounds[i] {
			i++
		}
		c[i]++
	}
	return c
}

func TestEventMetrics(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for range 3 {
		h.obs.Observe(ctx, obs(ports.ObservationRetry, time.Second))
	}
	h.obs.Observe(ctx, obs(ports.ObservationOutcomeUnknown, 0))
	h.obs.Observe(ctx, obs(ports.ObservationQueueSaturated, 0))
	h.obs.Observe(ctx, obs(ports.ObservationQueueSaturated, time.Second))
	// Kinds core does not emit are unrecognized, like any other string.
	for _, k := range []string{"receiver_gap", "not-a-kind"} {
		h.obs.Observe(ctx, obs(k, time.Second))
	}

	opts := []metricdatatest.Option{metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars()}
	counters := []struct {
		name, desc, unit string
		value            int64
	}{
		{"uma.retries", "Retried attempts.", "{retry}", 3},
		{"uma.outcomes.unknown", "Operations whose provider outcome is unknown.", "{outcome}", 1},
		{"uma.queue.saturated", "Claim rounds refused because their work kinds were at the in-flight cap.", "{round}", 2},
		{"uma.observer.unrecognized", "Observations of a kind this observer does not map.", "{observation}", 2},
	}
	for _, c := range counters {
		metricdatatest.AssertEqual(t, metricdata.Metrics{
			Name: c.name, Description: c.desc, Unit: c.unit,
			Data: metricdata.Sum[int64]{
				Temporality: metricdata.CumulativeTemporality, IsMonotonic: true,
				DataPoints: []metricdata.DataPoint[int64]{{Value: c.value}},
			},
		}, h.metric(t, c.name), opts...)
	}

	if n := len(h.spans.Ended()); n != 0 {
		t.Errorf("non-stage kinds produced %d spans, want 0", n)
	}
}

// TestBacklogGauge records the latest depth; Duration plays no part.
func TestBacklogGauge(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, depth := range []int64{7, 3} {
		ob := obs(ports.ObservationBacklog, time.Second)
		ob.Value = depth
		h.obs.Observe(ctx, ob)
	}
	metricdatatest.AssertEqual(t, metricdata.Metrics{
		Name: "uma.backlog", Description: "Work items waiting to be claimed, as of the latest claim round.", Unit: "{item}",
		Data: metricdata.Gauge[int64]{DataPoints: []metricdata.DataPoint[int64]{{Value: 3}}},
	}, h.metric(t, "uma.backlog"), metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())
	if n := len(h.spans.Ended()); n != 0 {
		t.Errorf("backlog produced %d spans, want 0", n)
	}
}

func TestNegativeDurationRecordsZero(t *testing.T) {
	h := newHarness(t)
	h.obs.Observe(context.Background(), obs(ports.ObservationCommit, -time.Second))
	s := h.spans.Ended()[0]
	if !s.StartTime().Equal(h.now) || !s.EndTime().Equal(h.now) {
		t.Errorf("span %v..%v, want zero length at %v", s.StartTime(), s.EndTime(), h.now)
	}
	hist := h.metric(t, "uma.stage.duration").Data.(metricdata.Histogram[float64])
	if dp := hist.DataPoints[0]; dp.Sum != 0 || dp.Count != 1 {
		t.Errorf("sum %v count %d, want 0 and 1", dp.Sum, dp.Count)
	}
}

// TestAttributeAllowList feeds every kind, plus unrecognized ones, with
// identifying content in every observation field, and checks that only
// allow-listed attributes come out and the content appears nowhere.
func TestAttributeAllowList(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const secretCorr = "corr-alice@example.com-token-s3cr3t"
	const secretKind = "dispatch:alice@example.com:Bearer s3cr3t"
	const secretOutcome ports.Outcome = "failed:alice@example.com:Bearer s3cr3t"
	const secretAdapter model.TypeKey = "com.example.chat/alice@example.com/s3cr3t"
	sentinels := []string{"alice", "example.com", "s3cr3t", "Bearer", "corr-"}
	fields := []struct {
		outcome ports.Outcome
		adapter model.TypeKey
	}{
		{secretOutcome, secretAdapter},
		{ports.OutcomeOK, testAdapter},
		{"", ""},
	}
	for _, f := range fields {
		for _, k := range append(allKinds, secretKind, "") {
			h.obs.Observe(ctx, ports.Observation{
				Kind:        k,
				Correlation: model.CorrelationID(secretCorr),
				Duration:    time.Second,
				Outcome:     f.outcome,
				Adapter:     f.adapter,
			})
		}
	}

	check := func(where string, kvs []attribute.KeyValue) {
		t.Helper()
		for _, kv := range kvs {
			vals, ok := allowed[kv.Key]
			if !ok {
				t.Errorf("%s: attribute %q not in allow-list", where, kv.Key)
				continue
			}
			if kv.Value.Type() != attribute.STRING || !vals[kv.Value.AsString()] {
				t.Errorf("%s: attribute %s=%q value not allowed", where, kv.Key, kv.Value.String())
			}
		}
	}
	noSentinel := func(where, s string) {
		t.Helper()
		for _, x := range sentinels {
			if strings.Contains(s, x) {
				t.Errorf("%s: %q leaks %q", where, s, x)
			}
		}
	}

	spans := h.spans.Ended()
	if len(spans) != 3*len(fields) {
		t.Fatalf("got %d spans, want %d", len(spans), 3*len(fields))
	}
	for _, s := range spans {
		noSentinel("span name", s.Name())
		check("span "+s.Name(), s.Attributes())
		for _, e := range s.Events() {
			noSentinel("span event", e.Name)
			check("span event "+e.Name, e.Attributes)
		}
		if len(s.Links()) != 0 {
			t.Errorf("span %s has links", s.Name())
		}
		noSentinel("span status", s.Status().Description)
		for _, kv := range s.Attributes() {
			noSentinel("span attribute", kv.Value.String())
		}
	}

	rm := h.collect(t)
	var points int
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			noSentinel("metric name", m.Name)
			noSentinel("metric description", m.Description)
			for _, set := range pointAttributes(t, m) {
				points++
				check("metric "+m.Name, set.ToSlice())
				noSentinel("metric attributes", set.Encoded(attribute.DefaultEncoder()))
			}
		}
	}
	if points == 0 {
		t.Fatal("no metric data points collected")
	}
}

func pointAttributes(t *testing.T, m metricdata.Metrics) []attribute.Set {
	t.Helper()
	var out []attribute.Set
	switch d := m.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	case metricdata.Gauge[int64]:
		for _, p := range d.DataPoints {
			out = append(out, p.Attributes)
		}
	default:
		t.Fatalf("metric %s: unexpected aggregation %T", m.Name, m.Data)
	}
	return out
}

// TestNoGlobalProviders checks that an observer built without options stays
// a no-op even when the process has global providers installed.
func TestNoGlobalProviders(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	prevTP, prevMP := otelglobal.GetTracerProvider(), otelglobal.GetMeterProvider()
	otelglobal.SetTracerProvider(tp)
	otelglobal.SetMeterProvider(mp)
	t.Cleanup(func() {
		otelglobal.SetTracerProvider(prevTP)
		otelglobal.SetMeterProvider(prevMP)
		_ = tp.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})

	o, err := New(WithTracerProvider(nil), WithMeterProvider(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range allKinds {
		o.Observe(context.Background(), obs(k, time.Second))
	}
	if n := len(spans.Ended()); n != 0 {
		t.Errorf("default observer recorded %d spans on the global provider", n)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	if len(rm.ScopeMetrics) != 0 {
		t.Errorf("default observer recorded metrics on the global provider: %+v", rm.ScopeMetrics)
	}
}

func TestConcurrentObserve(t *testing.T) {
	h := newHarness(t)
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for _, k := range allKinds {
				h.obs.Observe(context.Background(), obs(k, time.Millisecond))
			}
		}()
	}
	for range 8 {
		<-done
	}
	sum := h.metric(t, "uma.retries").Data.(metricdata.Sum[int64])
	if v := sum.DataPoints[0].Value; v != 8 {
		t.Errorf("retries %d, want 8", v)
	}
}

// TestOutcomeAndAdapterAttributes checks the closed value sets: declared
// values pass through, others collapse to OtherValue, empty ones are omitted.
func TestOutcomeAndAdapterAttributes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.obs.Observe(ctx, ports.Observation{Kind: ports.ObservationDispatch, Outcome: ports.OutcomeOK, Adapter: testAdapter})
	h.obs.Observe(ctx, ports.Observation{Kind: ports.ObservationDispatch, Outcome: "bogus", Adapter: "com.example.unlisted"})
	h.obs.Observe(ctx, ports.Observation{Kind: ports.ObservationRetry, Adapter: testAdapter})
	h.obs.Observe(ctx, ports.Observation{Kind: ports.ObservationRetry})

	stage := func(o, a string) attribute.Set {
		return attribute.NewSet(AttrStage.String(ports.ObservationDispatch), AttrOutcome.String(o), AttrAdapter.String(a))
	}
	spans := h.spans.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	for i, want := range []attribute.Set{stage("ok", string(testAdapter)), stage(OtherValue, OtherValue)} {
		if got := attribute.NewSet(spans[i].Attributes()...); !got.Equals(&want) {
			t.Errorf("span %d attributes %v, want %v", i, got.ToSlice(), want.ToSlice())
		}
	}
	hist := h.metric(t, "uma.stage.duration").Data.(metricdata.Histogram[float64])
	var sets []attribute.Set
	for _, p := range hist.DataPoints {
		sets = append(sets, p.Attributes)
	}
	for _, want := range []attribute.Set{stage("ok", string(testAdapter)), stage(OtherValue, OtherValue)} {
		found := false
		for _, s := range sets {
			found = found || s.Equals(&want)
		}
		if !found {
			t.Errorf("stage duration has no point with %v; got %v", want.ToSlice(), sets)
		}
	}
	metricdatatest.AssertEqual(t, metricdata.Metrics{
		Name: "uma.retries", Description: "Retried attempts.", Unit: "{retry}",
		Data: metricdata.Sum[int64]{
			Temporality: metricdata.CumulativeTemporality, IsMonotonic: true,
			DataPoints: []metricdata.DataPoint[int64]{
				{Attributes: attribute.NewSet(AttrAdapter.String(string(testAdapter))), Value: 1},
				{Attributes: attribute.NewSet(), Value: 1},
			},
		},
	}, h.metric(t, "uma.retries"), metricdatatest.IgnoreTimestamp(), metricdatatest.IgnoreExemplars())
}
