package conformance

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// ingressToken is the response token the ingress cases have the provider
// report. It must reach the store beside the record and nothing else.
const ingressToken = "tok-conformance-ingress-0001"

// interactionEvent is an action by sender on the provider message src
// (none when empty), with a response token valid for a quarter of an hour
// and a protocol deadline three seconds out; mod adjusts it.
func (e *adapterEnv) interactionEvent(id, sender, src string, mod ...func(*InboundInteraction)) InboundEvent {
	ch := adapterChanA
	in := InboundInteraction{
		Kind: model.InteractionAction, Action: "approve", Token: ingressToken, TokenTTL: 15 * time.Minute, Deadline: 3 * time.Second,
	}
	if e.h.NoResponseToken {
		in.Token, in.TokenTTL = "", 0
	}
	for _, m := range mod {
		m(&in)
	}
	ev := InboundEvent{
		EventID: e.shape(ch, FixtureEvent, "ix-"+id), MessageID: src,
		Destination: e.shape(ch, FixtureDestination, "support"), Sender: e.shape(ch, FixtureSender, sender),
		Room: e.shape(ch, FixtureRoom, "room-1"), Interaction: &in,
	}
	if s, ok := e.provider.(EventShaper); ok {
		ev = s.ShapeEvent(ch, ev)
	}
	return ev
}

// interactions returns the interactions of inbox A as the engine reports
// them.
func (e *adapterEnv) interactions(t *testing.T) []model.Interaction {
	t.Helper()
	var out []model.Interaction
	req := model.ListRequest{Call: model.Call{Correlation: "conformance-interactions"}, Filter: model.Filter{Inboxes: []model.InboxRef{adapterInboxA}}, Page: model.PageRequest{Limit: 100}}
	for {
		p, err := e.client.Interactions().List(e.ctx, req)
		if err != nil {
			t.Fatalf("Interactions.List: %v", err)
		}
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		req.Page.After = p.Next
	}
}

