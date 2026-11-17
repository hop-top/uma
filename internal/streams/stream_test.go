package streams_test

import (
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A native stream is one logical response: start, ordered updates and a
// finish, each its own operation, reach the provider in order on one
// provider stream.
func TestNativeStreamLifecycle(t *testing.T) {
	r := newRig(t, everything...)
	start, ref := r.start(model.StreamNative)
	wantState(t, start.Operation, model.Queued)
	if st := r.stream(ref); st.State != model.StreamOpening || st.NextSequence != 1 {
		t.Fatalf("stream after start = %+v, want opening with next sequence 1", st)
	}
	u1 := r.update(ref, 1, "Hel")
	u2 := r.update(ref, 2, "Hello")
	end := r.end(ref, 2, false)
	r.drain()
	if got, want := r.f.methods(), []string{"start", "update", "update", "finish"}; !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
	calls := r.f.recorded()
	if calls[1].sequence != 1 || calls[1].text != "Hel" || calls[2].sequence != 2 || calls[2].text != "Hello" {
		t.Fatalf("updates = %+v, want sequences 1 and 2 in order", calls[1:3])
	}
	for _, c := range calls[1:] {
		if c.stream != "ps-1" {
			t.Fatalf("call %s addressed provider stream %q, want ps-1", c.method, c.stream)
		}
	}
	for _, a := range []model.Acceptance{start, u1, u2, end} {
		wantState(t, r.op(a), model.ProviderAccepted)
	}
	st := r.stream(ref)
	if st.State != model.StreamFinished || st.AppliedSequence != 2 || st.ProviderStreamRef != "ps-1" {
		t.Fatalf("stream = %+v, want finished through sequence 2 on ps-1", st)
	}
	if st.LastOperation == nil || *st.LastOperation != end.Operation.Ref {
		t.Fatalf("last operation = %v, want the end %v", st.LastOperation, end.Operation.Ref)
	}
	if msgs, _ := r.recorded(); len(msgs) != 0 {
		t.Fatalf("stream stored messages %+v", msgs)
	}
}

// A command never runs before its predecessor has a provider answer, even
// when a worker claims it first.
func TestStreamCommandsRunInOrder(t *testing.T) {
	r := newRig(t, everything...)
	_, ref := r.start(model.StreamNative)
	r.update(ref, 1, "a")
	r.end(ref, 1, false)
	works := r.claim()
	slices.Reverse(works)
	r.exec(works)
	if got := r.f.methods(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("first round calls = %v, want only the start", got)
	}
	r.drain()
	if got, want := r.f.methods(), []string{"start", "update", "finish"}; !slices.Equal(got, want) {
		t.Fatalf("provider calls = %v, want %v", got, want)
	}
	if st := r.stream(ref); st.State != model.StreamFinished {
		t.Fatalf("stream state = %q, want finished", st.State)
	}
}

// The expected sequence rejects a stale, repeated or skipped update.
func TestStreamSequenceIsExpected(t *testing.T) {
	r := newRig(t, everything...)
	_, ref := r.start(model.StreamNative)
	for _, seq := range []uint64{0, 2} {
		_, err := r.s.Update(r.ctx, model.StreamUpdate{Command: r.cmd(), Stream: ref, Sequence: seq, Content: content("x")})
		enginetest.RequireCode(t, err, model.Conflict, "sequence out of order")
	}
	r.update(ref, 1, "a")
	_, err := r.s.Update(r.ctx, model.StreamUpdate{Command: r.cmd(), Stream: ref, Sequence: 1, Content: content("b")})
	enginetest.RequireCode(t, err, model.Conflict, "sequence out of order")
	_, err = r.s.End(r.ctx, model.EndStream{Command: r.cmd(), Stream: ref, FinalSequence: 0})
	enginetest.RequireCode(t, err, model.Conflict, "final sequence")
}

// A stream ends once. A retransmitted end reuses its result; another end,
// or an update after the end, conflicts.
func TestStreamEndsOnce(t *testing.T) {
	r := newRig(t, everything...)
	_, ref := r.start(model.StreamNative)
	endCmd := r.cmd()
	req := model.EndStream{Command: endCmd, Stream: ref, FinalSequence: 0}
	first, err := r.s.End(r.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.s.End(r.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("retransmitted end = %+v, %v; want the stored result replayed", again, err)
	}
	_, err = r.s.End(r.ctx, model.EndStream{Command: r.cmd(), Stream: ref, FinalSequence: 0})
	enginetest.RequireCode(t, err, model.Conflict, "stream already ended")
	_, err = r.s.End(r.ctx, model.EndStream{Command: r.cmd(), Stream: ref, FinalSequence: 0, Cancel: true})
	enginetest.RequireCode(t, err, model.Conflict, "stream already ended")
	_, err = r.s.Update(r.ctx, model.StreamUpdate{Command: r.cmd(), Stream: ref, Sequence: 1, Content: content("late")})
	enginetest.RequireCode(t, err, model.Conflict, "stream is not open")
	r.drain()
	if got := r.f.methods(); !slices.Equal(got, []string{"start", "finish"}) {
		t.Fatalf("provider calls = %v, want one start and one finish", got)
	}
	_, err = r.s.End(r.ctx, model.EndStream{Command: r.cmd(), Stream: ref, FinalSequence: 0})
	enginetest.RequireCode(t, err, model.Conflict, "stream already ended")
}

// Each mode needs its own declared capabilities; the engine never falls
// back to another mode.
func TestStreamModeNeedsItsCapabilities(t *testing.T) {
	for _, tc := range []struct {
		mode string
		caps []model.Capability
	}{
		{model.StreamNative, []model.Capability{model.CapabilitySend, model.CapabilityEdit}},
		{model.StreamEdit, []model.Capability{model.CapabilitySend, model.CapabilityStream}},
		{model.StreamBuffer, []model.Capability{model.CapabilityStream}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			r := newRig(t, tc.caps...)
			_, err := r.s.Start(r.ctx, model.StartStream{Command: r.cmd(), Conversation: r.conv.Ref, Audience: public(), Mode: tc.mode})
			enginetest.RequireCode(t, err, model.Unsupported, "does not declare")
		})
	}
	r := newRig(t, everything...)
	_, err := r.s.Start(r.ctx, model.StartStream{Command: r.cmd(), Conversation: r.conv.Ref, Audience: public(), Mode: "typewriter"})
	enginetest.RequireCode(t, err, model.Validation, "mode")
}

// Edit mode sends the first update and edits that message with later
// ones; the end needs no provider call.
func TestEditStream(t *testing.T) {
	r := newRig(t, model.CapabilitySend, model.CapabilityEdit)
	start, ref := r.start(model.StreamEdit)
	wantState(t, start.Operation, model.Committed)
	r.update(ref, 1, "Hel")
	r.update(ref, 2, "Hello")
	end := r.end(ref, 2, false)
	r.drain()
	calls := r.f.recorded()
	if got := r.f.methods(); !slices.Equal(got, []string{"send", "edit"}) {
		t.Fatalf("provider calls = %v, want a send then an edit", got)
	}
	if calls[1].target == nil || calls[1].target.ID != "m1" || calls[1].text != "Hello" {
		t.Fatalf("edit = %+v, want the sent message m1 edited to Hello", calls[1])
	}
	wantState(t, r.op(end), model.Committed)
	if st := r.stream(ref); st.State != model.StreamFinished || st.AppliedSequence != 2 || len(st.ProviderReferences) != 1 {
		t.Fatalf("stream = %+v, want finished on one message", st)
	}
}

// Buffer mode commits updates locally and sends the last one once at the
// end.
func TestBufferStream(t *testing.T) {
	r := newRig(t, model.CapabilitySend)
	_, ref := r.start(model.StreamBuffer)
	wantState(t, r.update(ref, 1, "draft").Operation, model.Committed)
	r.update(ref, 2, "final")
	r.drain()
	if n := len(r.f.recorded()); n != 0 {
		t.Fatalf("provider calls before the end = %d, want none", n)
	}
	end := r.end(ref, 2, false)
	r.drain()
	calls := r.f.recorded()
	if len(calls) != 1 || calls[0].method != "send" || calls[0].text != "final" {
		t.Fatalf("provider calls = %+v, want one send of the last update", calls)
	}
	wantState(t, r.op(end), model.ProviderAccepted)
	if st := r.stream(ref); st.State != model.StreamFinished {
		t.Fatalf("stream state = %q, want finished", st.State)
	}
}

// Finish and cancel record terminal, partial and unknown outcomes.
func TestStreamTerminalOutcomes(t *testing.T) {
	rejected := answer{out: adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Permanent, "no")}}
	lost := answer{out: adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "lost")}}
	accepted := answer{out: adapter.Outcome{State: model.ProviderAccepted}}
	for _, tc := range []struct {
		name    string
		updates uint64
		cancel  bool
		script  []answer
		want    string
	}{
		{"finished", 1, false, nil, model.StreamFinished},
		{"cancelled", 1, true, nil, model.StreamCancelled},
		{"finish refused after content", 1, false, []answer{accepted, accepted, rejected}, model.StreamPartial},
		{"last update refused", 2, false, []answer{accepted, accepted, rejected, accepted}, model.StreamPartial},
		{"finish refused before content", 0, false, []answer{accepted, rejected}, model.StreamFailed},
		{"finish answer lost", 1, false, []answer{accepted, accepted, lost}, model.StreamUnknown},
		{"start refused", 0, false, []answer{rejected}, model.StreamFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, everything...)
			r.f.respond(tc.script...)
			_, ref := r.start(model.StreamNative)
			for i := range tc.updates {
				r.update(ref, i+1, "part")
			}
			r.end(ref, tc.updates, tc.cancel)
			r.drain()
			if st := r.stream(ref); st.State != tc.want {
				t.Fatalf("stream state = %q (calls %v), want %q", st.State, r.f.methods(), tc.want)
			}
		})
	}
}

