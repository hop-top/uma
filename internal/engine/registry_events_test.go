package engine_test

import (
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// Event-kind definitions: the definition itself is the payload schema, so
// it declares no input or result schema, and its policy action gates
// delivery, so a builtin one must be a read-only action. Builtin kinds,
// interaction kinds included, are never registered.
func TestRegistryRejectsEventDefinitions(t *testing.T) {
	def := func(edit func(*adapter.ExtensionDefinition)) []adapter.ExtensionDefinition {
		x := pinnedDefinition()
		edit(&x)
		return []adapter.ExtensionDefinition{pinDefinition(), x}
	}
	descs := []adapter.Descriptor{enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)}
	cases := []struct {
		name string
		defs []adapter.ExtensionDefinition
		want string
	}{
		{"input schema", def(func(x *adapter.ExtensionDefinition) {
			x.InputSchema = model.Extension{Name: extEvent + "_input", Version: 1}
		}), "Config.Extensions[1]: input schema: an event kind declares none"},
		{"result schema", def(func(x *adapter.ExtensionDefinition) {
			x.ResultSchema = model.Extension{Name: extEvent + "_result", Version: 1}
		}), "Config.Extensions[1]: result schema: an event kind declares none"},
		{"builtin write action", def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionMessageReply }),
			"Config.Extensions[1]: authorize as: an event kind authorizes as a read action"},
		{"builtin admin action", def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionChannelReceive }),
			"Config.Extensions[1]: authorize as: an event kind authorizes as a read action"},
		{"builtin subscription action", def(func(x *adapter.ExtensionDefinition) { x.AuthorizeAs = model.ActionSubscriptionPoll }),
			"Config.Extensions[1]: authorize as: an event kind authorizes as a read action"},
		{"builtin message kind", def(func(x *adapter.ExtensionDefinition) { x.Name = model.EventMessageReceived }),
			"Config.Extensions[1]: name: unqualified and two-segment names are reserved for core"},
		{"interaction received", def(func(x *adapter.ExtensionDefinition) { x.Name = "interaction.received" }),
			"Config.Extensions[1]: name: unqualified and two-segment names are reserved for core"},
		{"interaction updated", def(func(x *adapter.ExtensionDefinition) { x.Name = "interaction.updated" }),
			"Config.Extensions[1]: name: unqualified and two-segment names are reserved for core"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := engine.New(registryConfig(descs, tc.defs...))
			if e != nil {
				t.Fatal("New returned an engine with an error")
			}
			enginetest.RequireCode(t, err, model.Validation, tc.want)
		})
	}
}

// Event-kind definitions may authorize as any builtin read action or a
// namespaced action.
func TestRegistryAcceptsEventActions(t *testing.T) {
	for _, action := range []string{model.ActionMessageRead, model.ActionInboxRead, model.ActionContactReadAll, model.ActionEventList, model.ActionIdentityHistory, "com.example.chat.messages.see_pins"} {
		x := pinnedDefinition()
		x.AuthorizeAs = action
		enginetest.MustNew(t, registryConfig(nil, pinDefinition(), x))
	}
}

// EventAction returns the policy action of a registered event kind; builtin
// kinds and other names have none.
func TestEventAction(t *testing.T) {
	x := pinnedDefinition()
	x.AuthorizeAs = "com.example.chat.messages.see_pins"
	v2 := x
	v2.Version = 2
	e := enginetest.MustNew(t, registryConfig(nil, pinDefinition(), x, v2))
	if a, ok := e.EventAction(extEvent); !ok || a != x.AuthorizeAs {
		t.Fatalf("EventAction(%s) = %q, %v; want %q", extEvent, a, ok, x.AuthorizeAs)
	}
	for _, k := range append(engine.BuiltinEvents(), pinDefinition().Name, "com.example.chat.messages.unknown", "") {
		if a, ok := e.EventAction(k); ok || a != "" {
			t.Errorf("EventAction(%q) = %q, %v; want none", k, a, ok)
		}
	}
}
