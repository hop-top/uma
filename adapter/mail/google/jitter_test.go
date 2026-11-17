package google

import (
	"testing"
	"time"
)

// jitterAt makes the factory's jitter source answer u, in [0, 1].
func jitterAt(e *env, u float64) { e.inst.f.opts.Jitter = func() float64 { return u } }

// The poll wait is the poll interval +-10%, following the injected source.
func TestPollWaitIsSpread(t *testing.T) {
	const poll = 5 * time.Minute
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, poll * 9 / 10}, {0.5, poll}, {1, poll * 11 / 10}} {
		e := newEnv(t, Config{})
		jitterAt(e, c.u)
		wait, err := e.inst.round(t.Context(), &testSink{})
		if err != nil {
			t.Fatal(err)
		}
		if wait != c.want {
			t.Errorf("source %v: wait %v, want %v", c.u, wait, c.want)
		}
	}
}

// With no injected source every wait stays within +-10% and the waits are
// not all equal, so channels drift out of phase.
func TestPollWaitDefaultSourceVaries(t *testing.T) {
	e := newEnv(t, Config{})
	e.f.opts.Jitter = nil
	seen := map[time.Duration]bool{}
	for range 50 {
		wait, err := e.inst.round(t.Context(), &testSink{})
		if err != nil {
			t.Fatal(err)
		}
		if wait < 5*time.Minute*9/10 || wait > 5*time.Minute*11/10 {
			t.Fatalf("wait %v outside +-10%% of the poll interval", wait)
		}
		seen[wait] = true
	}
	if len(seen) < 10 {
		t.Fatalf("50 waits took only %d values", len(seen))
	}
}

// The renewal time is the renewal interval +-10%, drawn once when the watch
// is made and kept; the watch is renewed there, not at the plain interval.
func TestWatchRenewalIsSpread(t *testing.T) {
	const interval = 24 * time.Hour
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, interval * 9 / 10}, {1, interval * 11 / 10}} {
		e := newEnv(t, Config{}, pushConfig)
		jitterAt(e, c.u)
		if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
			t.Fatal(err)
		}
		st := e.st(t)
		if want := testNow.Add(c.want); !st.WatchRenewAt.Equal(want) {
			t.Fatalf("source %v: renew at %v, want %v", c.u, st.WatchRenewAt, want)
		}
		e.clock.Advance(c.want - time.Minute)
		if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
			t.Fatal(err)
		}
		if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/watch"); n != 1 {
			t.Fatalf("source %v: %d watch calls before due, want 1", c.u, n)
		}
		e.clock.Advance(time.Minute)
		if _, err := e.inst.round(t.Context(), &testSink{}); err != nil {
			t.Fatal(err)
		}
		if n := e.g.count("POST /gmail/v1/users/" + testMailbox + "/watch"); n != 2 {
			t.Fatalf("source %v: %d watch calls once due, want a renewal", c.u, n)
		}
	}
}

// A late draw never carries the renewal past watchMargin before expiry.
func TestWatchRenewalCappedByExpiry(t *testing.T) {
	e := newEnv(t, Config{WatchRenewIntervalMS: int64(6 * 24 * time.Hour / time.Millisecond)}, pushConfig)
	st := receiverState{HistoryID: 1000, WatchedAt: testNow, WatchExpires: testNow.Add(6*24*time.Hour + 30*time.Minute), WatchRenewAt: testNow.Add(7 * 24 * time.Hour)}
	if due := e.inst.watchDue(st); !due.Equal(st.WatchExpires.Add(-watchMargin)) {
		t.Fatalf("due %v, want the expiry less the margin", due)
	}
}
