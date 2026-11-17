package postgres_test

import (
	"testing"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// TestDurableConformance backs Properties().Durable: the restart cases
// terminate every backend of the store's database, units in flight
// included, and reopen it on a new pool.
func TestDurableConformance(t *testing.T) {
	conformance.RunDurableSuite(t, fixture, func(t *testing.T, fx conformance.StoreFixture) conformance.StoreFixture {
		next := fx
		next.Store = postgres.RestartTestStore(t, fx.Store.(*postgres.Store))
		return next
	})
}

// TestRestartTerminatesUnitsInFlight: the restart the durable cases use is a
// crash, not a handover. A unit open across it cannot commit afterwards, and
// the successor never sees its writes.
func TestRestartTerminatesUnitsInFlight(t *testing.T) {
	ctx := t.Context()
	fx := fixture(t)
	st := fx.Store.(*postgres.Store)
	ch := model.ChannelRef{Tenant: tenant, ID: "00000001-0000-7000-8000-0000000000c1"}
	written := make(chan error, 1)
	commit := make(chan struct{})
	ended := make(chan error, 1)
	go func() {
		ended <- st.Transact(ctx, tenant, func(tx store.Tx) error {
			err := tx.PutChannel(ctx, model.Channel{Ref: ch, Type: "test.chat", Revision: "r1"}, "")
			written <- err
			<-commit
			return err
		})
	}()
	if err := <-written; err != nil {
		t.Fatalf("in-flight unit: %v", err)
	}
	next := postgres.RestartTestStore(t, st)
	close(commit)
	if err := <-ended; err == nil {
		t.Fatal("unit in flight across the restart committed")
	}
	err := next.View(ctx, tenant, func(r store.Reader) error {
		_, err := r.GetChannel(ctx, ch)
		return err
	})
	if model.CodeOf(err) != model.NotFound {
		t.Fatalf("in-flight unit's channel after restart = %v, want not_found", err)
	}
}
