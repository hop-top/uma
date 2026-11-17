package ingress_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// Delivery status reports move a settled reply on by rank, not by arrival:
// provider_accepted < failed < delivered < delivered and read. These tests
// drive the engine the way a status-reporting adapter does, one status
// envelope per report, and read back the operation and its delivery.updated
// events.

// report is one status report of a test: the delivery state it evidences,
// its failure, and when it reports the message read.
type report struct {
	name    string
	state   model.DeliveryState
	readAt  time.Time
	failure bool
}

// statusSeq numbers the status events of the tests, which run one at a time.
var statusSeq int

var (
	accepted  = report{name: "sent", state: model.ProviderAccepted}
	delivered = report{name: "delivered", state: model.Delivered}
	failed    = report{name: "failed", state: model.Failed, failure: true}
)

// read is a read report at minute m of the test day.
func read(m int) report {
	return report{name: "read", state: model.Delivered, readAt: t0.Add(time.Duration(m) * time.Minute)}
}

// statusFor is the status envelope of report r for an attempt of op. Every
// call is a new event: the same report sent again is a different event ID,
// as a gateway that retries under its own ids would send it.
func (d *desk) statusFor(op model.OperationRef, attempt model.ID, r report) model.Envelope {
	statusSeq++
	rep := &model.DeliveryReport{State: r.state}
	if r.failure {
		rep.Failure = &model.DeliveryFailure{Code: model.Permanent, Retry: model.NoRetry, SafeMessage: "gateway reported the delivery failed"}
	}
	if !r.readAt.IsZero() {
		at := r.readAt
		rep.ReadAt = &at
	}
	return model.Envelope{
		EventID: fmt.Sprintf("status-%d", statusSeq), Destination: dest, Kind: model.EnvelopeStatus,
		References: []model.ExternalRef{model.EchoRef(d.Channel.Ref, op, attempt)},
		Delivery:   rep,
	}
}

// sentReply is a reply the provider accepted and the engine settled
// provider_accepted, with the attempt that carried it.
func (d *desk) sentReply(opID model.OperationID) (model.Acceptance, model.ID) {
	d.T.Helper()
	d.MustAccept(d.comment("e-"+string(opID), "c-"+string(opID), "customer", "7"))
	a := d.reply(d.onlyConversation().Ref, opID, "on it")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		d.T.Fatalf("reply = %s, want provider_accepted", op.State)
	}
	sends := d.f.ProviderSends()
	return a, sends[len(sends)-1].Attempt
}

// strandedReply is a reply whose answer was lost: outcome_unknown, on a
// channel that cannot reconcile, so only status reports can settle it.
func (d *desk) strandedReply(opID model.OperationID) (model.Acceptance, model.ID) {
	d.T.Helper()
	d.MustAccept(d.comment("e-"+string(opID), "c-"+string(opID), "customer", "7"))
	d.f.Respond(enginetest.LostAnswer)
	a := d.reply(d.onlyConversation().Ref, opID, "on it")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.Unknown {
		d.T.Fatalf("reply = %s, want outcome_unknown", op.State)
	}
	sends := d.f.ProviderSends()
	return a, sends[len(sends)-1].Attempt
}

// transition is one delivery.updated event of an operation.
type transition struct {
	From, To model.DeliveryState
	Read     bool
	Trigger  string
}

func (d *desk) transitions(op model.OperationRef) []transition {
	d.T.Helper()
	var out []transition
	for _, ev := range d.Events() {
		if ev.Kind != model.EventDeliveryUpdated || ev.Resource != op.Ref() {
			continue
		}
		var p model.DeliveryUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			d.T.Fatal(err)
		}
		out = append(out, transition{From: p.PreviousState, To: p.State, Read: p.ReadConfirmedAt != nil, Trigger: p.Trigger})
	}
	return out
}

