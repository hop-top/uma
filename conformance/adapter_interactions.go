package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var participantID = model.ID("00000ad0-0000-7000-8000-0000000000e1")

// participant seeds channel A with a conversation to peer-1 whose stored
// participant is peer-1's identity, and returns both.
func (e *adapterEnv) participant(t *testing.T) (model.Conversation, model.Identity) {
	t.Helper()
	conv := e.outbound(t)
	route := e.route(adapterChanA, "peer-1")
	id := model.Identity{
		Ref:     model.IdentityRef{Inbox: adapterInboxA, ID: participantID},
		Contact: model.ContactRef{Tenant: adapterTenant, ID: participantID}, SourceID: route.ID,
		Namespace: route.Namespace, Kind: model.ContactKindPerson, Revision: "id1",
	}
	conv.Participants = []model.Participant{{Identity: id.Ref, Role: "participant"}}
	err := e.store.Transact(e.ctx, adapterTenant, func(tx store.Tx) error {
		key := model.IdentityKey{Inbox: adapterInboxA, Namespace: id.Namespace, SourceID: id.SourceID}
		if _, _, err := tx.ClaimIdentity(e.ctx, key, id); err != nil {
			return err
		}
		prev := conv.Revision
		conv.Revision = "cv2"
		return tx.PutConversation(e.ctx, conv, prev)
	})
	if err != nil {
		t.Fatal(err)
	}
	return conv, id
}

func command(opID model.OperationID) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(opID))}, Operation: opID}
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: s}}}
}

func (e *adapterEnv) startStream(t *testing.T, conv model.ConversationRef, mode string) model.StreamRef {
	t.Helper()
	a, err := e.client.Streams().Start(e.ctx, model.StartStream{
		Command: command(model.OperationID("op-start-" + mode)), Conversation: conv,
		Audience: model.Audience{Visibility: model.Public}, Mode: mode,
	})
	if err != nil {
		t.Fatalf("Streams.Start(%s): %v", mode, err)
	}
	if a.Operation.ResultStream == nil {
		t.Fatalf("Start result = %+v, want a stream", a.Operation)
	}
	return *a.Operation.ResultStream
}

func (e *adapterEnv) updateStream(t *testing.T, ref model.StreamRef, seq uint64, s string) {
	t.Helper()
	_, err := e.client.Streams().Update(e.ctx, model.StreamUpdate{
		Command: command(model.OperationID("op-update-" + s)), Stream: ref, Sequence: seq, Content: text(s),
	})
	if err != nil {
		t.Fatalf("Streams.Update(%d): %v", seq, err)
	}
}

func (e *adapterEnv) endStream(t *testing.T, ref model.StreamRef, final uint64, cancel bool, opID model.OperationID) model.Acceptance {
	t.Helper()
	a, err := e.client.Streams().End(e.ctx, model.EndStream{Command: command(opID), Stream: ref, FinalSequence: final, Cancel: cancel})
	if err != nil {
		t.Fatalf("Streams.End: %v", err)
	}
	return a
}

func (e *adapterEnv) getStream(t *testing.T, ref model.StreamRef) model.Stream {
	t.Helper()
	st, err := e.client.Streams().Get(e.ctx, model.Lookup[model.StreamRef]{Call: model.Call{Correlation: "conformance-get"}, Ref: ref})
	if err != nil {
		t.Fatalf("Streams.Get: %v", err)
	}
	return st
}

// steps returns the steps of channel A's stream calls.
func (e *adapterEnv) steps() []string {
	var out []string
	for _, c := range e.calls(adapterChanA, model.CapabilityStream) {
		out = append(out, c.Step)
	}
	return out
}

// settle runs the worker until the queued work, including commands that
// waited for a predecessor, has run.
func (e *adapterEnv) settle(t *testing.T) {
	t.Helper()
	e.start(t)
	for range 3 {
		e.later(t, time.Minute)
	}
	e.stop(t)
}

func wantStreamState(t *testing.T, st model.Stream, state string) {
	t.Helper()
	if st.State != state {
		t.Fatalf("stream state = %q, want %q", st.State, state)
	}
}

func onlyText(c model.Content) string {
	if len(c.Parts) != 1 {
		return ""
	}
	return c.Parts[0].Text
}

