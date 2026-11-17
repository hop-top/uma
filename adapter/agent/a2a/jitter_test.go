package a2a

import (
	"testing"
	"time"
)

func jitteredCall(poll time.Duration, source func() float64) *call {
	return &call{x: &Instance{f: &Factory{jitter: source}, cfg: config{poll: poll}}}
}

// The wait between store reads is the poll interval +-10%, following the
// injected source.
func TestPollWaitIsSpread(t *testing.T) {
	const poll = 250 * time.Millisecond
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, poll * 9 / 10}, {0.5, poll}, {1, poll * 11 / 10}} {
		if got := jitteredCall(poll, func() float64 { return c.u }).pollWait(); got != c.want {
			t.Errorf("source %v: wait %v, want %v", c.u, got, c.want)
		}
	}
}

// With no injected source every wait stays within +-10% and the waits are
// not all equal.
func TestPollWaitDefaultSourceVaries(t *testing.T) {
	c := jitteredCall(time.Second, nil)
	seen := map[time.Duration]bool{}
	for range 50 {
		d := c.pollWait()
		if d < 900*time.Millisecond || d > 1100*time.Millisecond {
			t.Fatalf("wait %v outside +-10%% of 1s", d)
		}
		seen[d] = true
	}
	if len(seen) < 10 {
		t.Fatalf("50 waits took only %d values", len(seen))
	}
}
