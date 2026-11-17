package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
)

// Search-based reconciliation lags: the provider applies a send, loses its
// answer, and its search shows the sent message only a minute later. The
// adapter declares a settle window of two minutes. A reconciliation inside
// the window that finds nothing is inconclusive, never "not sent": the
// operation stays outcome_unknown and nothing is sent again. The next
// reconciliation, after the window, finds the send.
func laggingSearchWithinSettleWindow(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	f.p.settle, f.p.lag, f.p.now = 2*time.Minute, time.Minute, f.fx.Now
	f.p.respond(applyLost)
	p := f.start(t)
	op := p.reply(t, "op-1")
	p.work()
	p.idle(t)
	wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)
	p.later(t, 30*time.Second)
	wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)
	if got := f.p.log(); len(got) != 2 || got[1] != "reconcile" {
		t.Fatalf("provider calls = %v, want the send, then one reconciliation inside the window", got)
	}
	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, 30*time.Second)
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d inside the settle window, want 1", n)
	}
	p.later(t, 2*time.Minute)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}
