package whatsapp_test

import (
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/conformance"
)

// The adapter passes the shared suite for receive_http and send against a
// server replaying the documented Cloud API shapes.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			f := newFakeMeta(t)
			return f.factory(), provider{f: f}
		},
		Capabilities: whatsapp.Capabilities(),
	})
}
