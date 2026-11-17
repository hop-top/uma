package streams_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// workFor returns the claimed item that executes op.
func workFor(t *testing.T, works []store.Work, op model.OperationRef) store.Work {
	t.Helper()
	for _, w := range works {
		if w.Resource == op.Ref() {
			return w
		}
	}
	t.Fatalf("no claimed item for %s in %d items", op.ID, len(works))
	return store.Work{}
}

var lostAnswer = answer{out: adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}}

// An update whose answer was lost holds back the next update while the
// adapter reconciles it: the later update runs only once the earlier one
// has an answer, so the provider sees the updates in order.
func TestStreamUpdateWaitsWhileItsPredecessorIsReconciled(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.f.respond(lostAnswer)
	u1 := r.update(ref, 1, "old")
	r.exec(r.claim())
	wantState(t, r.op(u1), model.Unknown)
	u2 := r.update(ref, 2, "new")
	r.clock.Advance(30 * time.Second)
	works := r.claim()
	before := len(r.f.recorded())
	r.exec([]store.Work{workFor(t, works, u2.Operation.Ref)})
	if calls := r.f.recorded(); len(calls) != before {
		t.Fatalf("provider calls = %+v: update 2 ran while update 1 was outcome_unknown", calls)
	}
	wantState(t, r.op(u2), model.Queued)

	r.f.mu.Lock()
	r.f.reconcile = func(context.Context, adapter.Effect) (adapter.Outcome, error) {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	r.f.mu.Unlock()
	r.exec([]store.Work{workFor(t, works, u1.Operation.Ref)})
	r.drain()

	var order []string
	for _, c := range r.f.recorded() {
		order = append(order, fmt.Sprintf("%s:%d", c.method, c.sequence))
	}
	if want := []string{"start:0", "update:1", "reconcile:0", "update:2"}; !slices.Equal(order, want) {
		t.Fatalf("provider calls = %v, want %v", order, want)
	}
	wantState(t, r.op(u1), model.ProviderAccepted)
	wantState(t, r.op(u2), model.ProviderAccepted)
	if st := r.stream(ref); st.AppliedSequence != 2 || st.State != model.StreamOpen {
		t.Fatalf("stream = %+v, want open and applied through sequence 2", st)
	}
}

// Without a reconciler nothing can resolve a lost answer, so nothing waits
// on it: the next update, which carries the whole response, is sent and
// supersedes it.
func TestStreamUpdateAfterUnresolvableUnknownIsSent(t *testing.T) {
	r := newRig(t, everything...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.f.respond(lostAnswer)
	u1 := r.update(ref, 1, "old")
	r.exec(r.claim())
	wantState(t, r.op(u1), model.Unknown)
	u2 := r.update(ref, 2, "new")
	r.exec(r.claim())
	wantState(t, r.op(u2), model.ProviderAccepted)
	r.end(ref, 2, false)
	r.drain()
	if got := r.f.methods(); !slices.Equal(got, []string{"start", "update", "update", "finish"}) {
		t.Fatalf("provider calls = %v", got)
	}
	if st := r.stream(ref); st.State != model.StreamFinished || st.AppliedSequence != 2 {
		t.Fatalf("stream = %+v, want finished through sequence 2", st)
	}
}

// Without a reconciler, an end whose final update's answer was lost
// re-asserts that update's content, so the stream ends showing it.
func TestStreamEndReassertsAnUnknownFinalUpdate(t *testing.T) {
	for _, tc := range []struct {
		mode, method string
	}{
		{model.StreamNative, "finish"},
		{model.StreamEdit, "edit"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := newRig(t, everything...)
			_, ref := r.start(tc.mode)
			r.update(ref, 1, "draft")
			r.drain()
			r.f.respond(lostAnswer)
			u2 := r.update(ref, 2, "final")
			r.exec(r.claim())
			wantState(t, r.op(u2), model.Unknown)
			end := r.end(ref, 2, false)
			r.drain()
			calls := r.f.recorded()
			last := calls[len(calls)-1]
			if last.method != tc.method || last.text != "final" {
				t.Fatalf("provider calls = %+v, want the end to %s with the final content", calls, tc.method)
			}
			wantState(t, r.op(end), model.ProviderAccepted)
			if st := r.stream(ref); st.State != model.StreamFinished || st.AppliedSequence != 2 {
				t.Fatalf("stream = %+v, want finished through sequence 2", st)
			}
		})
	}
}

// A command waiting behind an update the adapter cannot resolve stops at
// the stream deadline: it is never sent, and the stream ends partial.
func TestStreamDeadlineEndsAWaitingStreamPartial(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Duration
		want     time.Duration
	}{
		{"default", 0, model.DefaultStreamDeadline},
		{"requested", 10 * time.Minute, 10 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, append(everything, model.CapabilityReconcile)...)
			r.f.reconcile = func(context.Context, adapter.Effect) (adapter.Outcome, error) {
				return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "still unknown")}, nil
			}
			started := r.clock.Now()
			_, ref := r.startWith(model.StartStream{Mode: model.StreamNative, Deadline: tc.deadline})
			if st := r.stream(ref); !st.Deadline.Equal(started.Add(tc.want)) {
				t.Fatalf("stream deadline = %v, want %v", st.Deadline, started.Add(tc.want))
			}
			r.drain()
			r.f.respond(lostAnswer)
			r.update(ref, 1, "old")
			r.exec(r.claim())
			u2 := r.update(ref, 2, "new")
			end := r.end(ref, 2, false)

			r.clock.Set(started.Add(tc.want - time.Second))
			r.exec(r.claim())
			wantState(t, r.op(u2), model.Queued)
			r.drain()

			for _, c := range r.f.recorded() {
				if (c.method == "update" && c.sequence == 2) || c.method == "finish" {
					t.Fatalf("provider calls = %v: a command ran past the deadline", r.f.methods())
				}
			}
			if op := r.op(u2); op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.DeadlineExpired {
				t.Fatalf("waiting update = %+v, want failed with deadline_expired", op)
			}
			wantState(t, r.op(end), model.Failed)
			if st := r.stream(ref); st.State != model.StreamPartial {
				t.Fatalf("stream state = %q, want partial", st.State)
			}
		})
	}
}
