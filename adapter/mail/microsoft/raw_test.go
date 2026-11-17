package microsoft

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func rawReq(id string) adapter.RawRequest {
	return adapter.RawRequest{Message: model.ProviderMessageRef(testChan, id)}
}

// ReadRaw returns the MIME Graph serves for the message (GET
// .../messages/{id}/$value), unchanged, naming the message by its immutable
// ID, with that one read and nothing else.
func TestReadRawReturnsTheMIME(t *testing.T) {
	e := newEnv(t, Config{})
	raw := []byte("From: peer@example.org\r\nSubject: hello\r\nX-Custom: kept\r\n\r\nbody\r\n")
	deliverMIME(e, "m1", raw)
	before := len(e.g.requests())
	got, err := e.inst.ReadRaw(t.Context(), rawReq("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, raw) || got.MediaType != model.MediaTypeRawMessage {
		t.Fatalf("ReadRaw = %q (%s), want the served bytes", got.Bytes, got.MediaType)
	}
	reqs := e.g.requests()[before:]
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "GET /v1.0/users/"+testMailbox+"/messages/m1/$value") {
		t.Fatalf("requests = %v, want one GET of the message's $value", reqs)
	}
	e.g.mu.Lock()
	plain := slices.Clone(e.g.plain)
	e.g.mu.Unlock()
	if len(plain) != 0 {
		t.Errorf("requests without the immutable ID preference: %v", plain)
	}
}

// The adapter declares raw_read and reports it available.
func TestRawReadIsDeclaredAndAvailable(t *testing.T) {
	e := newEnv(t, Config{})
	if !slices.Contains(NewFactory(Options{}).Describe().Capabilities, model.CapabilityRawRead) {
		t.Fatal("descriptor does not declare raw_read")
	}
	st, err := e.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityRawRead && s.Available }) {
		t.Fatalf("statuses %+v, want raw_read available", st)
	}
}

// A message over the bound is a typed too_large refusal, never a truncated
// copy; one at the bound is read whole.
func TestReadRawBound(t *testing.T) {
	e := newEnv(t, Config{})
	deliverMIME(e, "fits", bytes.Repeat([]byte("a"), maxRawMessageBytes))
	got, err := e.inst.ReadRaw(t.Context(), rawReq("fits"))
	if err != nil || len(got.Bytes) != maxRawMessageBytes {
		t.Fatalf("a message at the bound: %d bytes, %v", len(got.Bytes), err)
	}
	deliverMIME(e, "big", bytes.Repeat([]byte("a"), maxRawMessageBytes+1))
	got, err = e.inst.ReadRaw(t.Context(), rawReq("big"))
	me := wantErr(t, err, model.Permanent)
	if me.Reason != model.TooLarge {
		t.Errorf("reason = %q, want %q", me.Reason, model.TooLarge)
	}
	if len(got.Bytes) != 0 {
		t.Errorf("an over-bound read returned %d bytes", len(got.Bytes))
	}
}

// A message Graph no longer holds is not found.
func TestReadRawOfGoneMessageIsNotFound(t *testing.T) {
	e := newEnv(t, Config{})
	_, err := e.inst.ReadRaw(t.Context(), rawReq("gone"))
	_ = wantErr(t, err, model.NotFound)
}

// A reference of another channel, namespace or kind, or without an ID, is
// refused before any request.
func TestReadRawRefusesForeignReferences(t *testing.T) {
	e := newEnv(t, Config{})
	deliverMIME(e, "m1", []byte("Subject: x\r\n\r\nx\r\n"))
	before := len(e.g.requests())
	other := model.ChannelRef{Tenant: testChan.Tenant, ID: "other"}
	for name, ref := range map[string]model.ExternalRef{
		"channel":   model.ProviderMessageRef(other, "m1"),
		"namespace": {Channel: testChan, Namespace: IdentityNamespace, Kind: model.RefKindMessage, ID: "m1"},
		"kind":      {Channel: testChan, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindInteraction, ID: "m1"},
		"empty":     model.ProviderMessageRef(testChan, ""),
	} {
		_, err := e.inst.ReadRaw(t.Context(), adapter.RawRequest{Message: ref})
		if model.CodeOf(err) != model.Validation {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	if n := len(e.g.requests()) - before; n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}
