package postgres_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// TestOperationBodyKeepsNoWriteTime: the write time a read shows is the
// written_at column. Whatever UpdatedAt a writer hands the store, the stored
// body holds none, so no second copy can go stale beside the column.
func TestOperationBodyKeepsNoWriteTime(t *testing.T) {
	ctx := t.Context()
	pool := postgres.NewTestDatabase(t, true)
	st, err := postgres.Open(ctx, pool, postgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	lie := time.Unix(1_000, 0).UTC()
	in := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000b2"}
	rec := store.OperationRecord{
		Operation: model.Operation{
			Ref: model.OperationRef{Tenant: tenant, ID: "00000000-0000-7000-8000-000000000011"}, State: model.Queued,
			Revision: "r1", UpdatedAt: lie,
		},
		Key:                store.OperationKey{Tenant: tenant, Caller: "actor", Kind: "messages.reply", Target: in.Ref(), ID: "k"},
		FingerprintVersion: 1, Fingerprint: []byte{1},
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		_, _, err := tx.ClaimOperation(ctx, rec)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	stored := func(when string) {
		t.Helper()
		var body []byte
		if err := pool.QueryRow(ctx, `SELECT body FROM uma_operations WHERE id = $1`, string(rec.Operation.Ref.ID)).Scan(&body); err != nil {
			t.Fatal(err)
		}
		var got store.OperationRecord
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if !got.Operation.UpdatedAt.IsZero() {
			t.Errorf("%s: stored body holds UpdatedAt %v, want none", when, got.Operation.UpdatedAt)
		}
	}
	stored("after the claim")
	next := rec.Operation
	next.State, next.Revision, next.UpdatedAt = model.Delivered, "r2", lie
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		return tx.UpdateOperation(ctx, next, "r1", store.Fence{})
	}); err != nil {
		t.Fatal(err)
	}
	stored("after an update")
}
