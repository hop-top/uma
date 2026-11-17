package runtime_test

import (
	"bytes"
	"errors"
	"strconv"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

func rawRequest(m model.Message) model.Lookup[model.MessageRef] {
	return model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "corr-raw"}, Ref: m.Ref}
}

// Raw returns the provider's copy of the message named by its stored
// provider reference, and leaves the message and the operation log as
// they were: nothing is stored.
func TestRawReadsTheProviderCopy(t *testing.T) {
	r := newRig(t, enginetest.WithRawRead(nil))
	m := r.SeedMessage("original")
	before := r.Message(m.Ref)
	got, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
	if err != nil {
		t.Fatal(err)
	}
	want := enginetest.RawBytes(enginetest.Target)
	if !bytes.Equal(got.Bytes, want) || got.Size != int64(len(want)) || got.MediaType != model.MediaTypeRawMessage || got.Ref != m.Ref {
		t.Fatalf("Raw = %+v, want %d bytes of %s for %v", got, len(want), model.MediaTypeRawMessage, m.Ref)
	}
	calls := r.F.RawCalls()
	if len(calls) != 1 || calls[0].Message != model.ProviderMessageRef(r.Ch.Ref, enginetest.Target) {
		t.Fatalf("provider calls = %+v, want one read of the stored provider message", calls)
	}
	if after := r.Message(m.Ref); after.Revision != before.Revision || len(after.Content.Parts) != len(before.Content.Parts) {
		t.Errorf("message changed by a raw read: %+v -> %+v", before, after)
	}
	if len(r.F.ProviderSends()) != 0 {
		t.Errorf("a raw read sent %d messages", len(r.F.ProviderSends()))
	}
}

// Each call reads the provider again; the answer is never kept.
func TestRawIsReadAtEveryCall(t *testing.T) {
	n := 0
	r := newRig(t, enginetest.WithRawRead(func(adapter.RawRequest) (adapter.RawMessage, error) {
		n++
		return adapter.RawMessage{MediaType: model.MediaTypeRawMessage, Bytes: []byte(strconv.Itoa(n))}, nil
	}))
	m := r.SeedMessage("original")
	for want := 1; want <= 2; want++ {
		got, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
		if err != nil || string(got.Bytes) != strconv.Itoa(want) {
			t.Fatalf("call %d: %+v, %v", want, got, err)
		}
	}
	if len(r.F.RawCalls()) != 2 {
		t.Fatalf("provider reads = %d, want 2", len(r.F.RawCalls()))
	}
}

// A channel whose adapter does not declare raw_read is unsupported, with no
// provider call.
func TestRawNeedsTheCapability(t *testing.T) {
	r := newRig(t)
	m := r.SeedMessage("original")
	_, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
	requireCode(t, err, model.Unsupported, "raw_read")
	if len(r.F.Calls()) != 0 {
		t.Fatalf("calls = %v, want none", r.F.Calls())
	}
}

// A capability the open instance reports withdrawn is not called.
func TestRawHonoursWithdrawnCapability(t *testing.T) {
	r := newRig(t, enginetest.WithRawRead(nil))
	m := r.SeedMessage("original")
	r.F.Withdraw(model.CapabilityRawRead, model.Unauthorized)
	_, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
	requireCode(t, err, model.Unauthorized, "")
	if len(r.F.RawCalls()) != 0 {
		t.Fatalf("provider reads = %d, want none", len(r.F.RawCalls()))
	}
}

