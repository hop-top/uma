package engine

import (
	"math/rand/v2"
	"time"

	"hop.top/uma/model"
)

// RetryPolicy bounds the engine's provider attempts. Its fields mirror the
// public policy one for one; the public New converts.
//
// A retry waits a full-jitter exponential backoff: a uniformly random delay
// in [0, min(MaxDelay, BaseDelay*2^(n-1))] before attempt n+1. A provider
// Retry-After longer than that delay replaces it. Zero fields take the
// defaults below.
type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
}

// Retry policy defaults.
const (
	DefaultMaxAttempts = 5
	DefaultBaseDelay   = time.Second
	DefaultMaxDelay    = 5 * time.Minute
)

func (p RetryPolicy) validate() error {
	switch {
	case p.MaxAttempts < 0:
		return ConfigError("Retry.MaxAttempts", "must not be negative")
	case p.BaseDelay < 0:
		return ConfigError("Retry.BaseDelay", "must not be negative")
	case p.MaxDelay < 0:
		return ConfigError("Retry.MaxDelay", "must not be negative")
	case p.BaseDelay > 0 && p.MaxDelay > 0 && p.BaseDelay > p.MaxDelay:
		return ConfigError("Retry.BaseDelay", "must not exceed MaxDelay")
	}
	return nil
}

// withDefaults fills zero fields.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.MaxAttempts == 0 {
		p.MaxAttempts = DefaultMaxAttempts
	}
	if p.BaseDelay == 0 {
		p.BaseDelay = DefaultBaseDelay
	}
	if p.MaxDelay == 0 {
		p.MaxDelay = DefaultMaxDelay
	}
	if p.BaseDelay > p.MaxDelay {
		p.BaseDelay = p.MaxDelay
	}
	return p
}

// ceiling returns the backoff ceiling after attempt n (n >= 1):
// min(MaxDelay, BaseDelay*2^(n-1)), without overflow.
func (p RetryPolicy) ceiling(n int) time.Duration {
	d := p.BaseDelay
	for i := 1; i < n && d < p.MaxDelay; i++ {
		d *= 2
	}
	return min(d, p.MaxDelay)
}

// Delay returns the wait before the attempt after attempt n: full jitter
// over the ceiling, replaced by retryAfter when that is longer. jitter
// returns a uniform value in [0, max].
func (p RetryPolicy) Delay(n int, retryAfter time.Duration, jitter func(max time.Duration) time.Duration) time.Duration {
	d := jitter(p.ceiling(n))
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// UniformJitter returns a uniformly random duration in [0, max] from the
// standard library generator; it is not used for anything secret.
func UniformJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1)) // #nosec G404 -- jitter, not a secret
}

// Retryable reports whether a failure that is known not to have applied the
// effect may be retried: its code is transient, unavailable or rate limited,
// and the source did not advise against retrying.
func Retryable(f *model.Error) bool {
	return f != nil && f.Code.DefaultRetry() == model.RetrySameOperation &&
		(f.Retry == "" || f.Retry == model.RetrySameOperation)
}
