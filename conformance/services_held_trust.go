package conformance

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// Approval batches and trust import through the public services: the
// counters a host reads back are the same on every store.

func heldSvcTrust(t *testing.T, env *mutationEnv, key string) model.TrustEntry {
	t.Helper()
	e, err := env.client.Operations().Trust(env.ctx, model.TrustLookup{Call: model.Call{Correlation: "trust"}, Channel: env.provRef.Channel, Key: key})
	if err != nil {
		t.Fatalf("Trust(%s): %v", key, err)
	}
	return e
}

func heldSvcApproveBatch(t *testing.T, env *mutationEnv, op model.Operation, batch string) model.Decided {
	t.Helper()
	d := heldSvcDecide(op)
	d.Batch = batch
	got, err := env.client.Operations().Approve(env.ctx, d)
	if err != nil {
		t.Fatalf("Approve(batch %q): %v", batch, err)
	}
	return got
}

func heldSvcApprovalBatch(t *testing.T, env *mutationEnv) {
	const key = "triage/batch"
	// One batch counts once per key, however many approvals share it.
	var first model.Operation
	for i, text := range []string{"one", "two", "three"} {
		op := env.heldReply(text, key)
		if i == 0 {
			first = op
		}
		if got := heldSvcApproveBatch(t, env, op, "action-1"); got.Replayed {
			t.Fatalf("approval %d replayed", i)
		}
	}
	if e := heldSvcTrust(t, env, key); e.Clean != 1 || e.Approvals != 1 {
		t.Fatalf("one batch of three approvals = %+v, want one", e)
	}
	// A replay counts nothing.
	if got := heldSvcApproveBatch(t, env, first, "action-1"); !got.Replayed {
		t.Fatalf("second approval = %+v, want a replay", got)
	}
	if e := heldSvcTrust(t, env, key); e.Clean != 1 || e.Approvals != 1 {
		t.Fatalf("a replay counted: %+v", e)
	}
	// Another batch and an unbatched approval each count.
	heldSvcApproveBatch(t, env, env.heldReply("four", key), "action-2")
	heldSvcApproveBatch(t, env, env.heldReply("five", key), "")
	if e := heldSvcTrust(t, env, key); e.Clean != 3 || e.Approvals != 3 {
		t.Fatalf("distinct and empty batches = %+v, want three", e)
	}
	// An invalid batch is refused and leaves the proposal held.
	held := env.heldReply("six", key)
	bad := heldSvcDecide(held)
	bad.Batch = strings.Repeat("b", model.MaxTrustBatchBytes+1)
	if _, err := env.client.Operations().Approve(env.ctx, bad); !model.IsCode(err, model.Validation) {
		t.Fatalf("Approve with an oversized batch = %v, want Validation", err)
	}
	if env.get(held.Ref).State != model.Held {
		t.Fatal("a refused batch changed the operation")
	}
	if e := heldSvcTrust(t, env, key); e.Clean != 3 || e.Approvals != 3 {
		t.Fatalf("a refused batch counted: %+v", e)
	}
}

func heldSvcImportTrust(t *testing.T, env *mutationEnv) {
	const key = "triage/import"
	ops := env.client.Operations()
	past := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	future := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	imp := func(clean, approvals, corrections int64, last *time.Time) model.ImportTrust {
		return model.ImportTrust{
			Call: model.Call{Correlation: "import"}, Channel: env.provRef.Channel, Key: key,
			Labels: map[string]string{"plugin": "triage"}, Clean: clean, Approvals: approvals, Corrections: corrections, LastCorrectionAt: last,
		}
	}
	do := func(in model.ImportTrust) model.TrustEntry {
		t.Helper()
		e, err := ops.ImportTrust(env.ctx, in)
		if err != nil {
			t.Fatalf("ImportTrust: %v", err)
		}
		return e
	}
	want := func(what string, e model.TrustEntry, clean, approvals, corrections int64) {
		t.Helper()
		if e.Clean != clean || e.Approvals != approvals || e.Corrections != corrections {
			t.Fatalf("%s = %+v, want %d/%d/%d", what, e, clean, approvals, corrections)
		}
		if got := heldSvcTrust(t, env, key); got.Clean != clean || got.Approvals != approvals || got.Corrections != corrections {
			t.Fatalf("%s read back = %+v", what, got)
		}
	}

	// Into an empty key.
	e := do(imp(5, 9, 2, &past))
	want("import into empty", e, 5, 9, 2)
	if e.LastCorrectionAt == nil || !e.LastCorrectionAt.Equal(past) || e.Labels["plugin"] != "triage" {
		t.Fatalf("import into empty = %+v", e)
	}
	// Idempotent.
	again := do(imp(5, 9, 2, &past))
	want("re-import", again, 5, 9, 2)
	if !again.UpdatedAt.Equal(e.UpdatedAt) {
		t.Fatalf("a re-import wrote: %v then %v", e.UpdatedAt, again.UpdatedAt)
	}
	// Never lowers what live decisions accumulated.
	heldSvcApproveBatch(t, env, env.heldReply("one", key), "")
	want("after a live approval", heldSvcTrust(t, env, key), 6, 10, 2)
	want("lower import", do(imp(5, 9, 2, &past)), 6, 10, 2)
	// A live correction after the imported history ends its streak: the
	// totals still rise, the consecutive count does not.
	rejected := env.heldReply("two", key)
	if _, err := ops.Reject(env.ctx, heldSvcDecide(rejected)); err != nil {
		t.Fatal(err)
	}
	want("after a live correction", heldSvcTrust(t, env, key), 0, 10, 3)
	want("stale import", do(imp(50, 60, 2, &past)), 0, 60, 3)
	// An import whose history ends later than the stored correction applies.
	got := do(imp(70, 80, 4, &future))
	want("later import", got, 70, 80, 4)
	if got.LastCorrectionAt == nil || !got.LastCorrectionAt.Equal(future) {
		t.Fatalf("LastCorrectionAt = %v, want the later", got.LastCorrectionAt)
	}

	// Authorization: trust.write, not trust.read or trust.reset.
	env.policy.deny(heldSvcBlind, model.ActionTrustWrite)
	if _, err := env.as(heldSvcBlind).Operations().ImportTrust(env.ctx, imp(99, 99, 0, nil)); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("ImportTrust without trust.write = %v, want Unauthorized", err)
	}
	want("after a refused import", heldSvcTrust(t, env, key), 70, 80, 4)
}
