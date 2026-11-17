package engine_test

import (
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// Core executes every builtin capability, interaction responses included,
// and every namespaced capability through the Extensions service.
func TestExecutes(t *testing.T) {
	for _, c := range engine.BuiltinCapabilities() {
		if !engine.Executes(c) {
			t.Errorf("Executes(%s) = false, want every builtin capability executed", c)
		}
	}
	if !engine.Executes("acme.lookup") {
		t.Error("Executes(acme.lookup) = false, want a namespaced capability executed")
	}
}

// event.list, the per-call grant of Events.List, is a builtin action
// registered exactly once.
func TestEventListActionRegisteredOnce(t *testing.T) {
	n := 0
	for _, a := range engine.BuiltinActions() {
		if a == model.ActionEventList {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s registered %d times, want 1", model.ActionEventList, n)
	}
}

// Every builtin action is classified, and no registry rule falls back to
// guessing from the action's name: a new model.Action constant with no
// entry in the table fails here and in TestBuiltinSetsMatchModel.
func TestEveryModelActionClassified(t *testing.T) {
	classes := engine.BuiltinActionClasses()
	n := 0
	for name, v := range modelStringConsts(t) {
		if !strings.HasPrefix(name, "Action") {
			continue
		}
		n++
		if _, ok := classes[v]; !ok {
			t.Errorf("model.%s (%q) has no classification in the builtin action table", name, v)
		}
	}
	if n == 0 || n != len(classes) {
		t.Errorf("%d model actions, %d classified", n, len(classes))
	}
}

// An effect may authorize as exactly the builtin actions the table marks
// effect-eligible; an event kind as exactly the read-only ones.
func TestExtensionKindsAgainstActionClasses(t *testing.T) {
	for action, class := range engine.BuiltinActionClasses() {
		for _, kind := range []string{model.ExtensionKindEffect, model.ExtensionKindEvent} {
			x := pinDefinition()
			if kind == model.ExtensionKindEvent {
				x = pinnedDefinition()
			}
			x.AuthorizeAs = action
			defs := []adapter.ExtensionDefinition{x}
			wantOK := (kind == model.ExtensionKindEffect && class == engine.ClassEffect) ||
				(kind == model.ExtensionKindEvent && class == engine.ClassRead)
			_, err := engine.New(registryConfig(nil, defs...))
			if wantOK != (err == nil) {
				t.Errorf("%s authorizing as %s (class %v): err = %v, want accepted = %v", kind, action, class, err, wantOK)
			}
		}
	}
}
