package ingress_test

import (
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func acceptAt(h *harness, env model.Envelope, cursor string) (model.IngressReceipt, error) {
	return h.Sink.Accept(h.Ctx, model.Ingress{Call: model.Call{Correlation: "corr-" + model.CorrelationID(env.EventID)}, Envelope: env, ReceiverCursor: cursor})
}

func cursorOf(t *testing.T, h *harness) string {
	t.Helper()
	var s adapter.Sink = h.Sink
	cs, ok := s.(adapter.CursorSink)
	if !ok {
		t.Fatal("engine sink does not implement adapter.CursorSink")
	}
	c, err := cs.ReceiverCursor(h.Ctx)
	if err != nil {
		t.Fatalf("ReceiverCursor: %v", err)
	}
	return c
}

// A receiver cursor commits with the ingress carrying it, duplicates
// included, and never with a rejected one, so a restarted receiver resumes
// after the last event the engine holds.
func TestReceiverCursorCommitsWithIngress(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), dest)
	if c := cursorOf(t, h); c != "" {
		t.Fatalf("cursor before any ingress = %q, want empty", c)
	}
	first := h.Inbound("e1", "m1", "u1", dest)
	if _, err := acceptAt(h, first, "page-1"); err != nil {
		t.Fatal(err)
	}
	if c := cursorOf(t, h); c != "page-1" {
		t.Fatalf("cursor = %q, want page-1", c)
	}
	if _, err := acceptAt(h, h.Inbound("e2", "m2", "u1", dest), ""); err != nil {
		t.Fatal(err)
	}
	if c := cursorOf(t, h); c != "page-1" {
		t.Fatalf("cursor = %q after an ingress without one, want page-1 kept", c)
	}
	if r, err := acceptAt(h, first, "page-2"); err != nil || !r.Duplicate {
		t.Fatalf("redelivery = %+v, %v", r, err)
	}
	if c := cursorOf(t, h); c != "page-2" {
		t.Fatalf("cursor = %q after a duplicate, want page-2", c)
	}
	_, err := acceptAt(h, h.Inbound("e3", "m3", "u1", "nowhere"), "page-3")
	requireCode(t, err, model.NotFound, "")
	if c := cursorOf(t, h); c != "page-2" {
		t.Fatalf("cursor = %q after a rejected ingress, want page-2", c)
	}
	_, err = acceptAt(h, h.Inbound("e4", "m4", "u1", dest), strings.Repeat("c", 1025))
	requireCode(t, err, model.Validation, "receiver_cursor")
}
