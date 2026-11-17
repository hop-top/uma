package services_test

import (
	"slices"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// holdLocalWrites opts the local writes that refuse a hold by default into
// being held.
func holdLocalWrites(c *engine.Config, _ *enginetest.OutFactory) {
	c.Held = engine.HeldPolicy{LabelConfigure: true, LabelDelete: true, ConversationTransition: true}
}

func (w *heldWorld) labelExists(ref model.LabelRef) (model.Label, bool) {
	w.T.Helper()
	l, err := w.C.Labels().Get(w.Ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: ref})
	if model.IsCode(err, model.NotFound) {
		return model.Label{}, false
	}
	if err != nil {
		w.T.Fatal(err)
	}
	return l, true
}

// The local writes opted in by Config.Held, as held intents.
func localWriteIntents() []heldIntent {
	opts := []enginetest.RigOption{holdLocalWrites}
	retargetLabel := func(w *heldWorld) any {
		return map[string]any{"tenant": string(tenantA), "id": "00000000-0000-7000-8000-00000000dead"}
	}
	return []heldIntent{
		{
			name: "label create", opts: opts, local: true, editWant: "Edited",
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Labels().Configure(w.Ctx, model.ConfigureLabel{
					Command: heldCmd(op, ""), Label: model.Label{Ref: model.LabelRef{Tenant: tenantA}, Name: "Fresh"},
				})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["label"].(map[string]any)["name"] = "Edited" })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["label"].(map[string]any)["ref"] = retargetLabel(w) })
			},
			applied: func(w *heldWorld, op model.Operation) []string {
				ref, err := op.Result.AsLabel()
				if err != nil {
					w.T.Fatal(err)
				}
				if l, ok := w.labelExists(ref); ok {
					return []string{l.Name}
				}
				return nil
			},
		},
		{
			name: "label update", opts: opts, local: true, editWant: "Edited",
			setup: func(w *heldWorld) { w.labels["Urgent"] = w.configure("lab-1", "Urgent").Ref },
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				l := w.getLabel(w.labelRef("Urgent"))
				l.Name = "Renamed"
				return w.plugin.Labels().Configure(w.Ctx, model.ConfigureLabel{Command: heldCmd(op, l.Revision), Label: l})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["label"].(map[string]any)["name"] = "Edited" })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["label"].(map[string]any)["ref"] = retargetLabel(w) })
			},
			// Renamed counts only once approved: before, the label is still Urgent.
			applied: func(w *heldWorld, _ model.Operation) []string {
				l := w.getLabel(w.labelRef("Urgent"))
				if l.Name == "Urgent" {
					return nil
				}
				return []string{l.Name}
			},
		},
		{
			name: "label delete", opts: opts, local: true,
			setup: func(w *heldWorld) { w.labels["Urgent"] = w.configure("lab-1", "Urgent").Ref },
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Labels().Delete(w.Ctx, model.DeleteLabel{
					Command: heldCmd(op, w.getLabel(w.labelRef("Urgent")).Revision), Label: w.labelRef("Urgent"),
				})
			},
			// A delete has nothing to edit: the same arguments are admitted.
			within: func(_ *heldWorld, st model.Extension) model.Extension { return st },
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["label"] = retargetLabel(w) })
			},
			// Reports the label while it is gone.
			applied: func(w *heldWorld, _ model.Operation) []string {
				if _, ok := w.labelExists(w.labelRef("Urgent")); ok {
					return nil
				}
				return []string{"deleted"}
			},
			editWant: "deleted",
		},
		{
			name: "transition", opts: opts, local: true, editWant: model.ConversationClosed,
			propose: func(w *heldWorld, op model.OperationID) (model.Acceptance, error) {
				return w.plugin.Conversations().Transition(w.Ctx, model.Transition{
					Command: heldCmd(op, w.getConv(w.Conv.Ref).Revision), Conversation: w.Conv.Ref, State: model.ConversationPending,
				})
			},
			within: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["state"] = model.ConversationClosed })
			},
			away: func(w *heldWorld, st model.Extension) model.Extension {
				return edited(w.T, st, func(m map[string]any) { m["conversation"] = retarget(w) })
			},
			applied: func(w *heldWorld, _ model.Operation) []string {
				if st := w.getConv(w.Conv.Ref).Status; st != model.ConversationOpen {
					return []string{st}
				}
				return nil
			},
		},
	}
}

