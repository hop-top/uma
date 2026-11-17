package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The inbound state cases have the provider report a conversation's state
// (StateProvider.DeliverState) and the engine apply it. Every report is
// complete: it names every item of the conversation, so no case depends on
// how a partial report overlays the held view.

// stateObserved is the instant a provider report of the cases is made
// after the instant new messages of the suite carry.
func stateObserved(after time.Duration) time.Time {
	return time.Unix(1_700_000_000, 0).UTC().Add(after)
}

// stateEvent is a complete report of the target's conversation, observed
// after the given time, in which set shows the state.
func (e *adapterEnv) stateEvent(t *testing.T, g mutationTarget, id string, after time.Duration, set func(*model.ProviderConversationState)) StateEvent {
	t.Helper()
	st := model.ProviderConversationState{Channel: adapterChanA, ObservedAt: stateObserved(after), Complete: true}
	if set != nil {
		set(&st)
	}
	ref := e.itemRef(t, g)
	return StateEvent{
		EventID: e.shape(adapterChanA, FixtureEvent, id), MessageID: ref.ID,
		Destination: e.shape(adapterChanA, FixtureDestination, "support"), State: st,
	}
}

// report has the provider report ev and hands it to the engine, requiring
// the adapter to acknowledge it.
func (e *adapterEnv) report(t *testing.T, sp StateProvider, ev StateEvent) {
	t.Helper()
	before := len(e.provider.Inbound(adapterChanA))
	if err := sp.DeliverState(adapterChanA, ev); err != nil {
		if err == ErrNotExpressible {
			t.Skipf("provider cannot report the state %+v", ev.State)
		}
		t.Fatalf("DeliverState %s: %v", ev.EventID, err)
	}
	wantAcked(t, e.pump(t, adapterChanA, before, InboundEvent{EventID: ev.EventID}))
}

// stateProviderOrSkip returns the provider's StateProvider, skipping the
// case without one or without inbound traffic.
func (e *adapterEnv) stateProviderOrSkip(t *testing.T) StateProvider {
	t.Helper()
	sp, ok := e.stateProvider()
	if !ok {
		t.Skip("provider does not implement StateProvider")
	}
	if !e.canReceive() {
		t.Skip("capabilities receive and receive_http not declared")
	}
	e.requireInbound(t)
	return sp
}

