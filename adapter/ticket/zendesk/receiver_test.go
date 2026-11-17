package zendesk_test

import (
	"testing"
	"time"
)

// exportStarts returns the start_time of every export request so far.
func (h *host) exportStarts() []int64 {
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	return append([]int64(nil), h.acct.starts...)
}

// A receiver connection resumes from the engine's receiver cursor, the
// page it last handed on, instead of re-reading the lookback window.
func TestReceiverResumesFromCursor(t *testing.T) {
	h := newHost(t)
	h.fake.addComment(h.acct, "poll-1", "100", hostBrand, "c-1", "42", "first", ptr(true), false, nil)
	h.receiveOnce()
	first := h.exportStarts()
	if len(first) == 0 {
		t.Fatal("no export request")
	}
	lookback := first[0]
	h.clock.Advance(time.Hour)
	h.fake.addComment(h.acct, "poll-2", "100", hostBrand, "c-2", "42", "second", ptr(true), false, nil)
	h.receiveOnce()
	starts := h.exportStarts()[len(first):]
	if len(starts) == 0 || starts[0] != h.fake.base {
		t.Fatalf("second connection started at %v (lookback start %d, events from %d), want the cursor", starts, lookback, h.fake.base)
	}
	if n := len(h.messages()); n != 2 {
		t.Fatalf("messages = %d, want 2", n)
	}
}
