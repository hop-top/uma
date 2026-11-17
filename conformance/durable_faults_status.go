package conformance

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

// status is a verified delivery status report for an attempt of op, as a
// status-reporting adapter hands it to the sink.
func (f *faultEnv) status(event string, op model.OperationRef, attempt model.ID, r model.DeliveryReport) model.Ingress {
	return model.Ingress{Call: model.Call{Correlation: "rx-" + model.CorrelationID(event)}, Envelope: model.Envelope{
		EventID: event, Destination: faultDest, Kind: model.EnvelopeStatus, Assurance: "verified",
		References: []model.ExternalRef{model.EchoRef(f.ch, op, attempt)},
		Audience:   model.Audience{Visibility: model.Public}, OccurredAt: f.fx.Now(), Delivery: &r,
	}}
}

// report pushes one status report and requires the sink to take it.
func (f *faultEnv) report(t *testing.T, event string, op model.OperationRef, attempt model.ID, r model.DeliveryReport) accepted {
	t.Helper()
	f.push(t, f.status(event, op, attempt, r))
	got := f.accept(t)
	if got.err != nil || !got.receipt.Committed {
		t.Fatalf("status %s = %+v, %v; want it committed", event, got.receipt, got.err)
	}
	return got
}

// A reply the provider accepted is moved on by the status reports that
// follow, from the evidence the store holds: reports that reached the store
// before a process died are acted on by the next one, reports repeat or
// arrive out of order without effect, and every real change is announced
// once. Delivered outranks a failure; a late sent and a late failure after
// delivery change nothing; the first read is recorded.
func statusReportsAdvanceAcrossRestart(t *testing.T, e *env, cross func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	needsAdvance(t, e)
	p := f.start(t)
	op := p.reply(t, "op-1")
	p.work()
	p.idle(t)
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")
	attempt := f.p.lastSend().Attempt
	moves := func() []model.DeliveryState {
		var out []model.DeliveryState
		for _, ev := range f.deliveries(t, op) {
			var pl model.DeliveryUpdatedPayload
			if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
				t.Fatal(err)
			}
			out = append(out, pl.State)
		}
		return out
	}

	// The late sent and the delivery reach the store; the process dies
	// before it acts on either.
	p = f.restart(t, cross)
	_, stop := p.receive()
	delivered := model.DeliveryReport{State: model.Delivered}
	f.report(t, "evt-delivered", op, attempt, delivered)
	f.report(t, "evt-sent", op, attempt, model.DeliveryReport{State: model.ProviderAccepted})
	stop()
	wantOpState(t, p.op(t, op), model.ProviderAccepted, "")

	p = f.restart(t, cross)
	p.work()
	p.idle(t)
	p.later(t, leaseExpiry)
	wantOpState(t, p.op(t, op), model.Delivered, "")
	want := []model.DeliveryState{model.ProviderAccepted, model.Delivered}
	if got := moves(); !slices.Equal(got, want) {
		t.Fatalf("delivery.updated = %v, want %v: one event per real change", got, want)
	}

	// Repeats, a late failure and a late sent change nothing and announce
	// nothing; the very event again is a duplicate.
	_, stop = p.receive()
	if r := f.report(t, "evt-delivered", op, attempt, delivered); !r.receipt.Duplicate {
		t.Fatalf("a repeated event = %+v, want a duplicate", r.receipt)
	}
	f.report(t, "evt-delivered-2", op, attempt, delivered)
	f.report(t, "evt-sent-2", op, attempt, model.DeliveryReport{State: model.ProviderAccepted})
	f.report(t, "evt-failed", op, attempt, model.DeliveryReport{State: model.Failed, Failure: &model.DeliveryFailure{
		Code: model.Permanent, Retry: model.NoRetry, SafeMessage: "gateway reported the delivery failed",
	}})
	stop()
	p.later(t, leaseExpiry)
	if got := p.op(t, op); got.State != model.Delivered || got.Failure != nil || got.ReadConfirmedAt != nil {
		t.Fatalf("reply = %s (failure %v, read %v) after repeats, want it unchanged", got.State, got.Failure, got.ReadConfirmedAt)
	}
	if got := moves(); !slices.Equal(got, want) {
		t.Fatalf("delivery.updated = %v after repeats, want %v", got, want)
	}

	// A read is recorded once, with its time, and keeps the reply delivered.
	readAt := f.fx.Now().Add(time.Minute)
	_, stop = p.receive()
	f.report(t, "evt-read", op, attempt, model.DeliveryReport{State: model.Delivered, ReadAt: &readAt})
	stop()
	p.later(t, leaseExpiry)
	got := p.op(t, op)
	if got.State != model.Delivered || got.ReadConfirmedAt == nil || !got.ReadConfirmedAt.Equal(readAt) {
		t.Fatalf("reply = %s read %v, want delivered, read at %v", got.State, got.ReadConfirmedAt, readAt)
	}
	if got := moves(); !slices.Equal(got, append(slices.Clone(want), model.Delivered)) {
		t.Fatalf("delivery.updated = %v after the read, want the read announced once", got)
	}
	if n := f.p.sent(); n != 1 {
		t.Fatalf("provider sends = %d, want 1", n)
	}
}

