package streams_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Whether a stream command waits on a predecessor whose outcome is unknown
// follows whether the runtime will reconcile that predecessor, not only the
// descriptor: a channel that declares reconcile but reports it unavailable
// for good takes the same path as an adapter that never declared it.

// order lists the provider calls as method:sequence.
func order(r *rig) []string {
	var out []string
	for _, c := range r.f.recorded() {
		out = append(out, fmt.Sprintf("%s:%d", c.method, c.sequence))
	}
	return out
}

// On a channel reporting reconcile unavailable for good, nothing will
// resolve a lost update, so the next update is sent at once, as without a
// reconciler.
func TestStreamUpdateAfterUnknownOnChannelWithoutReconcileIsSent(t *testing.T) {
	for _, reason := range []model.ErrorCode{model.Disabled, model.Unsupported} {
		t.Run(string(reason), func(t *testing.T) {
			r := newRig(t, append(everything, model.CapabilityReconcile)...)
			r.f.withdraw(model.CapabilityReconcile, reason)
			_, ref := r.start(model.StreamNative)
			r.drain()
			r.f.respond(lostAnswer)
			u1 := r.update(ref, 1, "old")
			r.exec(r.claim())
			if op := r.op(u1); op.State != model.Unknown || op.NextAttemptAt != nil {
				t.Fatalf("update 1 = %s, next attempt %v, want outcome_unknown with nothing scheduled", op.State, op.NextAttemptAt)
			}
			u2 := r.update(ref, 2, "new")
			r.exec(r.claim())
			wantState(t, r.op(u2), model.ProviderAccepted)
			r.end(ref, 2, false)
			r.drain()
			if got, want := order(r), []string{"start:0", "update:1", "update:2", "finish:2"}; !slices.Equal(got, want) {
				t.Fatalf("provider calls = %v, want %v", got, want)
			}
			if st := r.stream(ref); st.State != model.StreamFinished || st.AppliedSequence != 2 {
				t.Fatalf("stream = %+v, want finished through sequence 2", st)
			}
		})
	}
}

// The stream deadline bounds waiting only: on a channel reporting
// reconcile unavailable for good nothing waited on the lost update, so the
// next update is sent past the deadline too, as without a reconciler.
func TestStreamUpdateAfterDeadlineOnChannelWithoutReconcileIsSent(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	r.f.withdraw(model.CapabilityReconcile, model.Disabled)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.f.respond(lostAnswer)
	r.update(ref, 1, "old")
	r.exec(r.claim())
	r.clock.Advance(model.DefaultStreamDeadline + time.Minute)
	u2 := r.update(ref, 2, "new")
	r.exec(r.claim())
	wantState(t, r.op(u2), model.ProviderAccepted)
	if st := r.stream(ref); st.State != model.StreamOpen || st.AppliedSequence != 2 {
		t.Fatalf("stream = %+v, want open and applied through sequence 2", st)
	}
}

// Reconciliation that stayed inconclusive until the retry budget was spent
// resolves nothing any more: the update waiting on it is sent then,
// before the deadline, instead of waiting it out.
func TestStreamUpdateFollowsAbandonedReconciliation(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	r.f.reconcile = func(context.Context, adapter.Effect) (adapter.Outcome, error) {
		return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "still unknown")}, nil
	}
	_, ref := r.startWith(model.StartStream{Mode: model.StreamNative, Deadline: model.MaxStreamDeadline})
	r.drain()
	r.f.respond(lostAnswer)
	u1 := r.update(ref, 1, "old")
	r.exec(r.claim())
	u2 := r.update(ref, 2, "new")
	r.drain()
	if op := r.op(u1); op.State != model.Unknown || op.NextAttemptAt != nil {
		t.Fatalf("update 1 = %s, next attempt %v, want outcome_unknown with nothing scheduled", op.State, op.NextAttemptAt)
	}
	wantState(t, r.op(u2), model.ProviderAccepted)
	if st := r.stream(ref); !r.clock.Now().Before(st.Deadline) || st.State != model.StreamOpen || st.AppliedSequence != 2 {
		t.Fatalf("stream = %+v at %v, want open and applied through sequence 2 before its deadline", st, r.clock.Now())
	}
}

// On a channel reporting reconcile unavailable for good, an end whose
// final update's answer was lost re-asserts that update's content.
func TestStreamEndReassertsUnknownFinalUpdateOnChannelWithoutReconcile(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	r.f.withdraw(model.CapabilityReconcile, model.Disabled)
	_, ref := r.start(model.StreamNative)
	r.update(ref, 1, "draft")
	r.drain()
	r.f.respond(lostAnswer)
	u2 := r.update(ref, 2, "final")
	r.exec(r.claim())
	wantState(t, r.op(u2), model.Unknown)
	end := r.end(ref, 2, false)
	r.drain()
	calls := r.f.recorded()
	if last := calls[len(calls)-1]; last.method != "finish" || last.text != "final" {
		t.Fatalf("provider calls = %+v, want the end to finish with the final content", calls)
	}
	wantState(t, r.op(end), model.ProviderAccepted)
	if st := r.stream(ref); st.State != model.StreamFinished || st.AppliedSequence != 2 {
		t.Fatalf("stream = %+v, want finished through sequence 2", st)
	}
}

