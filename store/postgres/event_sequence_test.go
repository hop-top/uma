package postgres

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Event log positions under concurrent appenders. Units appending events
// to one tenant overlap without contending on a shared row, and a reader
// tailing the log by cursor, with or without Since, reads every committed
// event exactly once, whatever order the units commit in.

const seqTenant model.TenantID = "postgres-event-sequence"

var seqInbox = model.InboxRef{Tenant: seqTenant, ID: benchID(0xe0, 0)}

// seqRig is a store on a traced pool of its own database.
type seqRig struct {
	tr   *abortTracer
	st   *Store
	pool *pgxpool.Pool
}

func newSeqRig(t *testing.T, conns int32) *seqRig {
	t.Helper()
	tr := &abortTracer{}
	cfg := newDatabase(t, true).Config().Copy()
	cfg.ConnConfig.Tracer = tr
	cfg.MaxConns = conns
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	st, err := Open(t.Context(), pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return &seqRig{tr: tr, st: st, pool: pool}
}

// seqEvent is event i of space, about conversation conv of seqInbox,
// recorded at at.
func seqEvent(space, i, conv int, at time.Time) model.Event {
	c := model.ConversationRef{Inbox: seqInbox, ID: benchID(0xe1, conv)}
	return model.Event{
		Version: 1, ID: benchID(space, i), Kind: "sequence.appended", Tenant: seqTenant, Inbox: seqInbox.ID,
		Resource: c.Ref(), Actor: "sequence-actor", OccurredAt: at, RecordedAt: at,
		Audience: model.Audience{Visibility: model.Internal},
	}
}

// unit runs fn as the engine runs a caller's unit: a unit lost to a
// concurrent one (model.Transient) reruns, up to 16 times, after a
// full-jitter pause starting at 1ms and doubling to a 64ms cap.
func (r *seqRig) unit(ctx context.Context, fn func(store.Tx) error) (int, error) {
	pause := time.Millisecond
	for attempt := 1; ; attempt++ {
		err := r.st.Transact(ctx, seqTenant, fn)
		if !model.IsCode(err, model.Transient) || attempt == 16 {
			return attempt, err
		}
		time.Sleep(rand.N(pause) + 1) // #nosec G404 -- jitter, not security
		pause = min(2*pause, 64*time.Millisecond)
	}
}

// headAborts counts the serialization failures of statements writing the
// tenant's head row.
func (a *abortTracer) headAborts() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for k, v := range a.aborts {
		if strings.Contains(k, "EventHead") || strings.Contains(k, "EventRecordedMax") {
			n += v
		}
	}
	return n
}

// readAll pages the whole log from after at page size limit.
func (r *seqRig) readAll(t *testing.T, f model.Filter, after model.Cursor, limit int) ([]model.Event, model.Cursor) {
	t.Helper()
	var out []model.Event
	for {
		var p model.Page[model.Event]
		if err := r.st.View(t.Context(), seqTenant, func(rd store.Reader) (err error) {
			p, err = rd.Events(t.Context(), f, after, limit)
			return err
		}); err != nil {
			t.Fatalf("Events after %q: %v", after, err)
		}
		out = append(out, p.Items...)
		after = p.Next
		if p.Complete {
			return out, after
		}
	}
}

func ids(evs []model.Event) []model.ID {
	out := make([]model.ID, len(evs))
	for i, e := range evs {
		out[i] = e.ID
	}
	return out
}