// The read is authorized like a message read, under the message's own
// audience, before anything is read.
func TestRawIsAuthorizedAsAMessageRead(t *testing.T) {
	r := newRig(t, enginetest.WithRawRead(nil))
	m := r.SeedMessage("original")
	r.Policy.Set(true)
	_, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
	requireCode(t, err, model.Unauthorized, "")
	if len(r.F.RawCalls()) != 0 {
		t.Fatalf("a denied read reached the provider")
	}
	r.Policy.Set(false)
	if _, err := r.C.Messages().Raw(r.Ctx, rawRequest(m)); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, a := range r.Policy.Checks() {
		if a.Action == model.ActionMessageRead && a.Resource == m.Ref.Ref() && a.Audience.Visibility == m.Audience.Visibility {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s check on the message under its audience: %+v", model.ActionMessageRead, r.Policy.Checks())
	}
}

// A message outside the principal's tenant or unknown is not found.
func TestRawOfUnknownMessageIsNotFound(t *testing.T) {
	r := newRig(t, enginetest.WithRawRead(nil))
	m := r.SeedMessage("original")
	req := rawRequest(m)
	req.Ref.ID = "00000005-0000-7000-8000-0000000000ff"
	_, err := r.C.Messages().Raw(r.Ctx, req)
	requireCode(t, err, model.NotFound, "")
	if len(r.F.RawCalls()) != 0 {
		t.Fatal("an unknown message reached the provider")
	}
}

// A redacted or deleted message is a conflict, as every operation on one
// is, and the provider is not asked.
func TestRawOfScrubbedMessageIsConflict(t *testing.T) {
	for name, change := range map[string]func(*model.Message){
		"redacted": func(x *model.Message) { x.Redacted, x.Content = true, model.Content{} },
		"deleted":  func(x *model.Message) { x.Deleted, x.Content = true, model.Content{} },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, enginetest.WithRawRead(nil))
			m := r.SeedMessage("original")
			r.Mutate(m.Ref, change)
			_, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
			requireCode(t, err, model.Conflict, name)
			if len(r.F.RawCalls()) != 0 {
				t.Fatalf("a %s message reached the provider", name)
			}
		})
	}
}

// A message holding no provider reference on its channel has no provider
// copy to read.
func TestRawWithoutProviderReferenceIsConflict(t *testing.T) {
	r := newRig(t, enginetest.WithRawRead(nil))
	m := r.SeedMessage("original")
	r.Mutate(m.Ref, func(x *model.Message) { x.External = nil })
	_, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
	requireCode(t, err, model.Conflict, "provider copy")
	if len(r.F.RawCalls()) != 0 {
		t.Fatal("a message without a provider reference reached the provider")
	}
}

// An adapter's typed refusals reach the caller as they are: a message over
// the bound is permanent with the too_large reason, a message the provider
// no longer holds is not found, and an untyped failure is unavailable
// without its text.
func TestRawPassesTypedAdapterErrors(t *testing.T) {
	tooLarge := model.NewReasonError(model.Permanent, model.TooLarge, "message over the size limit")
	for _, c := range []struct {
		name   string
		err    error
		code   model.ErrorCode
		reason model.ErrorReason
	}{
		{"too large", tooLarge, model.Permanent, model.TooLarge},
		{"gone", model.NewError(model.NotFound, "message gone"), model.NotFound, ""},
		{"transient", model.NewError(model.Transient, "try again"), model.Transient, ""},
		{"untyped", errors.New("secret provider text"), model.Unavailable, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithRawRead(func(adapter.RawRequest) (adapter.RawMessage, error) {
				return adapter.RawMessage{}, c.err
			}))
			m := r.SeedMessage("original")
			got, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
			requireCode(t, err, c.code, "")
			if model.ReasonOf(err) != c.reason {
				t.Errorf("reason = %q, want %q", model.ReasonOf(err), c.reason)
			}
			if len(got.Bytes) != 0 {
				t.Errorf("a failed read returned %d bytes", len(got.Bytes))
			}
			var me *model.Error
			if errors.As(err, &me) && bytes.Contains([]byte(me.SafeMessage), []byte("secret")) {
				t.Errorf("provider text leaked: %q", me.SafeMessage)
			}
		})
	}
}

// An adapter answering no bytes is a fault of the adapter, never an empty
// message.
func TestRawRefusesAnEmptyAnswer(t *testing.T) {
	r := newRig(t, enginetest.WithRawRead(func(adapter.RawRequest) (adapter.RawMessage, error) {
		return adapter.RawMessage{MediaType: model.MediaTypeRawMessage}, nil
	}))
	m := r.SeedMessage("original")
	_, err := r.C.Messages().Raw(r.Ctx, rawRequest(m))
	requireCode(t, err, model.Permanent, "")
}
