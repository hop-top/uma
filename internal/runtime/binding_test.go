package runtime_test

import (
	"context"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func putWorkItem(t *testing.T, r *rig) model.WorkItem {
	t.Helper()
	w := model.WorkItem{
		Ref:      model.WorkItemRef{Conversation: r.Conv.Ref, ID: "00000009-0000-7000-8000-0000000000e1"},
		External: model.ExternalRef{Channel: r.Ch.Ref, Namespace: enginetest.RouteNSpace, Kind: model.RefKindWorkItem, ID: "7"},
		Subject:  "Broken build", Status: model.WorkItemOpen, Binding: model.WorkItemBindingActive, Revision: "w1",
	}
	if err := r.E.Store().Transact(r.Ctx, enginetest.TenantA, func(tx store.Tx) error { return tx.PutWorkItem(r.Ctx, w, "") }); err != nil {
		t.Fatal(err)
	}
	return w
}

// A reply the provider refuses because the issue is gone (410) or out of
// reach (404) moves the conversation's work item to that binding state,
// with history and a work_item.updated event; an outcome naming no
// binding, or a declared one other than deleted or inaccessible, leaves it
// active.
func TestRejectedReplySetsWorkItemBinding(t *testing.T) {
	for name, c := range map[string]struct {
		binding string
		want    string
	}{
		"deleted":      {model.WorkItemBindingDeleted, model.WorkItemBindingDeleted},
		"inaccessible": {model.WorkItemBindingInaccessible, model.WorkItemBindingInaccessible},
		"none":         {"", model.WorkItemBindingActive},
		"merged":       {model.WorkItemBindingMerged, model.WorkItemBindingActive},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			w := putWorkItem(t, r)
			r.F.Respond(func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
				return adapter.Outcome{State: model.Rejected, Binding: c.binding, Failure: model.NewError(model.NotFound, "issue gone")}, nil, false
			})
			a := r.Reply("op-1", "anyone?")
			r.Step("w")
			requireState(t, r.Op(a.Operation.Ref), model.Rejected, model.NotFound)
			var got model.WorkItem
			if err := r.E.Store().View(r.Ctx, enginetest.TenantA, func(rd store.Reader) error {
				var err error
				got, err = rd.GetWorkItem(r.Ctx, w.Ref)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if got.Binding != c.want {
				t.Fatalf("binding %q, want %q", got.Binding, c.want)
			}
			updated := 0
			for _, ev := range r.Events() {
				if ev.Kind == model.EventWorkItemUpdated {
					updated++
				}
			}
			if c.want == model.WorkItemBindingActive {
				if updated != 0 || got.Revision != w.Revision {
					t.Fatalf("work item rewritten (%d events) without a lost binding", updated)
				}
				return
			}
			if updated != 1 || len(got.History) != 1 || got.History[0].From != model.WorkItemBindingActive || got.History[0].To != c.want {
				t.Fatalf("events %d history %+v, want one transition to %s", updated, got.History, c.want)
			}
		})
	}
}
