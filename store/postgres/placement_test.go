package postgres

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// strand commits ev at a placeholder in a transaction of its own, as a unit
// whose placement never ran leaves it: committed, not yet in the log.
func strand(t *testing.T, pool *pgxpool.Pool, ev model.Event) {
	t.Helper()
	body, err := encode(&ev)
	if err != nil {
		t.Fatal(err)
	}
	ru, rn := instant(ev.RecordedAt)
	if _, err := db.New(pool).InsertEvent(t.Context(), db.InsertEventParams{
		TenantID: string(ev.Tenant), ID: string(ev.ID), Kind: ev.Kind, InboxID: optID(ev.Inbox), Body: body,
		WrittenAt: time.Now(), ConversationID: optID(store.EventConversation(ev)), RecordedUnix: ru, RecordedNanos: rn,
	}); err != nil {
		t.Fatal(err)
	}
}

// unplacedCount counts the events of the database at placeholders.
func unplacedCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM uma_events WHERE position < 0").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPurgeKeepsUnplacedEvents: a committed event still at its placeholder
// is not in the log: Events does not read it and the head does not cover
// it. A purge of every event deletes the placed ones only; the stranded
// event, placed afterwards, follows the base.
func TestPurgeKeepsUnplacedEvents(t *testing.T) {
	r := newSeqRig(t, 4)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	placed, stranded := seqEvent(0xe5, 0, 0, t0), seqEvent(0xe5, 1, 0, t0.Add(time.Second))
	if _, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{placed}) }); err != nil {
		t.Fatal(err)
	}
	strand(t, r.pool, stranded)
	if got, next := r.readAll(t, model.Filter{}, "", 10); !slices.Equal(ids(got), []model.ID{placed.ID}) || next != encodeCursor(1) {
		t.Fatalf("log = %v through %q, want only the placed event", ids(got), next)
	}
	var res store.PurgeResult
	if _, err := r.unit(ctx, func(tx store.Tx) (err error) {
		res, err = tx.Purge(ctx, store.PurgeRequest{Family: store.FamilyEvents, Before: time.Now().Add(time.Hour), Limit: 10})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if res.Purged != 1 || !res.Complete || res.EventBase != encodeCursor(1) {
		t.Fatalf("purge = %+v, want the placed event alone", res)
	}
	if err := r.st.placeCommitted(ctx, seqTenant); err != nil {
		t.Fatal(err)
	}
	got, next := r.readAll(t, model.Filter{}, encodeCursor(1), 10)
	if !slices.Equal(ids(got), []model.ID{stranded.ID}) || next != encodeCursor(2) {
		t.Fatalf("log after placement = %v through %q, want the stranded event after the base", ids(got), next)
	}
}

// TestUnplacedEventsArePlacedBeforeTheNextUnit: committed events a
// placement never covered are placed before the tenant's next unit of a
// store opened on the database, and of the store whose placement failed.
func TestUnplacedEventsArePlacedBeforeTheNextUnit(t *testing.T) {
	r := newSeqRig(t, 4)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	first, second := seqEvent(0xe6, 0, 0, t0), seqEvent(0xe6, 1, 0, t0)
	strand(t, r.pool, first)
	reopened, err := Open(ctx, r.pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.View(ctx, seqTenant, func(rd store.Reader) error {
		p, err := rd.Events(ctx, model.Filter{}, "", 10)
		if err == nil && !slices.Equal(ids(p.Items), []model.ID{first.ID}) {
			t.Errorf("first view of a reopened store = %v, want the stranded event", ids(p.Items))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	strand(t, r.pool, second)
	r.st.unplaced.mark(seqTenant)
	if got, _ := r.readAll(t, model.Filter{}, encodeCursor(1), 10); !slices.Equal(ids(got), []model.ID{second.ID}) {
		t.Errorf("view after a failed placement = %v, want the stranded event", ids(got))
	}
	if r.st.unplaced.seen(seqTenant) != 0 || unplacedCount(t, r.pool) != 0 {
		t.Errorf("placement still owed after the view placed it")
	}
}

// TestOwedPlacementClearsOnlyEarlierMarks: a placement clears the mark it
// saw before it began, never one set while it ran.
func TestOwedPlacementClearsOnlyEarlierMarks(t *testing.T) {
	var o owedPlacements
	o.mark("t")
	seen := o.seen("t")
	o.mark("t") // a placement failing while another runs
	o.clear("t", seen)
	if o.seen("t") == 0 {
		t.Fatal("a later mark was cleared by a placement that began before it")
	}
	o.clear("t", o.seen("t"))
	if o.seen("t") != 0 {
		t.Fatal("mark not cleared")
	}
	o.clear("u", 0)
}

// TestDuplicateEventIDs: a duplicate of an event in the unit's snapshot is
// model.Conflict and the call appends nothing, the unit staying usable; a
// duplicate of an event a concurrent unit commits fails the unit as
// model.Transient, and its rerun is the conflict.
func TestDuplicateEventIDs(t *testing.T) {
	r := newSeqRig(t, 6)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	kept, fresh, other := seqEvent(0xe7, 0, 0, t0), seqEvent(0xe7, 1, 0, t0), seqEvent(0xe7, 2, 0, t0)
	if _, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{kept}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := r.unit(ctx, func(tx store.Tx) error {
		err := tx.AppendEvents(ctx, []model.Event{fresh, kept})
		if !model.IsCode(err, model.Conflict) {
			t.Errorf("append of a held ID = %v, want conflict", err)
		}
		return tx.AppendEvents(ctx, []model.Event{other})
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.readAll(t, model.Filter{}, "", 10); !slices.Equal(ids(got), []model.ID{kept.ID, other.ID}) {
		t.Fatalf("log = %v, want %s %s: the conflicting call appended nothing", ids(got), kept.ID, other.ID)
	}

	race := seqEvent(0xe7, 3, 0, t0)
	appended, release := make(chan struct{}), make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- r.st.Transact(ctx, seqTenant, func(tx store.Tx) error {
			if err := tx.AppendEvents(ctx, []model.Event{race}); err != nil {
				return err
			}
			close(appended)
			<-release
			return nil
		})
	}()
	<-appended
	lost := make(chan error, 1)
	go func() {
		lost <- r.st.Transact(ctx, seqTenant, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{race}) })
	}()
	time.Sleep(100 * time.Millisecond) // the second insert waits on the first
	close(release)
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	if err := <-lost; !model.IsCode(err, model.Transient) {
		t.Errorf("append racing a concurrent unit's ID = %v, want transient", err)
	}
	err := r.st.Transact(ctx, seqTenant, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{race}) })
	if !model.IsCode(err, model.Conflict) {
		t.Errorf("rerun = %v, want conflict", err)
	}
}

// TestUnitPlacingItselfKeepsItsEventsAdjacent: a unit that reads the head
// after appending places its events inside the unit and its later appends
// at once, so a concurrent appender's events never fall between them.
func TestUnitPlacingItselfKeepsItsEventsAdjacent(t *testing.T) {
	r := newSeqRig(t, 6)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	a1, a2, b := seqEvent(0xe8, 0, 0, t0), seqEvent(0xe8, 1, 0, t0), seqEvent(0xe8, 2, 1, t0)
	placed, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		first := true
		_, err := r.unit(ctx, func(tx store.Tx) error {
			if err := tx.AppendEvents(ctx, []model.Event{a1}); err != nil {
				return err
			}
			if _, err := tx.EventHead(ctx); err != nil {
				return err
			}
			if first {
				first = false
				close(placed)
				<-release
			}
			return tx.AppendEvents(ctx, []model.Event{a2})
		})
		done <- err
	}()
	<-placed
	other := make(chan error, 1)
	go func() {
		_, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{b}) })
		other <- err
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-other; err != nil {
		t.Fatal(err)
	}
	got, _ := r.readAll(t, model.Filter{}, "", 10)
	if !slices.Equal(ids(got), []model.ID{a1.ID, a2.ID, b.ID}) {
		t.Fatalf("log = %v, want %s %s adjacent, then %s", ids(got), a1.ID, a2.ID, b.ID)
	}
}

