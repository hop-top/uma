package memory_test

import (
	"testing"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

func fixture(*testing.T) conformance.StoreFixture {
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	return conformance.StoreFixture{
		Store:   memory.New(memory.Options{Clock: clock}),
		Now:     clock.Now,
		Advance: func(d time.Duration) { clock.Advance(d) },
	}
}

func TestStoreConformance(t *testing.T) {
	conformance.RunStoreSuite(t, fixture)
}

// TestDurableLogicalConformance runs the durable suite's logical subset.
// Nothing survives a memory store's process, so it cannot run the restart
// cases; this is not durability evidence and the store reports Durable=false.
func TestDurableLogicalConformance(t *testing.T) {
	if fixture(t).Store.Properties().Durable {
		t.Fatal("memory store reports Durable=true")
	}
	conformance.RunDurableLogicalSuite(t, fixture)
}

func TestServiceConformance(t *testing.T) {
	conformance.RunServiceSuite(t, fixture)
}

func TestAttachmentConformance(t *testing.T) {
	conformance.RunAttachmentSuite(t, fixture, func(*testing.T) ports.BlobStore { return &portstest.MemoryBlobs{} })
}
