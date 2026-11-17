package conformance

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// extensionAudience is the audience extension cases execute under: not
// public, with a policy reference, so a widened or dropped audience shows.
var extensionAudience = model.Audience{Visibility: model.Internal, PolicyRef: "conformance-staff"}

// unknownField is a field no adapter declares; it must reach the provider
// unchanged.
const unknownField = "conformance_unknown_field"

// extensionAction returns the harness action with unknownField added to
// its data, or skips the case when the harness has no action.
func (e *adapterEnv) extensionAction(t *testing.T) model.Extension {
	t.Helper()
	a := e.h.Action.Clone()
	if a.Name == "" {
		t.Skip("AdapterHarness.Action not set")
	}
	fields := map[string]json.RawMessage{}
	if len(a.Data) > 0 {
		if err := json.Unmarshal(a.Data, &fields); err != nil {
			t.Fatalf("AdapterHarness.Action data is not a JSON object: %v", err)
		}
	}
	fields[unknownField] = json.RawMessage(`{"kept":[1,2.50,"<two>"],"empty":{}}`)
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	a.Data = data
	return a
}

func (e *adapterEnv) execute(conv model.ConversationRef, opID model.OperationID, action model.Extension) (model.Acceptance, error) {
	return e.client.Extensions().Execute(e.ctx, model.ExecuteExtension{
		Command:  model.Command{Call: model.Call{Correlation: model.CorrelationID("c-" + string(opID))}, Operation: opID},
		Target:   conv.Ref(),
		Audience: extensionAudience,
		Action:   action,
	})
}

// sameJSON reports whether a and b hold the same JSON value, numbers
// compared by their decimal text.
func sameJSON(a, b []byte) bool {
	decode := func(raw []byte) (any, bool) {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var v any
		return v, d.Decode(&v) == nil
	}
	x, okA := decode(a)
	y, okB := decode(b)
	return okA && okB && reflect.DeepEqual(x, y)
}