// A report moves a settled provider_accepted reply to the strongest state
// the reports so far show, announcing each real change once, whatever order
// the reports came in and however often they were sent.
func TestStatusReportsAdvanceASettledReply(t *testing.T) {
	const acc, del, fail = model.ProviderAccepted, model.Delivered, model.Failed
	for _, tt := range []struct {
		name    string
		reports []report
		want    model.DeliveryState
		read    time.Time // the read time recorded, if any
		// moves are the delivery.updated events after the settle, in order.
		moves []transition
	}{
		{"sent changes nothing", []report{accepted}, acc, time.Time{}, nil},
		{"delivered", []report{delivered}, del, time.Time{}, []transition{{acc, del, false, "provider"}}},
		{"delivered repeated", []report{delivered, delivered, delivered}, del, time.Time{}, []transition{{acc, del, false, "provider"}}},
		{"late sent after delivered", []report{delivered, accepted}, del, time.Time{}, []transition{{acc, del, false, "provider"}}},
		{"sent then delivered", []report{accepted, delivered}, del, time.Time{}, []transition{{acc, del, false, "provider"}}},
		{"read at once", []report{read(5)}, del, t0.Add(5 * time.Minute), []transition{{acc, del, true, "provider"}}},
		{
			"delivered then read", []report{delivered, read(5)}, del, t0.Add(5 * time.Minute),
			[]transition{{acc, del, false, "provider"}, {del, del, true, "provider"}},
		},
		{"read then delivered", []report{read(5), delivered}, del, t0.Add(5 * time.Minute), []transition{{acc, del, true, "provider"}}},
		{
			"the first read is kept", []report{delivered, read(5), read(2), read(9)}, del, t0.Add(5 * time.Minute),
			[]transition{{acc, del, false, "provider"}, {del, del, true, "provider"}},
		},
		{"failed", []report{failed}, fail, time.Time{}, []transition{{acc, fail, false, "provider"}}},
		{"failed repeated", []report{failed, failed}, fail, time.Time{}, []transition{{acc, fail, false, "provider"}}},
		{"sent after failed", []report{failed, accepted}, fail, time.Time{}, []transition{{acc, fail, false, "provider"}}},
		{
			"delivered after failed outranks it", []report{failed, delivered}, del, time.Time{},
			[]transition{{acc, fail, false, "provider"}, {fail, del, false, "provider"}},
		},
		{"failed after delivered is ignored", []report{delivered, failed}, del, time.Time{}, []transition{{acc, del, false, "provider"}}},
		{"failed after read is ignored", []report{read(5), failed}, del, t0.Add(5 * time.Minute), []transition{{acc, del, true, "provider"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// One report per worker pass: every move is its own unit and
			// its own event.
			d := newDesk(t)
			a, attempt := d.sentReply("op-1")
			for _, r := range tt.reports {
				d.MustAccept(d.statusFor(a.Operation.Ref, attempt, r))
				d.deliver()
			}
			op := d.op(a.Operation.Ref)
			checkMoved(t, d, op, tt.want, tt.read)
			if got := d.transitions(a.Operation.Ref)[1:]; !slices.Equal(got, tt.moves) {
				t.Errorf("delivery.updated after the settle = %+v, want %+v", got, tt.moves)
			}
			if n := len(d.f.ProviderSends()); n != 1 {
				t.Errorf("provider sends = %d, want the one", n)
			}

			// Every report before one worker pass: the same state, whichever
			// the order, with at most one event per state reached.
			b := newDesk(t)
			a2, attempt2 := b.sentReply("op-1")
			for _, r := range tt.reports {
				b.MustAccept(b.statusFor(a2.Operation.Ref, attempt2, r))
			}
			b.deliver()
			checkMoved(t, b, b.op(a2.Operation.Ref), tt.want, tt.read)
		})
	}
}

func checkMoved(t *testing.T, d *desk, op model.Operation, want model.DeliveryState, read time.Time) {
	t.Helper()
	if op.State != want {
		t.Fatalf("reply = %s, want %s", op.State, want)
	}
	switch {
	case read.IsZero() && op.ReadConfirmedAt != nil:
		t.Errorf("read confirmed at %v, want none", op.ReadConfirmedAt)
	case !read.IsZero() && (op.ReadConfirmedAt == nil || !op.ReadConfirmedAt.Equal(read)):
		t.Errorf("read confirmed at %v, want %v", op.ReadConfirmedAt, read)
	}
	switch f := op.Failure; {
	case want == model.Failed && (f == nil || f.Retry != model.NoRetry || f.SafeMessage == ""):
		t.Errorf("failure = %+v, want a final failure with a safe summary", f)
	case want != model.Failed && f != nil:
		t.Errorf("failure = %+v on a %s reply", f, want)
	}
}

// A report that arrives before the worker settled a lost answer is weighed
// with the others, not applied last-wins: delivered then sent is delivered,
// failed then sent is failed, and delivery outranks failure either way.
func TestStatusReportsBeforeSettlingAreRanked(t *testing.T) {
	for _, tt := range []struct {
		reports []report
		want    model.DeliveryState
		read    bool
	}{
		{[]report{delivered, accepted}, model.Delivered, false},
		{[]report{accepted, delivered}, model.Delivered, false},
		{[]report{read(3), accepted}, model.Delivered, true},
		{[]report{failed, accepted}, model.Failed, false},
		{[]report{accepted, failed}, model.Failed, false},
		{[]report{failed, delivered}, model.Delivered, false},
		{[]report{delivered, failed}, model.Delivered, false},
		{[]report{delivered, accepted, failed, delivered}, model.Delivered, false},
		{[]report{accepted, accepted}, model.ProviderAccepted, false},
	} {
		name := make([]string, len(tt.reports))
		for i, r := range tt.reports {
			name[i] = r.name
		}
		t.Run(strings.Join(name, ","), func(t *testing.T) {
			d := newDesk(t)
			a, attempt := d.strandedReply("op-1")
			for _, r := range tt.reports {
				d.MustAccept(d.statusFor(a.Operation.Ref, attempt, r))
			}
			d.deliver()
			op := d.op(a.Operation.Ref)
			if op.State != tt.want || (op.ReadConfirmedAt != nil) != tt.read {
				t.Fatalf("reply = %s (read %v), want %s (read %v)", op.State, op.ReadConfirmedAt, tt.want, tt.read)
			}
			// One settle, one event: outcome_unknown, then the strongest state.
			if got := d.transitions(a.Operation.Ref); len(got) != 2 || got[1].From != model.Unknown || got[1].To != tt.want {
				t.Errorf("delivery.updated = %+v, want the one settle to %s", got, tt.want)
			}
			if n := len(d.f.ProviderSends()); n != 1 {
				t.Errorf("provider sends = %d, want the lost one alone", n)
			}
		})
	}
}

// A report that lands while the send is still in flight is not lost to the
// answer that follows: the reply settles at the strongest of the two, in one
// delivery.updated event.
func TestStatusReportDuringSendIsKept(t *testing.T) {
	for _, tt := range []struct {
		name string
		r    report
		want model.DeliveryState
	}{
		{"delivered", delivered, model.Delivered},
		{"failed", failed, model.Failed},
		{"sent", accepted, model.ProviderAccepted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			d := newDesk(t)
			d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
			send := d.sendInFlight("c-2")
			a := d.reply(d.onlyConversation().Ref, "op-1", "on it")
			wait := d.deliverAsync()
			req := <-send.entered
			d.MustAccept(d.statusFor(a.Operation.Ref, req.Attempt, tt.r))
			send.answer <- enginetest.Accepted
			wait()
			d.deliver()
			op := d.op(a.Operation.Ref)
			if op.State != tt.want {
				t.Fatalf("reply = %s (%v), want %s", op.State, op.Failure, tt.want)
			}
			if got := d.transitions(a.Operation.Ref); len(got) != 1 || got[0].To != tt.want {
				t.Errorf("delivery.updated = %+v, want the one settle to %s", got, tt.want)
			}
		})
	}
}

