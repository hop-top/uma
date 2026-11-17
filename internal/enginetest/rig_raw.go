package enginetest

import (
	"context"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// RawBytes is what a rig adapter answers a raw read with when the test
// scripts nothing: a mail message naming the provider message it was read
// for.
func RawBytes(providerID string) []byte {
	return []byte("From: peer@example.test\r\nSubject: " + providerID + "\r\n\r\nbody of " + providerID + "\r\n")
}

// WithRawRead declares the raw_read capability. fn answers each read; nil
// answers RawBytes of the request's provider message ID as message/rfc822.
func WithRawRead(fn func(adapter.RawRequest) (adapter.RawMessage, error)) RigOption {
	return func(_ *engine.Config, f *OutFactory) { f.RawRead, f.RawFn = true, fn }
}

// ReadRaw records the read and answers it.
func (i *OutInstance) ReadRaw(_ context.Context, r adapter.RawRequest) (adapter.RawMessage, error) {
	i.F.mu.Lock()
	i.F.rawCalls = append(i.F.rawCalls, r)
	i.F.log = append(i.F.log, "raw")
	fn := i.F.RawFn
	i.F.mu.Unlock()
	if fn != nil {
		return fn(r)
	}
	return adapter.RawMessage{MediaType: model.MediaTypeRawMessage, Bytes: RawBytes(r.Message.ID)}, nil
}

// RawCalls returns the raw reads that reached the provider.
func (f *OutFactory) RawCalls() []adapter.RawRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.rawCalls)
}
