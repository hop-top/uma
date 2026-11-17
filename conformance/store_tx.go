package conformance

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var transactionCases = []storeCase{
	{"PropertiesWithoutIO", func(t *testing.T, e *env) {
		if p := e.s.Properties(); p.Backend == "" {
			t.Fatal("Properties().Backend is empty")
		}
	}},
	{"CommitIsVisible", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), "") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetChannel(e.ctx, ch)
			if err == nil && got.Revision != "r1" {
				t.Errorf("revision = %q, want r1", got.Revision)
			}
			return err
		})
	}},
	{"ReadYourWrites", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""); err != nil {
				return err
			}
			_, err := tx.GetChannel(e.ctx, ch)
			return err
		})
	}},
	{"CallbackRunsOnce", func(t *testing.T, e *env) {
		var runs int
		boom := errors.New("callback failed")
		err := e.s.Transact(e.ctx, tenantA, func(store.Tx) error { runs++; return boom })
		if !errors.Is(err, boom) || runs != 1 {
			t.Fatalf("err = %v, runs = %d; want callback error returned unchanged after one run", err, runs)
		}
		runs = 0
		e.mustTx(t, func(store.Tx) error { runs++; return nil })
		e.mustView(t, func(store.Reader) error { runs++; return nil })
		if runs != 2 {
			t.Fatalf("runs = %d, want 2", runs)
		}
	}},
	{"RollbackOnError", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		boom := errors.New("abort")
		err := e.txErr(func(tx store.Tx) error {
			if err := tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""); err != nil {
				return err
			}
			if err := tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.rolled_back")}); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want callback error", err)
		}
		e.requireAbsent(t, ch)
		e.requireNoEvents(t)
	}},
	{"RollbackOnPanic", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		var retained store.Tx
		func() {
			defer func() {
				if r := recover(); r != "boom" {
					t.Fatalf("recovered %v, want the callback's panic value", r)
				}
			}()
			_ = e.s.Transact(e.ctx, tenantA, func(tx store.Tx) error {
				retained = tx
				if err := tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""); err != nil {
					t.Errorf("PutChannel: %v", err)
				}
				panic("boom")
			})
		}()
		e.requireAbsent(t, ch)
		_, err := retained.GetChannel(e.ctx, ch)
		wantCode(t, err, model.Permanent)
	}},
	{"CanceledContextCommitsNothing", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		ctx, cancel := context.WithCancel(e.ctx)
		err := e.s.Transact(ctx, tenantA, func(tx store.Tx) error {
			if err := tx.PutChannel(ctx, e.channelRecord(ch, "r1"), ""); err != nil {
				return err
			}
			cancel()
			return nil
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		e.requireAbsent(t, ch)
	}},
	{"ViewReaderIsNotTx", func(t *testing.T, e *env) {
		e.mustView(t, func(r store.Reader) error {
			if _, ok := r.(store.Tx); ok {
				t.Error("View passed a Reader that implements store.Tx")
			}
			return nil
		})
	}},
	{"HandlesDoNotEscape", func(t *testing.T, e *env) {
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		var tx store.Tx
		var rd store.Reader
		e.mustTx(t, func(x store.Tx) error { tx = x; return nil })
		e.mustView(t, func(x store.Reader) error { rd = x; return nil })
		var failed store.Tx
		_ = e.txErr(func(x store.Tx) error { failed = x; return errors.New("abort") })

		wantCode(t, tx.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""), model.Permanent)
		wantCode(t, failed.PutChannel(e.ctx, e.channelRecord(ch, "r1"), ""), model.Permanent)
		_, err := tx.GetInbox(e.ctx, e.inbox)
		wantCode(t, err, model.Permanent)
		_, err = rd.GetInbox(e.ctx, e.inbox)
		wantCode(t, err, model.Permanent)
		_, err = tx.Events(e.ctx, model.Filter{}, "", 10)
		wantCode(t, err, model.Permanent)
		e.requireAbsent(t, ch)
	}},
	{"TenantIsolation", func(t *testing.T, e *env) {
		err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			_, err := r.GetInbox(e.ctx, e.inbox)
			return err
		})
		wantCode(t, err, model.NotFound)
		foreign := model.ChannelRef{Tenant: tenantB, ID: e.id(t)}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutChannel(e.ctx, e.channelRecord(foreign, "r1"), "")
		}), model.NotFound)
		// A record pointing at another tenant's channel is rejected too.
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.PutInbox(e.ctx, model.Inbox{
				Ref: model.InboxRef{Tenant: tenantA, ID: e.id(t)}, Channel: foreign, Revision: "r1",
			}, "")
		}), model.NotFound)
		// Same ID, other tenant: still not found.
		err = e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			_, err := r.GetInbox(e.ctx, model.InboxRef{Tenant: tenantB, ID: e.inbox.ID})
			return err
		})
		wantCode(t, err, model.NotFound)
	}},
	{"ConcurrentUnitsSerialize", func(t *testing.T, e *env) {
		// Concurrent read-modify-write units on one record: every unit either
		// commits on the revision it read or fails, so the revision chain has
		// no lost update.
		ch := model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutChannel(e.ctx, e.channelRecord(ch, "0"), "") })
		const workers = 8
		var commits atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				err := e.retryTx(func(tx store.Tx) error {
					cur, err := tx.GetChannel(e.ctx, ch)
					if err != nil {
						return err
					}
					next := cur
					next.Revision = cur.Revision + "+"
					return tx.PutChannel(e.ctx, next, cur.Revision)
				})
				if err == nil {
					commits.Add(1)
				} else if !model.IsCode(err, model.Conflict) {
					t.Errorf("unit: %v", err)
				}
			})
		}
		wg.Wait()
		e.mustView(t, func(r store.Reader) error {
			got, err := r.GetChannel(e.ctx, ch)
			if err == nil && int64(len(got.Revision)-1) != commits.Load() {
				t.Errorf("revision %q after %d commits: lost update", got.Revision, commits.Load())
			}
			return err
		})
	}},
}

func (e *env) requireAbsent(t *testing.T, ch model.ChannelRef) {
	t.Helper()
	err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
		_, err := r.GetChannel(e.ctx, ch)
		return err
	})
	wantCode(t, err, model.NotFound)
}

func (e *env) requireNoEvents(t *testing.T) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		p, err := r.Events(e.ctx, model.Filter{}, "", 100)
		if err == nil && len(p.Items) != 0 {
			t.Errorf("events = %d, want none", len(p.Items))
		}
		return err
	})
}
