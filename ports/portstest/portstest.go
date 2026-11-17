// Package portstest provides deterministic ports for fixtures and conformance
// tests. It is not a production generator or clock.
package portstest

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma/model"
)

// maxSequence is the largest counter that fits the 12-hex-digit node field.
const maxSequence = 1<<48 - 1

// SequenceIDs is a deterministic, concurrency-safe ports.IDs. Values are
// canonical version-7 UUID text, fmt.Sprintf("%08x-0000-7000-8000-%012x",
// seed, n) for n = 1, 2, 3, ..., so they pass model.ParseID and store
// unchanged in a Postgres uuid column. Values are deterministic only for a
// deterministic call order. The zero value uses seed 0.
type SequenceIDs struct {
	seed uint32
	n    atomic.Uint64
}

// NewSequenceIDs returns a generator whose seed separates its values from
// other instances in the same test.
func NewSequenceIDs(seed uint32) *SequenceIDs { return &SequenceIDs{seed: seed} }

// New returns the next ID. It fails with model.Permanent once the sequence is
// exhausted.
func (s *SequenceIDs) New(context.Context) (model.ID, error) {
	n := s.n.Add(1)
	if n > maxSequence {
		return "", model.NewError(model.Permanent, "sequence exhausted")
	}
	return model.ID(fmt.Sprintf("%08x-0000-7000-8000-%012x", s.seed, n)), nil
}

// FixedClock is a ports.Clock that moves only when told to. It is safe for
// concurrent use; the zero value reports the zero time.
type FixedClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewFixedClock returns a clock fixed at t.
func NewFixedClock(t time.Time) *FixedClock { return &FixedClock{now: t} }

// Now returns the current fixed time.
func (c *FixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to t.
func (c *FixedClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// Advance moves the clock forward by d and returns the new time.
func (c *FixedClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}
