package events_test

import (
	"encoding/json"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

const (
	pinnedKind   = "com.example.chat.messages.pinned"
	seePinsGrant = "com.example.chat.messages.see_pins"
)

func withPinned(c *engine.Config) {
	c.Extensions = []adapter.ExtensionDefinition{{
		Name: pinnedKind, Version: 1, RequiredCapability: "com.example.chat.messages.pin",
		AuthorizeAs: seePinsGrant, Kind: model.ExtensionKindEvent,
	}}
}

// pinned returns a registered extension event about the message of a
// message.received event, under the message's audience.
func pinned(about model.Event) model.Event {
	ev := about
	ev.ID = seqID(0xf0, 1)
	ev.Kind = pinnedKind
	ev.Audience = model.Audience{Visibility: model.Internal}
	ev.Payload = model.Extension{Name: pinnedKind, Version: 1, Data: json.RawMessage(`{"pin":"p-1"}`)}
	return ev
}

// A registered extension event reaches a subscriber only when it may read
// the event's resource at its audience and also holds the definition's
// policy action on that resource: a resource read alone, or the action
// alone, delivers nothing.
func TestPollGatesExtensionEventsOnTheirAction(t *testing.T) {
	for _, c := range []struct {
		name    string
		deny    string
		deliver bool
	}{
		{"both granted", "", true},
		{"action denied", seePinsGrant, false},
		{"resource read denied", model.ActionMessageRead, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newEvHarness(t, nil, withPinned)
			msg := h.newMessage(enginetest.InboxOne)
			ev := pinned(msg)
			h.append(ev)
			if c.deny != "" {
				h.gate.denyAction(c.deny)
			}
			sub := h.openFrom(model.StartBeginning, model.Filter{Kinds: []string{pinnedKind}})
			b := h.poll(sub, 10)
			if c.deliver {
				requireIDs(t, b.Events, ev)
			} else {
				requireIDs(t, b.Events)
			}
			if c.deny == "" {
				h.requireAsked(t, ev, seePinsGrant)
			}
		})
	}
}

// Builtin events are not gated by any extension action.
func TestPollBuiltinEventsIgnoreExtensionActions(t *testing.T) {
	h := newEvHarness(t, nil, withPinned)
	msg := h.message(enginetest.InboxOne)
	h.gate.denyAction(seePinsGrant)
	sub := h.openFrom(model.StartBeginning, model.Filter{})
	requireIDs(t, h.poll(sub, 10).Events, msg)
}

// requireAsked fails unless the policy was asked action on ev's resource,
// audience and revision.
func (h *evHarness) requireAsked(t *testing.T, ev model.Event, action string) {
	t.Helper()
	h.gate.mu.Lock()
	defer h.gate.mu.Unlock()
	for _, a := range h.gate.requests {
		if a.Action == action && a.Resource == ev.Resource && a.Audience == ev.Audience && a.Revision == ev.Revision {
			return
		}
	}
	t.Fatalf("policy never asked %s on %+v at %+v rev %q", action, ev.Resource, ev.Audience, ev.Revision)
}