// eventsOf returns the events of kind a fresh subscription from the
// beginning reads.
func (e *adapterEnv) eventsOf(t *testing.T, kind string) []model.Event {
	t.Helper()
	e.subs++
	sub, err := e.client.Events().Open(e.ctx, model.OpenSubscription{
		Command:  model.Command{Call: model.Call{Correlation: "c-events"}, Operation: model.OperationID(fmt.Sprintf("op-events-%d", e.subs))},
		Consumer: fmt.Sprintf("conformance-events-%d", e.subs), Filter: model.Filter{Kinds: []string{kind}},
		Start: model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		t.Fatalf("Events.Open: %v", err)
	}
	b, err := e.client.Events().Poll(e.ctx, model.Poll{Call: model.Call{Correlation: "c-poll"}, Subscription: sub.Subscription.Ref, Limit: 100})
	if err != nil {
		t.Fatalf("Events.Poll: %v", err)
	}
	return b.Events
}

// storedToken returns the response token the store holds for ref.
func (e *adapterEnv) storedToken(t *testing.T, ref model.InteractionRef) string {
	t.Helper()
	var tok store.ResponseToken
	e.view(t, func(r store.Reader) (err error) { tok, err = r.InteractionToken(e.ctx, ref); return err })
	return tok.Token
}

// receiveInteraction seeds the inbound inbox, lets the provider deliver a
// message from peer-1 and returns it with the interaction event on it by
// the same sender, once both are acknowledged.
func (e *adapterEnv) receiveInteraction(t *testing.T, mod ...func(*InboundInteraction)) (msg, ix InboundEvent) {
	t.Helper()
	e.inbound(t)
	msg = e.message(adapterChanA, "1", "peer-1", "support")
	wantAcked(t, e.receiveAll(t, adapterChanA, msg))
	ix = e.interactionEvent("1", "peer-1", msg.MessageID, mod...)
	wantAcked(t, e.receiveAll(t, adapterChanA, ix))
	return msg, ix
}

var interactionIngressCases = []adapterCase{
	// An interaction is an interaction record, never a message: it names
	// its actor, kind and identifiers, joins the conversation of the
	// message its control sits on when the adapter assigns provider message
	// IDs, and is announced by one interaction.received event.
	{name: "RecordedAsInteractionNotMessage", events: []string{model.EventInteractionReceived}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		msg, ix := e.receiveInteraction(t)
		ins := e.interactions(t)
		if len(ins) != 1 {
			t.Fatalf("interactions = %d, want 1", len(ins))
		}
		in := ins[0]
		if in.Kind != model.InteractionAction || in.Action != "approve" || in.Source != e.desc.Type || in.Audience.Visibility != model.Public {
			t.Errorf("interaction kind %q action %q source %q audience %+v, want a public approve action from %q",
				in.Kind, in.Action, in.Source, in.Audience, e.desc.Type)
		}
		if a := in.Actor.External; a.Channel != adapterChanA || a.ID != ix.Sender || !slices.Contains(e.desc.IdentityNamespaces, a.Namespace) {
			t.Errorf("interaction actor %+v, want sender %q on the channel in a declared namespace", a, ix.Sender)
		}
		if msgs := e.messages(t, adapterInboxA); len(msgs) != 1 {
			t.Fatalf("messages = %d, want only the one message the control sits on: an interaction is no message", len(msgs))
		}
		if got := e.eventsOf(t, model.EventInteractionReceived); len(got) != 1 || got[0].Resource != in.Ref.Ref() {
			t.Fatalf("interaction.received events = %+v, want one about the interaction", got)
		}
		if got := e.eventsOf(t, model.EventMessageReceived); len(got) != 1 {
			t.Fatalf("message.received events = %d, want only the message's", len(got))
		}
		if e.desc.Exchange.MessageIDs != adapter.MessageIDsProvider {
			return
		}
		src := findMessage(e.messages(t, adapterInboxA), adapterChanA, msg.MessageID)
		if src == nil || in.Message == nil || *in.Message != src.Ref || in.Ref.Conversation != src.Ref.Conversation {
			t.Errorf("interaction message %v conversation %v, want the source message %v and its conversation", in.Message, in.Ref.Conversation, src)
		}
	}},
	// A redelivered interaction is acknowledged again and recorded once.
	{name: "DuplicateReusesTheRecord", events: []string{model.EventInteractionReceived}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		_, ix := e.receiveInteraction(t)
		wantAcked(t, e.receiveAll(t, adapterChanA, ix))
		if n := len(e.interactions(t)); n != 1 {
			t.Fatalf("interactions = %d after a redelivery, want 1", n)
		}
		if got := e.eventsOf(t, model.EventInteractionReceived); len(got) != 1 {
			t.Fatalf("interaction.received events = %d after a redelivery, want 1", len(got))
		}
	}},
	// The actor is the authenticated sender; a name in the payload never
	// becomes an identity.
	{name: "ActorIsTheSenderNotAClaim", events: []string{model.EventInteractionReceived}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.inbound(t)
		ix := e.interactionEvent("1", "peer-1", "")
		ix.Claims = e.claims(adapterChanA, "peer-9")
		wantAcked(t, e.receiveAll(t, adapterChanA, ix))
		ins := e.interactions(t)
		if len(ins) != 1 || ins[0].Actor.External.ID != ix.Sender {
			t.Fatalf("interactions = %+v, want one by sender %q", ins, ix.Sender)
		}
		for _, id := range e.identities(t, adapterInboxA) {
			if strings.Contains(id.SourceID, "peer-9") {
				t.Errorf("identity %+v came from an unauthenticated claim", id)
			}
		}
	}},
	// An interaction whose protocol deadline already passed is still
	// recorded: the response may still be possible, and the late
	// acknowledgement is the provider's to judge.
	{name: "DeadlinePassedStillRecorded", events: []string{model.EventInteractionReceived}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.receiveInteraction(t, func(in *InboundInteraction) { in.Deadline = -time.Second })
		ins := e.interactions(t)
		if len(ins) != 1 || !ins[0].Deadline.Before(ins[0].RecordedAt) {
			t.Fatalf("interactions = %+v, want one recorded after its deadline", ins)
		}
	}},
	// The response token lives beside the record and is never read: not in
	// Get, List or an event.
	{name: "TokenStoredAndNeverRead", events: []string{model.EventInteractionReceived}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		if e.h.NoResponseToken {
			t.Skip("the provider has no response token")
		}
		_, ix := e.receiveInteraction(t)
		in := e.interactions(t)[0]
		if got := e.storedToken(t, in.Ref); got != ingressToken {
			t.Fatalf("stored token differs from the one the provider reported (empty: %v)", got == "")
		}
		got, err := e.client.Interactions().Get(e.ctx, model.Lookup[model.InteractionRef]{Call: model.Call{Correlation: "c-get"}, Ref: in.Ref})
		if err != nil {
			t.Fatalf("Interactions.Get: %v", err)
		}
		dump := fmt.Sprintf("%+v %+v", got, e.interactions(t))
		for _, kind := range []string{model.EventInteractionReceived, model.EventMessageReceived} {
			for _, ev := range e.eventsOf(t, kind) {
				dump += fmt.Sprintf("%+v %s", ev, ev.Payload.Data)
			}
		}
		if strings.Contains(dump, ingressToken) {
			t.Fatalf("the response token was read back for %s", ix.EventID)
		}
		if e.observed.disclose(ingressToken) {
			t.Fatal("the response token reached an observation")
		}
	}},
	// Redaction clears the submitted values and the token and keeps the
	// reference.
	{name: "RedactionClearsValuesAndToken", events: []string{model.EventInteractionReceived}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		e.receiveInteraction(t, func(in *InboundInteraction) {
			in.Arguments = "secret arguments"
			in.Fields = model.Extension{Name: "org.example.conformance.form", Version: 1, Data: []byte(`{"f":"secret"}`)}
		})
		in := e.interactions(t)[0]
		if in.Arguments == "" || len(in.Fields.Data) == 0 {
			t.Skip("the adapter does not carry arguments and fields to the record")
		}
		ref := in.Ref
		_, err := e.client.Privacy().Redact(e.ctx, model.Redact{
			Command: model.Command{Call: model.Call{Correlation: "c-redact"}, Operation: "op-redact", ExpectedRevision: in.Revision},
			Target:  ref.Ref(), Interaction: &ref, Reason: "erasure",
		})
		if err != nil {
			t.Fatalf("Privacy.Redact: %v", err)
		}
		got := e.interactions(t)[0]
		if !got.Redacted || got.Ref != in.Ref || got.Arguments != "" || len(got.Fields.Data) != 0 {
			t.Fatalf("interaction after redaction = %+v, want a tombstone keeping its reference", got)
		}
		if tok := e.storedToken(t, ref); tok != "" {
			t.Fatal("the response token survived redaction")
		}
	}},
}
