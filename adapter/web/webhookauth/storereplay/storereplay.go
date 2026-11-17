// Package storereplay is a restart-safe, shareable webhookauth.ReplayStore
// kept in the root store port, so a host that already runs a durable
// store.Store (the PostgreSQL store, for one) gets replay protection that
// survives a restart and holds across replicas without another service.
//
// # Layout
//
// Deliveries are spread over a fixed number of buckets by a digest of their
// key. Each bucket is one adapter-owned protocol record
// (store.Tx.PutProtocol) of one channel, holding the truncated digest and
// expiry of the deliveries it remembers and nothing else: no secret,
// signature, id, scope or body. The store therefore holds at most Buckets
// records, whatever the traffic.
//
// # Guarantees
//
//   - Reserve is atomic. A bucket changes by compare-and-set on its
//     revision, inside one unit: of concurrent reservations of one key, on
//     one process or on several sharing the backing store, exactly one
//     commits as fresh. A reservation that loses a race to another writer of
//     its bucket reads the winner's state and tries again.
//   - Expiry is judged by Options.Clock, the clock the verifier uses.
//   - Storage is bounded. A write drops the entries of its bucket that
//     expired, and a bucket holds at most BucketCapacity live entries: a
//     full bucket refuses (webhookauth.ErrReplayStoreFull) instead of
//     forgetting a live delivery.
//   - It fails closed. Any store failure, a channel the store does not
//     hold, state it cannot read and contention that never settles are
//     errors, which the verifier answers with 503 and records nothing.
package storereplay

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"hop.top/uma/adapter/web/webhookauth"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

const (
	// DefaultBuckets is the bucket count of a Store created with a
	// non-positive one.
	DefaultBuckets = 256
	// DefaultBucketCapacity is the live-entry limit of each bucket of a
	// Store created with a non-positive one. With the default buckets, a
	// store holds 65536 live deliveries when keys spread evenly.
	DefaultBucketCapacity = 256

	protocolName    = "webhookauth.replay"
	protocolVersion = 1

	// maxAttempts bounds how often one call retries after losing a race.
	maxAttempts = 16
	maxBackoff  = 20 * time.Millisecond
)

// Options configures a Store.
type Options struct {
	// Store keeps the state. Required. A durable one (see
	// store.Properties.Durable) gives restart safety; one shared by every
	// replica gives multi-instance safety.
	Store store.Store

	// Channel is the stored channel whose protocol records hold the state.
	// Required, and it must exist in Store: a Store never creates it. Its
	// tenant scopes every unit. Give each deployment of a webhook receiver
	// its own channel, or share one: keys already include Deps.Scope.
	Channel model.ChannelRef

	// Clock judges expiry. Nil uses the system clock. Replicas sharing a
	// store must use clocks that agree to well within the replay TTL, which
	// is at least two timestamp tolerances.
	Clock ports.Clock

	// Buckets is the number of records the state is spread over; 0 means
	// DefaultBuckets. It bounds both storage (Buckets records) and write
	// contention. Keep it fixed: records are named by it, so changing it
	// makes the old entries unreachable and reopens their replay windows.
	Buckets int

	// BucketCapacity is the most live entries one bucket holds; 0 means
	// DefaultBucketCapacity. A write rewrites its whole bucket, so keep it
	// in the hundreds and raise Buckets for more capacity.
	BucketCapacity int
}

// Store is a webhookauth.ReplayStore over a store.Store, safe for
// concurrent use.
type Store struct {
	st       store.Store
	ch       model.ChannelRef
	clock    ports.Clock
	buckets  int
	capacity int
	// gates holds one slot per bucket this process has written: its own
	// writers of one bucket take turns instead of racing each other.
	gates sync.Map // bucket name -> chan struct{}
}

var _ webhookauth.ReplayStore = (*Store)(nil)

// New returns a Store. It does no I/O: a missing channel shows on first use.
func New(o Options) (*Store, error) {
	switch {
	case o.Store == nil:
		return nil, errors.New("webhookauth/storereplay: store required")
	case o.Channel.Validate() != nil:
		return nil, errors.New("webhookauth/storereplay: a valid channel is required")
	case o.Buckets < 0 || o.BucketCapacity < 0:
		return nil, errors.New("webhookauth/storereplay: negative bucket count or capacity")
	}
	s := &Store{st: o.Store, ch: o.Channel, clock: o.Clock, buckets: o.Buckets, capacity: o.BucketCapacity}
	if s.buckets == 0 {
		s.buckets = DefaultBuckets
	}
	if s.capacity == 0 {
		s.capacity = DefaultBucketCapacity
	}
	return s, nil
}

func (s *Store) now() time.Time {
	if s.clock != nil {
		return s.clock.Now()
	}
	return time.Now()
}

// Reserve implements webhookauth.ReplayStore.
func (s *Store) Reserve(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if key == "" || ttl <= 0 {
		return false, errors.New("webhookauth/storereplay: replay reservation needs a key and a positive ttl")
	}
	bucket, id := s.locate(key)
	var fresh bool
	err := s.update(ctx, bucket, func(now time.Time, entries map[string]int64) (bool, error) {
		fresh = false
		if _, held := entries[id]; held {
			return false, nil
		}
		if len(entries) >= s.capacity {
			return false, webhookauth.ErrReplayStoreFull
		}
		entries[id] = now.Add(ttl).UnixNano()
		fresh = true
		return true, nil
	})
	if err != nil {
		return false, err
	}
	return fresh, nil
}

