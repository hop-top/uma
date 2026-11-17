package conformance

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
)

// The mutation cases run edits, deletes and reactions through the engine
// against the adapter under test. Their target is a message uma sent when
// the adapter sends, else one it received, in the provider's own form.
// Every case that scripts an answer does so after the target exists.

// wantRejected requires op to be final: rejected by the provider with
// code, never to be retried.
func wantRejected(t *testing.T, op model.Operation, code model.ErrorCode) {
	t.Helper()
	if op.State != model.Rejected || op.Failure == nil || (code != "" && op.Failure.Code != code) {
		t.Fatalf("operation = %s (failure %v), want rejected by the provider", op.State, op.Failure)
	}
	if op.Failure.Retry != model.NoRetry {
		t.Fatalf("retry advice = %q, want none", op.Failure.Retry)
	}
	if op.Pending != nil {
		t.Fatalf("settled operation still pending: %+v", op.Pending)
	}
}

var mutationCases = []adapterCase{
	{name: "EditTargetsStoredProviderMessage", needs: []model.Capability{model.CapabilityEdit}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		ref := e.mustMutate(t, kindEdit, g.msg, "op-edit")
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		calls := e.calls(adapterChanA, model.CapabilityEdit)
		if len(calls) != 1 || !calls[0].Applied || calls[0].Operation != ref {
			t.Fatalf("provider edits = %+v, want one applied edit of %v", calls, ref)
		}
		if calls[0].Target != g.ref() || calls[0].Route != g.conv.ReplyRoute {
			t.Fatalf("edit target %+v on route %+v, want the stored provider message %+v on %+v", calls[0].Target, calls[0].Route, g.ref(), g.conv.ReplyRoute)
		}
		if len(calls[0].Content.Parts) != 1 || calls[0].Content.Parts[0].Text != editText {
			t.Fatalf("edit content = %+v, want %q", calls[0].Content, editText)
		}
		if got := e.storedMessage(t, g.msg.Ref); !slices.Equal(got.External, g.msg.External) {
			t.Fatalf("message references = %+v, want %+v: an edit adds none", got.External, g.msg.External)
		}
		e.stop(t)
	}},
	{name: "EditAppliedReplacesContentAndRecordsEditor", needs: []model.Capability{model.CapabilityEdit}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		ref := e.mustMutate(t, kindEdit, g.msg, "op-edit")
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		got := e.storedMessage(t, g.msg.Ref)
		wantApplied(t, kindEdit, got, true)
		if got.Revision == g.msg.Revision {
			t.Fatal("an applied edit kept the message revision")
		}
		le := got.LastEdit
		if le == nil || le.Actor != "conformance-agent" || le.By != nil || le.Operation == nil || *le.Operation != ref || le.At.IsZero() {
			t.Fatalf("last edit = %+v, want the caller's, by operation %v", le, ref)
		}
		if !reflect.DeepEqual(got.Author, g.msg.Author) || !reflect.DeepEqual(got.Audience, g.msg.Audience) || (got.Origin != nil) != (g.msg.Origin != nil) {
			t.Fatalf("author, audience or origin changed: %+v, want %+v", got, g.msg)
		}
		e.wantChanges(t, ref, model.MessageChangeEdited)
		e.stop(t)
	}},
	{name: "EditRejectedLeavesMessage", needs: []model.Capability{model.CapabilityEdit}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeRejected})
		ref := e.mustMutate(t, kindEdit, g.msg, "op-edit")
		e.later(t, 0)
		e.later(t, time.Hour)
		wantRejected(t, e.op(t, ref), "")
		if n := len(e.calls(adapterChanA, model.CapabilityEdit)); n != 1 {
			t.Fatalf("provider edits = %d, want 1: a refusal is final", n)
		}
		e.wantUnchanged(t, g.msg)
		e.wantChanges(t, ref)
		e.stop(t)
	}},
	{name: "EditOfInboundMessageAllowedByProvider", needs: []model.Capability{model.CapabilityEdit}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		g := e.inboundTarget(t, "")
		if g.msg.Origin != nil {
			t.Fatalf("inbound message has a uma origin: %+v", g.msg.Origin)
		}
		ref := e.mustMutate(t, kindEdit, g.msg, "op-edit")
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		got := e.storedMessage(t, g.msg.Ref)
		wantApplied(t, kindEdit, got, true)
		if got.Author.Identity != g.msg.Author.Identity || got.LastEdit == nil || got.LastEdit.Operation == nil || *got.LastEdit.Operation != ref {
			t.Fatalf("message = %+v, want its author kept and the edit recorded under %v", got, ref)
		}
		e.stop(t)
	}},
	{name: "EditRefusedByProviderOwnership", needs: []model.Capability{model.CapabilityEdit}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		e.respond(t, adapterChanA, Response{Outcome: OutcomeUnauthorized})
		ref := e.mustMutate(t, kindEdit, g.msg, "op-edit")
		e.later(t, 0)
		e.later(t, time.Hour)
		wantRejected(t, e.op(t, ref), model.Unauthorized)
		if n := len(e.calls(adapterChanA, model.CapabilityEdit)); n != 1 {
			t.Fatalf("provider edits = %d, want 1: an ownership refusal is never retried", n)
		}
		e.wantUnchanged(t, g.msg)
		e.wantChanges(t, ref)
		e.stop(t)
	}},
	{name: "OlderEditCompletionDoesNotClobber", needs: []model.Capability{model.CapabilityEdit}, inbound: true, run: func(t *testing.T, e *adapterEnv) {
		g := e.inboundTarget(t, "")
		ref := e.mustMutate(t, kindEdit, g.msg, "op-older")
		newer := e.update(g, InboundEdited, 1)
		newer.Text = "newer inbound edit"
		wantAcked(t, e.receiveAll(t, adapterChanA, newer))
		if got := e.storedMessage(t, g.msg.Ref); len(got.Content.Parts) != 1 || got.Content.Parts[0].Text != newer.Text {
			t.Fatalf("message = %+v after the inbound edit, want %q", got.Content, newer.Text)
		}
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		if n := len(e.calls(adapterChanA, model.CapabilityEdit)); n != 1 {
			t.Fatalf("provider edits = %d, want the older edit sent once", n)
		}
		if !e.declares(model.CapabilityRefetch) {
			// A refetch may bring the provider's own copy back.
			if got := e.storedMessage(t, g.msg.Ref); got.Content.Parts[0].Text != newer.Text {
				t.Fatalf("message content = %q, want the newer inbound edit kept", got.Content.Parts[0].Text)
			}
		}
		e.wantChanges(t, ref, model.MessageChangeEditSuperseded)
		e.stop(t)
	}},
	{name: "PendingVisibleUntilSettled", needs: []model.Capability{model.CapabilityEdit}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		a, err := e.mutate(kindEdit, g.msg, "op-edit")
		ref := e.queued(t, a, err, g.msg)
		if a.Operation.Pending != nil {
			t.Fatalf("acceptance carries the pending change: %+v", a.Operation.Pending)
		}
		p := e.op(t, ref).Pending
		if p == nil || p.Operation != ref || p.BaseRevision != g.msg.Revision || p.Content == nil || len(p.Content.Parts) != 1 || p.Content.Parts[0].Text != editText {
			t.Fatalf("pending = %+v, want the queued edit on revision %q", p, g.msg.Revision)
		}
		e.wantUnchanged(t, g.msg)
		e.later(t, 0)
		op := e.op(t, ref)
		wantActApplied(t, op)
		if op.Pending != nil {
			t.Fatalf("settled edit still pending: %+v", op.Pending)
		}
		wantApplied(t, kindEdit, e.storedMessage(t, g.msg.Ref), true)
		e.stop(t)
	}},
	{name: "DeleteTombstonesOnApplied", needs: []model.Capability{model.CapabilityDelete}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		ref := e.mustMutate(t, kindDelete, g.msg, "op-delete")
		e.wantUnchanged(t, g.msg)
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		calls := e.calls(adapterChanA, model.CapabilityDelete)
		if len(calls) != 1 || !calls[0].Applied || calls[0].Target != g.ref() {
			t.Fatalf("provider deletes = %+v, want one applied delete of %+v", calls, g.ref())
		}
		got := e.storedMessage(t, g.msg.Ref)
		if !got.Deleted || got.Redacted || len(got.Content.Parts) != 0 || got.Reactions != nil || got.Revision == g.msg.Revision {
			t.Fatalf("message = %+v, want a tombstone", got)
		}
		e.wantChanges(t, ref, model.MessageChangeDeleted)
		_, err := e.mutate(kindDelete, got, "op-delete-again")
		if model.CodeOf(err) != model.Conflict {
			t.Fatalf("Delete of a deleted message = %v, want conflict", err)
		}
		e.stop(t)
	}},
	{name: "ReactAddThenRemove", needs: []model.Capability{model.CapabilityReact}, run: func(t *testing.T, e *adapterEnv) {
		g := e.anyTarget(t)
		ref := e.mustMutate(t, kindReact, g.msg, "op-react")
		e.later(t, 0)
		wantActApplied(t, e.op(t, ref))
		got := e.storedMessage(t, g.msg.Ref)
		if len(got.Reactions) != 1 || got.Reactions[0].Emoji != reactEmoji || got.Reactions[0].Actor != "conformance-agent" ||
			got.Reactions[0].By != nil || got.Reactions[0].Count != 1 {
			t.Fatalf("reactions = %+v, want the caller's %s", got.Reactions, reactEmoji)
		}
		if !slices.Equal(got.External, g.msg.External) {
			t.Fatalf("message references = %+v, want %+v: a reaction's reference stays on its entry", got.External, g.msg.External)
		}
		e.wantChanges(t, ref, model.MessageChangeReactionAdded)
		calls := e.calls(adapterChanA, model.CapabilityReact)
		if len(calls) != 1 || calls[0].Target != g.ref() || calls[0].Emoji != reactEmoji || calls[0].Remove {
			t.Fatalf("provider reactions = %+v, want one add of %s on %+v", calls, reactEmoji, g.ref())
		}

		a, err := e.react(got, "op-unreact", reactEmoji, true)
		removed := e.queued(t, a, err, got)
		e.later(t, 0)
		wantActApplied(t, e.op(t, removed))
		if after := e.storedMessage(t, g.msg.Ref); len(after.Reactions) != 0 {
			t.Fatalf("reactions = %+v after the removal, want none", after.Reactions)
		}
		e.wantChanges(t, removed, model.MessageChangeReactionRemoved)
		calls = e.calls(adapterChanA, model.CapabilityReact)
		if len(calls) != 2 || !calls[1].Remove || calls[1].Emoji != reactEmoji {
			t.Fatalf("provider reactions = %+v, want the add then its removal", calls)
		}
		e.stop(t)
	}},
	{name: "MutationAnswerLostIsNeverResent", anyOf: mutationCaps, run: func(t *testing.T, e *adapterEnv) {
		e.eachKind(t, e.kinds(true), func(t *testing.T, e *adapterEnv, k mutationKind) {
			g := e.anyTarget(t)
			e.respond(t, adapterChanA, Response{Outcome: OutcomeAnswerLost})
			ref := e.mustMutate(t, k, g.msg, "op-lost")
			e.later(t, 0)
			op := e.op(t, ref)
			wantOp(t, op, model.Unknown, model.OutcomeUnknown)
			if op.Failure.Retry != model.ReconcileFirst {
				t.Fatalf("lost answer retry advice = %q, want reconcile_first", op.Failure.Retry)
			}
			e.wantUnchanged(t, g.msg)
			e.later(t, time.Hour)
			if n := len(e.calls(adapterChanA, k.capability)); n != 1 {
				t.Fatalf("provider %s calls = %d, want 1: a lost answer is never resent", k.name, n)
			}
			if !e.reconciles() {
				wantOp(t, e.op(t, ref), model.Unknown, model.OutcomeUnknown)
				e.wantUnchanged(t, g.msg)
				return
			}
			wantActApplied(t, e.op(t, ref))
			wantApplied(t, k, e.storedMessage(t, g.msg.Ref), true)
			if n := len(e.calls(adapterChanA, model.CapabilityReconcile)); n == 0 {
				t.Fatal("no reconcile call; the applied mutation was adopted without reconciling")
			}
			e.stop(t)
		})
	}},
	{name: "MutationRevokedPermissionMakesNoCall", anyOf: mutationCaps, run: func(t *testing.T, e *adapterEnv) {
		e.eachKind(t, e.kinds(true), func(t *testing.T, e *adapterEnv, k mutationKind) {
			g := e.anyTarget(t)
			if err := e.provider.Revoke(adapterChanA, k.capability); err != nil {
				t.Skipf("provider cannot revoke the %s permission: %v", k.name, err)
			}
			ref := e.mustMutate(t, k, g.msg, "op-revoked")
			e.later(t, 0)
			e.later(t, time.Hour)
			op := e.op(t, ref)
			wantOp(t, op, model.Failed, model.Unauthorized)
			if op.Failure.Retry != model.NoRetry {
				t.Fatalf("retry advice = %q, want none", op.Failure.Retry)
			}
			if calls := e.calls(adapterChanA, k.capability); len(calls) != 0 {
				t.Fatalf("provider %s calls = %+v, want none", k.name, calls)
			}
			e.wantUnchanged(t, g.msg)
			e.stop(t)
		})
	}},
}

var mutationUnsupportedCases = []adapterCase{
	{name: "MutationNeedsDeclaredCapability", run: func(t *testing.T, e *adapterEnv) {
		missing := e.kinds(false)
		if len(missing) == 0 {
			t.Skip("capabilities edit, delete and react declared")
		}
		if !e.declares(model.CapabilitySend) && !e.canReceive() {
			t.Skip("no message to mutate: capabilities send and receive not declared")
		}
		e.eachKind(t, missing, func(t *testing.T, e *adapterEnv, k mutationKind) {
			g := e.anyTarget(t)
			before := len(e.provider.Calls(adapterChanA))
			_, err := e.mutate(k, g.msg, "op-undeclared")
			if model.CodeOf(err) != model.Unsupported {
				t.Fatalf("%s on a channel without %s = %v, want unsupported", k.name, k.capability, err)
			}
			e.later(t, time.Hour)
			if calls := e.provider.Calls(adapterChanA); len(calls) != before {
				t.Fatalf("provider calls = %+v, want none after the refusal", calls[before:])
			}
			e.wantUnchanged(t, g.msg)
			e.stop(t)
		})
	}},
}
