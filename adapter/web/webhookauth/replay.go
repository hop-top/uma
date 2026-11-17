package webhookauth

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"hop.top/uma/ports"
)

// ReplaySource says what identifies one delivery.
type ReplaySource string

const (
	// ReplayBySignature identifies a delivery by its decoded signature. A
	// signature is deterministic over the signed payload, so the same
	// captured request is the same delivery however its header is written.
	// Signature schemes only. Without a signed timestamp or id in the
	// payload, two genuine messages with identical bodies are one delivery.
	// With Config.SignatureList, every distinct signature of the list that
	// verified is an identity of the delivery, and it is a replay when any
	// of them was accepted before: the list is not signed, so a replay can
	// drop entries.
	ReplayBySignature ReplaySource = "signature"
	// ReplayByHeader identifies a delivery by an id header the sender sets.
	ReplayByHeader ReplaySource = "header"
)

// Replay turns on replay rejection: a delivery already accepted within TTL
// is ErrReplay. A delivery is recorded only after it fully verified, so a
// forged request can never use up an id. The zero value turns it off.
type Replay struct {
	Source ReplaySource

	// Header carries the delivery id (ReplayByHeader only). Include {id} in
	// Config.Payload so the signature covers it: an id outside the signature
	// can be swapped by anyone holding a captured request.
	Header string

	// TTL is how long a delivery is remembered. Without a timestamp window
	// the default is 24 hours. With one, an entry lives at least until the
	// request would be stale anyway: two tolerances, since a request stamped
	// at the future edge stays fresh that long. The default is exactly that,
	// and a shorter TTL is raised to it.
	TTL time.Duration

	// AllowUnsigned accepts an id header the signature does not cover. The
	// check then only suppresses honest duplicates. Required for bearer and
	// token, which sign nothing.
	AllowUnsigned bool
}

const defaultReplayTTL = 24 * time.Hour

// ReplayStore remembers deliveries for the Verifier. The host injects one
// per deployment; with several replicas it must be shared, and for replay
// protection to survive a restart it must be durable: the in-memory store
// forgets everything when the process stops, so a request captured inside
// the timestamp window can be sent again after a restart.
//
// Keys are opaque, fixed-size and scoped by Deps.Scope; nothing in them is
// a credential.
type ReplayStore interface {
	// Reserve records key for ttl. fresh is false when key is already
	// recorded and unexpired. It is atomic: of concurrent calls for one key,
	// exactly one sees fresh. An error means the store cannot say, and the
	// verifier then refuses the request (ErrUnavailable).
	Reserve(ctx context.Context, key string, ttl time.Duration) (fresh bool, err error)
	// Release forgets key so the sender's retry is accepted. It is a no-op
	// for an unknown key.
	Release(ctx context.Context, key string) error
}

// ErrReplayStoreFull is returned by MemoryReplayStore.Reserve when every
// slot holds a live entry. Evicting one would reopen its replay window, so
// the store refuses instead; the verifier answers 503 and the sender
// retries.
var ErrReplayStoreFull = errors.New("webhookauth: replay store full")

// DefaultReplayCapacity is the capacity of a MemoryReplayStore created
// with a non-positive one.
const DefaultReplayCapacity = 1 << 16

// MemoryReplayStore is a bounded in-memory ReplayStore, safe for concurrent
// use. It expires entries by time and never evicts a live one. It is lost
// on restart.
type MemoryReplayStore struct {
	mu      sync.Mutex
	max     int
	clock   ports.Clock
	entries map[string]time.Time
	queue   expiryQueue
}

// NewMemoryReplayStore returns a store holding at most capacity live
// entries (DefaultReplayCapacity when capacity <= 0), reading time from
// clock (the system clock when nil).
func NewMemoryReplayStore(capacity int, clock ports.Clock) *MemoryReplayStore {
	if capacity <= 0 {
		capacity = DefaultReplayCapacity
	}
	return &MemoryReplayStore{max: capacity, clock: clock, entries: map[string]time.Time{}}
}

func (s *MemoryReplayStore) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// Reserve implements ReplayStore.
func (s *MemoryReplayStore) Reserve(_ context.Context, key string, ttl time.Duration) (bool, error) {
	if key == "" || ttl <= 0 {
		return false, fmt.Errorf("webhookauth: replay reservation needs a key and a positive ttl")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.expire(now)
	if _, held := s.entries[key]; held {
		return false, nil
	}
	if len(s.entries) >= s.max {
		return false, ErrReplayStoreFull
	}
	exp := now.Add(ttl)
	s.entries[key] = exp
	heap.Push(&s.queue, expiry{key: key, at: exp})
	return true, nil
}

// Release implements ReplayStore.
func (s *MemoryReplayStore) Release(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
	// Released keys leave stale queue items behind; rebuild when they
	// outnumber live entries, so churn cannot grow the queue without bound.
	if len(s.queue) > 2*s.max+16 {
		s.queue = s.queue[:0]
		for k, at := range s.entries {
			s.queue = append(s.queue, expiry{key: k, at: at})
		}
		heap.Init(&s.queue)
	}
	return nil
}

// Len reports the live entries.
func (s *MemoryReplayStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expire(s.now())
	return len(s.entries)
}

// expire drops entries whose time has come. A queue item is current only
// when the map still holds that expiry for its key.
func (s *MemoryReplayStore) expire(now time.Time) {
	for len(s.queue) > 0 && !s.queue[0].at.After(now) {
		it := heap.Pop(&s.queue).(expiry)
		if at, ok := s.entries[it.key]; ok && at.Equal(it.at) {
			delete(s.entries, it.key)
		}
	}
}

type expiry struct {
	key string
	at  time.Time
}

type expiryQueue []expiry

func (q expiryQueue) Len() int           { return len(q) }
func (q expiryQueue) Less(i, j int) bool { return q[i].at.Before(q[j].at) }
func (q expiryQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *expiryQueue) Push(x any)        { *q = append(*q, x.(expiry)) }
func (q *expiryQueue) Pop() any {
	old := *q
	n := len(old)
	it := old[n-1]
	*q = old[:n-1]
	return it
}