// Release implements webhookauth.ReplayStore.
func (s *Store) Release(ctx context.Context, key string) error {
	if key == "" {
		return errors.New("webhookauth/storereplay: replay release needs a key")
	}
	bucket, id := s.locate(key)
	return s.update(ctx, bucket, func(_ time.Time, entries map[string]int64) (bool, error) {
		if _, held := entries[id]; !held {
			return false, nil
		}
		delete(entries, id)
		return true, nil
	})
}

// locate returns the record name and the entry name of key. Both come from a
// digest of it, so the state holds nothing of the key itself.
func (s *Store) locate(key string) (bucket, id string) {
	sum := sha256.Sum256([]byte(key))
	n := uint64(binary.BigEndian.Uint32(sum[:4])) % uint64(s.buckets) // #nosec G115 -- New refuses a negative bucket count
	return s.bucketName(int(n)), hex.EncodeToString(sum[4:20])        // #nosec G115 -- n is below the int bucket count
}

func (s *Store) bucketName(n int) string {
	return protocolName + "/" + strconv.Itoa(s.buckets) + "/" + strconv.Itoa(n)
}

// state is one bucket's stored value.
type state struct {
	// Seq counts the bucket's writes and names its revision.
	Seq uint64 `json:"seq"`
	// Entries maps an entry name to its expiry, Unix nanoseconds.
	Entries map[string]int64 `json:"entries"`
}

func revision(seq uint64) model.Revision { return model.Revision("r" + strconv.FormatUint(seq, 10)) }

// update runs mutate on bucket's live entries (the expired ones already
// dropped) in one unit and stores the result when mutate reports a change.
// A unit that loses a race to another writer of the bucket is run again on
// the winner's state. mutate may run several times and must start afresh.
func (s *Store) update(ctx context.Context, bucket string, mutate func(now time.Time, entries map[string]int64) (bool, error)) error {
	release, err := s.enter(ctx, bucket)
	if err != nil {
		return fmt.Errorf("webhookauth/storereplay: %w", err)
	}
	defer release()
	var lost error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := pause(ctx, attempt); err != nil {
				return fmt.Errorf("webhookauth/storereplay: %w", err)
			}
		}
		err := s.st.Transact(ctx, s.ch.Tenant, func(tx store.Tx) error {
			return s.apply(ctx, tx, bucket, mutate)
		})
		switch {
		case err == nil:
			return nil
		case model.IsCode(err, model.Conflict) || model.IsCode(err, model.Transient):
			lost = err
		case errors.Is(err, webhookauth.ErrReplayStoreFull):
			return err
		default:
			return fmt.Errorf("webhookauth/storereplay: %w", err)
		}
	}
	return fmt.Errorf("webhookauth/storereplay: bucket stayed contended after %d attempts: %w", maxAttempts, lost)
}

// enter admits one writer of bucket at a time within this process. Optimistic
// writes to one record commit one per round, so n local writers racing on it
// need n rounds and the unlucky ones exhaust their attempts; taking turns
// leaves the retry budget to contention between processes, whose count is
// the replica count, not the request rate. It waits for ctx.
func (s *Store) enter(ctx context.Context, bucket string) (func(), error) {
	g, _ := s.gates.LoadOrStore(bucket, make(chan struct{}, 1))
	gate := g.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *Store) apply(ctx context.Context, tx store.Tx, bucket string, mutate func(time.Time, map[string]int64) (bool, error)) error {
	cur, err := tx.Protocol(ctx, s.ch, bucket)
	var st state
	var expected model.Revision
	switch {
	case model.IsCode(err, model.NotFound):
		// No record yet, or no channel; the write below tells them apart.
	case err != nil:
		return err
	default:
		if st, err = decode(cur.Value); err != nil {
			return err
		}
		expected = cur.Revision
	}
	now := s.now()
	purged := drop(st.Entries, now)
	if st.Entries == nil {
		st.Entries = map[string]int64{}
	}
	changed, err := mutate(now, st.Entries)
	if err != nil {
		return err
	}
	if !changed && !purged {
		return nil
	}
	st.Seq++
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return tx.PutProtocol(ctx, store.ProtocolState{
		Channel:  s.ch,
		Key:      bucket,
		Value:    model.Extension{Name: protocolName, Version: protocolVersion, Data: raw},
		Revision: revision(st.Seq),
	}, expected)
}

// drop removes the entries expired at now and reports whether there were any.
func drop(entries map[string]int64, now time.Time) bool {
	at := now.UnixNano()
	var any bool
	for id, exp := range entries {
		if exp <= at {
			delete(entries, id)
			any = true
		}
	}
	return any
}

func decode(v model.Extension) (state, error) {
	var st state
	if v.Name != protocolName || v.Version != protocolVersion {
		return st, errors.New("replay state is not recognized")
	}
	if err := json.Unmarshal(v.Data, &st); err != nil {
		return state{}, errors.New("replay state is unreadable")
	}
	return st, nil
}

// pause waits a random time before retry number attempt, longer as
// attempts mount, so writers of one bucket stop colliding.
func pause(ctx context.Context, attempt int) error {
	limit := min(time.Duration(1<<min(attempt, 10))*time.Millisecond, maxBackoff)
	t := time.NewTimer(rand.N(limit) + 1) // #nosec G404 -- backoff jitter, not a secret
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