var interactionCases = []adapterCase{
	{name: "NativeStreamInOrder", needs: []model.Capability{model.CapabilityStream}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		ref := e.startStream(t, conv.Ref, model.StreamNative)
		e.updateStream(t, ref, 1, "Hel")
		e.updateStream(t, ref, 2, "Hello")
		e.endStream(t, ref, 2, false, "op-end")
		e.settle(t)
		if got, want := e.steps(), []string{StepStart, StepUpdate, StepUpdate, StepFinish}; !slices.Equal(got, want) {
			t.Fatalf("stream calls = %v, want %v", got, want)
		}
		calls := e.calls(adapterChanA, model.CapabilityStream)
		if calls[1].Sequence != 1 || onlyText(calls[1].Content) != "Hel" || calls[2].Sequence != 2 || onlyText(calls[2].Content) != "Hello" {
			t.Fatalf("updates = %+v, want sequences 1 then 2", calls[1:3])
		}
		for _, c := range calls {
			if c.Route != conv.ReplyRoute || !c.Applied {
				t.Fatalf("stream call %+v, want applied on the stored route", c)
			}
		}
		st := e.getStream(t, ref)
		wantStreamState(t, st, model.StreamFinished)
		if st.ProviderStreamRef == "" || st.AppliedSequence != 2 || len(st.ProviderReferences) == 0 {
			t.Fatalf("stream = %+v, want the provider stream and references through sequence 2", st)
		}
		if msgs := e.messages(t, adapterInboxA); len(msgs) != 0 {
			t.Fatalf("stream stored messages %+v", msgs)
		}
	}},
	{name: "NativeStreamCancel", needs: []model.Capability{model.CapabilityStream}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		ref := e.startStream(t, conv.Ref, model.StreamNative)
		e.updateStream(t, ref, 1, "part")
		e.endStream(t, ref, 1, true, "op-cancel")
		e.settle(t)
		if got, want := e.steps(), []string{StepStart, StepUpdate, StepCancel}; !slices.Equal(got, want) {
			t.Fatalf("stream calls = %v, want %v", got, want)
		}
		wantStreamState(t, e.getStream(t, ref), model.StreamCancelled)
	}},
	{name: "StreamEndsOnce", needs: []model.Capability{model.CapabilityStream}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		ref := e.startStream(t, conv.Ref, model.StreamNative)
		first := e.endStream(t, ref, 0, false, "op-end")
		again := e.endStream(t, ref, 0, false, "op-end")
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("retransmitted end = %+v, want the stored result", again)
		}
		_, err := e.client.Streams().End(e.ctx, model.EndStream{Command: command("op-end-2"), Stream: ref})
		if model.CodeOf(err) != model.Conflict {
			t.Fatalf("second end = %v, want conflict", err)
		}
		e.settle(t)
		if got, want := e.steps(), []string{StepStart, StepFinish}; !slices.Equal(got, want) {
			t.Fatalf("stream calls = %v, want %v", got, want)
		}
	}},
	{name: "NativeFinishRejectedIsPartial", needs: []model.Capability{model.CapabilityStream}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAccepted}, Response{Outcome: OutcomeAccepted}, Response{Outcome: OutcomeRejected})
		ref := e.startStream(t, conv.Ref, model.StreamNative)
		e.updateStream(t, ref, 1, "part")
		end := e.endStream(t, ref, 1, false, "op-end")
		e.settle(t)
		wantOp(t, e.op(t, end.Operation.Ref), model.Rejected, model.Permanent)
		wantStreamState(t, e.getStream(t, ref), model.StreamPartial)
	}},
	{name: "EditStream", needs: []model.Capability{model.CapabilitySend, model.CapabilityEdit}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		ref := e.startStream(t, conv.Ref, model.StreamEdit)
		e.updateStream(t, ref, 1, "Hel")
		e.updateStream(t, ref, 2, "Hello")
		e.endStream(t, ref, 2, false, "op-end")
		e.settle(t)
		sends, edits := e.calls(adapterChanA, model.CapabilitySend), e.calls(adapterChanA, model.CapabilityEdit)
		if len(sends) != 1 || len(edits) != 1 || len(sends[0].References) != 1 {
			t.Fatalf("sends %+v and edits %+v, want one of each", sends, edits)
		}
		if !slices.Equal(edits[0].References, sends[0].References) {
			t.Fatalf("edit touched %v, want the sent message %v", edits[0].References, sends[0].References)
		}
		wantStreamState(t, e.getStream(t, ref), model.StreamFinished)
	}},
	{name: "BufferStream", needs: []model.Capability{model.CapabilitySend}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		ref := e.startStream(t, conv.Ref, model.StreamBuffer)
		e.updateStream(t, ref, 1, "draft")
		e.updateStream(t, ref, 2, "final")
		e.endStream(t, ref, 2, false, "op-end")
		e.settle(t)
		sends := e.calls(adapterChanA, model.CapabilitySend)
		if len(sends) != 1 || onlyText(sends[0].Content) != "final" {
			t.Fatalf("sends = %+v, want one send of the last update", sends)
		}
		wantStreamState(t, e.getStream(t, ref), model.StreamFinished)
	}},
	{name: "EphemeralToParticipant", needs: []model.Capability{model.CapabilitySendEphemeral}, run: func(t *testing.T, e *adapterEnv) {
		conv, id := e.participant(t)
		a, err := e.client.Interactions().Ephemeral(e.ctx, model.Ephemeral{
			Command: command("op-ephemeral"), Conversation: conv.Ref, Recipient: id.Ref,
			Content: text("only you"), ExpiresAt: e.clock.Now().Add(time.Hour),
		})
		if err != nil {
			t.Fatalf("Interactions.Ephemeral: %v", err)
		}
		e.settle(t)
		wantOp(t, e.op(t, a.Operation.Ref), model.ProviderAccepted, "")
		calls := e.calls(adapterChanA, model.CapabilitySendEphemeral)
		if len(calls) != 1 || !calls[0].Applied || calls[0].Recipient.ID != id.SourceID || calls[0].Recipient.Namespace != id.Namespace {
			t.Fatalf("ephemeral sends = %+v, want one to %s", calls, id.SourceID)
		}
		if sends := e.calls(adapterChanA, model.CapabilitySend); len(sends) != 0 {
			t.Fatalf("ephemeral fell back to sends %+v", sends)
		}
		if msgs := e.messages(t, adapterInboxA); len(msgs) != 0 {
			t.Fatalf("ephemeral stored messages %+v", msgs)
		}
	}},
	{name: "EphemeralNotAfterDeadline", needs: []model.Capability{model.CapabilitySendEphemeral}, run: func(t *testing.T, e *adapterEnv) {
		conv, id := e.participant(t)
		a, err := e.client.Interactions().Ephemeral(e.ctx, model.Ephemeral{
			Command: command("op-ephemeral"), Conversation: conv.Ref, Recipient: id.Ref,
			Content: text("too late"), ExpiresAt: e.clock.Now().Add(time.Second),
		})
		if err != nil {
			t.Fatalf("Interactions.Ephemeral: %v", err)
		}
		e.clock.Advance(time.Second)
		e.settle(t)
		wantOp(t, e.op(t, a.Operation.Ref), model.Failed, model.DeadlineExpired)
		if calls := e.calls(adapterChanA, model.CapabilitySendEphemeral); len(calls) != 0 {
			t.Fatalf("expired ephemeral reached the provider: %+v", calls)
		}
	}},
	{name: "SignalSentOnceNotRecorded", needs: []model.Capability{model.CapabilitySignal}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		err := e.client.Interactions().Signal(e.ctx, model.Signal{
			Call: model.Call{Correlation: "c-signal"}, Conversation: conv.Ref, Kind: "typing",
			Audience: model.Audience{Visibility: model.Public}, ExpiresAt: e.clock.Now().Add(5 * time.Second),
		})
		if err != nil {
			t.Fatalf("Interactions.Signal: %v", err)
		}
		e.settle(t)
		signals := e.calls(adapterChanA, model.CapabilitySignal)
		if len(signals) != 1 || signals[0].Kind != "typing" || signals[0].Route != conv.ReplyRoute {
			t.Fatalf("signals = %+v, want one typing signal on the stored route", signals)
		}
		if n := len(e.provider.Calls(adapterChanA)); n != 1 {
			t.Fatalf("provider calls = %d, want only the signal", n)
		}
		if msgs := e.messages(t, adapterInboxA); len(msgs) != 0 {
			t.Fatalf("signal stored messages %+v", msgs)
		}
	}},
}

