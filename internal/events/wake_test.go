package events_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/events"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// appendThroughEngine commits evs in a unit the engine runs, as its
// services and ingress do.
func (h *evHarness) appendThroughEngine(evs ...model.Event) {
	h.t.Helper()
	err := h.e.Store().Transact(h.ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.AppendEvents(h.ctx, evs) })
	if err != nil {
		h.t.Fatal(err)
	}
}

// parkedWaiter never ends a wait on its own: only cancellation does. It
// reports each wait it enters.
type parkedWaiter struct{ entered chan struct{} }

func (w parkedWaiter) Wait(ctx context.Context, _ time.Duration) error {
	w.entered <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

// A waiting Poll returns as soon as a unit of its engine that appended
// events commits, not at the next rescan: the waiter here never wakes it.
// A unit that appends nothing, or rolls back, does not end the wait.
func TestPollWakesOnEngineCommit(t *testing.T) {
	w := parkedWaiter{entered: make(chan struct{}, 4)}
	h := newEvHarness(t, nil, func(c *engine.Config) { c.Waiter = w })
	sub := h.openFrom(model.StartTail, model.Filter{})
	type result struct {
		b   model.EventBatch
		err error
	}
	got := make(chan result, 1)
	go func() {
		req := h.pollReq(sub, 10)
		req.Wait = time.Hour
		b, err := h.bot.Events().Poll(h.ctx, req)
		got <- result{b, err}
	}()
	<-w.entered
	ev := h.newMessage(enginetest.InboxOne)
	if err := h.e.Store().Transact(h.ctx, enginetest.TenantA, func(tx store.Tx) error {
		if err := tx.AppendEvents(h.ctx, []model.Event{ev}); err != nil {
			return err
		}
		return model.NewError(model.Conflict, "rolled back")
	}); !model.IsCode(err, model.Conflict) {
		t.Fatalf("rolled-back unit = %v", err)
	}
	if err := h.e.Store().Transact(h.ctx, enginetest.TenantA, func(store.Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		t.Fatalf("poll returned %d events, %v before any commit appended one", len(r.b.Events), r.err)
	case <-w.entered:
		t.Fatal("poll woken by a unit that appended nothing")
	case <-time.After(20 * time.Millisecond):
	}
	h.appendThroughEngine(ev)
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatal(r.err)
		}
		requireIDs(t, r.b.Events, ev)
	case <-time.After(5 * time.Second):
		t.Fatal("poll still waiting after the commit")
	}
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Commit-to-delivery latency of a long poll with the system clock and the
// default waiter, commits landing anywhere in the rescan step: the median
// stays well under the step instead of near half of it.
func TestPollWakeLatency(t *testing.T) {
	h := newEvHarness(t, nil, func(c *engine.Config) { c.Clock, c.Waiter = wallClock{}, nil })
	sub := h.openFrom(model.StartTail, model.Filter{})
	const rounds = 10
	lat := make([]time.Duration, 0, rounds)
	for i := range rounds {
		type result struct {
			b   model.EventBatch
			at  time.Time
			err error
		}
		got := make(chan result, 1)
		go func() {
			req := h.pollReq(sub, 10)
			req.Wait = 5 * events.PollStep
			b, err := h.bot.Events().Poll(h.ctx, req)
			got <- result{b, time.Now(), err}
		}()
		// Offsets spread over the first half of the step; the poll is
		// parked by then.
		time.Sleep(10*time.Millisecond + time.Duration(i)*events.PollStep/(2*rounds))
		ev := h.newMessage(enginetest.InboxOne)
		h.appendThroughEngine(ev)
		committed := time.Now()
		r := <-got
		if r.err != nil {
			t.Fatal(r.err)
		}
		requireIDs(t, r.b.Events, ev)
		lat = append(lat, r.at.Sub(committed))
		h.ack(sub, r.b.AckToken)
	}
	slices.Sort(lat)
	if p50 := lat[len(lat)/2]; p50 > events.PollStep/10 {
		t.Fatalf("p50 commit-to-delivery = %v (all %v), want well under the %v rescan step", p50, lat, events.PollStep)
	}
}