// TestEventPlaceholdersMigrateDown: migrating below schema 23 places the
// events still at placeholders after each tenant's head, unit by unit,
// continuing the running RecordedAt maximum; the log reads the same before
// and after, and migrating up again resumes appends.
func TestEventPlaceholdersMigrateDown(t *testing.T) {
	r := newSeqRig(t, 4)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	early := seqEvent(0xe9, 0, 0, t0.Add(9*time.Second))
	if _, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{early}) }); err != nil {
		t.Fatal(err)
	}
	// Two stranded units: the first of two events, the second of one.
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u1 := []model.Event{seqEvent(0xe9, 1, 0, t0.Add(2*time.Second)), seqEvent(0xe9, 2, 0, t0.Add(12*time.Second))}
	for _, ev := range u1 {
		body, err := encode(&ev)
		if err != nil {
			t.Fatal(err)
		}
		ru, rn := instant(ev.RecordedAt)
		if _, err := db.New(tx).InsertEvent(ctx, db.InsertEventParams{
			TenantID: string(seqTenant), ID: string(ev.ID), Kind: ev.Kind, InboxID: optID(ev.Inbox), Body: body,
			WrittenAt: time.Now(), RecordedUnix: ru, RecordedNanos: rn,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	u2 := seqEvent(0xe9, 3, 0, t0.Add(time.Second))
	strand(t, r.pool, u2)
	if err := withProvider(ctx, r.pool, migrations(), func(p *goose.Provider) error {
		_, err := p.DownTo(ctx, 22)
		return err
	}); err != nil {
		t.Fatalf("down: %v", err)
	}
	type row struct {
		pos, maxUnix int64
		id           string
	}
	rows, err := r.pool.Query(ctx, "SELECT position, recorded_max_unix, id::text FROM uma_events WHERE tenant_id = $1 ORDER BY position", string(seqTenant))
	if err != nil {
		t.Fatal(err)
	}
	var got []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.pos, &x.maxUnix, &x.id); err != nil {
			t.Fatal(err)
		}
		got = append(got, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	at := func(d int) int64 { return t0.Unix() + int64(d) }
	want := []row{
		{1, at(9), string(early.ID)}, {2, at(9), string(u1[0].ID)}, {3, at(12), string(u1[1].ID)}, {4, at(12), string(u2.ID)},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("log after down = %+v, want %+v", got, want)
	}
	var head, headMax int64
	if err := r.pool.QueryRow(ctx, "SELECT position, recorded_max_unix FROM uma_event_heads WHERE tenant_id = $1", string(seqTenant)).Scan(&head, &headMax); err != nil {
		t.Fatal(err)
	}
	if head != 4 || headMax != at(12) {
		t.Errorf("head after down = %d with maximum %d, want 4 with %d", head, headMax, at(12))
	}
	if err := Migrate(ctx, r.pool); err != nil {
		t.Fatalf("Migrate after down: %v", err)
	}
	st, err := Open(ctx, r.pool, Options{})
	if err != nil {
		t.Fatal(err)
	}
	late := seqEvent(0xe9, 4, 0, t0)
	if err := st.Transact(ctx, seqTenant, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{late}) }); err != nil {
		t.Fatal(err)
	}
	if err := st.View(ctx, seqTenant, func(rd store.Reader) error {
		p, err := rd.Events(ctx, model.Filter{}, encodeCursor(4), 10)
		if err == nil && (!slices.Equal(ids(p.Items), []model.ID{late.ID}) || p.Next != encodeCursor(5)) {
			t.Errorf("append after up = %v through %q, want %s at 5", ids(p.Items), p.Next, late.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestOverlappingPlacementsBothSucceed: a placement that starts while
// another is still committing waits for it and then places the events it
// left, instead of computing positions from the same head and failing.
// The first placement is held at its head row write; meanwhile a unit
// that began before the placed event's unit commits.
func TestOverlappingPlacementsBothSucceed(t *testing.T) {
	r := newSeqRig(t, 8)
	ctx := t.Context()
	t0 := time.Unix(1_700_000_000, 0).UTC()
	first, older, newer := seqEvent(0xea, 0, 0, t0), seqEvent(0xea, 1, 0, t0), seqEvent(0xea, 2, 0, t0)
	if _, err := r.unit(ctx, func(tx store.Tx) error { return tx.AppendEvents(ctx, []model.Event{first}) }); err != nil {
		t.Fatal(err)
	}
	// The older unit inserts first and commits last.
	txOlder, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txOlder.Rollback(ctx) }()
	body, err := encode(&older)
	if err != nil {
		t.Fatal(err)
	}
	ru, rn := instant(older.RecordedAt)
	if _, err := db.New(txOlder).InsertEvent(ctx, db.InsertEventParams{
		TenantID: string(seqTenant), ID: string(older.ID), Kind: older.Kind, InboxID: optID(older.Inbox), Body: body,
		WrittenAt: time.Now(), RecordedUnix: ru, RecordedNanos: rn,
	}); err != nil {
		t.Fatal(err)
	}
	strand(t, r.pool, newer)
	hold, err := r.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, "SELECT 1 FROM uma_event_heads WHERE tenant_id = $1 FOR UPDATE", string(seqTenant)); err != nil {
		t.Fatal(err)
	}
	p1, p2 := make(chan error, 1), make(chan error, 1)
	go func() { p1 <- r.st.placeCommitted(ctx, seqTenant) }()
	time.Sleep(100 * time.Millisecond) // p1 placed newer and waits on the head row
	if err := txOlder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { p2 <- r.st.placeCommitted(ctx, seqTenant) }()
	time.Sleep(100 * time.Millisecond)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-p1; err != nil {
		t.Errorf("first placement: %v", err)
	}
	if err := <-p2; err != nil {
		t.Errorf("overlapping placement: %v", err)
	}
	if n := unplacedCount(t, r.pool); n != 0 {
		t.Errorf("%d events left at placeholders after both placements", n)
	}
	all, next := r.readAll(t, model.Filter{}, "", 10)
	if !slices.Equal(ids(all), []model.ID{first.ID, newer.ID, older.ID}) || next != encodeCursor(3) {
		t.Errorf("log = %v through %q, want %s %s %s", ids(all), next, first.ID, newer.ID, older.ID)
	}
}
