package conformance

import (
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// discoveryOperations pairs each operation an adapter capability backs with
// the builtin capability it executes and whether an executor of this
// version runs it. Edit, delete and react are executed by Messages.Edit,
// Messages.Delete and Messages.React (edit also by edit-mode streams) and
// work_item_act by WorkItems.Act, so discovery reports the adapter's status
// for each.
var discoveryOperations = []struct {
	op         string
	capability model.Capability
	executed   bool
}{
	{"Messages.Edit", model.CapabilityEdit, true},
	{"Messages.Delete", model.CapabilityDelete, true},
	{"Messages.React", model.CapabilityReact, true},
	{"WorkItems.Act", model.CapabilityWorkItemAct, true},
	{"Messages.Receipt", model.CapabilityReceipt, true},
	{"Conversations.AddParticipant", model.CapabilityMembership, true},
	{"Runtime.Import", model.CapabilityHistory, true},
}

// discoveryCases check Configuration.Capabilities against what core
// executes: a declared capability no executor runs is never reported
// available, however the adapter reports it.
var discoveryCases = []adapterCase{
	{name: "DiscoveryReportsEngineSupport", run: func(t *testing.T, e *adapterEnv) {
		e.seed(t, adapterChanA, adapterInboxA, e.selection(), "support")
		discovered, err := e.client.Configuration().Capabilities(e.ctx, model.Lookup[model.ChannelRef]{
			Call: model.Call{Correlation: "c-discovery"}, Ref: adapterChanA,
		})
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		inst := e.open(t, adapterChanA)
		for _, row := range discoveryOperations {
			t.Run(row.op, func(t *testing.T) {
				if !slices.Contains(e.desc.Capabilities, row.capability) {
					t.Skipf("%s not declared", row.capability)
				}
				i := slices.IndexFunc(discovered, func(s model.CapabilityStatus) bool { return s.Name == row.capability })
				if i < 0 {
					t.Fatalf("declared %s missing from discovery %+v", row.capability, discovered)
				}
				got := discovered[i]
				if !row.executed {
					if got.Available || got.Reason != model.Unsupported {
						t.Fatalf("%s discovered %+v while no executor runs %s, want unavailable (unsupported)", row.capability, got, row.op)
					}
					return
				}
				want := adapterStatus(t, e, inst, row.capability)
				if got.Available != want.Available || got.Reason != want.Reason {
					t.Fatalf("%s discovered %+v, want the adapter's status %+v", row.capability, got, want)
				}
			})
		}
	}},
}

// adapterStatus is c as inst reports it, overridden by its permission
// probe when the adapter declares one; unreported is unsupported.
func adapterStatus(t *testing.T, e *adapterEnv, inst adapter.Instance, c model.Capability) model.CapabilityStatus {
	t.Helper()
	st := model.CapabilityStatus{Name: c, Reason: model.Unsupported}
	pick := func(statuses []model.CapabilityStatus) {
		if i := slices.IndexFunc(statuses, func(s model.CapabilityStatus) bool { return s.Name == c }); i >= 0 {
			st = statuses[i]
		}
	}
	reported, err := inst.Capabilities(e.ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	pick(reported)
	if p, ok := inst.(adapter.PermissionProber); ok && slices.Contains(e.desc.Capabilities, model.CapabilityProbePermissions) {
		probed, err := p.Probe(e.ctx)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		pick(probed)
	}
	return st
}
