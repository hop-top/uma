package postgres

import (
	"context"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

const isolationTenant model.TenantID = "postgres-isolation-tenant"

// isolationOf reports the isolation level of the unit tx runs in.
func isolationOf(ctx context.Context, t *testing.T, tx store.Tx) string {
	t.Helper()
	var level string
	if err := tx.(txn).h.tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level); err != nil {
		t.Fatalf("SHOW transaction_isolation: %v", err)
	}
	return level
}

// TestUnitIsolationFollowsFirstCall: a unit whose first call is ClaimWork
// runs at READ COMMITTED and refuses any other call, rolling back; every
// other unit, including one that claims after another call, is
// SERIALIZABLE. A unit with no store call commits nothing and succeeds.
func TestUnitIsolationFollowsFirstCall(t *testing.T) {
	ctx := t.Context()
	st, err := Open(ctx, NewTestDatabase(t, true), Options{})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(tx store.Tx) error {
		_, err := tx.ClaimWork(ctx, store.WorkClaim{
			Kinds: []string{"postgres.test.isolation"}, Limit: 1, Owner: "w", Until: time.Now().Add(time.Minute),
		})
		return err
	}
	work := store.Work{
		ID: "00000000-0000-7000-8000-0000000000a1", Kind: "postgres.test.isolation",
		Resource: model.OperationRef{Tenant: isolationTenant, ID: "00000000-0000-7000-8000-0000000000a2"}.Ref(),
	}

	var level string
	if err := st.Transact(ctx, isolationTenant, func(tx store.Tx) error {
		if err := claim(tx); err != nil {
			return err
		}
		level = isolationOf(ctx, t, tx)
		return claim(tx)
	}); err != nil {
		t.Fatalf("claim unit: %v", err)
	}
	if level != "read committed" {
		t.Errorf("claim unit isolation = %q, want read committed", level)
	}

	err = st.Transact(ctx, isolationTenant, func(tx store.Tx) error {
		if err := claim(tx); err != nil {
			return err
		}
		return tx.EnqueueWork(ctx, work)
	})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("write in a claim unit = %v, want validation", err)
	}

	if err := st.Transact(ctx, isolationTenant, func(tx store.Tx) error {
		if err := tx.EnqueueWork(ctx, work); err != nil {
			return err
		}
		level = isolationOf(ctx, t, tx)
		return claim(tx)
	}); err != nil {
		t.Fatalf("write then claim: %v", err)
	}
	if level != "serializable" {
		t.Errorf("write unit isolation = %q, want serializable", level)
	}

	if err := st.Transact(ctx, isolationTenant, func(store.Tx) error { return nil }); err != nil {
		t.Fatalf("empty unit: %v", err)
	}
}
