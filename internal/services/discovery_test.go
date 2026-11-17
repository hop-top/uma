package services_test

import (
	"slices"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/services"
	"hop.top/uma/model"
)

// Discovery is the adapter declaration intersected with engine support and
// current status: a builtin capability no core operation executes is
// reported unavailable (model.Unsupported) however the adapter reports it.
// Every builtin capability is executed: edit by edit-mode streams and
// Messages.Edit, delete and react by Messages.Delete and Messages.React,
// work_item_act by WorkItems.Act, respond and respond_follow_up by
// Interactions.Respond, so discovery reports the adapter's status.
func TestCapabilitiesEngineSupport(t *testing.T) {
	rows := []struct {
		capability model.Capability
		executes   bool
	}{
		{model.CapabilityEdit, true},
		{model.CapabilityDelete, true},
		{model.CapabilityReact, true},
		{model.CapabilityWorkItemAct, true},
		{model.CapabilityRespond, true},
		{model.CapabilityRespondFollowUp, true},
		{model.CapabilityArchive, true},
		{model.CapabilityMove, true},
		{model.CapabilityReadState, true},
		{model.CapabilityFlag, true},
		{model.CapabilityTrash, true},
	}
	limits := model.Extension{Name: "acme.limits", Version: 1, Data: []byte(`{"max":1}`)}
	for _, r := range rows {
		t.Run(string(r.capability), func(t *testing.T) {
			if got := engine.Executes(r.capability); got != r.executes {
				t.Fatalf("engine.Executes(%s) = %v, want %v", r.capability, got, r.executes)
			}
			d := receiveDescriptor(thirdPartyKey)
			d.Capabilities = []model.Capability{model.CapabilitySend, r.capability}
			inst := statusReport{&outInstance{F: &outFactory{}}, []model.CapabilityStatus{
				{Name: model.CapabilitySend, Available: true},
				{Name: r.capability, Available: true, Limits: limits},
			}}
			statuses, err := services.CurrentCapabilities(t.Context(), d, inst)
			if err != nil {
				t.Fatal(err)
			}
			if len(statuses) != 2 || statuses[1].Name != r.capability || !statuses[0].Available {
				t.Fatalf("statuses %+v", statuses)
			}
			got := statuses[1]
			if !r.executes {
				if got.Available || got.Reason != model.Unsupported || got.Limits.Name != "" {
					t.Fatalf("%s reported %+v while no executor runs it, want unavailable (unsupported) without limits", r.capability, got)
				}
				return
			}
			if !got.Available || got.Limits.Name != limits.Name {
				t.Fatalf("%s reported %+v, want the adapter's status", r.capability, got)
			}
		})
	}
}

// Configuration.Capabilities reports edit, delete and react available on
// a channel whose adapter declares and implements them: core executes all
// three. A send-only adapter lists none of them.
func TestCapabilitiesDiscoveryReportsMutations(t *testing.T) {
	for _, full := range []bool{true, false} {
		var opts []enginetest.RigOption
		if full {
			opts = append(opts, enginetest.WithMutations)
		}
		r := newRig(t, opts...)
		got, err := r.C.Configuration().Capabilities(r.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: r.Ch.Ref})
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		for _, c := range []model.Capability{model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact} {
			i := slices.IndexFunc(got, func(s model.CapabilityStatus) bool { return s.Name == c })
			switch {
			case !full && i >= 0:
				t.Fatalf("send-only adapter lists %s: %+v", c, got)
			case full && (i < 0 || !got[i].Available || got[i].Reason != ""):
				t.Fatalf("%s discovered %+v, want available", c, got)
			}
		}
	}
}

// Configuration.Capabilities reports work_item_act as declaration, engine
// support and current status: listed and available on a channel whose
// adapter declares it, carrying the adapter's withdrawn status when the
// instance reports one, and absent from an adapter that does not declare it.
func TestCapabilitiesDiscoveryReportsWorkItemAct(t *testing.T) {
	find := func(t *testing.T, r *enginetest.Rig) (model.CapabilityStatus, bool) {
		t.Helper()
		got, err := r.C.Configuration().Capabilities(r.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: r.Ch.Ref})
		if err != nil {
			t.Fatalf("Capabilities: %v", err)
		}
		i := slices.IndexFunc(got, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityWorkItemAct })
		if i < 0 {
			return model.CapabilityStatus{}, false
		}
		return got[i], true
	}
	t.Run("declared", func(t *testing.T) {
		r := newRig(t, enginetest.WithWorkItemActs)
		if st, ok := find(t, r); !ok || !st.Available || st.Reason != "" {
			t.Fatalf("work_item_act discovered %+v (listed %v), want available", st, ok)
		}
	})
	t.Run("withdrawn by the instance", func(t *testing.T) {
		r := newRig(t, enginetest.WithWorkItemActs)
		r.F.Withdraw(model.CapabilityWorkItemAct, model.Unauthorized)
		if st, ok := find(t, r); !ok || st.Available || st.Reason != model.Unauthorized {
			t.Fatalf("work_item_act discovered %+v (listed %v), want unavailable (unauthorized)", st, ok)
		}
	})
	t.Run("undeclared", func(t *testing.T) {
		r := newRig(t)
		if st, ok := find(t, r); ok {
			t.Fatalf("adapter without work_item_act lists %+v", st)
		}
	})
}
