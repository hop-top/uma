package operations_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func trustSvc(r *enginetest.Rig) operations.Service { return r.C.Operations() }

func mustTrust(t *testing.T, r *enginetest.Rig, key string) model.TrustEntry {
	t.Helper()
	e, err := trustSvc(r).Trust(r.Ctx, r.Ch.Ref, key)
	if err != nil {
		t.Fatalf("Trust(%q): %v", key, err)
	}
	return e
}

// keysInBucket returns n distinct keys that share one bucket.
func keysInBucket(n int) []string {
	var keys []string
	for i := 0; len(keys) < n; i++ {
		k := fmt.Sprintf("key-%d", i)
		if operations.TrustBucketOf(k) == operations.TrustBucketOf("key-0") {
			keys = append(keys, k)
		}
	}
	return keys
}

func TestTrustBatchCountsOncePerKey(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	labels := map[string]string{"kind": "reply"}
	for _, batch := range []string{"b1", "b1", "b2", "b1"} {
		if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", labels, batch); err != nil {
			t.Fatal(err)
		}
	}
	e := mustTrust(t, r, "k")
	if e.Clean != 2 || e.Approvals != 2 || e.Corrections != 0 || e.Labels["kind"] != "reply" {
		t.Fatalf("entry = %+v, want 2 clean approvals from batches b1 and b2", e)
	}
	// The same batch under another key counts for that key.
	if e, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "other", nil, "b1"); err != nil || e.Clean != 1 {
		t.Fatalf("other key = %+v, %v", e, err)
	}
	// Only the last batches are remembered.
	for i := range operations.TrustBatchMemory {
		if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", nil, fmt.Sprintf("n%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	before := mustTrust(t, r, "k").Approvals
	if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", nil, fmt.Sprintf("n%d", operations.TrustBatchMemory-1)); err != nil {
		t.Fatal(err)
	}
	if got := mustTrust(t, r, "k").Approvals; got != before {
		t.Fatalf("a remembered batch counted again: %d -> %d", before, got)
	}
}

func TestTrustConcurrentApprovalsConverge(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	keys := keysInBucket(4)
	const per = 8
	var wg sync.WaitGroup
	errs := make(chan error, len(keys)*per)
	for _, k := range keys {
		for i := range per {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, k, nil, fmt.Sprintf("b%d", i))
				errs <- err
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent approval: %v", err)
		}
	}
	for _, k := range keys {
		if e := mustTrust(t, r, k); e.Clean != per || e.Approvals != per {
			t.Fatalf("%s = %+v, want %d: an increment was lost", k, e, per)
		}
	}
	all, err := s.Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{})
	if err != nil || len(all) != len(keys) {
		t.Fatalf("Trusts = %d entries, %v", len(all), err)
	}
}

func TestTrustCorrectionAndResets(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	for i := range 3 {
		if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "a", nil, fmt.Sprintf("b%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	r.Clock.Advance(time.Minute)
	c, err := s.RecordCorrection(r.Ctx, r.Ch.Ref, "a", nil)
	if err != nil || c.Clean != 0 || c.Corrections != 1 || c.Approvals != 3 || c.LastCorrectionAt == nil || !c.LastCorrectionAt.Equal(r.Clock.Now()) {
		t.Fatalf("correction = %+v, %v", c, err)
	}
	for _, k := range []string{"a", "b"} {
		for i := range 2 {
			if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, k, nil, fmt.Sprintf("x%d", i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := s.ResetTrust(r.Ctx, r.Ch.Ref, "a"); err != nil {
		t.Fatal(err)
	}
	if e := mustTrust(t, r, "a"); e.Clean != 0 || e.Approvals != 5 || e.Corrections != 1 {
		t.Fatalf("reset keeps totals: %+v", e)
	}
	if e := mustTrust(t, r, "b"); e.Clean != 2 {
		t.Fatalf("reset moved another key: %+v", e)
	}
	// A key never counted stays absent.
	if err := s.ResetTrust(r.Ctx, r.Ch.Ref, "never"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trust(r.Ctx, r.Ch.Ref, "never"); err == nil {
		t.Fatal("reset created an entry")
	}

	// A set is all or none.
	if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, "a", nil, "y"); err != nil {
		t.Fatal(err)
	}
	err = s.ResetTrustSet(r.Ctx, r.Ch.Ref, []string{"a", "b", ""})
	requireCode(t, err, model.Validation, "")
	if mustTrust(t, r, "a").Clean != 1 || mustTrust(t, r, "b").Clean != 2 {
		t.Fatal("a refused set reset some of its keys")
	}
	if err := s.ResetTrustSet(r.Ctx, r.Ch.Ref, []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if mustTrust(t, r, "a").Clean != 0 || mustTrust(t, r, "b").Clean != 0 {
		t.Fatal("set reset missed a key")
	}
}

func TestTrustListFiltersByLabel(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	for k, l := range map[string]map[string]string{
		"z": {"kind": "reply", "class": "a"}, "m": {"kind": "reply"}, "c": {"kind": "note"}, "u": nil,
	} {
		if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, k, l, "b"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{Labels: map[string]string{"kind": "reply"}})
	if err != nil || len(got) != 2 || got[0].Key != "m" || got[1].Key != "z" {
		t.Fatalf("Trusts(kind=reply) = %+v, %v", got, err)
	}
	if all, _ := s.Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{}); len(all) != 4 {
		t.Fatalf("Trusts() = %d entries", len(all))
	}
}

func TestTrustBucketFullIsRefusedNotDropped(t *testing.T) {
	r := newRig(t)
	s := trustSvc(r)
	keys := keysInBucket(operations.TrustBucketCapacity + 1)
	for _, k := range keys[:operations.TrustBucketCapacity] {
		if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, k, nil, "b"); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, keys[operations.TrustBucketCapacity], nil, "b")
	requireCode(t, err, model.Validation, "full")
	// An existing key still counts.
	if _, err := s.RecordCleanApproval(r.Ctx, r.Ch.Ref, keys[0], nil, "b2"); err != nil {
		t.Fatalf("existing key in a full bucket: %v", err)
	}
	if all, _ := s.Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{}); len(all) != operations.TrustBucketCapacity {
		t.Fatalf("entries = %d", len(all))
	}
}

func TestTrustStoredInBucketRecords(t *testing.T) {
	r := newRig(t)
	if _, err := trustSvc(r).RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", nil, "b"); err != nil {
		t.Fatal(err)
	}
	name := operations.TrustBucketKey(operations.TrustBucketOf("k"))
	var st store.ProtocolState
	err := r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		var err error
		st, err = rd.Protocol(r.Ctx, r.Ch.Ref, name)
		return err
	})
	if err != nil || st.Value.Name != "uma.trust" || st.Value.Version != 1 || len(name) != len("uma.trust.00") {
		t.Fatalf("bucket %s = %+v, %v", name, st, err)
	}
}

