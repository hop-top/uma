package postgres

import (
	"context"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

func errCursorExpired() error {
	return model.NewError(model.CursorExpired, "cursor: behind the retained event log")
}

func errSaturated() error {
	return model.NewError(model.Unavailable, "work kind at its in-flight cap")
}

// operationKind is the stored resource kind of operation references, which
// carry no inbox.
const operationKind = string(model.KindOperation)

func (r reader) EventBase(ctx context.Context) (model.Cursor, error) {
	var c model.Cursor
	err := r.h.call(ctx, func() error {
		base, err := r.h.q.EventBase(ctx, r.h.tenantID)
		if err != nil {
			return r.h.db(ctx, err)
		}
		c = encodeCursor(base)
		return nil
	})
	return c, err
}

func (r reader) WorkDepth(ctx context.Context, kind string) (store.WorkDepth, error) {
	var d store.WorkDepth
	err := r.h.call(ctx, func() error {
		if err := required("kind", kind); err != nil {
			return err
		}
		row, err := r.h.q.WorkDepth(ctx, db.WorkDepthParams{TenantID: r.h.tenantID, Kind: kind, Now: r.h.now()})
		if err != nil {
			return r.h.db(ctx, err)
		}
		d = store.WorkDepth{Pending: int(row.Pending), Claimed: int(row.Claimed), Quarantined: int(row.Quarantined)}
		return nil
	})
	return d, err
}

func (r reader) QuarantinedWork(ctx context.Context, res model.Ref) ([]store.Work, error) {
	var out []store.Work
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(res); err != nil {
			return err
		}
		kind, inbox, id := refArgs(res)
		rows, err := r.h.q.ListQuarantinedWork(ctx, db.ListQuarantinedWorkParams{
			TenantID: r.h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id,
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		for _, row := range rows {
			w := store.Work{ID: model.ID(row.ID), Kind: row.Kind, Resource: res, Claims: int(row.Claims)}
			if row.NotBefore != nil {
				w.NotBefore = *row.NotBefore
			}
			if row.FenceOwner != nil && row.FenceGeneration != nil {
				w.Fence = store.Fence{Owner: *row.FenceOwner, Generation: generation(*row.FenceGeneration)}
			}
			if row.Until != nil {
				w.Until = *row.Until
			}
			out = append(out, w)
		}
		return nil
	})
	return out, err
}

// claimedWork counts each kind's items holding an unexpired claim at now.
func (h *handle) claimedWork(ctx context.Context, kinds []string, now time.Time) (map[string]int, error) {
	rows, err := h.q.CountClaimedWork(ctx, db.CountClaimedWorkParams{TenantID: h.tenantID, Kinds: kinds, Now: now})
	if err != nil {
		return nil, h.db(ctx, err)
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.Kind] = int(row.Claimed)
	}
	return out, nil
}

func (t txn) QuarantineWork(ctx context.Context, w store.Work) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := w.ID.Validate(); err != nil {
			return err
		}
		row, err := h.q.GetWork(ctx, db.GetWorkParams{TenantID: h.tenantID, ID: string(w.ID)})
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		res := model.Ref{Tenant: h.tenant, Kind: model.ResourceKind(row.ResourceKind), ID: model.ID(row.ResourceID)}
		if row.ResourceInbox != nil {
			res.Inbox = model.ID(*row.ResourceInbox)
		}
		var stored store.Fence
		if row.FenceOwner != nil && row.FenceGeneration != nil {
			stored = store.Fence{Owner: *row.FenceOwner, Generation: generation(*row.FenceGeneration)}
		}
		lease, live, err := h.liveLease(ctx, res)
		if err != nil {
			return err
		}
		if !live || lease.Fence != w.Fence || stored != w.Fence || row.Quarantined {
			return conflict("fence is not the current lease")
		}
		if err := h.db(ctx, h.q.QuarantineWork(ctx, db.QuarantineWorkParams{TenantID: h.tenantID, ID: string(w.ID)})); err != nil {
			return err
		}
		kind, inbox, id := refArgs(res)
		_, err = h.q.ReleaseLease(ctx, db.ReleaseLeaseParams{
			TenantID: h.tenantID, ResourceKind: kind, ResourceInbox: inbox, ResourceID: id,
			Owner: w.Fence.Owner, Generation: storedGeneration(w.Fence.Generation),
		})
		return h.db(ctx, err)
	})
}

func (t txn) RequeueWork(ctx context.Context, id model.ID) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := id.Validate(); err != nil {
			return err
		}
		n, err := h.q.RequeueWork(ctx, db.RequeueWorkParams{TenantID: h.tenantID, ID: string(id)})
		if err != nil {
			return h.db(ctx, err)
		}
		if n == 1 {
			return nil
		}
		exists, err := h.q.WorkExists(ctx, db.WorkExistsParams{TenantID: h.tenantID, ID: string(id)})
		if err != nil {
			return h.db(ctx, err)
		}
		if !exists {
			return notFound()
		}
		return conflict("work is not quarantined")
	})
}

