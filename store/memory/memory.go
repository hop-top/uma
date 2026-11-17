// Package memory is an in-memory store.Store for tests, demonstrations and
// bots that need no durability. It imports only the standard library and the
// uma core packages.
//
// Each tenant's committed state is an immutable snapshot. A unit of work runs
// on a private copy of its tenant's snapshot, copying each table on first
// write, and commits by swapping the copy in under one mutex if no other unit
// committed to that tenant since the copy was taken; otherwise Transact fails
// with model.Transient and the callback is not rerun. Rollback, after a
// callback error or panic, discards the copy. Tenants never contend.
//
// # Limits
//
// Copying a table on first write makes a write cost grow linearly with the
// records its tables hold, so storing N records costs O(N²) in total. On an
// Apple M1 Pro (BenchmarkWriteGrowth, BenchmarkIngestGrowth), one message
// ingested through the engine into a tenant that already holds N messages
// takes about 0.1 ms at 100, 0.5 ms at 1,000 and 4 ms at 10,000 in one
// conversation; 0.2, 0.9 and 7 ms when every message is a new contact and
// conversation. Each ingest also allocates 2 to 4 KB per stored message (21 to
// 44 MB at 10,000), which the collector must reclaim, and the store retains 3
// to 6 KB per stored message. A write of one message record alone costs 2
// ms at 10,000 messages and 17 ms at 100,000.
//
// Reads copy nothing, but a list builds and orders the whole table before it
// cuts a page, so the first page of 50 messages of one conversation costs about
// 0.6 ms at 10,000 messages and 6 ms at 100,000, and a page late in the table
// costs 14 ms and 150 ms, with 28 MB and 320 MB allocated. Reading a whole
// table page by page costs time proportional to its size times its pages.
//
// Writers to one tenant also exclude each other: a unit that finds another
// has committed since its copy was taken fails with model.Transient, so
// concurrent workers on one tenant mostly repeat units (BenchmarkClaimWork).
// Tenants do not contend and each pays only for its own records, so splitting
// load across tenants bounds every one of these costs.
//
// Purge frees events, operations, attempts, ingress copies, exports and
// contacts, not messages or conversations, which only grow.
//
// That suits tests, demonstrations and bots that hold at most a few thousand
// messages per tenant (an ingest stays under a millisecond to about a thousand
// and within a few milliseconds to a few thousand) and need no durability. A host whose records keep
// accumulating, or that holds tens of thousands of messages in one tenant,
// belongs on a durable store.
//
// Nothing survives the process: Properties reports Durable=false, and
// engines composed with RequireDurable reject it. Reads list records in
// insertion order; cursors are positions in that order.
package memory

import (
	"context"
	"sync"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// SchemaVersion is the storage schema revision this backend implements.
const SchemaVersion = 1

// maxPage caps list page sizes; larger limits return short pages.
const maxPage = 1000

// Options configures a Store.
type Options struct {
	// Clock judges lease and batch expiry. Nil uses the system clock.
	Clock ports.Clock
}

// Store is an in-memory store.Store. The zero value is not usable; call New.
type Store struct {
	clock   ports.Clock
	mu      sync.Mutex
	tenants map[model.TenantID]*state
}

// New returns an empty store.
func New(o Options) *Store {
	c := o.Clock
	if c == nil {
		c = systemClock{}
	}
	return &Store{clock: c, tenants: map[model.TenantID]*state{}}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Properties reports a non-durable memory backend.
func (*Store) Properties() store.Properties {
	return store.Properties{Durable: false, SchemaVersion: SchemaVersion, Backend: "memory"}
}

// View runs fn once with a read-only Reader on tenant's committed snapshot.
func (s *Store) View(ctx context.Context, tenant model.TenantID, fn func(store.Reader) error) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	h := s.open(tenant, s.load(tenant))
	defer h.close()
	return fn(reader{h})
}

// Transact runs fn once on a private copy of tenant's state and commits the
// copy when fn returns nil and no other unit committed to tenant meanwhile.
func (s *Store) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	base := s.load(tenant)
	h := s.open(tenant, base.fork())
	// A panic in fn closes the handle and discards the copy: that is the
	// rollback. The panic then continues to the caller.
	defer h.close()
	if err := fn(tx{reader{h}}); err != nil {
		return err
	}
	h.close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !h.st.dirty {
		return nil
	}
	h.st.dirty = false
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.tenants[tenant]; cur != base && (cur != nil || base != empty) {
		return model.NewError(model.Transient, "concurrent unit committed; retry the unit")
	}
	s.tenants[tenant] = h.st
	return nil
}

// load returns tenant's committed snapshot. Snapshots are never mutated.
func (s *Store) load(tenant model.TenantID) *state {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.tenants[tenant]; ok {
		return st
	}
	return empty
}

// empty is the snapshot of a tenant with no committed unit.
var empty = &state{}

func (s *Store) open(tenant model.TenantID, st *state) *handle {
	return &handle{tenant: tenant, st: st, clock: s.clock}
}

// handle is the shared core of a Reader or Tx. Its mutex makes misuse from
// another goroutine safe, not supported.
type handle struct {
	mu     sync.Mutex
	closed bool
	tenant model.TenantID
	st     *state
	clock  ports.Clock
}

const errClosed = "store handle used after its callback returned"

// enter locks h for one call. On success the caller must call h.mu.Unlock.
func (h *handle) enter(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return model.NewError(model.Permanent, errClosed)
	}
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		return err
	}
	return nil
}

func (h *handle) close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
}

func (h *handle) now() time.Time { return h.clock.Now() }

var (
	_ store.Store  = (*Store)(nil)
	_ store.Reader = reader{}
	_ store.Tx     = tx{}
)