// Rejecting a held local write leaves nothing behind, and approval of a
// target that moved on is refused as stale.
func TestHeldLocalWritesRejectLeavesNothing(t *testing.T) {
	for _, in := range localWriteIntents() {
		t.Run(in.name, func(t *testing.T) {
			w := newHeldWorld(t, in.opts...)
			if in.setup != nil {
				in.setup(w)
			}
			beforeConv := w.getConv(w.Conv.Ref)
			a, err := in.propose(w, "op-r")
			if err != nil || a.Operation.State != model.Held {
				t.Fatalf("hold = %+v, %v", a.Operation, err)
			}
			d, err := w.C.Operations().Reject(w.Ctx, w.decide(a.Operation))
			if err != nil || d.Operation.State != model.Rejected {
				t.Fatalf("Reject = %+v, %v", d.Operation, err)
			}
			if got := in.applied(w, d.Operation); len(got) != 0 {
				t.Fatalf("applied after reject: %v", got)
			}
			if got := w.getConv(w.Conv.Ref); got.Revision != beforeConv.Revision || got.Status != beforeConv.Status || !slices.Equal(got.Labels, beforeConv.Labels) {
				t.Fatalf("conversation changed by a rejected hold: %+v", got)
			}
			if n := w.Step("w"); n != 0 {
				t.Fatalf("a rejected local write queued %d work items", n)
			}
		})
	}
}

// A hold on a write the config did not opt in stays refused, claiming
// nothing, whichever of the others is opted in.
func TestHeldLocalWritesRefusedUnlessOptedIn(t *testing.T) {
	for i, in := range localWriteIntents() {
		only := engine.HeldPolicy{}
		switch in.name {
		case "label create", "label update":
			only.LabelConfigure = true
		case "label delete":
			only.LabelDelete = true
		case "transition":
			only.ConversationTransition = true
		}
		for _, other := range localWriteIntents() {
			if other.name == in.name {
				continue
			}
			w := newHeldWorld(t, func(c *engine.Config, _ *enginetest.OutFactory) { c.Held = only })
			if other.setup != nil {
				other.setup(w)
			}
			optedIn := (in.name == "label create" && other.name == "label update") || (in.name == "label update" && other.name == "label create")
			_, err := other.propose(w, model.OperationID("op-x"+string(rune('a'+i))))
			if optedIn {
				if err != nil {
					t.Fatalf("%s with %s opted in: %v", other.name, in.name, err)
				}
				continue
			}
			if !model.IsCode(err, model.Unsupported) {
				t.Fatalf("%s with only %s opted in: err = %v, want unsupported", other.name, in.name, err)
			}
			var n int
			w.view(func(r store.Reader) error {
				p, err := r.Operations(w.Ctx, model.ListRequest{Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: 10}})
				n = len(p.Items)
				return err
			})
			if n != 0 {
				t.Fatalf("%s refused hold claimed %d operations", other.name, n)
			}
		}
	}
}

// A held proposal is checked as the command would be: a stale expected
// revision is refused at hold time, claiming nothing.
func TestHeldLocalWritesCheckedAtHold(t *testing.T) {
	w := newHeldWorld(t, holdLocalWrites)
	l := w.configure("lab-1", "Urgent")
	_, err := w.plugin.Labels().Delete(w.Ctx, model.DeleteLabel{Command: heldCmd("op-d", "stale"), Label: l.Ref})
	if !model.IsCode(err, model.Conflict) {
		t.Fatalf("stale label delete hold = %v, want conflict", err)
	}
	_, err = w.plugin.Conversations().Transition(w.Ctx, model.Transition{
		Command: heldCmd("op-t", "stale"), Conversation: w.Conv.Ref, State: model.ConversationClosed,
	})
	if !model.IsCode(err, model.Conflict) {
		t.Fatalf("stale transition hold = %v, want conflict", err)
	}
}
