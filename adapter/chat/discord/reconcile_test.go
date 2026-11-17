package discord

import (
	"context"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func reconcileSend(r *rig, e adapter.Effect, content string, at time.Time) (adapter.Outcome, error) {
	return r.x.Reconcile(context.Background(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilitySend, Content: text(content), AttemptedAt: at})
}

func TestReconcileSend(t *testing.T) {
	for _, nonceInReads := range []bool{true, false} {
		r := newRig(t)
		r.acct.nonceInReads = nonceInReads
		r.f.put("acct1", testRoom, testPerson, "before", "")
		e := r.effect(r.x.roomRef(testRoom))
		r.f.respond("acct1", answerLost)
		if _, err := r.x.Send(t.Context(), adapter.SendRequest{Effect: e, Content: text("found me")}); !model.IsCode(err, model.OutcomeUnknown) {
			t.Fatalf("lost answer = %v", err)
		}
		out, err := reconcileSend(r, e, "found me", testNow)
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
			t.Fatalf("nonce in reads %v: reconcile = %+v, %v", nonceInReads, out, err)
		}
		// Another operation's message with the same text is not this one's,
		// when reads show nonces.
		other := e
		other.Operation.ID = "00000ad0-0000-7000-8000-0000000000aa"
		out, _ = reconcileSend(r, other, "found me", testNow)
		if nonceInReads && out.State != model.Failed {
			t.Fatalf("another operation's message matched: %+v", out)
		}
		// Nothing of the operation after the attempt: not applied, a retry
		// is safe.
		third := e
		third.Operation.ID = "00000ad0-0000-7000-8000-0000000000ab"
		out, _ = reconcileSend(r, third, "never sent", testNow)
		if out.State != model.Failed || out.Failure.Code != model.NotFound {
			t.Fatalf("not sent = %+v", out)
		}
		// Sent before the attempt (beyond the skew) does not count.
		out, _ = reconcileSend(r, e, "found me", testNow.Add(time.Minute))
		if out.State != model.Failed {
			t.Fatalf("an earlier message counted: %+v", out)
		}
	}
	// A lost request applied nothing.
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "before", "")
	e := r.effect(r.x.roomRef(testRoom))
	r.f.respond("acct1", requestLost)
	_, _ = r.x.Send(t.Context(), adapter.SendRequest{Effect: e, Content: text("lost")})
	if out, _ := reconcileSend(r, e, "lost", testNow); out.State != model.Failed {
		t.Fatalf("lost request = %+v", out)
	}
}

// A listing cut short is inconclusive, never "not found".
func TestReconcileSendBounded(t *testing.T) {
	r := newRig(t)
	for i := range uint64(reconcilePages*pageSize + 1) {
		r.f.put("acct1", testRoom, testPerson, "noise", sid(testNow, 10+i))
	}
	out, err := reconcileSend(r, r.effect(r.x.roomRef(testRoom)), "needle", testNow)
	if err != nil || out.State != model.Unknown {
		t.Fatalf("reconcile past %d pages = %+v, %v, want unknown", reconcilePages, out, err)
	}
}

func TestReconcileMutations(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "x", "")
	sent, _ := r.x.Send(t.Context(), adapter.SendRequest{Effect: r.effect(r.x.roomRef(testRoom)), Content: text("v1")})
	e := r.effect(r.x.roomRef(testRoom))
	e.Target = &sent.References[0]
	rec := func(c model.Capability, content string, emoji string, remove bool) adapter.Outcome {
		t.Helper()
		out, err := r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: c, Content: text(content), Emoji: emoji, Remove: remove, AttemptedAt: testNow})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := rec(model.CapabilityEdit, "v2", "", false); out.State != model.Failed {
		t.Fatalf("edit not applied = %+v", out)
	}
	_, _ = r.x.Edit(t.Context(), adapter.EditRequest{Effect: e, Content: text("v2")})
	if out := rec(model.CapabilityEdit, "v2", "", false); out.State != model.ProviderAccepted {
		t.Fatalf("edit applied = %+v", out)
	}
	if out := rec(model.CapabilityReact, "", "👍", false); out.State != model.Failed {
		t.Fatalf("reaction not applied = %+v", out)
	}
	_, _ = r.x.React(t.Context(), adapter.ReactRequest{Effect: e, Emoji: "👍"})
	if out := rec(model.CapabilityReact, "", "👍", false); out.State != model.ProviderAccepted {
		t.Fatalf("reaction applied = %+v", out)
	}
	if out := rec(model.CapabilityReact, "", "👍", true); out.State != model.Failed {
		t.Fatalf("removal not applied = %+v", out)
	}
	if out := rec(model.CapabilityDelete, "", "", false); out.State != model.Failed {
		t.Fatalf("delete not applied = %+v", out)
	}
	_, _ = r.x.Delete(t.Context(), adapter.DeleteRequest{Effect: e})
	if out := rec(model.CapabilityDelete, "", "", false); out.State != model.ProviderAccepted {
		t.Fatalf("delete applied = %+v", out)
	}
	if out := rec(model.CapabilityEdit, "v2", "", false); out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("edit of a deleted message = %+v", out)
	}
	if _, err := r.x.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityRespond}); !model.IsCode(err, model.Unsupported) {
		t.Fatalf("respond reconcile = %v", err)
	}
}