var extensionCases = []adapterCase{
	// Typed extension round-trip, incompatible execute rejected, action
	// correlation preserved on replay.
	{name: "TypedRoundTripIncompatibleRejectedCorrelatedReplay", needs: []model.Capability{model.CapabilityExtensionExecute}, run: func(t *testing.T, e *adapterEnv) {
		action := e.extensionAction(t)
		conv := e.outbound(t)

		incompatible := []struct {
			action model.Extension
			code   model.ErrorCode
		}{
			{model.Extension{Name: action.Name, Version: math.MaxUint32, Data: action.Data}, model.SchemaMismatch},
			{model.Extension{Name: "org.example.conformance.unregistered", Version: action.Version, Data: action.Data}, model.Unsupported},
		}
		for _, bad := range incompatible {
			_, err := e.execute(conv.Ref, "op-ext-incompatible", bad.action)
			if !model.IsCode(err, bad.code) {
				t.Fatalf("Execute(%s v%d) = %v, want %s", bad.action.Name, bad.action.Version, err, bad.code)
			}
		}

		first, err := e.execute(conv.Ref, "op-ext", action)
		if err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if first.Operation.State != model.Queued || first.Replayed {
			t.Fatalf("Execute = %+v, want a new queued operation", first)
		}
		again, err := e.execute(conv.Ref, "op-ext", action)
		if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay = %+v, %v; want operation %v replayed", again, err, first.Operation.Ref)
		}
		e.start(t)
		e.stop(t)
		ref := first.Operation.Ref
		wantOp(t, e.op(t, ref), model.ProviderAccepted, "")

		calls := e.calls(adapterChanA, model.CapabilityExtensionExecute)
		if len(calls) != 1 {
			t.Fatalf("provider extension calls = %+v, want one: incompatible actions never execute and a replay never executes again", calls)
		}
		c := calls[0]
		if c.Operation != ref || !c.Applied {
			t.Fatalf("provider call = %+v, want an applied call carrying operation %v", c, ref)
		}
		if c.Route != conv.ReplyRoute {
			t.Fatalf("provider route = %+v, want the stored route %+v", c.Route, conv.ReplyRoute)
		}
		if c.Audience != extensionAudience {
			t.Fatalf("provider audience = %+v, want %+v", c.Audience, extensionAudience)
		}
		if c.Action == nil {
			t.Fatal("provider call carries no action")
		}
		if c.Action.Name != action.Name || c.Action.Version != action.Version || !sameJSON(c.Action.Data, action.Data) {
			t.Fatalf("provider action = %s v%d %s, want %s v%d with every field of %s",
				c.Action.Name, c.Action.Version, c.Action.Data, action.Name, action.Version, action.Data)
		}

		after, err := e.execute(conv.Ref, "op-ext", action)
		if err != nil || !after.Replayed || after.Operation.Ref != ref || after.Operation.State != model.ProviderAccepted {
			t.Fatalf("replay after execution = %+v, %v; want the settled operation %v", after, err, ref)
		}
		// Another payload that is still valid: only an unknown field differs.
		changed := action.Clone()
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(action.Data, &fields); err != nil {
			t.Fatal(err)
		}
		fields[unknownField] = json.RawMessage(`{"changed":true}`)
		if changed.Data, err = json.Marshal(fields); err != nil {
			t.Fatal(err)
		}
		if _, err := e.execute(conv.Ref, "op-ext", changed); !model.IsCode(err, model.Conflict) {
			t.Fatalf("other payload under the same operation ID = %v, want conflict", err)
		}
		if n := len(e.calls(adapterChanA, model.CapabilityExtensionExecute)); n != 1 {
			t.Fatalf("provider extension calls = %d after replays, want 1", n)
		}
	}},

	// Extension data core owns, malformed JSON and an oversize payload, is
	// refused at the call: nothing is stored under the operation ID and the
	// adapter never sees it, whatever the adapter validates itself.
	{name: "InvalidDataRejectedBeforeCommit", needs: []model.Capability{model.CapabilityExtensionExecute}, run: func(t *testing.T, e *adapterEnv) {
		action := e.extensionAction(t)
		conv := e.outbound(t)

		oversize := `{"conformance_oversize":"` + strings.Repeat("x", model.DefaultContentLimits().MaxStructuredBytes) + `"}`
		for _, c := range []struct{ name, data string }{
			{"malformed JSON", `{"conformance":`},
			{"oversize", oversize},
		} {
			bad := action.Clone()
			bad.Data = json.RawMessage(c.data)
			_, err := e.execute(conv.Ref, "op-ext-invalid", bad)
			if !model.IsCode(err, model.Validation) {
				t.Fatalf("Execute(%s) = %v, want %s", c.name, err, model.Validation)
			}
		}

		// The refused calls left nothing under their operation ID: the same
		// ID is free for a valid action, a new operation that is neither a
		// replay nor a conflict.
		ok, err := e.execute(conv.Ref, "op-ext-invalid", action)
		if err != nil || ok.Replayed || ok.Operation.State != model.Queued {
			t.Fatalf("Execute after refused data = %+v, %v; want a new queued operation", ok, err)
		}
		e.start(t)
		e.stop(t)
		wantOp(t, e.op(t, ok.Operation.Ref), model.ProviderAccepted, "")
		calls := e.calls(adapterChanA, model.CapabilityExtensionExecute)
		if len(calls) != 1 || calls[0].Operation != ok.Operation.Ref {
			t.Fatalf("provider extension calls = %+v, want only the valid operation %v", calls, ok.Operation.Ref)
		}
	}},

	// Fields the adapter owns are never applied when invalid. An adapter
	// whose factory implements adapter.ActionValidator has them refused at
	// the call (model.Validation, nothing stored, the operation ID free);
	// one that does not keeps the late path: the operation is stored, then
	// settles rejected with model.Validation and the provider applies
	// nothing.
	{name: "InvalidFieldsNeverApplied", needs: []model.Capability{model.CapabilityExtensionExecute}, run: func(t *testing.T, e *adapterEnv) {
		bad := e.h.InvalidAction.Clone()
		if bad.Name == "" {
			t.Skip("AdapterHarness.InvalidAction not set")
		}
		conv := e.outbound(t)
		acc, err := e.execute(conv.Ref, "op-ext-invalid-fields", bad)
		if _, early := e.factory.(adapter.ActionValidator); early {
			if !model.IsCode(err, model.Validation) {
				t.Fatalf("Execute(invalid fields) = %+v, %v; want %s at the call: the factory validates actions", acc, err, model.Validation)
			}
			// Nothing was stored: the operation ID is free for a valid action.
			ok, err := e.execute(conv.Ref, "op-ext-invalid-fields", e.extensionAction(t))
			if err != nil || ok.Replayed || ok.Operation.State != model.Queued {
				t.Fatalf("Execute after refused fields = %+v, %v; want a new queued operation", ok, err)
			}
		} else {
			if err != nil {
				t.Fatalf("Execute(invalid fields) = %v, want a queued operation: the factory does not validate actions", err)
			}
			e.start(t)
			e.stop(t)
			wantOp(t, e.op(t, acc.Operation.Ref), model.Rejected, model.Validation)
		}
		for _, c := range e.calls(adapterChanA, model.CapabilityExtensionExecute) {
			if c.Applied {
				t.Fatalf("provider applied invalid fields: %+v", c)
			}
		}
	}},
}
