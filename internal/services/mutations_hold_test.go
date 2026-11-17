package services_test

import (
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
)

// Command.Hold is excluded from the fingerprint: one reaction proposed with
// a hold and again without it claims one key and returns the held operation.
func TestCommandHoldExcludedFromFingerprint(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	p, err := enginetest.For(w.E, model.Principal{Tenant: tenantA, Actor: "plugin:triage"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := w.cmd("")
	held := cmd
	held.Hold = &model.HoldProposal{Reason: "review", TrustKey: "triage/react", TrustLabels: map[string]string{"k": "v"}}
	react := func(c model.Command) (model.Acceptance, error) {
		return p.Messages().React(w.Ctx, model.Reaction{Command: c, Message: w.target.Ref, Emoji: "+1"})
	}
	a, err := react(held)
	if err != nil || a.Operation.State != model.Held || a.Operation.Held == nil || a.Operation.Held.TrustKey != "triage/react" {
		t.Fatalf("held = %+v, %v", a.Operation, err)
	}
	b, err := react(cmd)
	if err != nil || !b.Replayed || b.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay without hold = %+v, %v; want the same operation, replayed", b, err)
	}
	cmd2 := w.cmd("")
	cmd2.Operation = cmd.Operation
	cmd2.Hold = held.Hold
	if c, err := react(cmd2); err != nil || !c.Replayed || c.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay with hold = %+v, %v", c, err)
	}
}

// A person's command cannot be held, and a malformed hold claims nothing.
func TestCommandHoldRefusals(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	cmd := w.cmd("")
	cmd.Hold = &model.HoldProposal{TrustKey: "k"}
	if _, err := w.react(w.target, cmd, "+1", false); !model.IsCode(err, model.Validation) {
		t.Fatalf("person hold err = %v, want validation", err)
	}
	long := w.cmd("")
	long.Hold = &model.HoldProposal{TrustKey: string(make([]byte, model.MaxTrustKeyBytes+1))}
	if _, err := w.react(w.target, long, "+1", false); !model.IsCode(err, model.Validation) {
		t.Fatalf("long trust key err = %v, want validation", err)
	}
}
