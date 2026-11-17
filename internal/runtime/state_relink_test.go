package runtime_test

import (
	"context"
	"slices"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// movedTo answers an applied move that gave every item the reference of
// the destination folder.
func movedTo(r *rig, items []model.ExternalRef) (enginetest.SendFunc, []adapter.ReferenceChange) {
	var changes []adapter.ReferenceChange
	for _, it := range items {
		changes = append(changes, adapter.ReferenceChange{Old: it, New: model.ProviderMessageRef(r.Ch.Ref, "f-archive:"+it.ID)})
	}
	return func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.ProviderAccepted, ReferenceChanges: changes}, nil, true
	}, changes
}

func storedExternal(r *rig, c adapter.ReferenceChange) (model.Message, bool) {
	r.T.Helper()
	for _, m := range r.StoredMessages() {
		if slices.Contains(m.External, c.Old) {
			return m, true
		}
	}
	return model.Message{}, false
}

// A move that reports a changed reference adds the new one ahead of the old
// on the message that held it, keeps the old one, and the next state
// operation names the new reference.
func TestStateMoveRelinksMessageReferences(t *testing.T) {
	r := newRig(t, enginetest.WithState(model.ScopeAccount))
	items := r.SeedProviderMessages(2)
	answer, changes := movedTo(r, items)
	r.F.Respond(answer)
	a, err := r.Move("op-move", model.MoveDestination{ID: "f-archive"})
	if err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	for k, c := range changes {
		m, ok := storedExternal(r, c)
		if !ok {
			t.Fatalf("no message holds %v any more", c.Old)
		}
		if !slices.Equal(m.External, []model.ExternalRef{c.New, c.Old}) {
			t.Fatalf("message %d references = %v, want the new one ahead of the kept old one", k, m.External)
		}
		if m.Revision == "m1" {
			t.Fatalf("message %d kept its revision", k)
		}
	}

	if _, err := r.SetRead("op-read", true); err != nil {
		t.Fatal(err)
	}
	r.Step("w")
	calls := stateCalls(r)
	if len(calls) != 2 {
		t.Fatalf("state calls = %d, want 2", len(calls))
	}
	want := []model.ExternalRef{changes[0].New, changes[1].New}
	if got := calls[1].State.Items; !slices.Equal(got, want) {
		t.Fatalf("items of the next operation = %v, want the re-resolved references %v", got, want)
	}
}

// A move the provider did not apply, or that reports no change, leaves
// the messages' references as they were.
func TestStateMoveWithoutChangeKeepsReferences(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func(items []model.ExternalRef, r *rig) enginetest.SendFunc
	}{
		{"rejected", func(items []model.ExternalRef, r *rig) enginetest.SendFunc {
			return func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
				return adapter.Outcome{State: model.Rejected, Failure: model.NewError(model.Permanent, "no"),
					ReferenceChanges: []adapter.ReferenceChange{{Old: items[0], New: model.ProviderMessageRef(r.Ch.Ref, "x")}}}, nil, true
			}
		}},
		{"stable", func([]model.ExternalRef, *rig) enginetest.SendFunc { return enginetest.Accepted }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithState(model.ScopeAccount))
			items := r.SeedProviderMessages(1)
			r.F.Respond(tc.fn(items, r))
			if _, err := r.Move("op-move", model.MoveDestination{ID: "f-archive"}); err != nil {
				t.Fatal(err)
			}
			r.Step("w")
			ms := r.StoredMessages()
			if len(ms) != 1 || !slices.Equal(ms[0].External, items) || ms[0].Revision != "m1" {
				t.Fatalf("messages = %+v, want the reference %v and the revision untouched", ms, items)
			}
		})
	}
}