func TestTrustReadAndResetAreAuthorized(t *testing.T) {
	const other model.ActorID = "reviewer-2"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: other, actions: []string{model.ActionTrustRead, model.ActionTrustReset}}
	})
	if _, err := trustSvc(r).RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", nil, "b"); err != nil {
		t.Fatal(err)
	}
	d := pluginClient(t, r, other).Operations()
	_, err := d.Trust(r.Ctx, r.Ch.Ref, "k")
	requireCode(t, err, model.Unauthorized, "")
	_, err = d.Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{})
	requireCode(t, err, model.Unauthorized, "")
	requireCode(t, d.ResetTrust(r.Ctx, r.Ch.Ref, "k"), model.Unauthorized, "")
	requireCode(t, d.ResetTrustSet(r.Ctx, r.Ch.Ref, []string{"k"}), model.Unauthorized, "")
	_, err = d.RecordCleanApproval(r.Ctx, r.Ch.Ref, "k", nil, "b2")
	requireCode(t, err, model.Unauthorized, "")
	_, err = d.RecordCorrection(r.Ctx, r.Ch.Ref, "k", nil)
	requireCode(t, err, model.Unauthorized, "")
	if e := mustTrust(t, r, "k"); e.Clean != 1 || e.Corrections != 0 {
		t.Fatalf("a denied write changed %+v", e)
	}
	// Another tenant's channel is not found, not unauthorized.
	_, err = trustSvc(r).Trust(r.Ctx, model.ChannelRef{Tenant: tenantB, ID: r.Ch.Ref.ID}, "k")
	requireCode(t, err, model.NotFound, "")
}

func TestHeldRefusesInvalidTrustKey(t *testing.T) {
	r := newRig(t)
	s := proposal(r, "p-1", "fp-1")
	s.Hold.TrustKey = string(make([]byte, model.MaxTrustKeyBytes+1))
	_, err := operations.Submit(r.Ctx, pluginClient(t, r, plugin).Client, s)
	requireCode(t, err, model.Validation, "")
}

