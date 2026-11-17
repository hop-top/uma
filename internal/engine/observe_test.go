package engine_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// recorder is a ports.Observer that keeps every observation.
type recorder struct {
	mu  sync.Mutex
	obs []ports.Observation
}

func (r *recorder) Observe(_ context.Context, o ports.Observation) {
	r.mu.Lock()
	r.obs = append(r.obs, o)
	r.mu.Unlock()
}

// take returns and clears the recorded observations.
func (r *recorder) take() []ports.Observation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.obs
	r.obs = nil
	return out
}

// stepClock advances by one millisecond on every reading, so every
// measured duration is positive.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(time.Millisecond)
	return c.now
}

// flakyStore fails the next n store units with a transient conflict.
type flakyStore struct {
	store.Store
	mu sync.Mutex
	n  int
}

func (s *flakyStore) fail(n int) {
	s.mu.Lock()
	s.n = n
	s.mu.Unlock()
}

func (s *flakyStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	s.mu.Lock()
	fail := s.n > 0
	if fail {
		s.n--
	}
	s.mu.Unlock()
	if fail {
		return model.NewError(model.Transient, "serialization conflict")
	}
	return s.Store.Transact(ctx, tenant, fn)
}

// closedOutcomes is the Outcome value set, the zero value included.
var closedOutcomes = []ports.Outcome{
	"", ports.OutcomeOK, ports.OutcomeDuplicate, ports.OutcomeRetry, ports.OutcomeFailed, ports.OutcomeUnknown,
}

type wantObs struct {
	kind    string
	outcome ports.Outcome
}

// requireObservations checks got against want in order. Every observation
// must name the adapter type key typ and carry only closed values: no
// identity, content or credential reaches a field.
func requireObservations(t *testing.T, got []ports.Observation, typ model.TypeKey, corr model.CorrelationID, want []wantObs) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("observations = %+v, want %d: %+v", got, len(want), want)
	}
	for i, o := range got {
		if o.Kind != want[i].kind || o.Outcome != want[i].outcome {
			t.Errorf("observation %d = %s/%q, want %s/%q", i, o.Kind, o.Outcome, want[i].kind, want[i].outcome)
		}
		if o.Adapter != typ {
			t.Errorf("observation %d adapter = %q, want %q", i, o.Adapter, typ)
		}
		if !slices.Contains(closedOutcomes, o.Outcome) {
			t.Errorf("observation %d outcome %q outside the closed set", i, o.Outcome)
		}
		if o.Correlation != corr {
			t.Errorf("observation %d correlation = %q, want %q", i, o.Correlation, corr)
		}
		if o.Duration < 0 {
			t.Errorf("observation %d duration = %v", i, o.Duration)
		}
	}
}

// Ingress reports each store unit attempt as commit and each Accept call as
// ingest, both timed on the engine clock, with the channel's adapter type.
func TestIngressObservations(t *testing.T) {
	rec := &recorder{}
	fs := &flakyStore{}
	h := enginetest.NewHarness(t, enginetest.ChatType, func(c *engine.Config) {
		c.Observer = rec
		c.Clock = &stepClock{now: enginetest.T0}
		fs.Store = c.Store
		c.Store = fs
	})
	h.Configure(enginetest.PerContact(false), enginetest.Dest)
	rec.take()

	env := h.Inbound("e1", "m1", "u1", enginetest.Dest)
	fs.fail(1)
	if r := h.MustAccept(env); !r.Committed {
		t.Fatalf("receipt = %+v", r)
	}
	if r := h.MustAccept(env); !r.Duplicate {
		t.Fatalf("redelivery receipt = %+v, want duplicate", r)
	}
	bad := env
	bad.Destination = ""
	if _, err := h.Accept(bad); !model.IsCode(err, model.Validation) {
		t.Fatalf("Accept without destination: %v", err)
	}

	got := rec.take()
	requireObservations(t, got, enginetest.ChatType, "corr-e1", []wantObs{
		{ports.ObservationCommit, ports.OutcomeRetry},
		{ports.ObservationCommit, ports.OutcomeOK},
		{ports.ObservationIngest, ports.OutcomeOK},
		{ports.ObservationCommit, ports.OutcomeOK},
		{ports.ObservationIngest, ports.OutcomeDuplicate},
		{ports.ObservationIngest, ports.OutcomeFailed},
	})
	for i, o := range got {
		if o.Duration <= 0 {
			t.Errorf("observation %d (%s) duration = %v, want measured", i, o.Kind, o.Duration)
		}
	}
	if ingest, commits := got[2].Duration, got[0].Duration+got[1].Duration; ingest < commits {
		t.Errorf("ingest %v shorter than its commits %v", ingest, commits)
	}
}

// Runtime reports every provider call as dispatch, timed on the engine
// clock, each later attempt of an operation as retry and each operation
// entering outcome_unknown once.
func TestRuntimeObservations(t *testing.T) {
	rec := &recorder{}
	r := enginetest.NewRig(t, func(c *engine.Config, _ *enginetest.OutFactory) { c.Observer = rec })
	const call = 250 * time.Millisecond
	slow := func(fn enginetest.SendFunc) enginetest.SendFunc {
		return func(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error, bool) {
			r.Clock.Advance(call)
			return fn(ctx, req)
		}
	}
	permanent := func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{}, model.NewError(model.Permanent, "refused"), false
	}

	r.F.Respond(slow(enginetest.TransientFailure), slow(enginetest.Accepted))
	r.Reply("op-1", "hello")
	withoutBacklog(rec.take())
	r.Step("w")
	requireObservations(t, withoutBacklog(rec.take()), enginetest.OutKey, "", []wantObs{{ports.ObservationDispatch, ports.OutcomeRetry}})
	r.Clock.Advance(time.Minute)
	r.Step("w")
	requireObservations(t, withoutBacklog(rec.take()), enginetest.OutKey, "", []wantObs{
		{ports.ObservationRetry, ""},
		{ports.ObservationDispatch, ports.OutcomeOK},
	})

	r.F.Respond(slow(enginetest.LostAnswer))
	r.Reply("op-2", "hello")
	withoutBacklog(rec.take())
	r.Step("w")
	requireObservations(t, withoutBacklog(rec.take()), enginetest.OutKey, "", []wantObs{
		{ports.ObservationDispatch, ports.OutcomeUnknown},
		{ports.ObservationOutcomeUnknown, ""},
	})

	r.F.Respond(slow(permanent))
	r.Reply("op-3", "hello")
	withoutBacklog(rec.take())
	r.Step("w")
	got := withoutBacklog(rec.take())
	requireObservations(t, got, enginetest.OutKey, "", []wantObs{{ports.ObservationDispatch, ports.OutcomeFailed}})
	if got[0].Duration != call {
		t.Errorf("dispatch duration = %v, want %v", got[0].Duration, call)
	}
}

// withoutBacklog drops the backlog gauge every claim round reports; the
// runtime tests cover it.
func withoutBacklog(obs []ports.Observation) []ports.Observation {
	return slices.DeleteFunc(obs, func(o ports.Observation) bool { return o.Kind == ports.ObservationBacklog })
}

// An engine without an Observer observes nothing and does not fail.
func TestObserveWithoutObserver(t *testing.T) {
	e := &engine.Engine{}
	e.Observe(context.Background(), ports.Observation{Kind: ports.ObservationIngest})
}
