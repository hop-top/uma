package adapter

import (
	"math/rand/v2"
	"time"
)

// Spread returns d scaled by a factor in [1-frac, 1+frac], drawn from
// source, which answers uniformly in [0, 1]: 0 gives 1-frac, 0.5 gives d,
// 1 gives 1+frac. It is how a receiver that waits a fixed interval keeps
// many channels, started together, from staying in phase: each wait is
// d +-frac instead of d. A nil source is the standard library's generator
// (not for secrets); a source answering outside [0, 1] is clamped. A
// non-positive d or frac returns d.
//
// Hosts and tests inject source to make waits deterministic.
func Spread(d time.Duration, frac float64, source func() float64) time.Duration {
	if d <= 0 || frac <= 0 {
		return d
	}
	var u float64
	if source != nil {
		u = source()
	} else {
		u = rand.Float64() // #nosec G404 -- jitter, not a secret
	}
	u = min(max(u, 0), 1)
	return time.Duration(float64(d) * (1 - frac + 2*frac*u))
}