// Decisions move the counters in their own unit.
func TestDecisionsMoveTrustCounters(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	ops := r.C.Operations()
	hold := func(id model.OperationID) model.Operation {
		return propose(t, r, c, replyProposal(r, id, "hello "+string(id))).Operation
	}

	a := hold("p-1")
	if _, err := ops.Approve(r.Ctx, decide(a)); err != nil {
		t.Fatal(err)
	}
	e := mustTrust(t, r, "triage/reply")
	if e.Clean != 1 || e.Approvals != 1 || e.Labels["kind"] != "reply" {
		t.Fatalf("after approve = %+v", e)
	}
	// A replay of the decision counts nothing.
	if got, err := ops.Approve(r.Ctx, decide(a)); err != nil || !got.Replayed {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 1 || e.Approvals != 1 {
		t.Fatalf("a replayed approval counted: %+v", e)
	}

	// Edit with no named keys corrects the operation's own key.
	b := hold("p-2")
	if _, err := ops.Edit(r.Ctx, model.EditHeld{DecideHeld: decide(b), Args: replyArgs(r, "edited")}); err != nil {
		t.Fatal(err)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 0 || e.Corrections != 1 || e.Approvals != 1 {
		t.Fatalf("after edit = %+v", e)
	}
	if _, err := ops.Edit(r.Ctx, model.EditHeld{DecideHeld: decide(b), Args: replyArgs(r, "edited")}); err != nil {
		t.Fatal(err)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Corrections != 1 {
		t.Fatalf("a replayed edit counted: %+v", e)
	}

	// An edit that names the keys it changed corrects only those.
	for i := range 2 {
		if _, err := trustSvc(r).RecordCleanApproval(r.Ctx, r.Ch.Ref, "triage/reply", nil, fmt.Sprintf("s%d", i)); err != nil {
			t.Fatal(err)
		}
		if _, err := trustSvc(r).RecordCleanApproval(r.Ctx, r.Ch.Ref, "dim/tone", nil, fmt.Sprintf("s%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	cc := hold("p-3")
	if _, err := ops.Edit(r.Ctx, model.EditHeld{DecideHeld: decide(cc), Args: replyArgs(r, "edited 2"), CorrectedKeys: []string{"dim/tone"}}); err != nil {
		t.Fatal(err)
	}
	if e := mustTrust(t, r, "dim/tone"); e.Clean != 0 || e.Corrections != 1 {
		t.Fatalf("named key = %+v", e)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 2 || e.Corrections != 1 {
		t.Fatalf("an edit moved a key it did not name: %+v", e)
	}

	// Reject corrects the operation's key.
	d := hold("p-4")
	if _, err := ops.Reject(r.Ctx, decide(d)); err != nil {
		t.Fatal(err)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 0 || e.Corrections != 2 {
		t.Fatalf("after reject = %+v", e)
	}
}

// A refused decision (expired) writes no counter, and an operation held
// with no trust key counts nowhere.
func TestRefusedDecisionAndUnkeyedHoldCountNothing(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(2 * time.Hour)
	if _, err := r.C.Operations().Approve(r.Ctx, decide(held)); err == nil {
		t.Fatal("expired approval allowed")
	}
	if all, _ := trustSvc(r).Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{}); len(all) != 0 {
		t.Fatalf("an expired proposal counted: %+v", all)
	}
	r = newRig(t)
	c = pluginClient(t, r, plugin)
	s := replyProposal(r, "p-2", "bye")
	s.Hold.TrustKey, s.Hold.TrustLabels, s.Hold.ExpiresAt = "", nil, nil
	un := propose(t, r, c, s).Operation
	if _, err := r.C.Operations().Approve(r.Ctx, decide(un)); err != nil {
		t.Fatal(err)
	}
	if all, _ := trustSvc(r).Trusts(r.Ctx, r.Ch.Ref, model.TrustFilter{}); len(all) != 0 {
		t.Fatalf("an unkeyed hold counted: %+v", all)
	}
}

// Approvals sharing a batch count once per key; no batch is per operation.
func TestApprovalBatchCountsOncePerKey(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	ops := r.C.Operations()
	// One hold per target and kind is open at a time, so operations held
	// together differ in kind.
	kinds := []string{model.ActionMessageReply, model.ActionMessageNote, model.ActionConversationTransition}
	hold := func(id model.OperationID, kind string) model.Operation {
		return propose(t, r, c, withTarget(r, proposal(r, id, "fp-"+string(id)), kind)).Operation
	}
	batched := func(op model.Operation, batch string) model.DecideHeld {
		d := decide(op)
		d.Batch = batch
		return d
	}

	var group []model.Operation
	for i, k := range kinds {
		group = append(group, hold(model.OperationID(fmt.Sprintf("p-%d", i)), k))
	}
	for _, op := range group {
		got, err := ops.Approve(r.Ctx, batched(op, "action-1"))
		if err != nil || got.Replayed {
			t.Fatalf("approve = %+v, %v", got, err)
		}
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 1 || e.Approvals != 1 {
		t.Fatalf("one batch counted more than once: %+v", e)
	}
	// A replay of a batched approval changes nothing.
	if got, err := ops.Approve(r.Ctx, batched(group[0], "action-1")); err != nil || !got.Replayed {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 1 {
		t.Fatalf("replay counted: %+v", e)
	}

	// A different batch and an unbatched approval each count.
	if _, err := ops.Approve(r.Ctx, batched(hold("p-4", kinds[0]), "action-2")); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Approve(r.Ctx, decide(hold("p-5", kinds[0]))); err != nil {
		t.Fatal(err)
	}
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 3 || e.Approvals != 3 {
		t.Fatalf("batches = %+v", e)
	}

	// An invalid batch is refused before anything is written.
	_, err := ops.Approve(r.Ctx, batched(hold("p-6", kinds[0]), string(make([]byte, model.MaxTrustBatchBytes+1))))
	requireCode(t, err, model.Validation, "")
	if e := mustTrust(t, r, "triage/reply"); e.Clean != 3 {
		t.Fatalf("invalid batch counted: %+v", e)
	}
}
