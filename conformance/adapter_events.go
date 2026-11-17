package conformance

import (
	"errors"
	"slices"
	"testing"

	"hop.top/uma/model"
)

// ExtensionEvent is one provider notice of a registered event kind, for
// the adapter to normalize into a model.EnvelopeExtensionEvent envelope.
type ExtensionEvent struct {
	// EventID is the provider's ID of the notice.
	EventID string
	// Destination is the provider address the notice belongs to.
	Destination string
	// MessageID is the provider ID of a message delivered earlier that the
	// notice is about; empty for a notice about the inbox.
	MessageID string
	// Event is the notice's kind, version and data.
	Event model.Extension
}

// EventProvider is implemented by a Provider whose adapter emits
// extension events of the kinds it declares.
type EventProvider interface {
	// DeliverEvent queues one notice on channel's account, for the adapter
	// to normalize and hand to its sink. It returns ErrNotExpressible for
	// a notice the provider cannot express.
	DeliverEvent(channel model.ChannelRef, ev ExtensionEvent) error
}

// deliverEvent delivers ev through the EventProvider and pumps it into the
// engine, returning its result.
func (e *adapterEnv) deliverEvent(t *testing.T, ev ExtensionEvent) InboundResult {
	t.Helper()
	before := len(e.provider.Inbound(adapterChanA))
	if err := e.provider.(EventProvider).DeliverEvent(adapterChanA, ev); err != nil {
		if errors.Is(err, ErrNotExpressible) {
			t.Skipf("provider cannot express extension event %s", ev.EventID)
		}
		t.Fatalf("DeliverEvent %s: %v", ev.EventID, err)
	}
	return e.pump(t, adapterChanA, before, InboundEvent{EventID: ev.EventID})[0]
}

// eventPayload returns the harness event, or skips the case when the
// harness has none or the provider cannot deliver one.
func (e *adapterEnv) eventPayload(t *testing.T) model.Extension {
	t.Helper()
	x := e.h.Event.Clone()
	if x.Name == "" {
		t.Skip("AdapterHarness.Event not set")
	}
	if _, ok := e.provider.(EventProvider); !ok {
		t.Skip("provider is no EventProvider")
	}
	if !slices.Contains(e.desc.Events, x.Name) {
		t.Fatalf("AdapterHarness.Event %s is not an event kind the adapter declares", x.Name)
	}
	return x
}

var extensionEventCases = []adapterCase{
	// A provider notice of a registered event kind reaches a subscriber of
	// the kind once, redelivery included, as the event about the message it
	// names with the data the adapter emitted; a notice of an unregistered
	// kind is refused and the provider keeps it.
	{name: "RegisteredEventReachesSubscriber", inbound: true, run: func(t *testing.T, e *adapterEnv) {
		x := e.eventPayload(t)
		e.inbound(t)
		msg := e.message(adapterChanA, "1", "peer-1", "support")
		wantAcked(t, e.receiveAll(t, adapterChanA, msg))
		stored := findMessage(e.messages(t, adapterInboxA), adapterChanA, msg.MessageID)
		if stored == nil {
			t.Fatalf("no message references the provider message ID %q", msg.MessageID)
		}

		ev := ExtensionEvent{
			EventID: e.shape(adapterChanA, FixtureEvent, "evt-ext-1"), Destination: msg.Destination,
			MessageID: msg.MessageID, Event: x,
		}
		for range 2 {
			if r := e.deliverEvent(t, ev); !r.Acked {
				t.Fatalf("extension event %s not acknowledged after the sink accepted it", ev.EventID)
			}
		}
		unknown := ev
		unknown.EventID = e.shape(adapterChanA, FixtureEvent, "evt-ext-2")
		unknown.Event.Name = "org.example.conformance.unregistered"
		if r := e.deliverEvent(t, unknown); r.Acked {
			t.Fatal("an extension event of an unregistered kind was acknowledged")
		}

		sub, err := e.client.Events().Open(e.ctx, model.OpenSubscription{
			Command:  model.Command{Call: model.Call{Correlation: "c-ext-sub"}, Operation: "op-ext-sub"},
			Consumer: "conformance-extension-events", Filter: model.Filter{Kinds: []string{x.Name}},
			Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
		})
		if err != nil {
			t.Fatalf("Events.Open: %v", err)
		}
		b, err := e.client.Events().Poll(e.ctx, model.Poll{Call: model.Call{Correlation: "c-ext-poll"}, Subscription: sub.Subscription.Ref, Limit: 10})
		if err != nil {
			t.Fatalf("Events.Poll: %v", err)
		}
		if len(b.Events) != 1 {
			t.Fatalf("subscriber got %d events of %s, want exactly one", len(b.Events), x.Name)
		}
		got := b.Events[0]
		switch {
		case got.Kind != x.Name:
			t.Fatalf("event kind = %q, want %q", got.Kind, x.Name)
		case got.Resource != stored.Ref.Ref():
			t.Fatalf("event resource = %+v, want the message %+v", got.Resource, stored.Ref.Ref())
		case got.Payload.Name != x.Name || got.Payload.Version != x.Version || !sameJSON(got.Payload.Data, x.Data):
			t.Fatalf("event payload = %s v%d %s, want %s v%d %s", got.Payload.Name, got.Payload.Version, got.Payload.Data, x.Name, x.Version, x.Data)
		}
	}},
}
