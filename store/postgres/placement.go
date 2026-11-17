package postgres

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

// Event placement. AppendEvents inserts a unit's events at placeholders,
// negative positions no read selects, and touches no row other appenders
// write: units appending to one tenant at SERIALIZABLE no longer fail one
// another on the tenant's head row. Positions are assigned by placement,
// always under the tenant's placement lock and together with the head row
// in one transaction, so the head covers assigned positions only and a
// reader bounded by it never moves past an event that is placed later:
//
//   - After a unit commits, placeCommitted places every committed event of
//     the tenant still at a placeholder, its own included, in a READ
//     COMMITTED transaction sent as one batch: unit by unit, a unit's
//     events adjacent and in append order. Events are placed in the order
//     they become readable; one placement often covers several units.
//   - A unit that reads the log after appending (Events, EventHead,
//     AdvanceCheckpoint) places its own events first, inside the unit,
//     from the head row as of its snapshot: a placement committed since
//     fails the unit as a serialization failure, and its rerun reads the
//     newer head. Holding the placement lock to commit, it places its
//     later appends at once, so a unit's events stay adjacent.
//
// A placement that fails after its unit committed leaves the events
// committed but unread. The store marks the tenant and places its events
// before its next unit of the tenant; Open marks every tenant holding
// placeholders, for a host that stopped between commit and placement; and
// every placement of the tenant, by any process, covers them.

// placeTimeout bounds a placement after commit, which runs even when the
// unit's context ended with it.
const placeTimeout = 10 * time.Second

// unplaced is one of a unit's own events still at its placeholder.
type unplaced struct {
	placeholder int64
	recordedAt  time.Time
}

// placeOwn places the unit's own events still at placeholders, after the
// head in the unit's snapshot, in append order, continuing the running
// RecordedAt maximum. Its reads of the log then include them, as the store
// contract requires. Placement takes the tenant's placement lock to commit,
// as every appender took the head row before placeholders.
func (h *handle) placeOwn(ctx context.Context) error {
	evs := h.unplaced
	if len(evs) == 0 {
		return nil
	}
	if err := h.db(ctx, h.q.LockEventPlacement(ctx, h.tenantID)); err != nil {
		return err
	}
	head, err := h.q.AdvanceEventHead(ctx, db.AdvanceEventHeadParams{TenantID: h.tenantID, N: int64(len(evs))})
	if err != nil {
		return h.db(ctx, err)
	}
	latest := evs[0].recordedAt
	for _, e := range evs[1:] {
		if e.recordedAt.After(latest) {
			latest = e.recordedAt
		}
	}
	lu, ln := instant(latest)
	prior, err := h.q.RaiseEventRecordedMax(ctx, db.RaiseEventRecordedMaxParams{TenantID: h.tenantID, MaxUnix: lu, MaxNanos: ln})
	if err != nil {
		return h.db(ctx, err)
	}
	// recordedMax is the running maximum of RecordedAt along the log,
	// continued from the head row's.
	recordedMax := time.Unix(prior.RecordedMaxUnix, int64(prior.RecordedMaxNanos))
	arg := db.PlaceEventsParams{TenantID: h.tenantID}
	pos := head - int64(len(evs))
	for _, e := range evs {
		pos++
		if e.recordedAt.After(recordedMax) {
			recordedMax = e.recordedAt
		}
		mu, mn := instant(recordedMax)
		arg.Placeholders = append(arg.Placeholders, e.placeholder)
		arg.Positions = append(arg.Positions, pos)
		arg.MaxUnix = append(arg.MaxUnix, mu)
		arg.MaxNanos = append(arg.MaxNanos, mn)
	}
	if err := h.db(ctx, h.q.PlaceEvents(ctx, arg)); err != nil {
		return err
	}
	h.unplaced, h.placing = nil, true
	return nil
}

// placeCommitted places every committed event of tenant still at a
// placeholder: lock, place and commit in one round trip. The batch begins
// its own READ COMMITTED transaction whatever the session default, so the
// placing statement's snapshot is taken after the lock is held. A failure
// marks the tenant for its next unit.
func (s *Store) placeCommitted(ctx context.Context, tenant model.TenantID) error {
	seen := s.unplaced.seen(tenant)
	ctx, cancel := context.WithTimeout(ctx, placeTimeout)
	defer cancel()
	b := &pgx.Batch{}
	b.Queue("BEGIN ISOLATION LEVEL READ COMMITTED")
	q := db.New(batchQueue{b})
	// Queuing never fails; the batch reports the statements' errors.
	_ = q.LockEventPlacement(ctx, string(tenant))
	_ = q.PlaceCommittedEvents(ctx, string(tenant))
	b.Queue("COMMIT")
	if err := s.pool.SendBatch(ctx, b).Close(); err != nil {
		s.unplaced.mark(tenant)
		return classify(ctx, err)
	}
	s.unplaced.clear(tenant, seen)
	return nil
}

// placeMarked places tenant's events when a placement is owed: a unit of
// tenant runs it before it begins, so it reads the events a failed
// placement left unread. A failure leaves the mark for the next unit.
func (s *Store) placeMarked(ctx context.Context, tenant model.TenantID) {
	if s.unplaced.seen(tenant) != 0 {
		_ = s.placeCommitted(ctx, tenant)
	}
}

// markUnplaced marks every tenant holding events at placeholders.
func (s *Store) markUnplaced(ctx context.Context) error {
	tenants, err := db.New(s.pool).UnplacedTenants(ctx)
	if err != nil {
		return classify(ctx, err)
	}
	for _, t := range tenants {
		s.unplaced.mark(model.TenantID(t))
	}
	return nil
}

// owedPlacements records the tenants whose committed events await a
// placement. A mark carries the sequence number it was set at, so a
// placement clears only marks set before it began: a placement that
// started earlier may not cover the events of a later mark.
type owedPlacements struct {
	marks sync.Map // model.TenantID -> uint64
	seq   atomic.Uint64
}

func (o *owedPlacements) mark(t model.TenantID) { o.marks.Store(t, o.seq.Add(1)) }

// seen returns tenant's current mark, 0 when none.
func (o *owedPlacements) seen(t model.TenantID) uint64 {
	v, ok := o.marks.Load(t)
	if !ok {
		return 0
	}
	n, _ := v.(uint64)
	return n
}

func (o *owedPlacements) clear(t model.TenantID, seen uint64) {
	if seen != 0 {
		o.marks.CompareAndDelete(t, seen)
	}
}

// batchQueue is a db.DBTX that queues each statement on a batch instead of
// running it, so generated statements without results travel in one round
// trip. Statements returning rows cannot be queued.
type batchQueue struct{ b *pgx.Batch }

var errQueued = errors.New("postgres: a statement returning rows cannot be queued")

func (q batchQueue) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q.b.Queue(sql, args...)
	return pgconn.CommandTag{}, nil
}

func (batchQueue) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errQueued }

func (batchQueue) QueryRow(context.Context, string, ...any) pgx.Row { return queuedRow{} }

type queuedRow struct{}

func (queuedRow) Scan(...any) error { return errQueued }