// An update waits for its predecessor's provider answer, even when a
// worker runs it first.
func TestStreamUpdateWaitsForPredecessor(t *testing.T) {
	r := newRig(t, everything...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	r.update(ref, 1, "old")
	r.update(ref, 2, "new")
	works := r.claim()
	if len(works) != 2 {
		t.Fatalf("claimed %d items, want 2", len(works))
	}
	r.exec(works[1:])
	if n := len(r.f.recorded()); n != 1 {
		t.Fatalf("update 2 ran before update 1 had an answer: %v", r.f.methods())
	}
	r.exec(works[:1])
	r.drain()
	calls := r.f.recorded()
	if len(calls) != 3 || calls[1].text != "old" || calls[2].text != "new" {
		t.Fatalf("provider calls = %+v, want old then new", calls)
	}
}

// An update at or below the applied sequence is never sent: it would
// overwrite newer content.
func TestStreamSupersededUpdateIsNotSent(t *testing.T) {
	r := newRig(t, everything...)
	_, ref := r.start(model.StreamNative)
	r.drain()
	u := r.update(ref, 1, "old")
	r.tx(func(tx store.Tx) error {
		st, err := tx.Stream(r.ctx, ref)
		if err != nil {
			return err
		}
		prev := st.Revision
		st.AppliedSequence, st.Revision = 1, "later"
		return tx.PutStream(r.ctx, st, prev)
	})
	r.drain()
	op := r.op(u)
	if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Conflict {
		t.Fatalf("superseded update = %+v, want failed with a conflict", op)
	}
	if got := r.f.methods(); !slices.Equal(got, []string{"start"}) {
		t.Fatalf("provider calls = %v, want only the start", got)
	}
}
