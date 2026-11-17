package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// stagingCases cover staging intents: recording before a blob is staged,
// binding on commit, clearing on cleanup and the sweep's abandonment past
// a horizon, which binding then refuses.
var stagingCases = []storeCase{
	{"RecordReadAndConflict", func(t *testing.T, e *env) {
		in := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) })
		if got := e.staging(t, in.Key); got != in {
			t.Fatalf("Staging = %+v, want %+v", got, in)
		}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) }), model.Conflict)
		again := e.intent(t)
		again.Key = in.Key
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, again) }), model.Conflict)
	}},
	{"RecordRejectsInvalidAndForeign", func(t *testing.T, e *env) {
		bad := []struct {
			name string
			edit func(*store.StagingIntent)
		}{
			{"malformed key id", func(in *store.StagingIntent) { in.Key.ID = "blob-1" }},
			{"no key tenant", func(in *store.StagingIntent) { in.Key.Tenant = "" }},
			{"already abandoned", func(in *store.StagingIntent) { in.Abandoned = true }},
			{"no attachment id", func(in *store.StagingIntent) { in.Attachment.ID = "" }},
		}
		for _, c := range bad {
			in := e.intent(t)
			c.edit(&in)
			wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) }), model.Validation, "RecordStaging of an intent with ", c.name)
		}
		foreignKey := e.intent(t)
		foreignKey.Key.Tenant = tenantB
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, foreignKey) }), model.NotFound)
		foreignAttachment := e.intent(t)
		foreignAttachment.Attachment.Inbox.Tenant = tenantB
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, foreignAttachment) }), model.NotFound)
	}},
	{"TenantScope", func(t *testing.T, e *env) {
		in := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) })
		err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			_, err := r.Staging(e.ctx, in.Key)
			return err
		})
		wantCode(t, err, model.NotFound)
		err = e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error { return tx.BindStaging(e.ctx, in.Key) })
		wantCode(t, err, model.NotFound)
		err = e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			got, err := tx.AbandonStaging(e.ctx, store.StagingSweep{Before: e.fx.Now().Add(time.Hour), Limit: 10})
			if err == nil && (len(got.Intents) != 0 || !got.Complete) {
				t.Errorf("tenant B sweep = %+v, want nothing", got)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := e.staging(t, in.Key); got.Abandoned {
			t.Fatal("another tenant's sweep abandoned the intent")
		}
	}},
	{"BindRemovesTheIntent", func(t *testing.T, e *env) {
		in := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) })
		e.mustTx(t, func(tx store.Tx) error { return tx.BindStaging(e.ctx, in.Key) })
		e.requireNoStaging(t, in.Key)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.BindStaging(e.ctx, in.Key) }), model.NotFound)
		// A bind rolled back with its unit leaves the intent.
		other := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, other) })
		err := e.txErr(func(tx store.Tx) error {
			if err := tx.BindStaging(e.ctx, other.Key); err != nil {
				return err
			}
			return model.NewError(model.Unavailable, "roll back")
		})
		wantCode(t, err, model.Unavailable)
		if got := e.staging(t, other.Key); got != other {
			t.Fatalf("intent after a rolled-back bind = %+v", got)
		}
	}},
	{"ClearIsIdempotent", func(t *testing.T, e *env) {
		in := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) })
		e.mustTx(t, func(tx store.Tx) error { return tx.ClearStaging(e.ctx, in.Key) })
		e.requireNoStaging(t, in.Key)
		e.mustTx(t, func(tx store.Tx) error { return tx.ClearStaging(e.ctx, in.Key) })
		wantCode(t, e.txErr(func(tx store.Tx) error {
			return tx.ClearStaging(e.ctx, ports.BlobKey{Tenant: tenantA, ID: "blob-1"})
		}), model.Validation)
	}},
	{"AbandonPastTheHorizonOnly", func(t *testing.T, e *env) {
		e.needClock(t)
		old := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, old) })
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		recent := e.intent(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, recent) })

		got := e.abandon(t, horizon, 10)
		abandoned := old
		abandoned.Abandoned = true
		if len(got.Intents) != 1 || got.Intents[0] != abandoned || !got.Complete {
			t.Fatalf("sweep = %+v, want only the old intent, abandoned, complete", got)
		}
		if s := e.staging(t, old.Key); !s.Abandoned {
			t.Fatalf("old intent after the sweep = %+v, want abandoned", s)
		}
		if s := e.staging(t, recent.Key); s != recent {
			t.Fatalf("recent intent after the sweep = %+v, want it untouched", s)
		}
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.BindStaging(e.ctx, old.Key) }), model.Conflict)
		if s := e.staging(t, old.Key); !s.Abandoned {
			t.Fatal("a refused bind removed the abandoned intent")
		}
		// An abandoned intent is found again until cleared, whatever the
		// horizon.
		if again := e.abandon(t, horizon.Add(-time.Hour), 10); len(again.Intents) != 1 || again.Intents[0] != abandoned {
			t.Fatalf("repeated sweep = %+v, want the abandoned intent again", again)
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.ClearStaging(e.ctx, old.Key) })
		if again := e.abandon(t, horizon, 10); len(again.Intents) != 0 || !again.Complete {
			t.Fatalf("sweep after clearing = %+v, want nothing", again)
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.BindStaging(e.ctx, recent.Key) })
		e.requireNoStaging(t, recent.Key)
	}},
	{"AbandonIsBoundedInRecordingOrder", func(t *testing.T, e *env) {
		e.needClock(t)
		var in []store.StagingIntent
		for range 3 {
			i := e.intent(t)
			e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, i) })
			in = append(in, i)
			e.fx.Advance(time.Second)
		}
		horizon := e.fx.Now()
		got := e.abandon(t, horizon, 2)
		if len(got.Intents) != 2 || got.Complete || got.Intents[0].Key != in[0].Key || got.Intents[1].Key != in[1].Key {
			t.Fatalf("bounded sweep = %+v, want the first two and more left", got)
		}
		got = e.abandon(t, horizon, 10)
		if len(got.Intents) != 3 || !got.Complete {
			t.Fatalf("sweep = %+v, want all three", got)
		}
		for i, s := range got.Intents {
			if s.Key != in[i].Key || !s.Abandoned {
				t.Errorf("intent %d (blob %s) = %s, want it abandoned", i, in[i].Key.ID, brief(s))
			}
		}
		for _, limit := range []int{0, -1} {
			wantCode(t, e.txErr(func(tx store.Tx) error {
				_, err := tx.AbandonStaging(e.ctx, store.StagingSweep{Before: horizon, Limit: limit})
				return err
			}), model.Validation, "AbandonStaging at limit ", limit)
		}
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, err := tx.AbandonStaging(e.ctx, store.StagingSweep{Limit: 1})
			return err
		}), model.Validation)
	}},
}

// intent returns a fresh staging intent for an attachment in the seeded
// inbox.
func (e *env) intent(t *testing.T) store.StagingIntent {
	t.Helper()
	return store.StagingIntent{
		Key:        ports.BlobKey{Tenant: tenantA, ID: e.id(t)},
		Attachment: model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)},
	}
}

func (e *env) staging(t *testing.T, key ports.BlobKey) store.StagingIntent {
	t.Helper()
	var got store.StagingIntent
	e.mustView(t, func(r store.Reader) error {
		var err error
		got, err = r.Staging(e.ctx, key)
		return err
	})
	return got
}

func (e *env) requireNoStaging(t *testing.T, key ports.BlobKey) {
	t.Helper()
	err := e.s.View(e.ctx, tenantA, func(r store.Reader) error {
		_, err := r.Staging(e.ctx, key)
		return err
	})
	wantCode(t, err, model.NotFound)
}

func (e *env) abandon(t *testing.T, before time.Time, limit int) store.StagingBatch {
	t.Helper()
	var got store.StagingBatch
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		got, err = tx.AbandonStaging(e.ctx, store.StagingSweep{Before: before, Limit: limit})
		return err
	})
	return got
}
