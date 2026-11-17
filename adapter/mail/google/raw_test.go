package google

import (
	"bytes"
	"net/http"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func rawReq(id string) adapter.RawRequest {
	return adapter.RawRequest{Message: model.ProviderMessageRef(testChan, id)}
}

// ReadRaw returns the bytes Gmail holds for the message, whole and
// unchanged: attachments and every header included, with one format=raw
// read and nothing else.
func TestReadRawReturnsTheMessageBytes(t *testing.T) {
	e := newEnv(t, Config{})
	raw := rawMessage(testPeer, testMailbox, "<m1@remote.test>", "hello", "body text", "X-Custom: kept")
	e.g.deliver(e.box.user, "m1", "thread-m1", raw)
	got, err := e.inst.ReadRaw(t.Context(), rawReq("m1"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, raw) || got.MediaType != model.MediaTypeRawMessage {
		t.Fatalf("ReadRaw = %q (%s), want the delivered bytes", got.Bytes, got.MediaType)
	}
	prefix := "GET /gmail/v1/users/" + testMailbox + "/messages/m1?format=raw"
	if e.g.count(prefix) != 1 {
		t.Errorf("raw reads = %d, want 1 in %v", e.g.count(prefix), e.g.requests())
	}
	for _, r := range e.g.requests() {
		if len(r) >= 3 && r[:3] != "GET" {
			t.Errorf("a raw read made a %q request", r)
		}
	}
}

// The adapter declares raw_read and reports it available.
func TestRawReadIsDeclaredAndAvailable(t *testing.T) {
	e := newEnv(t, Config{})
	if !slices.Contains(e.f.Describe().Capabilities, model.CapabilityRawRead) {
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

// A message over the raw bound is a typed too_large refusal, never a
// truncated copy.
func TestReadRawOverTheBoundIsTooLarge(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.c.rawLimit = smallRawLimit
	e.g.deliver(e.box.user, "big", "thread-big", bigMessage("big"))
	got, err := e.inst.ReadRaw(t.Context(), rawReq("big"))
	me := wantCode(t, err, model.Permanent)
	if me.Reason != model.TooLarge {
		t.Errorf("reason = %q, want %q", me.Reason, model.TooLarge)
	}
	if len(got.Bytes) != 0 {
		t.Errorf("an over-bound read returned %d bytes", len(got.Bytes))
	}
}

// A message under the bound is read whole even when a bigger one is not.
func TestReadRawBoundIsPerMessage(t *testing.T) {
	e := newEnv(t, Config{})
	e.inst.c.rawLimit = smallRawLimit
	e.inbound("small", testPeer, "fits")
	if _, err := e.inst.ReadRaw(t.Context(), rawReq("small")); err != nil {
		t.Fatal(err)
	}
}

// A message Gmail no longer holds is not found.
func TestReadRawOfGoneMessageIsNotFound(t *testing.T) {
	e := newEnv(t, Config{})
	_, err := e.inst.ReadRaw(t.Context(), rawReq("gone"))
	_ = wantCode(t, err, model.NotFound)
}

// A reference of another channel, namespace or kind, or without an ID, is
// refused before any request.
func TestReadRawRefusesForeignReferences(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
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
	if n := len(e.g.requests()); n != 0 {
		t.Errorf("requests = %v, want none", e.g.requests())
	}
}

// A refusal of the read, such as a missing scope, reaches the caller typed.
func TestReadRawKeepsProviderRefusals(t *testing.T) {
	e := newEnv(t, Config{})
	e.inbound("m1", testPeer, "hi")
	e.g.mu.Lock()
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		googleError(w, http.StatusForbidden, "insufficientPermissions", "Request had insufficient authentication scopes.")
		return true
	}
	e.g.mu.Unlock()
	_, err := e.inst.ReadRaw(t.Context(), rawReq("m1"))
	_ = wantCode(t, err, model.Unauthorized)
}
