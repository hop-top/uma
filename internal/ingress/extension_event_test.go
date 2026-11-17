package ingress_test

import (
	"encoding/json"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Event kinds of the extension-event adapter: pinned is declared with its
// capability; labeled is declared without its capability; archived is
// registered but not declared; pin is an effect.
const (
	evPinned      = "com.example.chat.messages.pinned"
	evLabeled     = "com.example.chat.messages.labeled"
	evArchived    = "com.example.chat.messages.archived"
	evPin         = "com.example.chat.messages.pin"
	evPinCap      = model.Capability("com.example.chat.messages.pin")
	evLabelCap    = model.Capability("com.example.chat.messages.label")
	evSeePins     = "com.example.chat.messages.see_pins"
	evUnregistred = "com.example.chat.messages.unknown"
)

// eventAdapter is the harness adapter of its type declaring the extension
// event kinds and the capability of pinned.
type eventAdapter struct{ ingressAdapter }

func (a eventAdapter) Describe() adapter.Descriptor {
	d := a.ingressAdapter.Describe()
	d.Capabilities = append(d.Capabilities, evPinCap)
	d.Events = append(d.Events, evPinned, evLabeled)
	return d
}

// withExtensionEvents registers the event definitions and declares them on
// the harness adapter of every type.
func withExtensionEvents(c *engine.Config) {
	for i, f := range c.Adapters {
		if a, ok := f.(ingressAdapter); ok {
			c.Adapters[i] = eventAdapter{a}
		}
	}
	event := func(name string, capability model.Capability) adapter.ExtensionDefinition {
		return adapter.ExtensionDefinition{Name: name, Version: 1, Kind: model.ExtensionKindEvent, RequiredCapability: capability, AuthorizeAs: evSeePins}
	}
	c.Extensions = append(c.Extensions,
		event(evPinned, evPinCap), event(evLabeled, evLabelCap), event(evArchived, evPinCap),
		adapter.ExtensionDefinition{Name: evPin, Version: 1, Kind: model.ExtensionKindEffect, RequiredCapability: evPinCap, AuthorizeAs: evPin},
	)
}

func eventHarness(t *testing.T, typ model.TypeKey) (*harness, model.Inbox) {
	t.Helper()
	h := newHarness(t, typ, withExtensionEvents)
	return h, h.Configure(perContact(false), dest)
}

// pinnedEvent is an extension.event envelope carrying pinned v1 about the
// provider message msgID, or about the inbox when msgID is empty.
func pinnedEvent(eventID, msgID string, v model.Visibility) model.Envelope {
	return model.Envelope{
		EventID: eventID, MessageID: msgID, Destination: dest, Kind: model.EnvelopeExtensionEvent,
		Audience: model.Audience{Visibility: v}, OccurredAt: t0,
		Event: &model.Extension{Name: evPinned, Version: 1, Data: json.RawMessage(`{"pin":"p-1","by":"provider-user-7"}`)},
	}
}

func eventsOf(h *harness, kind string) []model.Event {
	var out []model.Event
	for _, ev := range h.Events() {
		if ev.Kind == kind {
			out = append(out, ev)
		}
	}
	return out
}

// An extension event about the inbox appends exactly one event of the
// registered kind, its payload the envelope's event as emitted, under the
// envelope's audience, and writes nothing else.
func TestExtensionEventOnInbox(t *testing.T) {
	h, in := eventHarness(t, chatType)
	env := pinnedEvent("ev-1", "", model.Internal)
	r := h.MustAccept(env)
	if !r.Committed || r.Duplicate {
		t.Fatalf("receipt = %+v, want committed", r)
	}
	evs := h.Events()
	if len(evs) != 1 {
		t.Fatalf("events = %v, want one", eventKinds(evs))
	}
	ev := evs[0]
	want := in.Ref.Ref()
	switch {
	case ev.Kind != evPinned:
		t.Fatalf("kind = %q, want %q", ev.Kind, evPinned)
	case ev.Resource != want || ev.Inbox != want.Inbox || ev.Tenant != tenantA:
		t.Fatalf("resource = %+v (inbox %q), want the inbox %+v", ev.Resource, ev.Inbox, want)
	case ev.Payload.Name != evPinned || ev.Payload.Version != 1 || string(ev.Payload.Data) != string(env.Event.Data):
		t.Fatalf("payload = %s v%d %s, want the emitted event", ev.Payload.Name, ev.Payload.Version, ev.Payload.Data)
	case ev.Audience != env.Audience:
		t.Fatalf("audience = %+v, want %+v", ev.Audience, env.Audience)
	case ev.Actor != serviceAct || ev.Causation != r.ID || ev.Correlation != "corr-ev-1":
		t.Fatalf("event %+v, want actor %s caused by %s", ev, serviceAct, r.ID)
	case ev.Revision != in.Revision:
		t.Fatalf("revision = %q, want the inbox revision %q", ev.Revision, in.Revision)
	}
	if err := ev.Validate(); err != nil {
		t.Fatalf("event invalid: %v", err)
	}
	if len(h.Messages(in.Ref)) != 0 || len(h.Conversations(in.Ref)) != 0 || len(h.Identities(in.Ref)) != 0 || len(h.Contacts()) != 0 {
		t.Fatal("an extension event wrote records besides its event")
	}
}

// A redelivered extension event is a duplicate: one event in the log.
func TestExtensionEventRedeliveryIsDuplicate(t *testing.T) {
	h, _ := eventHarness(t, chatType)
	env := pinnedEvent("ev-1", "", model.Public)
	first := h.MustAccept(env)
	again := h.MustAccept(env)
	if !again.Duplicate || again.ID != first.ID {
		t.Fatalf("redelivery = %+v, want duplicate of %s", again, first.ID)
	}
	other := pinnedEvent("ev-2", "", model.Public)
	if r := h.MustAccept(other); r.Duplicate {
		t.Fatal("a new event ID was taken as a duplicate")
	}
	if n := len(eventsOf(h, evPinned)); n != 2 {
		t.Fatalf("pinned events = %d, want 2", n)
	}
}

// An extension event naming a provider message is about that message. A
// non-public message gives the event its audience; a public message takes
// the envelope's: never wider than the message.
func TestExtensionEventOnMessage(t *testing.T) {
	h, in := eventHarness(t, chatType)
	h.MustAccept(h.Inbound("e-pub", "m-pub", "u1", dest))
	private := h.Inbound("e-priv", "m-priv", "u1", dest)
	private.Audience = model.Audience{Visibility: model.Restricted, PolicyRef: "staff"}
	h.MustAccept(private)
	pub, priv := h.MessageByProviderID(in.Ref, "m-pub"), h.MessageByProviderID(in.Ref, "m-priv")

	h.MustAccept(pinnedEvent("ev-pub", "m-pub", model.Internal))
	h.MustAccept(pinnedEvent("ev-priv", "m-priv", model.Public))
	evs := eventsOf(h, evPinned)
	if len(evs) != 2 {
		t.Fatalf("pinned events = %d, want 2", len(evs))
	}
	for i, c := range []struct {
		msg      model.Message
		audience model.Audience
	}{
		{pub, model.Audience{Visibility: model.Internal}},
		{priv, private.Audience},
	} {
		ev := evs[i]
		res := c.msg.Ref.Ref()
		if ev.Resource != res || ev.Inbox != in.Ref.ID || ev.Revision != c.msg.Revision {
			t.Fatalf("event %d resource %+v rev %q, want message %+v rev %q", i, ev.Resource, ev.Revision, res, c.msg.Revision)
		}
		if ev.Audience != c.audience {
			t.Fatalf("event %d audience = %+v, want %+v", i, ev.Audience, c.audience)
		}
	}
	if got := h.MessageByProviderID(in.Ref, "m-pub"); got.Revision != pub.Revision {
		t.Fatal("an extension event changed its message")
	}
}

// An unknown target message is not found and leaves no trace, so the
// adapter redelivers it once the message arrives; a provider ID stored
// twice is a conflict.
func TestExtensionEventTargetLookup(t *testing.T) {
	h, in := eventHarness(t, chatType)
	env := pinnedEvent("ev-early", "m-late", model.Public)
	_, err := h.Accept(env)
	requireCode(t, err, model.NotFound, "target message not found")
	if len(h.Events()) != 0 {
		t.Fatal("events appended for an unknown target")
	}
	h.MustAccept(h.Inbound("e-late", "m-late", "u1", dest))
	if r := h.MustAccept(env); r.Duplicate {
		t.Fatal("the rejected event was claimed")
	}
	if n := len(eventsOf(h, evPinned)); n != 1 {
		t.Fatalf("pinned events = %d, want 1", n)
	}

	m := h.MessageByProviderID(in.Ref, "m-late")
	twin := m
	twin.Ref.ID, twin.Revision = h.ID(), "r1"
	h.Tx(func(tx store.Tx) error { return tx.PutMessage(h.Ctx, twin, "") })
	_, err = h.Accept(pinnedEvent("ev-twin", "m-late", model.Public))
	requireCode(t, err, model.Conflict, "")
}

// An extension event about a redacted or a provider-deleted message is
// accepted and appends no event: the log never carries notices about erased
// or withdrawn content, as edits and reactions to such a message are skipped.
func TestExtensionEventOnRedactedMessage(t *testing.T) {
	h, in := eventHarness(t, chatType)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	m := h.MessageByProviderID(in.Ref, "m1")
	_, err := h.Client.Privacy().Redact(h.Ctx, model.Redact{
		Command: model.Command{Call: model.Call{Correlation: "corr"}, Operation: "op-redact", ExpectedRevision: m.Revision},
		Target:  m.Ref.Ref(), Message: &m.Ref,
	})
	if err != nil {
		t.Fatalf("Redact: %v", err)
	}
	if !h.MessageByProviderID(in.Ref, "m1").Redacted {
		t.Fatal("setup: message not redacted")
	}
	r := h.MustAccept(pinnedEvent("ev-1", "m1", model.Public))
	if !r.Committed || r.Duplicate {
		t.Fatalf("receipt = %+v, want committed", r)
	}
	if n := len(eventsOf(h, evPinned)); n != 0 {
		t.Fatalf("pinned events = %d about a redacted message, want 0", n)
	}

	h.MustAccept(h.Inbound("e2", "m2", "u1", dest))
	del := h.Inbound("e-del", "m2", "u1", dest)
	del.Kind, del.Content = model.EnvelopeMessageDeleted, model.Content{}
	h.MustAccept(del)
	if got := h.MessageByProviderID(in.Ref, "m2"); !got.Deleted || got.Redacted {
		t.Fatalf("setup: provider deletion = deleted %v redacted %v", got.Deleted, got.Redacted)
	}
	h.MustAccept(pinnedEvent("ev-2", "m2", model.Public))
	if n := len(eventsOf(h, evPinned)); n != 0 {
		t.Fatalf("pinned events = %d about a provider-deleted message, want 0", n)
	}
}

// Under sender-chosen message IDs, an extension event never attaches to a
// message: a sender could name anyone's message.
func TestExtensionEventSenderChosenIDs(t *testing.T) {
	h, _ := eventHarness(t, model.TypeSmtpMail)
	_, err := h.Accept(pinnedEvent("ev-1", "<m1@a.example>", model.Public))
	requireCode(t, err, model.Validation, "envelope.message_id")
	if r := h.MustAccept(pinnedEvent("ev-2", "", model.Public)); !r.Committed {
		t.Fatalf("inbox event under sender-chosen IDs = %+v, want committed", r)
	}
}

// Emitting needs channel.receive like every ingress; a denial writes
// nothing.
func TestExtensionEventNeedsReceive(t *testing.T) {
	h, p := receiveHarness(t, withExtensionEvents)
	h.Configure(perContact(false), dest)
	p.denied.Store(true)
	_, err := h.Accept(pinnedEvent("ev-1", "", model.Public))
	requireCode(t, err, model.Unauthorized, "")
	if len(h.Events()) != 0 {
		t.Fatal("event appended under a denied ingress")
	}
	asked := p.requests()
	if len(asked) == 0 || asked[len(asked)-1].Action != model.ActionChannelReceive {
		t.Fatalf("authorization requests = %+v, want channel.receive", asked)
	}
}

// Every malformed extension event is rejected before any write: unknown
// and undeclared kinds and missing declarations are invalid, a name of
// another extension kind or an unregistered version is a schema mismatch,
// and no sender, content, references, provider payload or other kind's
// state rides along.
func TestExtensionEventRejects(t *testing.T) {
	h, in := eventHarness(t, chatType)
	edit := func(f func(*model.Envelope)) model.Envelope {
		env := pinnedEvent("ev-bad", "", model.Public)
		f(&env)
		return env
	}
	named := func(name string, version uint32) model.Envelope {
		return edit(func(e *model.Envelope) { e.Event.Name, e.Event.Version = name, version })
	}
	onMessage := h.Inbound("e-x", "m-x", "u1", dest)
	onMessage.Event = &model.Extension{Name: evPinned, Version: 1}
	cases := []struct {
		name string
		env  model.Envelope
		code model.ErrorCode
		want string
	}{
		{"no event", edit(func(e *model.Envelope) { e.Event = nil }), model.Validation, "envelope.event: required"},
		{"event on a message", onMessage, model.Validation, "envelope.event: only an extension.event"},
		{"no event ID", edit(func(e *model.Envelope) { e.EventID, e.MessageID = "", "m1" }), model.Validation, "envelope.event_id"},
		{"zero version", named(evPinned, 0), model.Validation, "version"},
		{"invalid JSON", edit(func(e *model.Envelope) { e.Event.Data = json.RawMessage(`{`) }), model.Validation, "data"},
		{"unregistered", named(evUnregistred, 1), model.Validation, "unknown event kind"},
		{"builtin kind", named(model.EventMessageReceived, 1), model.Validation, "unknown event kind"},
		{"interaction kind", named("interaction.received", 1), model.Validation, "unknown event kind"},
		{"undeclared", named(evArchived, 1), model.Validation, "unknown event kind"},
		{"effect name", named(evPin, 1), model.SchemaMismatch, "not an event kind"},
		{"unregistered version", named(evPinned, 2), model.SchemaMismatch, "version not registered"},
		{"capability not declared", named(evLabeled, 1), model.Validation, "capability"},
		{"no audience", edit(func(e *model.Envelope) { e.Audience = model.Audience{} }), model.Validation, "visibility"},
		{"sender", edit(func(e *model.Envelope) { e.Sender = h.SenderRef("u1") }), model.Validation, "envelope.sender"},
		{"system actor", edit(func(e *model.Envelope) { e.SystemActor = true }), model.Validation, "envelope.system_actor"},
		{"sender kind", edit(func(e *model.Envelope) { e.SenderKind = model.ContactKindBot }), model.Validation, "envelope.sender_kind"},
		{"person sender kind", edit(func(e *model.Envelope) { e.SenderKind = model.ContactKindPerson }), model.Validation, "envelope.sender_kind"},
		{"sender attributes", edit(func(e *model.Envelope) { e.SenderAttributes = []model.Attribute{attrPhone} }), model.Validation, "envelope.sender_attributes"},
		{"content", edit(func(e *model.Envelope) { e.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "hi"}}} }),
			model.Validation, "envelope.content"},
		{"references", edit(func(e *model.Envelope) { e.References = []model.ExternalRef{h.Ref(model.RefKindRoom, "r1", "")} }),
			model.Validation, "envelope.references"},
		{"payload", edit(func(e *model.Envelope) { e.Payload = model.Extension{Name: "com.example.chat.raw", Version: 1} }),
			model.Validation, "envelope.payload"},
		{"interaction", edit(func(e *model.Envelope) { e.Interaction = &model.InteractionInput{ID: "i1"} }), model.Validation, "envelope.interaction"},
		{"work item", edit(func(e *model.Envelope) { e.WorkItem = &model.WorkItemState{} }), model.Validation, "envelope.work_item"},
		{"delivery", edit(func(e *model.Envelope) { e.Delivery = &model.DeliveryReport{State: model.Delivered} }), model.Validation, "envelope.delivery"},
		{"task", edit(func(e *model.Envelope) { e.Task = &model.TaskBinding{} }), model.Validation, "envelope.task"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := h.Accept(c.env)
			requireCode(t, err, c.code, c.want)
		})
	}
	if len(h.Events()) != 0 || len(h.Messages(in.Ref)) != 0 {
		t.Fatal("a rejected extension event wrote something")
	}
}
