package imap_test

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

func readRaw(t *testing.T, r *stateRig, ref model.ExternalRef) (adapter.RawMessage, error) {
	t.Helper()
	return r.inst.ReadRaw(t.Context(), adapter.RawRequest{Message: ref})
}

func TestRawReadDeclaredAndAvailable(t *testing.T) {
	d := imap.NewFactory(imap.Options{}).Describe()
	if !slices.Contains(d.Capabilities, model.CapabilityRawRead) {
		t.Fatalf("capabilities %v lack raw_read", d.Capabilities)
	}
	var _ adapter.RawReader = (*imap.Instance)(nil)
	r := newStateRig(t, nil)
	st, err := r.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityRawRead && s.Available }) {
		t.Fatalf("statuses %+v, want raw_read available", st)
	}
}

// ReadRaw returns the message's bytes whole, found by its Message-ID, and
// leaves the mailbox as it was: the message stays unread.
func TestReadRawReturnsTheMessageAndKeepsItUnread(t *testing.T) {
	r := newStateRig(t, nil)
	ref := r.deliver("<raw1@example.org>")
	want := mail("<raw1@example.org>", "text of <raw1@example.org>")
	got, err := readRaw(t, r, ref)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Bytes, want) || got.MediaType != model.MediaTypeRawMessage {
		t.Fatalf("ReadRaw = %q (%s), want %q", got.Bytes, got.MediaType, want)
	}
	if n := r.srv.seen("INBOX"); n != 0 {
		t.Errorf("%d messages marked \\Seen: a raw read must peek", n)
	}
	if n := r.srv.commands("STORE") + r.srv.commands("APPEND") + r.srv.commands("EXPUNGE") + r.srv.commands("COPY") + r.srv.commands("MOVE"); n != 0 {
		t.Errorf("a raw read made %d mailbox-changing commands", n)
	}
}

// A message that left the retrieved mailbox is found where the adapter
// follows it, as a state change finds it.
func TestReadRawFindsAMessageInAnArchive(t *testing.T) {
	r := newStateRig(t, nil)
	r.srv.deliver("Archive", mail("<old@example.org>", "archived"), time.Now())
	got, err := readRaw(t, r, ref("<old@example.org>"))
	if err != nil || !bytes.Equal(got.Bytes, mail("<old@example.org>", "archived")) {
		t.Fatalf("ReadRaw = %q, %v", got.Bytes, err)
	}
}

// A message the mailbox does not hold is not found.
func TestReadRawOfMissingMessageIsNotFound(t *testing.T) {
	r := newStateRig(t, nil)
	_, err := readRaw(t, r, ref("<absent@example.org>"))
	if model.CodeOf(err) != model.NotFound {
		t.Fatalf("error = %v, want not_found", err)
	}
}

// A message over Config.MaxMessageBytes is a typed too_large refusal, never
// a truncated copy, and its body is never fetched.
func TestReadRawOverTheBoundIsTooLarge(t *testing.T) {
	r := newStateRig(t, func(_ *serverOptions, c *imap.Config) { c.MaxMessageBytes = 400 })
	small := r.deliver("<small@example.org>")
	r.srv.deliver("INBOX", mail("<big@example.org>", string(bytes.Repeat([]byte("x"), 800))), time.Now())
	got, err := readRaw(t, r, ref("<big@example.org>"))
	for _, l := range r.srv.clientLines() {
		if strings.Contains(l, "BODY.PEEK[]") {
			t.Errorf("the body of an over-bound message was fetched: %q", l)
		}
	}
	me, _ := err.(*model.Error)
	if me == nil || me.Code != model.Permanent || me.Reason != model.TooLarge {
		t.Fatalf("error = %v, want permanent too_large", err)
	}
	if len(got.Bytes) != 0 {
		t.Errorf("an over-bound read returned %d bytes", len(got.Bytes))
	}
	// A message under the bound is read whole on the same account.
	if _, err := readRaw(t, r, small); err != nil {
		t.Fatalf("a message under the bound: %v", err)
	}
}

// A reference of another channel, namespace or kind, or without an ID, is
// refused before the server is contacted.
func TestReadRawRefusesForeignReferences(t *testing.T) {
	r := newStateRig(t, nil)
	other := model.ChannelRef{Tenant: channelA.Tenant, ID: "other"}
	before := len(r.srv.clientLines())
	for name, x := range map[string]model.ExternalRef{
		"channel":   model.ProviderMessageRef(other, "<a@x>"),
		"namespace": {Channel: channelA, Namespace: imap.Namespace, Kind: model.RefKindMessage, ID: "<a@x>"},
		"kind":      {Channel: channelA, Namespace: model.ProviderMessageNamespace, Kind: model.RefKindInteraction, ID: "<a@x>"},
		"empty":     model.ProviderMessageRef(channelA, ""),
	} {
		if _, err := readRaw(t, r, x); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: error = %v, want validation", name, err)
		}
	}
	if n := len(r.srv.clientLines()) - before; n != 0 {
		t.Errorf("%d commands reached the server, want none", n)
	}
}
