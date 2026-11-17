package conformance

import (
	"bytes"
	"testing"

	"hop.top/uma/model"
)

// RawProvider is implemented by the Provider of an adapter declaring
// raw_read: the raw cases compare what Messages.Raw returns with what the
// provider holds. Without it they skip.
type RawProvider interface {
	// RawCopy returns the whole copy the provider holds of the provider
	// message id of channel, byte for byte as it serves it. It returns
	// ErrNotExpressible when the provider cannot say.
	RawCopy(channel model.ChannelRef, id string) ([]byte, error)
}

var rawCap = []model.Capability{model.CapabilityRawRead}

// rawRequest asks for the raw copy of m.
func rawRequest(m model.Message) model.Lookup[model.MessageRef] {
	return model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "conformance-raw"}, Ref: m.Ref}
}

var adapterRawCases = []adapterCase{
	{name: "RawReadReturnsTheProviderCopy", needs: rawCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		rp, ok := e.provider.(RawProvider)
		if !ok {
			t.Skip("provider does not implement RawProvider")
		}
		g := e.inboundTarget(t, "")
		want, err := rp.RawCopy(adapterChanA, g.id)
		if err == ErrNotExpressible {
			t.Skip("provider cannot give its copy of the message")
		}
		if err != nil {
			t.Fatalf("RawCopy: %v", err)
		}
		calls := len(e.provider.Calls(adapterChanA))
		got, err := e.client.Messages().Raw(e.ctx, rawRequest(g.msg))
		if err != nil {
			t.Fatalf("Messages.Raw: %v", err)
		}
		if !bytes.Equal(got.Bytes, want) {
			t.Fatalf("Raw bytes = %q, want the provider's copy %q", got.Bytes, want)
		}
		if got.Size != int64(len(want)) || got.MediaType == "" || got.Ref != g.msg.Ref {
			t.Errorf("Raw = size %d, media type %q, ref %v; want size %d, a media type, ref %v", got.Size, got.MediaType, got.Ref, len(want), g.msg.Ref)
		}
		// A read changes nothing at the provider and nothing in uma.
		for _, c := range e.provider.Calls(adapterChanA)[calls:] {
			if c.Applied {
				t.Fatalf("a raw read applied %+v at the provider", c)
			}
		}
		e.wantUnchanged(t, g.msg)
		// Read again, the provider's copy is read again.
		again, err := e.client.Messages().Raw(e.ctx, rawRequest(g.msg))
		if err != nil || !bytes.Equal(again.Bytes, want) {
			t.Fatalf("second Messages.Raw = %q, %v; want the same copy", again.Bytes, err)
		}
		e.stop(t)
	}},
	{name: "RawReadOfUnknownMessageIsNotFound", needs: rawCap, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		g := e.inboundTarget(t, "")
		req := rawRequest(g.msg)
		req.Ref.ID = "00000000-0000-7000-8000-0000000000ee"
		_, err := e.client.Messages().Raw(e.ctx, req)
		if model.CodeOf(err) != model.NotFound {
			t.Fatalf("Messages.Raw of an unknown message = %v, want not_found", err)
		}
		e.stop(t)
	}},
}

var adapterRawUnsupportedCases = []adapterCase{
	{name: "RawReadNeedsDeclaredCapability", absent: rawCap, run: func(t *testing.T, e *adapterEnv) {
		if !e.canReceive() {
			t.Skip("no message to read: capabilities receive and receive_http not declared")
		}
		g := e.inboundTarget(t, "")
		before := len(e.provider.Calls(adapterChanA))
		_, err := e.client.Messages().Raw(e.ctx, rawRequest(g.msg))
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("Messages.Raw on a channel without raw_read = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != before {
			t.Fatalf("provider calls = %+v, want none after the refusal", calls[before:])
		}
		e.stop(t)
	}},
}
