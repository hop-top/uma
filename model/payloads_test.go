package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

// builtinPayloads maps every builtin event kind to its version 1 payload.
var builtinPayloads = map[string]any{
	EventMessageReceived:     MessageReceivedPayload{},
	EventMessageCreated:      MessageCreatedPayload{},
	EventMessageUpdated:      MessageUpdatedPayload{},
	EventConversationChanged: ConversationChangedPayload{},
	EventDeliveryUpdated:     DeliveryUpdatedPayload{},
	EventWorkItemUpdated:     WorkItemUpdatedPayload{},
	EventIdentityLinked:      IdentityLinkedPayload{},
	EventContactMerged:       ContactMergedPayload{},

	EventConversationLabelAdded:   ConversationLabelPayload{},
	EventConversationLabelRemoved: ConversationLabelPayload{},
	EventTaskUpdated:              TaskUpdatedPayload{},
	EventContactCreated:           ContactCreatedPayload{},
	EventContactUpdated:           ContactUpdatedPayload{},
	EventInteractionReceived:      InteractionReceivedPayload{},
	EventInteractionUpdated:       InteractionUpdatedPayload{},
}

// TestEveryBuiltinEventHasPayload: each builtin kind has a payload type and
// none is left over.
func TestEveryBuiltinEventHasPayload(t *testing.T) {
	kinds := []string{
		EventMessageReceived, EventMessageCreated, EventMessageUpdated, EventConversationChanged,
		EventDeliveryUpdated, EventWorkItemUpdated, EventIdentityLinked, EventContactMerged,
		EventConversationLabelAdded, EventConversationLabelRemoved, EventTaskUpdated,
		EventContactCreated, EventContactUpdated,
		EventInteractionReceived, EventInteractionUpdated,
	}
	if len(builtinPayloads) != len(kinds) {
		t.Fatalf("payload types = %d, builtin kinds = %d", len(builtinPayloads), len(kinds))
	}
	for _, k := range kinds {
		if _, ok := builtinPayloads[k]; !ok {
			t.Errorf("%s has no payload type", k)
		}
	}
}

// contentTypes are types that hold message bodies, provider payloads or
// field values; no builtin payload may reach one.
var contentTypes = map[reflect.Type]bool{
	reflect.TypeFor[Content]():         true,
	reflect.TypeFor[Part]():            true,
	reflect.TypeFor[Extension]():       true,
	reflect.TypeFor[Message]():         true,
	reflect.TypeFor[Attribute]():       true,
	reflect.TypeFor[Evidence]():        true,
	reflect.TypeFor[json.RawMessage](): true,
	reflect.TypeFor[[]byte]():          true,
}

// TestPayloadsCarryNoContent: payloads are references plus change summary.
func TestPayloadsCarryNoContent(t *testing.T) {
	for kind, p := range builtinPayloads {
		walkPayload(t, kind, reflect.TypeOf(p), map[reflect.Type]bool{})
	}
}

func walkPayload(t *testing.T, path string, typ reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if contentTypes[typ] {
		t.Errorf("%s: payload reaches content type %s", path, typ)
		return
	}
	if seen[typ] {
		return
	}
	seen[typ] = true
	switch typ.Kind() {
	case reflect.Pointer, reflect.Slice:
		walkPayload(t, path, typ.Elem(), seen)
	case reflect.Map, reflect.Interface:
		t.Errorf("%s: payload holds open-ended %s", path, typ)
	case reflect.Struct:
		for i := range typ.NumField() {
			f := typ.Field(i)
			walkPayload(t, path+"."+f.Name, f.Type, seen)
		}
	}
}

// TestPayloadOptionalRefsAbsent: unset optional references are absent, not
// null.
func TestPayloadOptionalRefsAbsent(t *testing.T) {
	b, err := json.Marshal(MessageReceivedPayload{Trigger: TriggerProvider})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"author", "in_reply_to"} {
		if _, ok := fields[k]; ok {
			t.Errorf("%s present in %s", k, b)
		}
	}
	for _, k := range []string{"message", "trigger", "source", "author_kind", "conversation_created"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("%s missing from %s", k, b)
		}
	}
}