// A failure names the attempt that failed. A reply that was retried, and
// whose later attempt the provider accepted, is not failed by a late report
// about the first attempt; a delivery of either attempt still delivers it.
func TestStatusFailureOfAnEarlierAttemptDoesNotFailTheReply(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	d.f.Respond(enginetest.TransientFailure)
	a := d.reply(d.onlyConversation().Ref, "op-1", "on it")
	d.deliver()
	d.Clock.Advance(time.Hour)
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("reply = %s, want provider_accepted on the second attempt", op.State)
	}
	sends := d.f.ProviderSends()
	if len(sends) != 2 {
		t.Fatalf("sends = %d, want two attempts", len(sends))
	}
	first, second := sends[0].Attempt, sends[1].Attempt

	d.MustAccept(d.statusFor(a.Operation.Ref, first, failed))
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("after a failure of the first attempt: %s, want provider_accepted", op.State)
	}
	d.MustAccept(d.statusFor(a.Operation.Ref, second, failed))
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.Failed {
		t.Fatalf("after a failure of the attempt that carried it: %s, want failed", op.State)
	}
	d.MustAccept(d.statusFor(a.Operation.Ref, first, delivered))
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.Delivered {
		t.Fatalf("after a delivery: %s, want delivered", op.State)
	}
}

// Only a reply or a conversation start has a delivery to report. Evidence
// naming another operation, an edit say, settles it as before but never
// moves it on.
func TestStatusReportDoesNotAdvanceAMutation(t *testing.T) {
	d := newMutationDesk(t)
	m := d.target()
	a := d.edit("op-edit", m, "corrected")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("edit = %s, want provider_accepted", op.State)
	}
	calls := d.f.MutationCalls()
	d.MustAccept(d.statusFor(a.Operation.Ref, calls[len(calls)-1].Effect.Attempt, delivered))
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("edit after a delivery report = %s, want it left provider_accepted", op.State)
	}
	if got := d.transitions(a.Operation.Ref); len(got) != 1 {
		t.Errorf("delivery.updated = %+v, want the settle alone", got)
	}
}

