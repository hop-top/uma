package postgres

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

func (r reader) Export(ctx context.Context, ref model.ExportRef) (store.ExportRecord, error) {
	return getBody[store.ExportRecord](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetExport(ctx, db.GetExportParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		return row.Body, err
	})
}

func (t txn) PutExport(ctx context.Context, v store.ExportRecord, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		ref := v.Export.Ref
		if err := firstErr(h.scoped(ref), h.checkKey(v.Blob)); err != nil {
			return err
		}
		if !v.Operation.IsZero() {
			if err := h.scoped(v.Operation); err != nil {
				return err
			}
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		rev, id := v.Export.Revision, string(ref.ID)
		return h.put(ctx, rev, expected,
			func() (string, error) {
				row, err := h.q.GetExport(ctx, db.GetExportParams{TenantID: h.tenantID, ID: id})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertExport(ctx, db.InsertExportParams{
					TenantID: h.tenantID, ID: id, Body: body, Revision: string(rev), WrittenAt: h.now(),
				})
			},
			func() (int64, error) {
				return h.q.UpdateExport(ctx, db.UpdateExportParams{
					TenantID: h.tenantID, ID: id, Body: body, Revision: string(rev), Expected: string(expected),
				})
			})
	})
}

// purgeExports deletes up to lim exports first written before before and
// records a staging intent for each one's blob key.
func (h *handle) purgeExports(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	rows, err := h.q.ListPurgeableExports(ctx, db.ListPurgeableExportsParams{TenantID: h.tenantID, Before: before, Lim: lim + 1})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	res := store.PurgeResult{Complete: len(rows) <= int(lim)}
	rows = rows[:min(len(rows), int(lim))]
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		rec, err := decode[store.ExportRecord](row.Body)
		if err != nil {
			return store.PurgeResult{}, err
		}
		if _, err := h.q.InsertStaging(ctx, h.stagingParams(store.StagingIntent{Key: rec.Blob, Export: rec.Export.Ref})); err != nil {
			return store.PurgeResult{}, h.db(ctx, err)
		}
		ids = append(ids, row.ID)
		res.Blobs = append(res.Blobs, rec.Blob)
	}
	if len(ids) > 0 {
		if err := h.db(ctx, h.q.DeleteExports(ctx, db.DeleteExportsParams{TenantID: h.tenantID, Ids: ids})); err != nil {
			return store.PurgeResult{}, err
		}
	}
	res.Purged = len(ids)
	return res, nil
}