func (t txn) Purge(ctx context.Context, req store.PurgeRequest) (store.PurgeResult, error) {
	h := t.h
	var out store.PurgeResult
	err := h.call(ctx, func() error {
		if req.Before.IsZero() {
			return invalid("before: required")
		}
		if req.Limit <= 0 {
			return invalid("limit: must be positive")
		}
		lim, _ := pageLimit(req.Limit)
		var err error
		switch req.Family {
		case store.FamilyEvents:
			out, err = h.purgeEvents(ctx, req.Before, lim)
		case store.FamilyOperations:
			out, err = h.purgeOperations(ctx, req.Before, lim)
		case store.FamilyAttempts:
			out, err = h.purgeAttempts(ctx, req.Before, lim)
		case store.FamilyIngress:
			out, err = h.purgeIngress(ctx, req.Before, lim)
		case store.FamilyExports:
			out, err = h.purgeExports(ctx, req.Before, lim)
		case store.FamilyContacts:
			out, err = h.purgeContacts(ctx, req.Before, lim)
		case store.FamilyInteractionTokens:
			out, err = h.purgeInteractionTokens(ctx, req.Before, lim)
		default:
			return invalid("family: unknown")
		}
		return err
	})
	return out, err
}

// purgeEvents deletes the longest prefix of at most lim events written
// before before and records the last deleted position as the base. Rows
// are read one beyond the limit to report completeness.
func (h *handle) purgeEvents(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	rows, err := h.q.ListEventStamps(ctx, db.ListEventStampsParams{TenantID: h.tenantID, Lim: lim + 1})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	n := 0
	for n < len(rows) && n < int(lim) && rows[n].WrittenAt.Before(before) {
		n++
	}
	if n > 0 {
		through := rows[n-1].Position
		if err := h.db(ctx, h.q.DeleteEventsThrough(ctx, db.DeleteEventsThroughParams{TenantID: h.tenantID, Through: through})); err != nil {
			return store.PurgeResult{}, err
		}
		if err := h.db(ctx, h.q.SetEventBase(ctx, db.SetEventBaseParams{TenantID: h.tenantID, Base: through})); err != nil {
			return store.PurgeResult{}, err
		}
	}
	base, err := h.q.EventBase(ctx, h.tenantID)
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	more := n < len(rows) && rows[n].WrittenAt.Before(before)
	return store.PurgeResult{Purged: n, Complete: !more, EventBase: encodeCursor(base)}, nil
}

func (h *handle) purgeOperations(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	rows, err := h.q.ListPurgeableOperations(ctx, db.ListPurgeableOperationsParams{
		TenantID: h.tenantID, Before: before, ResourceKind: operationKind, Now: h.now(), Lim: lim + 1,
	})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	ids := make([]string, 0, len(rows))
	for i, row := range rows {
		if i == int(lim) {
			break
		}
		ids = append(ids, row.ID)
	}
	if len(ids) > 0 {
		if err := h.db(ctx, h.q.DeleteOperationAttempts(ctx, db.DeleteOperationAttemptsParams{TenantID: h.tenantID, Ids: ids})); err != nil {
			return store.PurgeResult{}, err
		}
		if err := h.db(ctx, h.q.DeleteOperations(ctx, db.DeleteOperationsParams{TenantID: h.tenantID, Ids: ids})); err != nil {
			return store.PurgeResult{}, err
		}
	}
	return store.PurgeResult{Purged: len(ids), Complete: len(rows) <= int(lim)}, nil
}

func (h *handle) purgeAttempts(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	rows, err := h.q.ListPurgeableAttempts(ctx, db.ListPurgeableAttemptsParams{
		TenantID: h.tenantID, Before: before, ResourceKind: operationKind, Now: h.now(), Lim: lim + 1,
	})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	seqs := make([]int64, 0, len(rows))
	for i, row := range rows {
		if i == int(lim) {
			break
		}
		seqs = append(seqs, row.Seq)
	}
	if len(seqs) > 0 {
		if err := h.db(ctx, h.q.DeleteAttempts(ctx, db.DeleteAttemptsParams{TenantID: h.tenantID, Seqs: seqs})); err != nil {
			return store.PurgeResult{}, err
		}
	}
	return store.PurgeResult{Purged: len(seqs), Complete: len(rows) <= int(lim)}, nil
}

func (h *handle) purgeIngress(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	rows, err := h.q.ListPurgeableIngress(ctx, db.ListPurgeableIngressParams{TenantID: h.tenantID, Before: before, Lim: lim + 1})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	ids := rows[:min(len(rows), int(lim))]
	if len(ids) > 0 {
		if err := h.db(ctx, h.q.DeleteIngress(ctx, db.DeleteIngressParams{TenantID: h.tenantID, Ids: ids})); err != nil {
			return store.PurgeResult{}, err
		}
	}
	return store.PurgeResult{Purged: len(ids), Complete: len(rows) <= int(lim)}, nil
}

// purgeContacts deletes up to lim contacts no identity points at whose
// latest link record leaving them was written before before.
func (h *handle) purgeContacts(ctx context.Context, before time.Time, lim int32) (store.PurgeResult, error) {
	rows, err := h.q.ListPurgeableContacts(ctx, db.ListPurgeableContactsParams{TenantID: h.tenantID, Before: before, Lim: lim + 1})
	if err != nil {
		return store.PurgeResult{}, h.db(ctx, err)
	}
	ids := rows[:min(len(rows), int(lim))]
	if len(ids) > 0 {
		if err := h.db(ctx, h.q.DeleteContacts(ctx, db.DeleteContactsParams{TenantID: h.tenantID, Ids: ids})); err != nil {
			return store.PurgeResult{}, err
		}
	}
	return store.PurgeResult{Purged: len(ids), Complete: len(rows) <= int(lim)}, nil
}