// A report the operation's state already outranks queues no work and
// writes no event: a gateway that repeats itself costs one evidence entry.
func TestStatusReportAlreadyHeldQueuesNothing(t *testing.T) {
	d := newDesk(t)
	a, attempt := d.sentReply("op-1")
	d.MustAccept(d.statusFor(a.Operation.Ref, attempt, delivered))
	d.deliver()
	before := len(d.Events())
	for range 3 {
		d.MustAccept(d.statusFor(a.Operation.Ref, attempt, delivered))
		d.MustAccept(d.statusFor(a.Operation.Ref, attempt, accepted))
	}
	if n := d.pendingOperationWork(); n != 0 {
		t.Errorf("pending operation work = %d, want none", n)
	}
	d.deliver()
	if n := len(d.Events()); n != before {
		t.Errorf("events grew from %d to %d", before, n)
	}
	if op := d.op(a.Operation.Ref); op.State != model.Delivered {
		t.Errorf("reply = %s, want delivered", op.State)
	}
}

// pendingOperationWork counts the claimable operation work items.
func (d *desk) pendingOperationWork() int {
	d.T.Helper()
	works, err := d.RT.ClaimWork(d.Ctx, tenantA, runtime.WorkerOptions{
		Owner: "probe", Limit: 10, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute,
	})
	if err != nil {
		d.T.Fatal(err)
	}
	return len(works)
}

// The receipt of a status report names the operation the report reached,
// and none when it reached nothing, so the adapter that took the report can
// tell its sender. A repeated event returns the first answer.
func TestStatusReceiptNamesTheOperationReached(t *testing.T) {
	d := newDesk(t)
	a, attempt := d.sentReply("op-1")
	const other model.ID = "00000000-0000-7000-8000-0000000000ff"
	otherOp := model.OperationRef{Tenant: a.Operation.Ref.Tenant, ID: other}

	reached := d.statusFor(a.Operation.Ref, attempt, delivered)
	r := d.MustAccept(reached)
	if !r.Committed || r.Operation == nil || *r.Operation != a.Operation.Ref {
		t.Fatalf("receipt %+v, want it to name %v", r, a.Operation.Ref)
	}
	again := d.MustAccept(reached)
	if !again.Duplicate || again.Operation == nil || *again.Operation != a.Operation.Ref {
		t.Fatalf("duplicate receipt %+v, want the first answer", again)
	}
	// Evidence the operation already outranks still reached it.
	if r := d.MustAccept(d.statusFor(a.Operation.Ref, attempt, accepted)); r.Operation == nil {
		t.Errorf("a late sent reached no operation: %+v", r)
	}
	for name, env := range map[string]model.Envelope{
		"unknown operation": d.statusFor(otherOp, attempt, delivered),
		"unknown attempt":   d.statusFor(a.Operation.Ref, other, delivered),
		"no attempt":        {EventID: "status-none", Destination: dest, Kind: model.EnvelopeStatus, Delivery: &model.DeliveryReport{State: model.Delivered}},
	} {
		if r := d.MustAccept(env); !r.Committed || r.Operation != nil {
			t.Errorf("%s: receipt %+v, want it to name no operation", name, r)
		}
	}
	// A message that is not an echo names none either.
	if r := d.MustAccept(d.comment("e-plain", "c-plain", "customer", "7")); r.Operation != nil {
		t.Errorf("a plain message named %v", r.Operation)
	}
}

// A failure report about an attempt the runtime already counted as not
// applied, and is about to retry, does not fail the reply: the retry runs,
// and the reply is settled by what the retry shows.
func TestStatusFailureWhileRetryPendingDoesNotPreemptTheRetry(t *testing.T) {
	d := newDesk(t)
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	d.f.Respond(enginetest.TransientFailure)
	a := d.reply(d.onlyConversation().Ref, "op-1", "on it")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.RetryPending {
		t.Fatalf("reply = %s, want retry_pending", op.State)
	}
	first := d.f.ProviderSends()[0].Attempt
	d.MustAccept(d.statusFor(a.Operation.Ref, first, failed))
	if op := d.op(a.Operation.Ref); op.State != model.RetryPending {
		t.Fatalf("after the report: %s, want it still retry_pending", op.State)
	}
	d.Clock.Advance(time.Hour)
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("after the retry: %s (%v), want provider_accepted", op.State, op.Failure)
	}
	if n := len(d.f.ProviderSends()); n != 2 {
		t.Fatalf("sends = %d, want the retry made", n)
	}
}
