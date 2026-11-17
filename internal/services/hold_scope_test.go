package services_test

import (
	"testing"

	"hop.top/uma/model"
)

// The duplicate guard of a held start is scoped to the inbox and the
// recipient: a proposal for another recipient is held on its own, a second
// one for the same recipient is answered with the first.
func TestHeldStartDuplicateGuardIsPerRecipient(t *testing.T) {
	eachStartMode(t, func(t *testing.T, w *startWorld) {
		a, err := w.hold("op-1", "u-1", "one")
		if err != nil || a.AlreadyHeld {
			t.Fatalf("first = %+v, %v", a, err)
		}
		b, err := w.hold("op-2", "u-2", "two")
		if err != nil || b.AlreadyHeld || b.Operation.State != model.Held || b.Operation.Ref == a.Operation.Ref {
			t.Fatalf("other recipient = %+v, %v; want its own held operation", b, err)
		}
		c, err := w.hold("op-3", "u-1", "three")
		if err != nil || !c.AlreadyHeld || c.Operation.Ref != a.Operation.Ref {
			t.Fatalf("same recipient = %+v, %v; want the first held operation", c, err)
		}
	})
}

// A held label create is scoped to the label's name, folded as label names
// are unique: another name is held on its own, a case variant of a held
// name is answered with it.
func TestHeldLabelCreateDuplicateGuardIsPerName(t *testing.T) {
	w := newHeldWorld(t, holdLocalWrites)
	create := func(op model.OperationID, name string) (model.Acceptance, error) {
		return w.plugin.Labels().Configure(w.Ctx, model.ConfigureLabel{
			Command: heldCmd(op, ""), Label: model.Label{Ref: model.LabelRef{Tenant: tenantA}, Name: name},
		})
	}
	a, err := create("op-1", "Fresh")
	if err != nil || a.AlreadyHeld {
		t.Fatalf("first = %+v, %v", a, err)
	}
	b, err := create("op-2", "Other")
	if err != nil || b.AlreadyHeld || b.Operation.State != model.Held || b.Operation.Ref == a.Operation.Ref {
		t.Fatalf("other name = %+v, %v; want its own held operation", b, err)
	}
	c, err := create("op-3", "FRESH")
	if err != nil || !c.AlreadyHeld || c.Operation.Ref != a.Operation.Ref {
		t.Fatalf("same name = %+v, %v; want the first held operation", c, err)
	}
}
