package runtime_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// replyMessage queues a reply and returns its outbound message, which has
// no provider message until the reply settles.
func replyMessage(t *testing.T, r *rig) (model.Acceptance, model.Message) {
	t.Helper()
	a := r.Reply("op-reply", "hello")
	return a, r.Message(*a.Operation.ResultMessage)
}

// An edit of a message whose sending reply has no provider answer yet
// waits for it without calling the provider, then targets the provider
// message the reply recorded.
func TestEditWaitsForUnsettledOrigin(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	r.F.Respond(transientFailure, func(_ context.Context, req adapter.SendRequest) (adapter.Outcome, error, bool) {
		return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{model.ProviderMessageRef(req.Route.Channel, "pm-reply")}}, nil, true
	})
	reply, m := replyMessage(t, r)
	a := r.Edit("op-edit", m, "corrected")
	r.Step("w")
	requireState(t, r.Op(reply.Operation.Ref), model.RetryPending, model.Transient)
	op := r.Op(a.Operation.Ref)
	requireState(t, op, model.Queued, "")
	if len(r.F.MutationCalls()) != 0 || len(r.Evidence(a.Operation.Ref)) != 0 {
		t.Fatalf("waiting edit reached the provider: %v", r.F.Calls())
	}
	r.Clock.Advance(time.Minute)
	r.Step("w")
	r.Step("w")
	requireState(t, r.Op(reply.Operation.Ref), model.ProviderAccepted, "")
	requireState(t, r.Op(a.Operation.Ref), model.ProviderAccepted, "")
	sent := r.Message(m.Ref)
	calls := r.F.MutationCalls()
	if len(calls) != 1 || calls[0].Effect.Target == nil || *calls[0].Effect.Target != *operations.ProviderMessageRef(sent, r.Ch.Ref) {
		t.Fatalf("calls = %+v, want one edit of the reply's provider message %v", calls, sent.External)
	}
	if text(sent) != "corrected" {
		t.Fatalf("message = %+v", sent)
	}
}

// The wait for a provider message is bounded: an origin stuck
// outcome_unknown is waited for only until the intent's deadline, then
// the mutation fails without a call; an origin that settled without a
// provider message fails it at once.
func TestMutationWaitForProviderMessageIsBounded(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		r := newRig(t, enginetest.WithMutations)
		r.F.Respond(lostAnswer)
		reply, m := replyMessage(t, r)
		r.Step("w")
		requireState(t, r.Op(reply.Operation.Ref), model.Unknown, model.OutcomeUnknown)
		a := r.Edit("op-edit", m, "corrected")
		r.Step("w")
		requireState(t, r.Op(a.Operation.Ref), model.Queued, "")
		r.Clock.Advance(model.DefaultStreamDeadline - time.Second)
		r.Step("w")
		requireState(t, r.Op(a.Operation.Ref), model.Queued, "")
		r.Clock.Advance(2 * time.Second)
		r.Step("w")
		requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Permanent)
		if len(r.F.MutationCalls()) != 0 {
			t.Fatalf("calls = %v", r.F.Calls())
		}
		if r.Step("w") != 0 {
			t.Fatal("failed mutation left work")
		}
	})
	t.Run("settled origin", func(t *testing.T) {
		r := newRig(t, enginetest.WithMutations)
		r.F.Respond(rejectedAnswer)
		reply, m := replyMessage(t, r)
		r.Step("w")
		requireState(t, r.Op(reply.Operation.Ref), model.Rejected, model.Unauthorized)
		a := r.Edit("op-edit", m, "corrected")
		r.Step("w")
		requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Permanent)
		if len(r.F.MutationCalls()) != 0 {
			t.Fatalf("calls = %v", r.F.Calls())
		}
	})
}

// A target deleted, redacted or turned into a live stream's message after
// the claim fails the mutation without a provider call.
func TestEditOfDeletedMessageFailsWithoutCall(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(r *rig, m model.Message)
	}{
		{"deleted", func(r *rig, m model.Message) {
			r.Mutate(m.Ref, func(x *model.Message) { x.Deleted, x.Content = true, model.Content{} })
		}},
		{"redacted", func(r *rig, m model.Message) {
			r.Mutate(m.Ref, func(x *model.Message) { x.Redacted, x.Content = true, model.Content{} })
		}},
		{"live stream", func(r *rig, m model.Message) {
			st := model.Stream{
				Ref: model.StreamRef{Conversation: r.Conv.Ref, ID: "00000007-0000-7000-8000-000000000001"}, Mode: model.StreamEdit,
				State: model.StreamOpen, Audience: m.Audience, ProviderReferences: []model.ExternalRef{m.External[0]}, Revision: "s1",
			}
			if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error { return tx.PutStream(r.Ctx, st, "") }); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t, enginetest.WithMutations)
			m := r.SeedMessage("original")
			a := r.Edit("op-edit", m, "corrected")
			c.change(r, m)
			r.Step("w")
			requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Conflict)
			if len(r.F.MutationCalls()) != 0 {
				t.Fatalf("calls = %v, want none", r.F.Calls())
			}
			if u := updates(t, r, a.Operation.Ref); len(u) != 0 {
				t.Fatalf("message.updated = %+v", u)
			}
		})
	}
}

