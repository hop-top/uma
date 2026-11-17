package portstest_test

import (
	"testing"

	"hop.top/uma/conformance"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

func TestMemoryBlobsConformance(t *testing.T) {
	conformance.RunBlobStoreSuite(t, func(*testing.T) ports.BlobStore { return &portstest.MemoryBlobs{} })
}
