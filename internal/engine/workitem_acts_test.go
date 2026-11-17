package engine_test

import (
	"context"
	"reflect"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

var _ adapter.WorkItemActor = fullInstance{}

func (fullInstance) ActWorkItem(context.Context, adapter.WorkItemActRequest) (adapter.WorkItemActOutcome, error) {
	return adapter.WorkItemActOutcome{}, nil
}

// issueActs declares an open ⇄ closed provider lifecycle with reasons, as
// an issue tracker does.
func issueActs() []adapter.WorkItemTransition {
	return []adapter.WorkItemTransition{
		{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}, Reasons: []string{"completed", "not_planned"}},
		{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}, Reasons: []string{"reopened"}},
	}
}

// Declared work item transitions go with the work_item_act capability and
// name declared statuses and segment reasons, each once.
func TestRegistryWorkItemActs(t *testing.T) {
	acts := func(edit func([]adapter.WorkItemTransition)) []adapter.Descriptor {
		x := fullDescriptor()
		x.WorkItemActs = issueActs()
		edit(x.WorkItemActs)
		return []adapter.Descriptor{x}
	}
	noActs := fullDescriptor()
	noActs.WorkItemActs = nil
	actsOnly := enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)
	actsOnly.WorkItemActs = issueActs()
	const at = "Config.Adapters[0].WorkItemActs"
	for _, tc := range []struct {
		name  string
		descs []adapter.Descriptor
		want  string
	}{
		{"capability without transitions", []adapter.Descriptor{noActs}, "Config.Adapters[0]: work item acts: required by the work_item_act capability"},
		{"transitions without capability", []adapter.Descriptor{actsOnly}, at + ": declared without the work_item_act capability"},
		{"empty target", acts(func(x []adapter.WorkItemTransition) { x[0].To = "" }), at + "[0].To: must be a declared work item status"},
		{"undeclared target", acts(func(x []adapter.WorkItemTransition) { x[0].To = "done" }), at + "[0].To: must be a declared work item status"},
		{"duplicate target", acts(func(x []adapter.WorkItemTransition) { x[1].To = model.WorkItemClosed }), at + "[1].To: duplicate"},
		{"no source", acts(func(x []adapter.WorkItemTransition) { x[0].From = nil }), at + "[0].From: required"},
		{"undeclared source", acts(func(x []adapter.WorkItemTransition) { x[0].From = []string{"triage"} }), at + "[0].From[0]: must be a declared work item status"},
		{"duplicate source", acts(func(x []adapter.WorkItemTransition) {
			x[0].From = []string{model.WorkItemOpen, model.WorkItemOpen}
		}), at + "[0].From[1]: duplicate"},
		{"bad reason", acts(func(x []adapter.WorkItemTransition) { x[0].Reasons[1] = "Not Planned" }), at + "[0].Reasons[1]: status_reason:"},
		{"empty reason", acts(func(x []adapter.WorkItemTransition) { x[0].Reasons[0] = "" }), at + "[0].Reasons[0]: status_reason:"},
		{"duplicate reason", acts(func(x []adapter.WorkItemTransition) { x[0].Reasons[1] = "completed" }), at + "[0].Reasons[1]: duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := engine.New(registryConfig(tc.descs, pinDefinition(), pinnedDefinition()))
			if e != nil {
				t.Fatal("New returned an engine with an error")
			}
			enginetest.RequireCode(t, err, model.Validation, tc.want)
		})
	}

	d := fullDescriptor()
	e := enginetest.MustNew(t, registryConfig([]adapter.Descriptor{d}, pinDefinition(), pinnedDefinition()))
	got := e.Describe()[0].WorkItemActs
	if !reflect.DeepEqual(got, issueActs()) {
		t.Fatalf("Describe WorkItemActs = %+v, want %+v", got, issueActs())
	}
	got[0].From[0], got[0].Reasons[0], got[1].To = "mutated", "mutated", "mutated"
	d.WorkItemActs[0].Reasons[1] = "mutated"
	if again := e.Describe()[0].WorkItemActs; !reflect.DeepEqual(again, issueActs()) {
		t.Fatalf("WorkItemActs aliased: %+v", again)
	}
}

// A provider-owned status moves only along a declared transition with a
// declared reason or none: reopening a closed item works only where the
// adapter declares it, and nothing is coerced.
func TestReopenAllowedOnlyWhenDeclared(t *testing.T) {
	d := fullDescriptor()
	for _, ok := range []struct{ from, to, reason string }{
		{model.WorkItemOpen, model.WorkItemClosed, ""},
		{model.WorkItemOpen, model.WorkItemClosed, "completed"},
		{model.WorkItemOpen, model.WorkItemClosed, "not_planned"},
		{model.WorkItemClosed, model.WorkItemOpen, ""},
		{model.WorkItemClosed, model.WorkItemOpen, "reopened"},
	} {
		if err := engine.WorkItemActDeclared(d, ok.from, ok.to, ok.reason); err != nil {
			t.Errorf("%s → %s (%q): %v", ok.from, ok.to, ok.reason, err)
		}
	}
	for _, bad := range []struct{ name, from, to, reason, want string }{
		{"undeclared reason", model.WorkItemOpen, model.WorkItemClosed, "duplicate", "status reason"},
		{"reason of another transition", model.WorkItemClosed, model.WorkItemOpen, "completed", "status reason"},
		{"undeclared target", model.WorkItemOpen, model.WorkItemResolved, "", "no work item transition"},
		{"undeclared source", model.WorkItemPending, model.WorkItemClosed, "", "no work item transition"},
		{"same status", model.WorkItemClosed, model.WorkItemClosed, "", "no work item transition"},
		{"unmapped status", "", model.WorkItemClosed, "", "no work item transition"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			enginetest.RequireCode(t, engine.WorkItemActDeclared(d, bad.from, bad.to, bad.reason), model.Unsupported, bad.want)
		})
	}
	closeOnly := fullDescriptor()
	closeOnly.WorkItemActs = issueActs()[:1]
	enginetest.RequireCode(t, engine.WorkItemActDeclared(closeOnly, model.WorkItemClosed, model.WorkItemOpen, "reopened"),
		model.Unsupported, "no work item transition")
	none := enginetest.ReceiveDescriptor(enginetest.ThirdPartyKey)
	enginetest.RequireCode(t, engine.WorkItemActDeclared(none, model.WorkItemOpen, model.WorkItemClosed, ""),
		model.Unsupported, "does not declare work_item_act")
}