// Reconcile temporarily unavailable defers the predecessor's pass, so the
// next update still waits for it, then runs once the pass resolved it.
func TestStreamUpdateWaitsWhileReconcileIsTemporarilyUnavailable(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.f.withdraw(model.CapabilityReconcile, model.Unavailable)
	r.f.respond(lostAnswer)
	u1 := r.update(ref, 1, "old")
	r.exec(r.claim())
	if op := r.op(u1); op.State != model.Unknown || op.NextAttemptAt == nil {
		t.Fatalf("update 1 = %s, next attempt %v, want outcome_unknown with a pass scheduled", op.State, op.NextAttemptAt)
	}
	u2 := r.update(ref, 2, "new")
	before := len(r.f.recorded())
	r.exec([]store.Work{workFor(t, r.claim(), u2.Operation.Ref)})
	if len(r.f.recorded()) != before {
		t.Fatalf("provider calls = %v: update 2 ran while update 1 awaited reconciliation", order(r))
	}
	wantState(t, r.op(u2), model.Queued)

	r.f.restore(model.CapabilityReconcile)
	r.drain()
	if got, want := order(r), []string{"start:0", "update:1", "reconcile:0", "update:1", "update:2"}; !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
	wantState(t, r.op(u2), model.ProviderAccepted)
	if st := r.stream(ref); st.AppliedSequence != 2 {
		t.Fatalf("stream = %+v, want applied through sequence 2", st)
	}
}

// Availability is read when the predecessor's pass runs: reconcile
// disabled after the pass was scheduled settles the predecessor without a
// call, and the update waiting on it is then sent instead of waiting out
// the stream deadline.
func TestStreamUpdateFollowsPredecessorSettledWithoutReconcile(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.f.respond(lostAnswer)
	u1 := r.update(ref, 1, "old")
	r.exec(r.claim())
	u2 := r.update(ref, 2, "new")
	r.exec([]store.Work{workFor(t, r.claim(), u2.Operation.Ref)})
	wantState(t, r.op(u2), model.Queued)

	r.f.withdraw(model.CapabilityReconcile, model.Disabled)
	r.drain()
	if op := r.op(u1); op.State != model.Unknown || op.NextAttemptAt != nil {
		t.Fatalf("update 1 = %s, next attempt %v, want outcome_unknown with nothing scheduled", op.State, op.NextAttemptAt)
	}
	if got, want := order(r), []string{"start:0", "update:1", "update:2"}; !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
	wantState(t, r.op(u2), model.ProviderAccepted)
	if st := r.stream(ref); st.State != model.StreamOpen || st.AppliedSequence != 2 {
		t.Fatalf("stream = %+v, want open and applied through sequence 2", st)
	}
}

// A predecessor that reconciliation showed was not applied is retried
// while it stays outcome_unknown with nothing scheduled. The next update
// waits for that retry rather than overtaking it: the retry, still in
// flight, would otherwise land after it.
func TestStreamUpdateWaitsForItsPredecessorsRetryInFlight(t *testing.T) {
	r := newRig(t, append(everything, model.CapabilityReconcile)...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.f.respond(lostAnswer)
	u1 := r.update(ref, 1, "old")
	r.exec(r.claim())
	u2 := r.update(ref, 2, "new")

	entered, release := make(chan struct{}), make(chan struct{})
	retries := 0
	r.f.mu.Lock()
	r.f.pause = func(c call) {
		if c.method == "update" && c.sequence == 1 {
			if retries++; retries == 1 {
				close(entered)
				<-release
			}
		}
	}
	r.f.mu.Unlock()

	r.clock.Advance(30 * time.Second)
	works := r.claim()
	done := make(chan error, 1)
	go func() { done <- r.rt.ExecuteClaimed(r.ctx, enginetest.TenantA, workFor(t, works, u1.Operation.Ref)) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("update 1 pass ended (%v) without retrying", err)
	}
	if op := r.op(u1); op.State != model.Unknown || op.NextAttemptAt != nil {
		close(release)
		t.Fatalf("update 1 = %s, next attempt %v, want outcome_unknown with its retry under way", op.State, op.NextAttemptAt)
	}
	before := len(r.f.recorded())
	r.exec([]store.Work{workFor(t, works, u2.Operation.Ref)})
	after := len(r.f.recorded())
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("execute update 1: %v", err)
	}
	if after != before {
		t.Fatalf("provider calls = %v: update 2 overtook the retry of update 1", order(r))
	}
	wantState(t, r.op(u2), model.Queued)

	r.drain()
	if got, want := order(r), []string{"start:0", "update:1", "reconcile:0", "update:1", "update:2"}; !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
	wantState(t, r.op(u1), model.ProviderAccepted)
	wantState(t, r.op(u2), model.ProviderAccepted)
}