// A lost answer without a reconciler leaves the mutation outcome_unknown,
// the message untouched and the effect never retried.
func TestMutationOutcomeUnknownLeavesMessage(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	r.F.Respond(lostAnswer)
	a := r.Edit("op-edit", m, "corrected")
	r.Step("w")
	r.Clock.Advance(time.Hour)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	if got := r.Message(m.Ref); got.Revision != m.Revision || text(got) != "original" {
		t.Fatalf("message = %+v, want it untouched", got)
	}
	if n := len(r.F.MutationCalls()); n != 1 {
		t.Fatalf("edit calls = %d, want 1", n)
	}
}

// A lost mutation answer is reconciled before any retry, with the
// capability, content and reaction the attempt carried; the reconciled
// effect then applies.
func TestMutationUnknownIsReconciledBeforeRetry(t *testing.T) {
	r := newRig(t, enginetest.WithMutations, withReconciler)
	m := r.SeedMessage("original")
	r.F.Respond(lostAnswer, lostAnswer)
	edit := r.Edit("op-edit", m, "corrected")
	react := r.React("op-react", m, "👍", false)
	r.Step("w")
	r.Clock.Advance(time.Minute)
	r.Step("w")
	requireState(t, r.Op(edit.Operation.Ref), model.ProviderAccepted, "")
	requireState(t, r.Op(react.Operation.Ref), model.ProviderAccepted, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"edit", "react", "reconcile", "reconcile"}) {
		t.Fatalf("calls = %v, want each effect once, then reconciled", got)
	}
	rc := r.F.Reconciled()
	byOp := map[model.OperationRef]adapter.ReconcileRequest{rc[0].Operation: rc[0], rc[1].Operation: rc[1]}
	e, x := byOp[edit.Operation.Ref], byOp[react.Operation.Ref]
	if e.Capability != model.CapabilityEdit || len(e.Content.Parts) != 1 || e.Content.Parts[0].Text != "corrected" || e.Target == nil {
		t.Fatalf("edit reconcile = %+v", e)
	}
	if x.Capability != model.CapabilityReact || x.Emoji != "👍" || x.Remove || len(x.Content.Parts) != 0 {
		t.Fatalf("react reconcile = %+v", x)
	}
	got := r.Message(m.Ref)
	if text(got) != "corrected" || len(got.Reactions) != 1 {
		t.Fatalf("message = %+v, want both applied", got)
	}
}

// Every attempt reauthorizes the caller under the operation's action with
// the message's audience; a revoked permission makes no call.
func TestEditRevokedPermissionMakesNoCall(t *testing.T) {
	r := newRig(t, enginetest.WithMutations)
	m := r.SeedMessage("original")
	a := r.Edit("op-edit", m, "corrected")
	r.Policy.Set(true)
	r.Step("w")
	r.Policy.Set(false)
	requireState(t, r.Op(a.Operation.Ref), model.Failed, model.Unauthorized)
	if len(r.F.MutationCalls()) != 0 {
		t.Fatalf("calls = %v", r.F.Calls())
	}
	r2 := newRig(t, enginetest.WithMutations)
	m2 := r2.SeedMessage("original")
	r2.Delete("op-delete", m2)
	r2.Step("w")
	if got := r2.Policy.Actions(); len(got) < 2 || got[len(got)-1] != model.ActionMessageDelete {
		t.Fatalf("policy checks = %v, want message.delete reauthorized at execution", got)
	}
}

// A provider refusal of the account's change is final: the operation is
// rejected, the message untouched, nothing retried.
func TestProviderOwnershipRejectionIsFinal(t *testing.T) {
	r := newRig(t, enginetest.WithMutations, withReconciler)
	m := r.SeedMessage("original")
	r.F.Respond(rejectedAnswer)
	a := r.Delete("op-delete", m)
	r.Step("w")
	r.Clock.Advance(time.Hour)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Rejected, model.Unauthorized)
	if got := r.Message(m.Ref); got.Deleted || got.Revision != m.Revision {
		t.Fatalf("message = %+v, want it untouched", got)
	}
	if got := r.F.Calls(); !slices.Equal(got, []string{"delete"}) {
		t.Fatalf("calls = %v, want one delete", got)
	}
}

// Evidence that the provider applied the effect, such as a delivery
// report, settles the operation without loading its job: the settle
// reloads the target and the caller itself.
func TestMutationSettledByAppliedEvidenceApplies(t *testing.T) {
	r := newRig(t, enginetest.WithMutations, withReconciler)
	m := r.SeedMessage("original")
	r.F.Respond(lostAnswer)
	a := r.Edit("op-edit", m, "corrected")
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Unknown, model.OutcomeUnknown)
	ev := r.Evidence(a.Operation.Ref)
	err := operations.AppendEvidence(r.Ctx, r.E, store.AttemptEvidence{
		Operation: a.Operation.Ref, Attempt: ev[0].Attempt, State: model.Delivered, ObservedAt: r.Clock.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r.Clock.Advance(time.Minute)
	r.Step("w")
	requireState(t, r.Op(a.Operation.Ref), model.Delivered, "")
	if got := r.F.Calls(); !slices.Equal(got, []string{"edit"}) {
		t.Fatalf("calls = %v, want the edit only", got)
	}
	got := r.Message(m.Ref)
	if text(got) != "corrected" || got.LastEdit == nil || got.LastEdit.Actor != enginetest.Actor {
		t.Fatalf("message = %+v, want the edit applied by the caller", got)
	}
}