func percentile(d []time.Duration, p float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

// TestConcurrentAppendsAcrossConversationsDoNotContend: callers appending
// events about different conversations at once commit without losing a
// unit to one another on the tenant's head row, and the log holds every
// event once, at dense positions. The test logs serialization failures by
// statement and the latency of each logical unit, reruns included.
func TestConcurrentAppendsAcrossConversationsDoNotContend(t *testing.T) {
	const workers, units = 8, 25
	r := newSeqRig(t, 2*workers)
	ctx := t.Context()
	start := make(chan struct{})
	lat := make([]time.Duration, workers*units)
	var attempts, lost atomic.Int64
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			<-start
			for i := range units {
				ev := seqEvent(0xe2, w*units+i, w, time.Now())
				began := time.Now()
				n, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{ev}) })
				lat[w*units+i] = time.Since(began)
				attempts.Add(int64(n))
				if err != nil {
					lost.Add(1)
					t.Errorf("worker %d unit %d: %v", w, i, err)
				}
			}
		})
	}
	began := time.Now()
	close(start)
	wg.Wait()
	wall := time.Since(began)
	r.tr.log(t)
	t.Logf("%d units: %d attempts, %d lost; latency p50 %v p95 %v p99 %v max %v; wall %v (%.0f units/s)",
		workers*units, attempts.Load(), lost.Load(), percentile(lat, 0.5), percentile(lat, 0.95), percentile(lat, 0.99),
		percentile(lat, 1), wall, float64(workers*units)/wall.Seconds())
	if n := r.tr.headAborts(); n != 0 {
		t.Errorf("%d serialization failures on the event head row, want none", n)
	}
	// Every unit's placement succeeded before it returned.
	if n := unplacedCount(t, r.pool); n != 0 || r.st.unplaced.seen(seqTenant) != 0 {
		t.Errorf("%d events left at placeholders, placement owed %v; want every placement done", n, r.st.unplaced.seen(seqTenant) != 0)
	}
	all, next := r.readAll(t, model.Filter{}, "", 1000)
	if len(all) != workers*units || next != encodeCursor(workers*units) {
		t.Fatalf("log = %d events through %q, want %d at dense positions", len(all), next, workers*units)
	}
	got := ids(all)
	slices.Sort(got)
	if len(slices.Compact(got)) != workers*units {
		t.Fatalf("log holds an event more than once")
	}
}

// TestOutOfOrderCommitIsReadOnce: unit A appends first and commits last;
// unit B appends after A and commits while A is still open. B commits
// without waiting for A, and readers tailing the log by cursor, one of
// them with Since, read each event exactly once: the reader that read B
// and advanced past it still reads A after A commits.
func TestOutOfOrderCommitIsReadOnce(t *testing.T) {
	r := newSeqRig(t, 8)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	first := seqEvent(0xe3, 0, 0, t0)
	if _, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{first}) }); err != nil {
		t.Fatal(err)
	}
	since := t0.Add(8 * time.Second)
	plain, cur := r.readAll(t, model.Filter{}, "", 10)
	if !slices.Equal(ids(plain), []model.ID{first.ID}) {
		t.Fatalf("first read = %v, want %s", ids(plain), first.ID)
	}
	_, sinceCur := r.readAll(t, model.Filter{Since: &since}, "", 10)

	a := seqEvent(0xe3, 1, 1, t0.Add(10*time.Second)) // late enough for Since
	b := seqEvent(0xe3, 2, 2, t0.Add(5*time.Second))  // too early for Since
	appendedA, releaseA := make(chan struct{}), make(chan struct{})
	var aCommitted atomic.Bool
	aDone := make(chan error, 1)
	go func() {
		_, err := r.unit(ctx, func(tx store.Tx) error {
			if err := tx.AppendEvents(ctx, []model.Event{a}); err != nil {
				return err
			}
			select {
			case <-appendedA:
			default:
				close(appendedA)
			}
			select {
			case <-releaseA:
			case <-time.After(5 * time.Second):
			}
			return nil
		})
		aCommitted.Store(true)
		aDone <- err
	}()
	<-appendedA
	if _, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{b}) }); err != nil {
		t.Fatal(err)
	}
	if aCommitted.Load() {
		t.Errorf("B committed only after A: appenders of one tenant waited for each other")
	}
	var seen, seenSince []model.ID
	got, cur := r.readAll(t, model.Filter{}, cur, 10)
	seen = append(seen, ids(got)...)
	got, sinceCur = r.readAll(t, model.Filter{Since: &since}, sinceCur, 10)
	seenSince = append(seenSince, ids(got)...)
	close(releaseA)
	if err := <-aDone; err != nil {
		t.Fatal(err)
	}
	got, _ = r.readAll(t, model.Filter{}, cur, 10)
	seen = append(seen, ids(got)...)
	got, _ = r.readAll(t, model.Filter{Since: &since}, sinceCur, 10)
	seenSince = append(seenSince, ids(got)...)
	if !slices.Equal(seen, []model.ID{b.ID, a.ID}) {
		t.Errorf("tailing reader read %v, want %s then %s, each once", seen, b.ID, a.ID)
	}
	if !slices.Equal(seenSince, []model.ID{a.ID}) {
		t.Errorf("tailing reader with Since read %v, want %s once", seenSince, a.ID)
	}
	all, _ := r.readAll(t, model.Filter{}, "", 10)
	if !slices.Equal(ids(all), []model.ID{first.ID, b.ID, a.ID}) {
		t.Errorf("log = %v, want commit order %s %s %s", ids(all), first.ID, b.ID, a.ID)
	}
}

