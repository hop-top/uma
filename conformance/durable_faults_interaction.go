package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The provider posts a sent message and its recipient presses a control on
// it before the send's answer reaches the engine: the interaction is
// recorded naming the provider message and no local message, since no
// message holds the provider message ID yet. The worker then settles the
// send from its answer and records that ID on the outbound message, which
// links the interaction to it. The link emits no event: interaction.received
// is the interaction's only event.
func interactionBeforeSendAnswerIsLinked(t *testing.T, e *env, _ func(*testing.T)) {
	f := newFaultEnv(t, e, true)
	sending := f.gate("provider send")
	f.p.respond(held(sending))
	p := f.start(t)
	_, stop := p.receive()
	defer stop()
	op := p.reply(t, "op-1")
	p.work()
	sending.await(t)
	src := f.p.ref(f.p.lastSend())
	f.push(t, f.interaction("ix-1", src))
	if r := f.accept(t); r.err != nil || !r.receipt.Committed || r.receipt.Duplicate {
		t.Fatalf("interaction ingress = %+v, %v; want it recorded", r.receipt, r.err)
	}
	before := f.interactions(t)
	if len(before) != 1 || before[0].Message != nil || before[0].MessageExternal == nil || *before[0].MessageExternal != src {
		t.Fatalf("interactions before the answer = %+v, want one naming provider message %v and no local message", before, src)
	}
	sending.open()
	p.idle(t)
	got := p.op(t, op)
	wantOpState(t, got, model.ProviderAccepted, "")
	after := f.interactions(t)
	if len(after) != 1 || got.ResultMessage == nil || after[0].Message == nil || *after[0].Message != *got.ResultMessage {
		t.Fatalf("interactions after the answer = %+v, want the one linked to the reply's message %v", after, got.ResultMessage)
	}
	if after[0].Revision == before[0].Revision || *after[0].MessageExternal != src {
		t.Fatalf("linked interaction = %+v, want a new revision and its provider message kept", after[0])
	}
	var received, updated int
	for _, ev := range f.allEvents(t, model.Filter{}, 50) {
		switch ev.Kind {
		case model.EventInteractionReceived:
			received++
		case model.EventInteractionUpdated:
			updated++
		}
	}
	if received != 1 || updated != 0 {
		t.Fatalf("interaction.received = %d, interaction.updated = %d; want 1 and none", received, updated)
	}
}

// interaction is an action by the fault provider's user on provider
// message src, with a deadline a minute out.
func (f *faultEnv) interaction(event string, src model.ExternalRef) model.Ingress {
	actor := model.ExternalRef{Channel: f.ch, Namespace: faultNS, Kind: "user", ID: "u-1"}
	now := f.fx.Now()
	return model.Ingress{Call: model.Call{Correlation: "rx-" + model.CorrelationID(event)}, Envelope: model.Envelope{
		EventID: event, Destination: faultDest, Kind: model.EnvelopeInteraction,
		Sender: actor, Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: now,
		Interaction: &model.InteractionInput{
			ID: event, Kind: model.InteractionAction, Action: "approve", Actor: actor, Message: &src,
			Deadline: now.Add(time.Minute),
		},
	}}
}

// interactions returns the interactions the store holds, in recording
// order.
func (f *faultEnv) interactions(t *testing.T) []model.Interaction {
	t.Helper()
	var out []model.Interaction
	f.mustView(t, func(r store.Reader) error {
		p, err := r.Interactions(f.ctx, list(100))
		out = p.Items
		return err
	})
	return out
}
