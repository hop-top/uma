package conformance

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// capabilityInterfaces maps every builtin capability to the adapter
// interface that serves it.
var capabilityInterfaces = map[model.Capability]func(adapter.Instance) bool{
	model.CapabilityReceive:          func(i adapter.Instance) bool { _, ok := i.(adapter.Receiver); return ok },
	model.CapabilityReceiveHTTP:      func(i adapter.Instance) bool { _, ok := i.(adapter.HTTPReceiver); return ok },
	model.CapabilitySend:             func(i adapter.Instance) bool { _, ok := i.(adapter.Sender); return ok },
	model.CapabilityEdit:             func(i adapter.Instance) bool { _, ok := i.(adapter.Editor); return ok },
	model.CapabilityDelete:           func(i adapter.Instance) bool { _, ok := i.(adapter.Deleter); return ok },
	model.CapabilityReact:            func(i adapter.Instance) bool { _, ok := i.(adapter.Reactor); return ok },
	model.CapabilityReconcile:        func(i adapter.Instance) bool { _, ok := i.(adapter.Reconciler); return ok },
	model.CapabilitySignal:           func(i adapter.Instance) bool { _, ok := i.(adapter.Signaler); return ok },
	model.CapabilitySendEphemeral:    func(i adapter.Instance) bool { _, ok := i.(adapter.EphemeralSender); return ok },
	model.CapabilityStream:           func(i adapter.Instance) bool { _, ok := i.(adapter.Streamer); return ok },
	model.CapabilityExtensionQuery:   func(i adapter.Instance) bool { _, ok := i.(adapter.ExtensionReader); return ok },
	model.CapabilityExtensionExecute: func(i adapter.Instance) bool { _, ok := i.(adapter.ExtensionExecutor); return ok },
	model.CapabilityProbePermissions: func(i adapter.Instance) bool { _, ok := i.(adapter.PermissionProber); return ok },
	model.CapabilityInternalNote:     func(i adapter.Instance) bool { _, ok := i.(adapter.NoteSender); return ok },
	model.CapabilityRefetch:          func(i adapter.Instance) bool { _, ok := i.(adapter.Refetcher); return ok },
	model.CapabilityWorkItemAct:      func(i adapter.Instance) bool { _, ok := i.(adapter.WorkItemActor); return ok },
	model.CapabilityRespond:          func(i adapter.Instance) bool { _, ok := i.(adapter.InteractionResponder); return ok },
	model.CapabilityReceipt:          func(i adapter.Instance) bool { _, ok := i.(adapter.ReceiptSender); return ok },
	model.CapabilityMembership:       func(i adapter.Instance) bool { _, ok := i.(adapter.MembershipChanger); return ok },
	model.CapabilityHistory:          func(i adapter.Instance) bool { _, ok := i.(adapter.HistoryReader); return ok },
	model.CapabilityRawRead:          func(i adapter.Instance) bool { _, ok := i.(adapter.RawReader); return ok },
	model.CapabilityArchive:          func(i adapter.Instance) bool { _, ok := i.(adapter.Archiver); return ok },
	model.CapabilityMove:             func(i adapter.Instance) bool { _, ok := i.(adapter.Mover); return ok },
	model.CapabilityReadState:        func(i adapter.Instance) bool { _, ok := i.(adapter.ReadStateSetter); return ok },
	model.CapabilityFlag:             func(i adapter.Instance) bool { _, ok := i.(adapter.Flagger); return ok },
	model.CapabilityTrash:            func(i adapter.Instance) bool { _, ok := i.(adapter.Trasher); return ok },
}

// sharedInterfaces maps the builtin capabilities served by another
// capability's interface: declaring one requires the interface, which
// alone does not imply the capability. start_conversation is a Sender
// that also reaches recipients no inbound message named; respond_follow_up
// is an InteractionResponder that also gives follow-up responses.
var sharedInterfaces = map[model.Capability]func(adapter.Instance) bool{
	model.CapabilityStartConversation: func(i adapter.Instance) bool { _, ok := i.(adapter.Sender); return ok },
	model.CapabilityReplyRecipients:   func(i adapter.Instance) bool { _, ok := i.(adapter.Sender); return ok },
	model.CapabilityRespondFollowUp:   func(i adapter.Instance) bool { _, ok := i.(adapter.InteractionResponder); return ok },
}

