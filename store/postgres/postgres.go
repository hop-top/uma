package postgres

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// maxPage caps list page sizes; larger limits return short pages.
const maxPage = 1000

// Options configures a Store.
type Options struct {
	// Clock judges lease and batch expiry. Nil uses the system clock. Every
	// process sharing a database must use clocks that agree.
	Clock ports.Clock
}

// Store is a PostgreSQL store.Store. It does not own its pool: closing the
// pool is the host's job, after the last unit returned.
type Store struct {
	pool  *pgxpool.Pool
	clock ports.Clock
	// unplaced marks the tenants whose committed events await a placement
	// a unit of the tenant runs first (placement.go).
	unplaced owedPlacements
}

// Open returns a store on pool after checking that the database schema is the
// one this package embeds. It never migrates: a database without the schema,
// or with an older or newer one, fails with model.SchemaMismatch. Run
// Migrate first. Tenants holding committed events a stopped host left
// unplaced have them placed before the store's first unit of the tenant.
func Open(ctx context.Context, pool *pgxpool.Pool, o Options) (*Store, error) {
	if pool == nil {
		return nil, model.NewError(model.Validation, "pool: required")
	}
	if err := checkSchema(ctx, pool); err != nil {
		return nil, err
	}
	c := o.Clock
	if c == nil {
		c = systemClock{}
	}
	s := &Store{pool: pool, clock: c}
	if err := s.markUnplaced(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Properties reports the postgres backend, durable, and its schema version.
// The durable conformance suite backs the claim: committed units survive
// terminated connections and a killed server; units in flight leave no
// trace.
func (*Store) Properties() store.Properties {
	return store.Properties{Durable: true, SchemaVersion: SchemaVersion, Backend: "postgres"}
}

var (
	viewOptions     = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	transactOptions = pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadWrite}
	claimOptions    = pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadWrite}
)

// View runs fn once with a read-only Reader on a consistent snapshot of
// tenant's committed state.
func (s *Store) View(ctx context.Context, tenant model.TenantID, fn func(store.Reader) error) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.placeMarked(ctx, tenant)
	tx, err := s.pool.BeginTx(ctx, viewOptions)
	if err != nil {
		return classify(ctx, err)
	}
	h := s.open(tenant)
	h.begun(tx, false)
	// Closing the handle before rollback makes a retained Reader fail
	// instead of racing the rollback.
	defer func() {
		_ = h.close()
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()
	return fn(reader{h})
}

// Transact runs fn once in one transaction and commits when fn returns
// nil, no store call in the unit failed in the database and ctx is still
// live. The transaction begins at the unit's first store call: a unit whose
// first call is ClaimWork is a claim unit and runs at READ COMMITTED, every
// other unit SERIALIZABLE. A claim unit makes ClaimWork calls only. Once a
// unit that appended events commits, Transact places them (placement.go)
// before it returns, so the caller's next unit reads them.
func (s *Store) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	if err := tenant.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.placeMarked(ctx, tenant)
	h := s.open(tenant)
	h.begin = func(ctx context.Context, claim bool) (pgx.Tx, error) {
		if claim {
			return s.pool.BeginTx(ctx, claimOptions)
		}
		return s.pool.BeginTx(ctx, transactOptions)
	}
	committed := false
	// A panic in fn closes the handle and rolls back; the panic then
	// continues to the caller. The handle is closed before its transaction
	// is read, so no call can begin one afterwards.
	defer func() {
		_ = h.close()
		if h.tx != nil && !committed {
			_ = h.tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	if err := fn(txn{reader{h}}); err != nil {
		return err
	}
	if err := h.close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.tx == nil {
		// No store call: nothing to commit.
		return nil
	}
	unplaced := len(h.unplaced) > 0
	if err := h.tx.Commit(ctx); err != nil {
		if unplaced {
			// The commit may have landed: its events are owed a placement.
			s.unplaced.mark(tenant)
		}
		return commitError(ctx, err)
	}
	committed = true
	if unplaced {
		// The unit committed whatever the placement's outcome; a failed
		// placement is owed to the tenant's next unit.
		_ = s.placeCommitted(context.WithoutCancel(ctx), tenant)
	}
	return nil
}

func (s *Store) open(tenant model.TenantID) *handle {
	return &handle{tenant: tenant, tenantID: string(tenant), clock: s.clock}
}

// handle is the shared core of a Reader or Tx. Its mutex serializes calls on
// the unit's single connection; use from another goroutine is safe, not
// supported.
type handle struct {
	mu       sync.Mutex
	closed   bool
	tenant   model.TenantID
	tenantID string
	q        *db.Queries
	clock    ports.Clock
	// failed is the first database error in the unit. PostgreSQL aborts the
	// transaction on any statement error, so the unit cannot continue.
	failed error
	// begin opens a Transact unit's transaction at its first call, for a
	// claim unit when claim is set. tx is the open transaction; a View's
	// is open from the start.
	begin func(ctx context.Context, claim bool) (pgx.Tx, error)
	tx    pgx.Tx
	// claimUnit marks a unit begun by ClaimWork, at READ COMMITTED.
	claimUnit bool
	// unplaced lists the unit's own appended events still at placeholders;
	// placing is set once the unit placed events itself.
	unplaced []unplaced
	placing  bool
}

const errClosed = "store handle used after its callback returned"

// errClaimUnit refuses a call other than ClaimWork in a claim unit: it runs
// at READ COMMITTED, which only ClaimWork's queries are written for.
func errClaimUnit() error {
	return &model.Error{Code: model.Validation, Retry: model.NoRetry,
		SafeMessage: "unit began with ClaimWork: a claim unit makes ClaimWork calls only; run other calls in their own unit"}
}

// begun binds h to its open transaction.
func (h *handle) begun(tx pgx.Tx, claim bool) {
	h.tx, h.q, h.claimUnit = tx, db.New(tx), claim
}

// enter locks h for one call, a ClaimWork call when claim is set, and
// begins the unit's transaction at the first. On success the caller must
// call h.mu.Unlock.
func (h *handle) enter(ctx context.Context, claim bool) error {
	h.mu.Lock()
	switch {
	case h.closed:
		h.mu.Unlock()
		return model.NewError(model.Permanent, errClosed)
	case h.failed != nil:
		err := h.failed
		h.mu.Unlock()
		return err
	case h.claimUnit && !claim:
		h.mu.Unlock()
		return errClaimUnit()
	}
	if err := ctx.Err(); err != nil {
		h.mu.Unlock()
		return err
	}
	if h.tx == nil {
		tx, err := h.begin(ctx, claim)
		if err != nil {
			h.failed = classify(ctx, err)
			h.mu.Unlock()
			return h.failed
		}
		h.begun(tx, claim)
	}
	return nil
}

// close marks h closed and reports the unit's database failure, if any.
func (h *handle) close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	return h.failed
}

// db records a database error as the unit's failure and returns it
// classified. pgx.ErrNoRows never reaches here: lookups translate it first.
func (h *handle) db(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	e := classify(ctx, err)
	if h.failed == nil {
		h.failed = e
	}
	return e
}

func (h *handle) now() time.Time { return h.clock.Now() }

var (
	_ store.Store  = (*Store)(nil)
	_ store.Reader = reader{}
	_ store.Tx     = txn{}
)
