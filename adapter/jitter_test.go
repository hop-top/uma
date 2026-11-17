package adapter_test

import (
	"testing"
	"time"

	"hop.top/uma/adapter"
)

func TestSpreadBoundsAndDeterminism(t *testing.T) {
	const d = 5 * time.Minute
	for _, c := range []struct {
		u    float64
		want time.Duration
	}{{0, d * 9 / 10}, {0.5, d}, {1, d * 11 / 10}, {-3, d * 9 / 10}, {7, d * 11 / 10}} {
		if got := adapter.Spread(d, 0.1, func() float64 { return c.u }); got != c.want {
			t.Errorf("source %v: got %v, want %v", c.u, got, c.want)
		}
	}
	for range 1000 {
		got := adapter.Spread(d, 0.1, nil)
		if got < d*9/10 || got > d*11/10 {
			t.Fatalf("default source gave %v outside +-10%% of %v", got, d)
		}
	}
	if got := adapter.Spread(0, 0.1, nil); got != 0 {
		t.Errorf("zero duration spread to %v", got)
	}
	if got := adapter.Spread(d, 0, nil); got != d {
		t.Errorf("zero fraction spread to %v", got)
	}
}
