package postgres_test

import (
	"reflect"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A status report's receipt names the operation the report reached. The
// store returns it unchanged to a repeated claim, tells it apart from a
// receipt without it, and refuses one in another tenant.
func TestIngressReceiptKeepsItsOperation(t *testing.T) {
	ctx := t.Context()
	st := fixture(t).Store
	const tenant model.TenantID = "receipt-operation"
	ch := model.ChannelRef{Tenant: tenant, ID: "00000001-0000-7000-8000-0000000000c1"}
	in := store.IngressRecord{ID: "00000001-0000-7000-8000-0000000000d1", Channel: ch, DedupKey: "evt-1", VerifierVersion: "v1"}
	op := model.OperationRef{Tenant: tenant, ID: "00000001-0000-7000-8000-0000000000e1"}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{Ref: ch, Type: "test.chat", Revision: "r1"}, ""); err != nil {
			return err
		}
		_, _, err := tx.ClaimIngress(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rc := model.IngressReceipt{ID: in.ID, Durable: true, Committed: true, Operation: &op}
	complete := func(r model.IngressReceipt) error {
		return st.Transact(ctx, tenant, func(tx store.Tx) error { return tx.CompleteIngress(ctx, in.ID, r) })
	}
	if err := complete(rc); err != nil {
		t.Fatalf("CompleteIngress: %v", err)
	}
	if err := complete(rc.Clone()); err != nil {
		t.Fatalf("CompleteIngress with the equal receipt: %v", err)
	}
	if err := complete(model.IngressReceipt{ID: in.ID, Durable: true, Committed: true}); model.CodeOf(err) != model.Conflict {
		t.Fatalf("CompleteIngress with a receipt naming no operation = %v, want a conflict", err)
	}
	var got store.IngressRecord
	var created bool
	if err := st.Transact(ctx, tenant, func(tx store.Tx) (err error) {
		got, created, err = tx.ClaimIngress(ctx, in)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if created || !reflect.DeepEqual(got.Receipt, rc) {
		t.Fatalf("repeated claim = %+v (created %v), want the stored receipt %+v", got.Receipt, created, rc)
	}

	foreign := model.OperationRef{Tenant: "another-tenant", ID: op.ID}
	other := store.IngressRecord{ID: "00000001-0000-7000-8000-0000000000d2", Channel: ch, DedupKey: "evt-2", VerifierVersion: "v1"}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		if _, _, err := tx.ClaimIngress(ctx, other); err != nil {
			return err
		}
		return tx.CompleteIngress(ctx, other.ID, model.IngressReceipt{ID: other.ID, Committed: true, Operation: &foreign})
	}); err == nil {
		t.Fatal("a receipt naming another tenant's operation was stored")
	}
}
