package postgres

import (
	"context"

	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// checkOperationKey validates an operation key and scopes it to the unit's
// tenant.
func (h *handle) checkOperationKey(k store.OperationKey) error {
	return firstErr(
		h.scoped(k.Target),
		h.sameTenant(k.Tenant),
		k.Caller.Validate(),
		required("key.kind", k.Kind),
		k.ID.Validate(),
	)
}

// operationByKey reads the operation claimed under a checked key.
func (h *handle) operationByKey(ctx context.Context, k store.OperationKey) (db.GetOperationByKeyRow, error) {
	return h.q.GetOperationByKey(ctx, db.GetOperationByKeyParams{
		TenantID: h.tenantID, Caller: string(k.Caller), Kind: k.Kind, TargetKind: string(k.Target.Kind),
		TargetInbox: optID(k.Target.Inbox), TargetID: string(k.Target.ID), OperationKey: string(k.ID),
	})
}

func (r reader) OperationByKey(ctx context.Context, k store.OperationKey) (store.OperationRecord, error) {
	var out store.OperationRecord
	err := r.h.call(ctx, func() error {
		if err := r.h.checkOperationKey(k); err != nil {
			return err
		}
		row, err := r.h.operationByKey(ctx, k)
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out, err = decodeOperation(row.Body, row.WrittenAt)
		return err
	})
	return out, err
}