// TestConcurrentAppendsAreTailedOnce: appenders commit events with
// RecordedAt out of position order while two readers tail the log, one
// with Since; some units read the head after appending, which places their
// events inside the unit. Each reader reads every committed event it
// selects exactly once, in log order. A unit lost to concurrent units past
// its reruns commits nothing and is only counted.
func TestConcurrentAppendsAreTailedOnce(t *testing.T) {
	const workers, units = 6, 20
	r := newSeqRig(t, 3*workers)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	since := t0.Add(30 * time.Second)
	var mu sync.Mutex
	want := map[model.ID]bool{} // every committed event
	wantSince := map[model.ID]bool{}
	var lost atomic.Int64
	var appenders sync.WaitGroup
	for w := range workers {
		appenders.Go(func() {
			rng := rand.New(rand.NewPCG(uint64(w), 1)) // #nosec G404 G115 -- test data, w >= 0
			for i := range units {
				n := 1 + rng.IntN(3)
				evs := make([]model.Event, n)
				for k := range evs {
					at := t0.Add(time.Duration(rng.IntN(60)) * time.Second)
					evs[k] = seqEvent(0xe4+w, i*3+k, w, at)
				}
				readsHead := rng.IntN(8) == 0
				if _, err := r.unit(ctx, func(tx store.Tx) error {
					before, err := tx.EventHead(ctx)
					if err != nil {
						return err
					}
					if err := tx.AppendEvents(ctx, evs); err != nil {
						return err
					}
					if readsHead {
						after, err := tx.EventHead(ctx)
						if err != nil {
							return err
						}
						if after == before {
							return fmt.Errorf("head did not move within the unit")
						}
					}
					return nil
				}); err != nil {
					if !model.IsCode(err, model.Transient) {
						t.Errorf("worker %d unit %d: %v", w, i, err)
					}
					lost.Add(1)
					continue
				}
				mu.Lock()
				for _, e := range evs {
					want[e.ID] = true
					if !e.RecordedAt.Before(since) {
						wantSince[e.ID] = true
					}
				}
				mu.Unlock()
			}
		})
	}
	done := make(chan struct{})
	tail := func(f model.Filter) []model.ID {
		var seen []model.ID
		var cur model.Cursor
		for last := false; ; {
			select {
			case <-done:
				last = true
			default:
			}
			got, next := r.readAll(t, f, cur, 7)
			seen, cur = append(seen, ids(got)...), next
			if last {
				return seen
			}
		}
	}
	var plain, withSince []model.ID
	var tailers sync.WaitGroup
	tailers.Go(func() { plain = tail(model.Filter{}) })
	tailers.Go(func() { withSince = tail(model.Filter{Since: &since}) })
	appenders.Wait()
	close(done)
	tailers.Wait()
	r.tr.log(t)
	t.Logf("%d units lost to concurrent units", lost.Load())
	all, _ := r.readAll(t, model.Filter{}, "", 1000)
	if len(all) != len(want) {
		t.Fatalf("log = %d events, want the %d committed", len(all), len(want))
	}
	var allSince []model.ID
	for _, e := range all {
		if wantSince[e.ID] {
			allSince = append(allSince, e.ID)
		}
	}
	if !slices.Equal(plain, ids(all)) {
		t.Errorf("tailing reader read %d events, want the log's %d once each in log order", len(plain), len(all))
	}
	if !slices.Equal(withSince, allSince) {
		t.Errorf("tailing reader with Since read %d events, want the %d at or after Since once each in log order", len(withSince), len(allSince))
	}
}
