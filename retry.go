package uma

import "time"

// RetryPolicy bounds the engine's provider attempts. The engine is the only
// retry owner for outbound effects: adapters disable SDK and transport
// retries, and a host retries a command only by resubmitting the same
// operation ID with the same payload.
//
// A retry waits a full-jitter exponential backoff: a uniformly random delay
// in [0, min(MaxDelay, BaseDelay*2^(n-1))] before attempt n+1. A provider
// Retry-After longer than that delay replaces it. Zero fields take the
// defaults below.
type RetryPolicy struct {
	// MaxAttempts caps provider attempts per operation, counting the first.
	// Default 5.
	MaxAttempts int
	// BaseDelay is the backoff ceiling after the first attempt. Default 1s.
	BaseDelay time.Duration
	// MaxDelay caps every backoff ceiling. Default 5m. A provider
	// Retry-After may exceed it.
	MaxDelay time.Duration
}

// Retry policy defaults.
const (
	DefaultMaxAttempts = 5
	DefaultBaseDelay   = time.Second
	DefaultMaxDelay    = 5 * time.Minute
)
