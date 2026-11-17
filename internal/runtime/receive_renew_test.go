package runtime_test

import (
	"strconv"
	"testing"
	"time"

	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

func renewRef(n int) model.ChannelRef {
	return model.ChannelRef{Tenant: "t", ID: model.ID("chan-" + strconv.Itoa(n))}
}

func jitterOf(frac float64) func(time.Duration) time.Duration {
	return func(max time.Duration) time.Duration { return time.Duration(frac * float64(max)) }
}

// Every renewal wait is within +-10% of the renewal period, the first one
// within [half, full), whatever the jitter source answers.
func TestRenewWaitBounds(t *testing.T) {
	lo, hi := runtime.ReceiverRenew*9/10, runtime.ReceiverRenew*11/10
	for _, frac := range []float64{0, 0.25, 0.5, 1} {
		w := runtime.RenewWait(renewRef(1), false, jitterOf(frac))
		if w < lo || w > hi {
			t.Fatalf("wait %v with jitter %v outside [%v, %v]", w, frac, lo, hi)
		}
	}
	// A source answering beyond its bound cannot push a wait out.
	if w := runtime.RenewWait(renewRef(1), false, func(m time.Duration) time.Duration { return 10 * m }); w != runtime.ReceiverRenewMax {
		t.Fatalf("unbounded jitter gave %v, want the cap %v", w, runtime.ReceiverRenewMax)
	}
	if w := runtime.RenewWait(renewRef(1), false, func(time.Duration) time.Duration { return -time.Hour }); w != lo {
		t.Fatalf("negative jitter gave %v, want the floor %v", w, lo)
	}
	for n := range 500 {
		w := runtime.RenewWait(renewRef(n), true, nil)
		if w < runtime.ReceiverRenew/2 || w >= runtime.ReceiverRenew {
			t.Fatalf("first wait %v of channel %d outside [%v, %v)", w, n, runtime.ReceiverRenew/2, runtime.ReceiverRenew)
		}
	}
}

// The lease is renewed before it expires even when every wait is the
// longest: the cap leaves a margin of the lease minus the longest wait, and
// two renewals missed in a row still leave the lease held at the second.
func TestRenewWaitStaysInsideLease(t *testing.T) {
	longest := runtime.RenewWait(renewRef(1), false, func(m time.Duration) time.Duration { return m })
	if longest >= runtime.ReceiverLease {
		t.Fatalf("longest wait %v reaches the lease %v", longest, runtime.ReceiverLease)
	}
	if 2*longest >= runtime.ReceiverLease {
		t.Fatalf("two longest waits %v do not fit the lease %v", 2*longest, runtime.ReceiverLease)
	}
}

// The same channel gets the same first wait; channels get different ones,
// so they do not renew in lockstep.
func TestRenewFirstWaitIsStablePerChannel(t *testing.T) {
	if a, b := runtime.RenewWait(renewRef(7), true, nil), runtime.RenewWait(renewRef(7), true, nil); a != b {
		t.Fatalf("first wait not stable: %v then %v", a, b)
	}
	seen := map[time.Duration]int{}
	for n := range 200 {
		seen[runtime.RenewWait(renewRef(n), true, nil)]++
	}
	if len(seen) < 100 {
		t.Fatalf("200 channels share only %d first waits", len(seen))
	}
	if runtime.RenewWait(renewRef(1), true, nil) == runtime.RenewWait(renewRef(2), true, nil) {
		t.Fatal("two channels renew in lockstep")
	}
}

// Later waits follow the injected source: deterministic, and different
// draws move the phase.
func TestRenewWaitFollowsJitterSource(t *testing.T) {
	a := runtime.RenewWait(renewRef(1), false, jitterOf(0.2))
	if b := runtime.RenewWait(renewRef(1), false, jitterOf(0.2)); a != b {
		t.Fatalf("same draw, different waits: %v, %v", a, b)
	}
	if runtime.RenewWait(renewRef(1), false, jitterOf(0.9)) == a {
		t.Fatal("jitter source ignored")
	}
}