var interactionUnsupportedCases = []adapterCase{
	{name: "NativeStreamWithoutStream", absent: []model.Capability{model.CapabilityStream}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		_, err := e.client.Streams().Start(e.ctx, model.StartStream{
			Command: command("op-start"), Conversation: conv.Ref, Audience: model.Audience{Visibility: model.Public}, Mode: model.StreamNative,
		})
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("native stream without stream = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
	{name: "EphemeralWithoutSendEphemeral", absent: []model.Capability{model.CapabilitySendEphemeral}, run: func(t *testing.T, e *adapterEnv) {
		conv, id := e.participant(t)
		_, err := e.client.Interactions().Ephemeral(e.ctx, model.Ephemeral{
			Command: command("op-ephemeral"), Conversation: conv.Ref, Recipient: id.Ref,
			Content: text("only you"), ExpiresAt: e.clock.Now().Add(time.Hour),
		})
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("ephemeral without send_ephemeral = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none: no fallback", calls)
		}
	}},
	{name: "SignalWithoutSignal", absent: []model.Capability{model.CapabilitySignal}, run: func(t *testing.T, e *adapterEnv) {
		conv := e.outbound(t)
		err := e.client.Interactions().Signal(e.ctx, model.Signal{
			Call: model.Call{Correlation: "c-signal"}, Conversation: conv.Ref, Kind: "typing",
			Audience: model.Audience{Visibility: model.Public}, ExpiresAt: e.clock.Now().Add(5 * time.Second),
		})
		if model.CodeOf(err) != model.Unsupported {
			t.Fatalf("signal without signal = %v, want unsupported", err)
		}
		if calls := e.provider.Calls(adapterChanA); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none", calls)
		}
	}},
}
