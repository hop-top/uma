package postgres_test

import (
	"context"
	"testing"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/postgres"
)

func fixture(t *testing.T) conformance.StoreFixture {
	pool := postgres.NewTestDatabase(t, true)
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	st, err := postgres.Open(t.Context(), pool, postgres.Options{Clock: clock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return conformance.StoreFixture{
		Store:   st,
		Now:     clock.Now,
		Advance: func(d time.Duration) { clock.Advance(d) },
		SubscriptionRows: func(ctx context.Context) (n int, err error) {
			err = pool.QueryRow(ctx, "SELECT count(*) FROM uma_subscriptions").Scan(&n)
			return n, err
		},
	}
}

func TestStoreConformance(t *testing.T) {
	conformance.RunStoreSuite(t, fixture)
}

func TestServiceConformance(t *testing.T) {
	conformance.RunServiceSuite(t, fixture)
}

func TestAttachmentConformance(t *testing.T) {
	conformance.RunAttachmentSuite(t, fixture, func(*testing.T) ports.BlobStore { return &portstest.MemoryBlobs{} })
}
