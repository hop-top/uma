package services_test

import (
	"slices"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// A write that commits locally with no approval step to hold it (a status
// transition, a label definition) refuses a Command.Hold as unsupported,
// never applies it. Nothing is claimed either: the
// same command without the hold is a fresh write, not a replay.
func TestLocalWriteWithHoldIsRefusedNotApplied(t *testing.T) {
	s := newSvc(t)
	l := s.configure("op-l", "Urgent")
	plugin, err := enginetest.For(s.E, model.Principal{Tenant: tenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	held := func(op model.OperationID, rev model.Revision) model.Command {
		c := cmd(op, rev)
		c.Hold = &model.HoldProposal{Reason: "review", TrustKey: "triage/local"}
		return c
	}
	cases := []struct {
		name string
		rev  func() model.Revision
		run  func(c model.Command) (model.Acceptance, error)
	}{
		{"transition", func() model.Revision { return s.getConv(s.Conv.Ref).Revision }, func(c model.Command) (model.Acceptance, error) {
			return plugin.Conversations().Transition(s.Ctx, model.Transition{Command: c, Conversation: s.Conv.Ref, State: model.ConversationClosed})
		}},
		{"label create", func() model.Revision { return "" }, func(c model.Command) (model.Acceptance, error) {
			return plugin.Labels().Configure(s.Ctx, model.ConfigureLabel{
				Command: c, Label: model.Label{Ref: model.LabelRef{Tenant: tenantA}, Name: "Fresh"},
			})
		}},
		{"label delete", func() model.Revision { return s.getLabel(l.Ref).Revision }, func(c model.Command) (model.Acceptance, error) {
			return plugin.Labels().Delete(s.Ctx, model.DeleteLabel{Command: c, Label: l.Ref})
		}},
	}
	for i, tc := range cases {
		op := model.OperationID("op-hold-" + string(rune('a'+i)))
		before := s.getConv(s.Conv.Ref)
		rev := tc.rev()
		a, err := tc.run(held(op, rev))
		if !model.IsCode(err, model.Unsupported) {
			t.Fatalf("%s: held = %+v, %v; want unsupported", tc.name, a.Operation, err)
		}
		if got := s.getConv(s.Conv.Ref); got.Revision != before.Revision || got.Status != before.Status || !slices.Equal(got.Labels, before.Labels) {
			t.Fatalf("%s: conversation changed by a refused hold: %+v", tc.name, got)
		}
		if _, err := s.C.Labels().Get(s.Ctx, model.Lookup[model.LabelRef]{Call: call(), Ref: l.Ref}); err != nil {
			t.Fatalf("%s: label gone after a refused hold: %v", tc.name, err)
		}
		a, err = tc.run(cmd(op, rev))
		if err != nil || a.Replayed {
			t.Fatalf("%s: same key without hold = %+v, %v; want a fresh write (the hold claimed nothing)", tc.name, a, err)
		}
	}
}
