package operations_test

import (
	"context"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func noopApplier(context.Context, *engine.Engine, store.Tx, model.Operation, store.OperationKey, model.Extension, model.Revision) error {
	return nil
}

// A write that applies a local mutation when approved is held only when
// its intent has an Applier: without one it is refused at submit and
// claims nothing.
func TestHoldRefusedWithoutApplierForLocalWrites(t *testing.T) {
	const intent = "test.local.noapplier"
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	for name, mod := range map[string]func(*operations.Submission){
		"local": func(s *operations.Submission) { s.Local = true },
		"apply": func(s *operations.Submission) {
			s.Apply = func(context.Context, store.Tx, model.OperationRef, model.Revision) error { return nil }
		},
	} {
		s := proposal(r, model.OperationID("p-"+name), "fp-"+name)
		s.Intent.Name = intent
		mod(&s)
		_, err := operations.Submit(r.Ctx, c.Client, s)
		requireCode(t, err, model.Unsupported, "")
		if n := len(listHeld(t, r, c)); n != 0 {
			t.Fatalf("%s: a refused hold claimed %d operations", name, n)
		}
	}
}

// An approval never commits a local intent with nothing applied: when the
// applier that was there at hold time is gone at decision time, approve
// and edit are refused, the operation stays held, and reject still works.
func TestApproveRefusedWhenLocalApplierIsGone(t *testing.T) {
	for _, local := range []bool{true, false} {
		intent := "test.local.gone"
		if !local {
			intent = "test.apply.gone"
		}
		operations.RegisterApplier(intent, noopApplier)
		operations.RegisterValidator(intent, func(context.Context, store.Reader, store.OperationKey, model.Extension, model.Extension) error {
			return nil
		})
		r := newRig(t)
		s := proposal(r, "p-1", "fp-1")
		s.Intent.Name = intent
		s.Local = local
		if !local {
			s.Apply = func(context.Context, store.Tx, model.OperationRef, model.Revision) error { return nil }
		}
		held := propose(t, r, pluginClient(t, r, plugin), s).Operation
		operations.UnregisterApplier(intent)
		t.Cleanup(func() { operations.UnregisterValidator(intent) })

		_, err := r.C.Operations().Approve(r.Ctx, decide(held))
		requireCode(t, err, model.Unsupported, "")
		_, err = r.C.Operations().Edit(r.Ctx, model_edit(held, intent))
		requireCode(t, err, model.Unsupported, "")
		requireHeld(t, r, held.Ref)
		if n := r.Step("w"); n != 0 {
			t.Fatalf("a refused approval queued %d work items", n)
		}
		got, err := r.C.Operations().Reject(r.Ctx, decide(held))
		if err != nil || got.Operation.State != model.Rejected {
			t.Fatalf("Reject = %+v, %v", got, err)
		}
	}
}

func model_edit(op model.Operation, intent string) model.EditHeld {
	return model.EditHeld{DecideHeld: decide(op), Args: model.Extension{Name: intent, Version: 1, Data: []byte(`{"v":2}`)}}
}

// A provider-work intent has no local mutation: it needs no applier, is
// held, and its approval queues its work.
func TestProviderWorkIntentNeedsNoApplier(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), proposal(r, "p-1", "fp-1")).Operation
	if h := held.Held; h.Applies || h.Local {
		t.Fatalf("provider-work hold recorded a local mutation: %+v", h)
	}
	got, err := r.C.Operations().Approve(r.Ctx, decide(held))
	if err != nil || got.Operation.State != model.Queued {
		t.Fatalf("Approve = %+v, %v", got, err)
	}
}

// A held local write commits in the unit of its approval even when the
// process that decides it never registered the intent as local: the hold
// recorded it.
func TestHeldBlockRecordsLocalMutation(t *testing.T) {
	const intent = "test.local.recorded"
	operations.RegisterApplier(intent, noopApplier)
	t.Cleanup(func() { operations.UnregisterApplier(intent) })
	r := newRig(t)
	s := proposal(r, "p-1", "fp-1")
	s.Intent.Name, s.Local = intent, true
	held := propose(t, r, pluginClient(t, r, plugin), s).Operation
	if h := held.Held; !h.Applies || !h.Local {
		t.Fatalf("held block = %+v, want applies and local", h)
	}
	got, err := r.C.Operations().Approve(r.Ctx, decide(held))
	if err != nil || got.Operation.State != model.Committed {
		t.Fatalf("Approve = %+v, %v", got, err)
	}
	if n := r.Step("w"); n != 0 {
		t.Fatalf("a local approval queued %d work items", n)
	}
}

// Every held operation names the intent it stored.
func TestHeldBlockRequiresIntent(t *testing.T) {
	if err := (model.HeldBlock{Proposer: plugin}).Validate(); !model.IsCode(err, model.Validation) {
		t.Fatalf("no intent = %v, want validation", err)
	}
	in := model.Extension{Name: "x.y", Version: 1}
	if err := (model.HeldBlock{Proposer: plugin, Intent: &in}).Validate(); err != nil {
		t.Fatalf("with intent = %v", err)
	}
}

// A proposal that stores no intent is refused at submit and claims nothing.
func TestHoldWithoutIntentIsRefused(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	s := proposal(r, "p-1", "fp-1")
	s.Intent = model.Extension{}
	_, err := operations.Submit(r.Ctx, c.Client, s)
	requireCode(t, err, model.Validation, "intent")
	if n := len(listHeld(t, r, c)); n != 0 {
		t.Fatalf("a refused hold claimed %d operations", n)
	}
}