// rankedCase is a case whose reply's answer is lost on a channel that
// cannot reconcile, so only status reports can settle it, and which then
// receives reports in the given order before the worker runs. The reply
// settles at the strongest of them, not the last, in one delivery.updated
// event, and is never sent again.
func rankedCase(reports []model.DeliveryReport, want model.DeliveryState) func(*testing.T, *env, func(*testing.T)) {
	return func(t *testing.T, e *env, cross func(*testing.T)) {
		f := newFaultEnv(t, e, false)
		needsAdvance(t, e)
		f.p.respond(applyLost)
		p := f.start(t)
		op := p.reply(t, "op-1")
		p.work()
		p.idle(t)
		wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)
		attempt := f.p.lastSend().Attempt

		p = f.restart(t, cross)
		_, stop := p.receive()
		for i, r := range reports {
			f.report(t, "evt-"+string(rune('a'+i)), op, attempt, r)
		}
		stop()
		wantOpState(t, p.op(t, op), model.Unknown, model.OutcomeUnknown)

		p.work()
		p.idle(t)
		p.later(t, leaseExpiry)
		code := model.ErrorCode("")
		if want == model.Failed {
			code = model.Permanent
		}
		wantOpState(t, p.op(t, op), want, code)
		var states []model.DeliveryState
		for _, ev := range f.deliveries(t, op) {
			var pl model.DeliveryUpdatedPayload
			if err := json.Unmarshal(ev.Payload.Data, &pl); err != nil {
				t.Fatal(err)
			}
			states = append(states, pl.State)
		}
		if wantStates := []model.DeliveryState{model.Unknown, want}; !slices.Equal(states, wantStates) {
			t.Fatalf("delivery.updated = %v, want %v", states, wantStates)
		}
		if n := f.p.sent(); n != 1 {
			t.Fatalf("provider sends = %d, want the lost one alone", n)
		}
	}
}

var (
	sentReport      = model.DeliveryReport{State: model.ProviderAccepted}
	deliveredReport = model.DeliveryReport{State: model.Delivered}
	failedReport    = model.DeliveryReport{State: model.Failed, Failure: &model.DeliveryFailure{
		Code: model.Permanent, Retry: model.NoRetry, SafeMessage: "gateway reported the delivery failed",
	}}
)

var statusRankCases = []durableCase{
	{"StatusDeliveredThenSentSettlesDelivered", rankedCase([]model.DeliveryReport{deliveredReport, sentReport}, model.Delivered)},
	{"StatusSentThenDeliveredSettlesDelivered", rankedCase([]model.DeliveryReport{sentReport, deliveredReport}, model.Delivered)},
	{"StatusFailedThenSentSettlesFailed", rankedCase([]model.DeliveryReport{failedReport, sentReport}, model.Failed)},
	{"StatusFailedThenDeliveredSettlesDelivered", rankedCase([]model.DeliveryReport{failedReport, deliveredReport}, model.Delivered)},
	{"StatusDeliveredThenFailedSettlesDelivered", rankedCase([]model.DeliveryReport{deliveredReport, failedReport}, model.Delivered)},
}
