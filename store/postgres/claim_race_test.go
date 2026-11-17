package postgres_test

import (
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// TestClaimKeyRaceIsTransient: a unit that found an operation key free and
// then loses it to a unit that committed in between fails with
// model.Transient, the serialization failure it is, not model.Conflict, the
// answer for a key reused with another payload. The outcome must not depend
// on which index the unit's read of the key took, so the ledger carries its
// list indexes here.
func TestClaimKeyRaceIsTransient(t *testing.T) {
	ctx := t.Context()
	st, err := postgres.Open(ctx, postgres.NewTestDatabase(t, true), postgres.Options{})
	if err != nil {
		t.Fatal(err)
	}
	in := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000b1"}
	record := func(id model.ID) store.OperationRecord {
		return store.OperationRecord{
			Operation: model.Operation{Ref: model.OperationRef{Tenant: tenant, ID: id}, State: model.Queued, Revision: "r1"},
			Key: store.OperationKey{
				Tenant: tenant, Caller: "actor", Kind: "messages.reply", Target: in.Ref(), ID: "same-key",
			},
			FingerprintVersion: 1, Fingerprint: []byte{1},
		}
	}
	err = st.Transact(ctx, tenant, func(tx store.Tx) error {
		if _, err := tx.OperationByKey(ctx, record("00000000-0000-7000-8000-000000000001").Key); model.CodeOf(err) != model.NotFound {
			t.Fatalf("OperationByKey = %v, want not found", err)
		}
		// Another unit claims the key and commits before this one writes.
		if err := st.Transact(ctx, tenant, func(other store.Tx) error {
			_, _, err := other.ClaimOperation(ctx, record("00000000-0000-7000-8000-000000000002"))
			return err
		}); err != nil {
			t.Fatalf("concurrent claim: %v", err)
		}
		_, _, err := tx.ClaimOperation(ctx, record("00000000-0000-7000-8000-000000000003"))
		return err
	})
	if got := model.CodeOf(err); got != model.Transient {
		t.Fatalf("claim that lost the race = %v (%v), want transient", got, err)
	}
	// Run again, the unit finds the claim and replays it.
	var created bool
	var got store.OperationRecord
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		var err error
		got, created, err = tx.ClaimOperation(ctx, record("00000000-0000-7000-8000-000000000003"))
		return err
	}); err != nil || created || got.Operation.Ref.ID != "00000000-0000-7000-8000-000000000002" {
		t.Fatalf("rerun = %v created=%v ref=%v, want the first claim replayed", err, created, got.Operation.Ref)
	}
}