var contractCases = []adapterCase{
	{name: "DescriptorMatchesHarness", run: func(t *testing.T, e *adapterEnv) {
		d := e.desc
		if !sameCapabilities(d.Capabilities, e.h.Capabilities) {
			t.Fatalf("Describe declares %v, harness expects %v", d.Capabilities, e.h.Capabilities)
		}
		if d.ContractMajor != adapter.ContractMajor {
			t.Errorf("ContractMajor = %d, want %d", d.ContractMajor, adapter.ContractMajor)
		}
		if err := model.ValidateTypeKey(d.Type); err != nil {
			t.Errorf("type key %q: %v", d.Type, err)
		}
		if issues := e.factory.Validate(e.provider.Account(adapterChanA).Configuration); len(issues) > 0 {
			t.Errorf("Validate(account configuration) = %+v, want none", issues)
		}
		receives := slices.Contains(d.Capabilities, model.CapabilityReceive) || slices.Contains(d.Capabilities, model.CapabilityReceiveHTTP)
		if receives && len(d.IdentityNamespaces) == 0 {
			t.Error("an adapter that receives declares no identity namespace")
		}
		described := e.engine.Describe()
		if len(described) != 1 || described[0].Type != d.Type {
			t.Errorf("engine describes %+v, want the registered adapter", described)
		}
	}},
	{name: "InterfacesMatchCapabilities", run: func(t *testing.T, e *adapterEnv) {
		inst := e.open(t, adapterChanA)
		for c, implements := range capabilityInterfaces {
			declared := slices.Contains(e.desc.Capabilities, c)
			switch has := implements(inst); {
			case declared && !has:
				t.Errorf("%s declared but %T lacks its interface", c, inst)
			case has && !declared:
				t.Errorf("%T implements the %s interface without declaring it", inst, c)
			}
		}
		for c, implements := range sharedInterfaces {
			if slices.Contains(e.desc.Capabilities, c) && !implements(inst) {
				t.Errorf("%s declared but %T lacks its interface", c, inst)
			}
		}
		statuses, err := inst.Capabilities(e.ctx)
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		var reported []model.Capability
		for _, s := range statuses {
			if slices.Contains(reported, s.Name) {
				t.Errorf("Capabilities reports %s twice", s.Name)
			}
			reported = append(reported, s.Name)
			if !slices.Contains(e.desc.Capabilities, s.Name) {
				t.Errorf("Capabilities reports undeclared %s", s.Name)
			}
		}
	}},
	{name: "StatusesCoverDeclared", run: func(t *testing.T, e *adapterEnv) {
		statuses, err := e.open(t, adapterChanA).Capabilities(e.ctx)
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		for _, c := range e.desc.Capabilities {
			var got []model.CapabilityStatus
			for _, s := range statuses {
				if s.Name == c {
					got = append(got, s)
				}
			}
			if len(got) != 1 {
				t.Errorf("Capabilities reports %s %d times, want exactly once: the engine treats an unreported capability as unavailable", c, len(got))
				continue
			}
			switch s := got[0]; {
			case s.Available && s.Reason != "":
				t.Errorf("%s reported available with reason %q", c, s.Reason)
			case !s.Available && !s.Reason.Valid():
				t.Errorf("%s reported unavailable without a valid reason (%q)", c, s.Reason)
			}
		}
	}},
	{name: "ProbeReportsDeclaredOnly", needs: []model.Capability{model.CapabilityProbePermissions}, run: func(t *testing.T, e *adapterEnv) {
		statuses, err := e.open(t, adapterChanA).(adapter.PermissionProber).Probe(e.ctx)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		for _, s := range statuses {
			if !slices.Contains(e.desc.Capabilities, s.Name) {
				t.Errorf("Probe reports undeclared %s", s.Name)
			}
		}
	}},
}

// open opens an instance on ch's account directly through the factory,
// with the dependencies the engine gives it except protocol state.
func (e *adapterEnv) open(t *testing.T, ch model.ChannelRef) adapter.Instance {
	t.Helper()
	acct := e.provider.Account(ch)
	inst, err := e.factory.Open(e.ctx, model.Channel{
		Ref: ch, Type: e.desc.Type, Configuration: acct.Configuration, CredentialRef: acct.CredentialRef, Revision: "ch1",
	}, adapter.Dependencies{Credentials: e.creds, HTTP: e.http, Clock: e.clock, Observer: e.observed})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst
}

func sameCapabilities(a, b []model.Capability) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y)) && len(a) == len(b)
}
