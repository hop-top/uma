package telegram

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitsOf runs the receiver against an idle server until it has paused n
// times and returns the pauses it asked for.
func waitsOf(t *testing.T, r *rig, n int) []time.Duration {
	t.Helper()
	var mu sync.Mutex
	var waits []time.Duration
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r.x.wait = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		done := len(waits) >= n
		mu.Unlock()
		if done {
			cancel()
		}
		return ctx.Err()
	}
	if err := r.x.Run(ctx, &cursorSink{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	return waits
}

// An idle server is paced at the poll interval +-10%, following the
// injected source: the whole interval is waited only when the read took no
// time, so a pause is at most that, and not much less.
func TestIdlePauseIsSpread(t *testing.T) {
	const interval = 10 * time.Second
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, interval * 9 / 10}, {0.5, interval}, {1, interval * 11 / 10}} {
		r := newRig(t)
		r.x.opts.PollInterval = interval
		r.x.opts.Jitter = func() float64 { return c.u }
		for _, d := range waitsOf(t, r, 2) {
			if d > c.want || d < c.want-500*time.Millisecond {
				t.Errorf("source %v: pause %v, want the rest of %v", c.u, d, c.want)
			}
		}
	}
}

// With no injected source the pauses stay within +-10% and are not all
// equal.
func TestIdlePauseDefaultSourceVaries(t *testing.T) {
	r := newRig(t)
	r.x.opts.PollInterval = 10 * time.Second
	seen := map[time.Duration]bool{}
	for _, d := range waitsOf(t, r, 30) {
		if d > 11*time.Second {
			t.Fatalf("pause %v over +10%% of 10s", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("30 pauses took only %d values", len(seen))
	}
}
