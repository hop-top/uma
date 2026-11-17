package postgres

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// checkKey validates a blob key and requires it in the unit's tenant.
func (h *handle) checkKey(k ports.BlobKey) error {
	if err := k.Validate(); err != nil {
		return err
	}
	return h.sameTenant(k.Tenant)
}

func (h *handle) intent(k ports.BlobKey, inbox, attachment, export *string, abandoned bool) store.StagingIntent {
	in := store.StagingIntent{Key: k, Abandoned: abandoned}
	if export != nil {
		in.Export = model.ExportRef{Tenant: h.tenant, ID: model.ID(*export)}
		return in
	}
	in.Attachment = model.AttachmentRef{
		Inbox: model.InboxRef{Tenant: h.tenant, ID: model.ID(deref(inbox))}, ID: model.ID(deref(attachment)),
	}
	return in
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// checkOwner requires exactly one owner, in the unit's tenant.
func (h *handle) checkOwner(in store.StagingIntent) error {
	switch {
	case in.Attachment.IsZero() == in.Export.IsZero():
		return invalid("staging intent: exactly one of attachment and export required")
	case !in.Export.IsZero():
		return h.scoped(in.Export)
	}
	return h.scoped(in.Attachment)
}

// stagingParams is the insert of in's row.
func (h *handle) stagingParams(in store.StagingIntent) db.InsertStagingParams {
	p := db.InsertStagingParams{TenantID: h.tenantID, BlobID: string(in.Key.ID), WrittenAt: h.now()}
	if !in.Export.IsZero() {
		id := string(in.Export.ID)
		p.ExportID = &id
		return p
	}
	inbox, att := string(in.Attachment.Inbox.ID), string(in.Attachment.ID)
	p.InboxID, p.AttachmentID = &inbox, &att
	return p
}

func (r reader) Staging(ctx context.Context, k ports.BlobKey) (store.StagingIntent, error) {
	h := r.h
	var out store.StagingIntent
	err := h.call(ctx, func() error {
		if err := h.checkKey(k); err != nil {
			return err
		}
		row, err := h.q.GetStaging(ctx, db.GetStagingParams{TenantID: h.tenantID, BlobID: string(k.ID)})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		out = h.intent(k, row.InboxID, row.AttachmentID, row.ExportID, row.Abandoned)
		return nil
	})
	return out, err
}

func (t txn) RecordStaging(ctx context.Context, in store.StagingIntent) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.checkKey(in.Key), h.checkOwner(in)); err != nil {
			return err
		}
		if in.Abandoned {
			return invalid("abandoned: a recorded intent is not abandoned")
		}
		n, err := h.q.InsertStaging(ctx, h.stagingParams(in))
		if err != nil {
			return h.db(ctx, err)
		}
		if n == 0 {
			return conflict("blob key already recorded")
		}
		return nil
	})
}

func (t txn) BindStaging(ctx context.Context, k ports.BlobKey) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.checkKey(k); err != nil {
			return err
		}
		n, err := h.q.BindStaging(ctx, db.BindStagingParams{TenantID: h.tenantID, BlobID: string(k.ID)})
		if err != nil {
			return h.db(ctx, err)
		}
		if n == 1 {
			return nil
		}
		if _, err := h.q.GetStaging(ctx, db.GetStagingParams{TenantID: h.tenantID, BlobID: string(k.ID)}); err != nil {
			return h.lookupErr(ctx, err)
		}
		return conflict("staging intent abandoned")
	})
}

func (t txn) ClearStaging(ctx context.Context, k ports.BlobKey) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.checkKey(k); err != nil {
			return err
		}
		return h.db(ctx, h.q.DeleteStaging(ctx, db.DeleteStagingParams{TenantID: h.tenantID, BlobID: string(k.ID)}))
	})
}

func (t txn) AbandonStaging(ctx context.Context, sw store.StagingSweep) (store.StagingBatch, error) {
	h := t.h
	var out store.StagingBatch
	err := h.call(ctx, func() error {
		if sw.Before.IsZero() {
			return invalid("before: required")
		}
		if sw.Limit <= 0 {
			return invalid("limit: must be positive")
		}
		lim, _ := pageLimit(sw.Limit)
		rows, err := h.q.ListStagingSweep(ctx, db.ListStagingSweepParams{TenantID: h.tenantID, Before: sw.Before, Lim: lim + 1})
		if err != nil {
			return h.db(ctx, err)
		}
		out = store.StagingBatch{Complete: len(rows) <= int(lim)}
		rows = rows[:min(len(rows), int(lim))]
		if len(rows) == 0 {
			return nil
		}
		ids := make([]string, len(rows))
		for i, row := range rows {
			ids[i] = row.BlobID
			k := ports.BlobKey{Tenant: h.tenant, ID: model.ID(row.BlobID)}
			out.Intents = append(out.Intents, h.intent(k, row.InboxID, row.AttachmentID, row.ExportID, true))
		}
		return h.db(ctx, h.q.AbandonStaging(ctx, db.AbandonStagingParams{TenantID: h.tenantID, Ids: ids}))
	})
	return out, err
}