// reportableSteps returns the first step of every declared capability that
// has a report showing it.
func (e *adapterEnv) reportableSteps(t *testing.T) []*stateStep {
	t.Helper()
	var out []*stateStep
	for _, c := range e.declaredState() {
		for _, s := range e.capabilitySteps(t, c) {
			if s.before == nil && s.report != nil && s.holds != nil {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func (e *adapterEnv) wantNoReadMarker(t *testing.T, g mutationTarget) {
	t.Helper()
	e.view(t, func(r store.Reader) error {
		if m, err := r.ReadMarker(e.ctx, g.conv.Ref, "conformance-agent"); !model.IsCode(err, model.NotFound) {
			t.Fatalf("read marker = %+v, %v; want none: a provider read state never touches it", m, err)
		}
		return nil
	})
}

var adapterStateInboundCases = []adapterCase{
	{name: "StateMoveReResolvesReferences", needs: []model.Capability{model.CapabilityMove}, run: func(t *testing.T, e *adapterEnv) {
		if _, ok := e.stateProvider(); !ok {
			t.Skip("provider does not implement StateProvider")
		}
		steps := e.capabilitySteps(t, model.CapabilityMove)
		e.eachStep(t, steps[:1], func(t *testing.T, e *adapterEnv, s *stateStep) {
			g := e.stateTarget(t)
			old := e.itemRef(t, g)
			e.applyStep(t, g, s, "op-move")
			e.wantReferencesResolved(t, g, old)

			// The next change names the references the move re-resolved:
			// a provider that no longer holds the old one refuses it.
			var next *stateStep
			for _, o := range e.stateSteps(t) {
				if o.before == nil || o.before == s {
					if next == nil && o != s && o.holds != nil {
						next = o
					}
				}
			}
			if next == nil {
				e.stop(t)
				return
			}
			e.prepare(t, g, next)
			cur := e.itemRef(t, g)
			ref := e.applyStep(t, g, next, "op-next")
			calls := e.calls(adapterChanA, next.cap)
			if last := calls[len(calls)-1]; last.Operation != ref || len(last.Items) != 1 || last.Items[0] != cur {
				t.Fatalf("%s after the move: call items = %v, want the re-resolved reference %v", next.name, last.Items, cur)
			}
			e.wantItem(t, g, next)
			e.stop(t)
		})
	}},
	{name: "StateReportIsAppliedOnceInOrder", anyOf: stateAny, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		sp := e.stateProviderOrSkip(t)
		steps := e.reportableSteps(t)
		if len(steps) == 0 {
			t.Skip("no state a report can show")
		}
		g := e.stateTarget(t)
		show := func(st *model.ProviderConversationState) {
			for _, s := range steps {
				s.report(st)
			}
		}
		e.report(t, sp, e.stateEvent(t, g, "state-1", time.Minute, show))
		got := e.storedConversation(t, g.conv.Ref)
		st, ok := channelState(got)
		if !ok || !st.Complete || !st.ObservedAt.Equal(stateObserved(time.Minute)) {
			t.Fatalf("provider state = %+v, %v; want the report, complete, observed when the provider says", st, ok)
		}
		for _, s := range steps {
			if !s.holds(st) {
				t.Fatalf("provider state %+v does not show %s as reported", st, s.name)
			}
		}
		changes := e.stateChanges(t)
		if len(changes) != 1 || changes[0].payload.Trigger != model.TriggerProvider || changes[0].payload.Operation != nil {
			t.Fatalf("provider_state changes = %+v, want one by the provider, naming no operation", changes)
		}
		if got.Revision == g.conv.Revision {
			t.Fatal("an applied report kept the conversation revision")
		}
		e.wantNoReadMarker(t, g)

		// The same state again, a redelivery under another event ID, changes
		// nothing and announces nothing.
		e.report(t, sp, e.stateEvent(t, g, "state-2", 2*time.Minute, show))
		if again := e.storedConversation(t, g.conv.Ref); again.Revision != got.Revision || len(e.stateChanges(t)) != 1 {
			t.Fatalf("a report of the held state changed the conversation (revision %q, was %q) or announced it", again.Revision, got.Revision)
		}

		// A report observed before the held state does not undo it.
		e.report(t, sp, e.stateEvent(t, g, "state-stale", 30*time.Second, nil))
		if stale := e.storedConversation(t, g.conv.Ref); stale.Revision != got.Revision || len(e.stateChanges(t)) != 1 {
			t.Fatalf("a stale report changed the conversation (revision %q, was %q)", stale.Revision, got.Revision)
		}

		// A newer one that differs applies.
		e.report(t, sp, e.stateEvent(t, g, "state-3", 3*time.Minute, nil))
		next := e.storedConversation(t, g.conv.Ref)
		st, _ = channelState(next)
		for _, s := range steps {
			if s.holds(st) {
				t.Fatalf("provider state %+v still shows %s after a newer report of none", st, s.name)
			}
		}
		if cs := e.stateChanges(t); len(cs) != 2 || cs[1].payload.Trigger != model.TriggerProvider {
			t.Fatalf("provider_state changes = %d, want the two applied reports", len(cs))
		}
		e.wantNoReadMarker(t, g)
	}},
	{name: "StateReportOfUnknownConversationRecordsNothing", anyOf: stateAny, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		sp := e.stateProviderOrSkip(t)
		g := e.stateTarget(t)
		ev := e.stateEvent(t, g, "state-unknown", time.Minute, func(s *model.ProviderConversationState) { s.Archived = true })
		ev.MessageID = e.shape(adapterChanA, FixtureMessage, "msg-unknown-conversation")
		convs, msgs := len(e.conversations(t, adapterInboxA)), len(e.messages(t, adapterInboxA))
		e.report(t, sp, ev)
		if n := len(e.conversations(t, adapterInboxA)); n != convs {
			t.Fatalf("conversations = %d after a report of an unknown one, want %d", n, convs)
		}
		if n := len(e.messages(t, adapterInboxA)); n != msgs {
			t.Fatalf("messages = %d after a report of an unknown conversation, want %d", n, msgs)
		}
		if got := e.storedConversation(t, g.conv.Ref); got.Revision != g.conv.Revision || len(got.ProviderState) != 0 || len(e.stateChanges(t)) != 0 {
			t.Fatalf("conversation %+v changed by a report of another one", got)
		}
	}},
	{name: "StateReportOfUnsettledChangeSettlesItOnce", anyOf: stateAny, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		sp := e.stateProviderOrSkip(t)
		steps := e.reportableSteps(t)
		if len(steps) == 0 {
			t.Skip("no state a report can show")
		}
		s := steps[0]
		g := e.stateTarget(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeAnswerLost})
		a, err := s.submit(e, g.conv.Ref, "op-echo")
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		ref := a.Operation.Ref
		e.later(t, 0)
		wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
		e.wantHolds(t, g, s, false)

		// The provider's own report of the change settles the operation
		// without sending it again.
		e.report(t, sp, e.stateEvent(t, g, "state-echo", time.Minute, s.report))
		e.later(t, 0)
		op := e.op(t, ref)
		wantActApplied(t, op)
		if op.PendingState != nil {
			t.Fatalf("settled operation still pending: %+v", op.PendingState)
		}
		e.later(t, time.Hour)
		if n := len(e.calls(adapterChanA, s.cap)); n != 1 {
			t.Fatalf("provider %s calls = %d, want 1: the echo settles it, nothing is sent again", s.cap, n)
		}
		e.wantHolds(t, g, s, true)
		if cs := e.stateChanges(t); len(cs) != 1 || cs[0].payload.Trigger != model.TriggerProvider {
			t.Fatalf("provider_state changes = %+v, want the one the report applied", cs)
		}
		e.wantNoReadMarker(t, g)
		e.stop(t)
	}},
	{name: "StateReportNamesReResolvedReference", needs: []model.Capability{model.CapabilityMove}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		sp := e.stateProviderOrSkip(t)
		g := e.stateTarget(t)
		move := e.capabilitySteps(t, model.CapabilityMove)[0]
		e.applyStep(t, g, move, "op-move")
		e.report(t, sp, e.stateEvent(t, g, "state-moved", time.Minute, func(s *model.ProviderConversationState) { s.Read = boolValue(true) }))
		st, ok := channelState(e.storedConversation(t, g.conv.Ref))
		if !ok || st.Read == nil || !*st.Read {
			t.Fatalf("provider state = %+v, %v; want the report applied: the conversation is found by the reference the move re-resolved", st, ok)
		}
		e.stop(t)
	}},
}
