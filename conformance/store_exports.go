package conformance

import (
	"reflect"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// exportCases cover export records: compare-and-set writes, tenant scope,
// staging intents owned by an export, and the exports purge, which hands
// each deleted export's bytes to the staging sweep.
var exportCases = []storeCase{
	{"ExportRecordLifecycle", func(t *testing.T, e *env) {
		rec := e.exportRecord(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutExport(e.ctx, rec, "") })
		e.wantExport(t, rec)
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutExport(e.ctx, rec, "") }), model.Conflict)
		next := rec.Clone()
		next.Export.Revision = "x2"
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutExport(e.ctx, next, "x0") }), model.Conflict)
		e.wantExport(t, rec)

		foreignBlob := e.exportRecord(t)
		foreignBlob.Blob.Tenant = tenantB
		if err := e.txErr(func(tx store.Tx) error { return tx.PutExport(e.ctx, foreignBlob, "") }); err == nil {
			t.Fatal("export bound to another tenant's blob was stored")
		}
		foreign := e.exportRecord(t)
		foreign.Export.Ref.Tenant = tenantB
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutExport(e.ctx, foreign, "") }), model.NotFound)
		e.wantForeign(t, func(r store.Reader) error { _, err := r.Export(e.ctx, rec.Export.Ref); return err })
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Export(e.ctx, model.ExportRef{Tenant: tenantA, ID: e.id(t)})
			wantCode(t, err, model.NotFound)
			return nil
		})
	}},
	{"StagingIntentOwnedByAnExport", func(t *testing.T, e *env) {
		in := store.StagingIntent{
			Key:    ports.BlobKey{Tenant: tenantA, ID: e.id(t)},
			Export: model.ExportRef{Tenant: tenantA, ID: e.id(t)},
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.RecordStaging(e.ctx, in) })
		if got := e.staging(t, in.Key); got != in {
			t.Fatalf("Staging = %+v, want %+v", got, in)
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.BindStaging(e.ctx, in.Key) })
		e.requireNoStaging(t, in.Key)

		both := e.intent(t)
		both.Export = in.Export
		neither := e.intent(t)
		neither.Attachment = model.AttachmentRef{}
		for _, bad := range []struct {
			name string
			in   store.StagingIntent
		}{
			{"intent naming both an attachment and an export", both},
			{"intent naming neither an attachment nor an export", neither},
		} {
			wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, bad.in) }), model.Validation, bad.name)
		}
		foreign := in
		foreign.Key.ID, foreign.Export.Tenant = e.id(t), tenantB
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.RecordStaging(e.ctx, foreign) }), model.NotFound)
	}},
	{"ExportsPurgeHandTheirBytesToTheSweep", func(t *testing.T, e *env) {
		e.needClock(t)
		old := e.exportRecord(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutExport(e.ctx, old, "") })
		horizon := e.fx.Now().Add(time.Second)
		e.fx.Advance(time.Minute)
		recent := e.exportRecord(t)
		e.mustTx(t, func(tx store.Tx) error { return tx.PutExport(e.ctx, recent, "") })

		got := e.purge(t, store.FamilyExports, horizon, 10)
		if got.Purged != 1 || !got.Complete || !reflect.DeepEqual(got.Blobs, []ports.BlobKey{old.Blob}) {
			t.Fatalf("purge = %+v, want the old export and its blob key", got)
		}
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Export(e.ctx, old.Export.Ref)
			wantCode(t, err, model.NotFound)
			_, err = r.Export(e.ctx, recent.Export.Ref)
			return err
		})
		if in := e.staging(t, old.Blob); in.Export != old.Export.Ref || in.Abandoned || !in.Attachment.IsZero() {
			t.Errorf("staging intent = %+v, want one owned by the purged export", in)
		}
		if again := e.purge(t, store.FamilyExports, horizon, 10); again.Purged != 0 || !again.Complete || len(again.Blobs) != 0 {
			t.Errorf("repeated purge = %+v, want nothing", again)
		}
	}},
}

func (e *env) exportRecord(t *testing.T) store.ExportRecord {
	t.Helper()
	contact := model.ContactRef{Tenant: tenantA, ID: e.id(t)}
	return store.ExportRecord{
		Export: model.Export{
			Ref: model.ExportRef{Tenant: tenantA, ID: e.id(t)}, Filter: model.Filter{Contact: &contact, Inboxes: []model.InboxRef{e.inbox}},
			Format: model.ExportFormatJSONL, Size: 42, Digest: "sha256:" + digestHex,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Revision: "x1",
		},
		Blob:      ports.BlobKey{Tenant: tenantA, ID: e.id(t)},
		Operation: model.OperationRef{Tenant: tenantA, ID: e.id(t)},
		Creator:   "staff-1",
	}
}

func (e *env) wantExport(t *testing.T, want store.ExportRecord) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.Export(e.ctx, want.Export.Ref)
		if err == nil && !reflect.DeepEqual(got, want) {
			wantEqual(t, "export record "+string(want.Export.Ref.ID), got, want)
		}
		return err
	})
}
