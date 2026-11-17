package conformance

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Held-operation decision cases. A plugin's proposal is held with no work;
// a decision is a compare-and-swap transition of that one ledger entry and
// the trust counters move in its unit. cross is the restart: each step runs
// in a process opened on what the previous one committed.
var heldDecisionCases = []durableCase{
	{"HeldApproveUnderOriginalKey", heldApproveOriginalKey},
	{"HeldEditKeepsLineage", heldEditLineage},
	{"HeldRejectAppliesNothing", heldRejectNothing},
	{"HeldStaleTargetAndRevisionRefused", heldStaleRefusals},
	{"HeldDuplicateGuardAndScope", heldDuplicateGuard},
	{"HeldTrustCountersBatchOnceAndReset", heldTrustBatchAndReset},
	{"HeldTrustConcurrentApprovals", heldTrustConcurrent},
	{"HeldEventsOncePerTransition", heldEventsOnce},
}

const (
	heldPlugin   model.ActorID = "plugin:triage"
	heldReviewer model.ActorID = "reviewer"
)

func (e *env) heldConversationOpen(t *testing.T) model.ConversationRef {
	t.Helper()
	ref := e.conversationRef(t, e.inbox)
	e.mustTx(t, func(tx store.Tx) error { return tx.PutConversation(e.ctx, e.conversation(ref, "c1"), "") })
	return ref
}

// advanceHeldConversation moves the conversation to a new revision.
func (e *env) advanceHeldConversation(t *testing.T, ref model.ConversationRef, to model.Revision) {
	t.Helper()
	e.mustTx(t, func(tx store.Tx) error {
		cur, err := tx.GetConversation(e.ctx, ref)
		if err != nil {
			return err
		}
		prev := cur.Revision
		cur.Revision = to
		return tx.PutConversation(e.ctx, cur, prev)
	})
}

func (e *env) heldClient(t *testing.T, eng *engine.Engine, actor model.ActorID) *engine.Client {
	t.Helper()
	c, err := eng.For(model.Principal{Tenant: tenantA, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func heldReplyArgs(conv model.ConversationRef, text string) model.Extension {
	data, _ := json.Marshal(operations.ReplyIntent{
		Conversation: conv,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
		Audience:     model.Audience{Visibility: model.Public},
	})
	return model.Extension{Name: operations.IntentReply, Version: 1, Data: data}
}

// heldReply is a held reply to conv under trust key trust, expiring in an
// hour.
func (e *env) heldReply(conv model.ConversationRef, id model.OperationID, text, trust string) operations.Submission {
	exp := e.fx.Now().Add(heldTTL).UTC()
	return operations.Submission{
		Kind: model.ActionMessageReply, Target: conv.Ref(), Owner: conv.Ref(),
		Command:     model.Command{Call: model.Call{Correlation: "c"}, Operation: id},
		Fingerprint: []byte("fp-" + string(id)),
		Intent:      heldReplyArgs(conv, text),
		Hold:        &operations.Hold{Reason: "r", TrustKey: trust, TrustLabels: map[string]string{"kind": "reply"}, ExpiresAt: &exp},
	}
}

func (e *env) heldSubmit(t *testing.T, s operations.Submission) model.Acceptance {
	t.Helper()
	a, err := operations.Submit(e.ctx, e.heldClient(t, e.sweepEngine(t, nil), heldPlugin), s)
	if err != nil {
		t.Fatalf("Submit %s: %v", s.Command.Operation, err)
	}
	return a
}

func (e *env) heldOps(t *testing.T) operations.Service {
	t.Helper()
	return operations.New(e.heldClient(t, e.sweepEngine(t, nil), heldReviewer))
}

func heldDecide(op model.Operation) model.DecideHeld {
	return model.DecideHeld{Call: model.Call{Correlation: "d"}, Operation: op.Ref, ExpectedRevision: op.Revision, Note: "n"}
}

func (e *env) heldTrust(t *testing.T, key string) model.TrustEntry {
	t.Helper()
	got, err := e.heldOps(t).Trust(e.ctx, e.channel, key)
	if err != nil {
		t.Fatalf("Trust(%q): %v", key, err)
	}
	return got
}

// heldWork counts the operation work claimed now.
func (e *env) heldWork(t *testing.T) int {
	t.Helper()
	var n int
	e.mustTx(t, func(tx store.Tx) error {
		ws, err := tx.ClaimWork(e.ctx, store.WorkClaim{
			Kinds: []string{operations.WorkKindOperation}, Limit: 100, Owner: "held-probe", Until: e.fx.Now().Add(time.Minute),
		})
		n = len(ws)
		return err
	})
	return n
}

func heldEventsOf(t *testing.T, e *env, kind string) []model.Event {
	t.Helper()
	return e.allEvents(t, model.Filter{Kinds: []string{kind}}, 50)
}

func heldApproveOriginalKey(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.heldConversationOpen(t)
	held := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	if held.Operation.State != model.Held || held.Replayed || held.AlreadyHeld {
		t.Fatalf("proposal = %+v", held)
	}
	if n := e.heldWork(t); n != 0 {
		t.Fatalf("held operation queued %d work items", n)
	}
	cross(t)
	got := e.heldOps(t)
	first, err := got.Approve(e.ctx, heldDecide(held.Operation))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	op := first.Operation
	if op.State != model.Queued || op.Ref != held.Operation.Ref || *op.Key != *held.Operation.Key ||
		op.Held.Decision != model.HeldApproved || op.Held.DecidedBy != heldReviewer || op.Held.DecidedAt == nil ||
		first.Replayed || first.Correction || first.Decision != model.HeldApproved || first.TrustKey != "triage/reply" {
		t.Fatalf("approved = %+v", first)
	}
	if op.Key.Caller != heldPlugin || op.Key.ID != "p-1" {
		t.Fatalf("key = %+v, want the proposer's original key", op.Key)
	}
	cross(t)
	again, err := e.heldOps(t).Approve(e.ctx, heldDecide(held.Operation))
	if err != nil || !again.Replayed || again.Operation.Revision != op.Revision || again.Operation.State != model.Queued {
		t.Fatalf("second Approve = %+v, %v; want the first, replayed", again, err)
	}
	// A replay of the proposal returns the decided operation, not a second one.
	rep := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	if !rep.Replayed || rep.Operation.Ref != op.Ref {
		t.Fatalf("proposal replay = %+v", rep)
	}
	if n := e.heldWork(t); n != 1 {
		t.Fatalf("work items after approve and replays = %d, want exactly 1", n)
	}
	if tr := e.heldTrust(t, "triage/reply"); tr.Clean != 1 || tr.Approvals != 1 || tr.Corrections != 0 {
		t.Fatalf("trust = %+v, want one clean approval counted once", tr)
	}
	// Any other decision of a decided operation conflicts.
	if _, err := e.heldOps(t).Reject(e.ctx, heldDecide(held.Operation)); !model.IsCode(err, model.Conflict) {
		t.Fatalf("Reject after approve = %v, want Conflict", err)
	}
}

func heldEditLineage(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.heldConversationOpen(t)
	// One clean approval first, so the edit's reset of the streak shows.
	clean := e.heldSubmit(t, e.heldReply(e.heldConversationOpen(t), "clean", "ok", "triage/reply"))
	if _, err := e.heldOps(t).Approve(e.ctx, heldDecide(clean.Operation)); err != nil {
		t.Fatal(err)
	}
	held := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	cross(t)
	args := heldReplyArgs(conv, "hello, edited")
	got, err := e.heldOps(t).Edit(e.ctx, model.EditHeld{DecideHeld: heldDecide(held.Operation), Args: args})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	op, h := got.Operation, got.Operation.Held
	if op.Ref != held.Operation.Ref || *op.Key != *held.Operation.Key || op.State != model.Queued ||
		op.Revision == held.Operation.Revision || h.Decision != model.HeldEdited || !got.Correction ||
		string(h.ProposedFingerprint) != "fp-p-1" || h.EditedArgs == nil || string(h.EditedArgs.Data) != string(args.Data) ||
		len(h.Edits) != 1 || h.Edits[0].By != heldReviewer || string(h.Edits[0].Before.Data) != string(held.Operation.Held.Intent.Data) ||
		string(h.Edits[0].After.Data) != string(args.Data) {
		t.Fatalf("edited = %+v", got)
	}
	cross(t)
	// The proposal replays against the proposed fingerprint, not the edited one.
	rep := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	if !rep.Replayed || rep.Operation.Ref != op.Ref {
		t.Fatalf("replay after edit = %+v", rep)
	}
	same, err := e.heldOps(t).Edit(e.ctx, model.EditHeld{DecideHeld: heldDecide(held.Operation), Args: args})
	if err != nil || !same.Replayed || same.Operation.Revision != op.Revision {
		t.Fatalf("repeated edit = %+v, %v; want replayed", same, err)
	}
	if _, err := e.heldOps(t).Edit(e.ctx, model.EditHeld{DecideHeld: heldDecide(held.Operation), Args: heldReplyArgs(conv, "other")}); !model.IsCode(err, model.Conflict) {
		t.Fatalf("edit with other arguments of a decided operation = %v, want Conflict", err)
	}
	if tr := e.heldTrust(t, "triage/reply"); tr.Clean != 0 || tr.Approvals != 1 || tr.Corrections != 1 {
		t.Fatalf("trust = %+v, want the edit counted once as a correction", tr)
	}
}

func heldRejectNothing(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.heldConversationOpen(t)
	held := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	cross(t)
	got, err := e.heldOps(t).Reject(e.ctx, heldDecide(held.Operation))
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if got.Operation.State != model.Rejected || got.Decision != model.HeldRejected || !got.Correction ||
		got.Operation.Held.DecidedBy != heldReviewer || got.Operation.Held.DecisionNote != "n" {
		t.Fatalf("rejected = %+v", got)
	}
	cross(t)
	if n := e.heldWork(t); n != 0 {
		t.Fatalf("a rejected proposal queued %d work items", n)
	}
	again, err := e.heldOps(t).Reject(e.ctx, heldDecide(held.Operation))
	if err != nil || !again.Replayed || again.Operation.Revision != got.Operation.Revision {
		t.Fatalf("second Reject = %+v, %v; want replayed", again, err)
	}
	if _, err := e.heldOps(t).Approve(e.ctx, heldDecide(held.Operation)); !model.IsCode(err, model.Conflict) {
		t.Fatalf("Approve after reject = %v, want Conflict", err)
	}
	if tr := e.heldTrust(t, "triage/reply"); tr.Corrections != 1 || tr.Clean != 0 || tr.Approvals != 0 {
		t.Fatalf("trust = %+v, want one correction", tr)
	}
}

func heldStaleRefusals(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.heldConversationOpen(t)
	held := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	if held.Operation.Held.TargetRevision != "c1" {
		t.Fatalf("target revision at hold = %q", held.Operation.Held.TargetRevision)
	}
	e.advanceHeldConversation(t, conv, "c2")
	cross(t)
	ops := e.heldOps(t)
	stale := func(err error, reason model.ErrorReason, what string) {
		t.Helper()
		if !model.IsCode(err, model.Conflict) || !model.IsReason(err, reason) {
			t.Fatalf("%s = %v, want Conflict with reason %q", what, err, reason)
		}
	}
	_, err := ops.Approve(e.ctx, heldDecide(held.Operation))
	stale(err, model.StaleTarget, "Approve of a stale target")
	_, err = ops.Edit(e.ctx, model.EditHeld{DecideHeld: heldDecide(held.Operation), Args: heldReplyArgs(conv, "x")})
	stale(err, model.StaleTarget, "Edit of a stale target")
	badRev := heldDecide(held.Operation)
	badRev.ExpectedRevision = "not-the-revision"
	for name, call := range map[string]func(model.DecideHeld) error{
		"Approve": func(d model.DecideHeld) error { _, err := ops.Approve(e.ctx, d); return err },
		"Reject":  func(d model.DecideHeld) error { _, err := ops.Reject(e.ctx, d); return err },
		"Edit": func(d model.DecideHeld) error {
			_, err := ops.Edit(e.ctx, model.EditHeld{DecideHeld: d, Args: heldReplyArgs(conv, "x")})
			return err
		},
	} {
		stale(call(badRev), model.StaleRevision, name+" with a stale revision")
		// Force never skips the revision compare.
		badRev.Force = true
		stale(call(badRev), model.StaleRevision, name+" forced with a stale revision")
		badRev.Force = false
	}
	if st := e.operationRecord(t, held.Operation.Ref).Operation.State; st != model.Held {
		t.Fatalf("refused decisions left the operation %s", st)
	}
	if n := e.heldWork(t); n != 0 {
		t.Fatalf("refused decisions queued %d work items", n)
	}
	forced := heldDecide(held.Operation)
	forced.Force = true
	got, err := e.heldOps(t).Approve(e.ctx, forced)
	if err != nil || got.Operation.State != model.Queued {
		t.Fatalf("forced Approve = %+v, %v", got, err)
	}
}

func (e *env) heldCount(t *testing.T) int {
	t.Helper()
	var n int
	e.mustView(t, func(r store.Reader) error {
		p, err := r.Operations(e.ctx, model.ListRequest{
			Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: 100},
		})
		n = len(p.Items)
		return err
	})
	return n
}

func heldDuplicateGuard(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.heldConversationOpen(t)
	first := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	cross(t)
	second := e.heldSubmit(t, e.heldReply(conv, "p-2", "hello again", "triage/reply"))
	if !second.AlreadyHeld || !second.Replayed || second.Operation.Ref != first.Operation.Ref {
		t.Fatalf("second proposal = %+v, want the held operation marked already held", second)
	}
	if n := e.heldCount(t); n != 1 {
		t.Fatalf("held operations = %d, want 1", n)
	}
	// A replay under the proposer's own key is no duplicate.
	if rep := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply")); rep.AlreadyHeld || !rep.Replayed {
		t.Fatalf("replay = %+v, want replayed and not already held", rep)
	}
	// Another target is not a duplicate.
	other := e.heldSubmit(t, e.heldReply(e.heldConversationOpen(t), "p-3", "hi", "triage/reply"))
	if other.AlreadyHeld || other.Operation.Ref == first.Operation.Ref {
		t.Fatalf("other target = %+v", other)
	}
	// Scope narrows the guard: one kind and target, one held operation per
	// scope (a recipient, a label name).
	scoped := e.heldConversationOpen(t)
	propose := func(id model.OperationID, scope string) model.Acceptance {
		s := e.heldReply(scoped, id, "x", "triage/scoped")
		s.HoldScope = scope
		return e.heldSubmit(t, s)
	}
	alice, bob := propose("s-a", "alice"), propose("s-b", "bob")
	if alice.AlreadyHeld || bob.AlreadyHeld || alice.Operation.Ref == bob.Operation.Ref {
		t.Fatalf("scopes alice = %+v, bob = %+v; want two held operations", alice, bob)
	}
	cross(t)
	if dup := propose("s-c", "alice"); !dup.AlreadyHeld || dup.Operation.Ref != alice.Operation.Ref {
		t.Fatalf("same scope = %+v, want alice's held operation", dup)
	}
	if n := e.heldCount(t); n != 4 {
		t.Fatalf("held operations = %d, want 4", n)
	}
	// Once decided, the target takes a new proposal.
	if _, err := e.heldOps(t).Reject(e.ctx, heldDecide(first.Operation)); err != nil {
		t.Fatal(err)
	}
	if again := e.heldSubmit(t, e.heldReply(conv, "p-4", "later", "triage/reply")); again.AlreadyHeld || again.Operation.State != model.Held {
		t.Fatalf("proposal after the decision = %+v", again)
	}
}

func heldTrustBatchAndReset(t *testing.T, e *env, cross func(*testing.T)) {
	labels := map[string]string{"kind": "reply"}
	for _, batch := range []string{"b1", "b1", "b2", "b1"} {
		if _, err := e.heldOps(t).RecordCleanApproval(e.ctx, e.channel, "k", labels, batch); err != nil {
			t.Fatal(err)
		}
	}
	cross(t)
	if _, err := e.heldOps(t).RecordCleanApproval(e.ctx, e.channel, "k", labels, "b2"); err != nil {
		t.Fatal(err)
	}
	if tr := e.heldTrust(t, "k"); tr.Clean != 2 || tr.Approvals != 2 || tr.Labels["kind"] != "reply" {
		t.Fatalf("trust = %+v, want one count per batch, across a restart", tr)
	}
	if _, err := e.heldOps(t).RecordCorrection(e.ctx, e.channel, "k", nil); err != nil {
		t.Fatal(err)
	}
	if tr := e.heldTrust(t, "k"); tr.Clean != 0 || tr.Corrections != 1 || tr.Approvals != 2 || tr.LastCorrectionAt == nil {
		t.Fatalf("after a correction = %+v", tr)
	}
	for _, k := range []string{"k", "j"} {
		for _, b := range []string{"n1", "n2", "n3"} {
			if _, err := e.heldOps(t).RecordCleanApproval(e.ctx, e.channel, k, nil, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	cross(t)
	if err := e.heldOps(t).ResetTrust(e.ctx, e.channel, "k"); err != nil {
		t.Fatal(err)
	}
	if tr := e.heldTrust(t, "k"); tr.Clean != 0 || tr.Approvals != 5 || tr.Corrections != 1 {
		t.Fatalf("reset keeps totals: %+v", tr)
	}
	if tr := e.heldTrust(t, "j"); tr.Clean != 3 {
		t.Fatalf("reset moved another key: %+v", tr)
	}
	// A reset set is all or none: one bad key leaves every counter alone.
	if err := e.heldOps(t).ResetTrustSet(e.ctx, e.channel, []string{"j", string(make([]byte, model.MaxTrustKeyBytes+1))}); err == nil {
		t.Fatal("ResetTrustSet with an invalid key succeeded")
	}
	if tr := e.heldTrust(t, "j"); tr.Clean != 3 {
		t.Fatalf("a refused reset set moved a counter: %+v", tr)
	}
	if err := e.heldOps(t).ResetTrustSet(e.ctx, e.channel, []string{"j", "never"}); err != nil {
		t.Fatal(err)
	}
	if tr := e.heldTrust(t, "j"); tr.Clean != 0 || tr.Approvals != 3 {
		t.Fatalf("after ResetTrustSet = %+v", tr)
	}
	if _, err := e.heldOps(t).Trust(e.ctx, e.channel, "never"); !model.IsCode(err, model.NotFound) {
		t.Fatalf("Trust of a key never counted = %v, want NotFound", err)
	}
}

func heldTrustConcurrent(t *testing.T, e *env, cross func(*testing.T)) {
	const n = 6
	held := make([]model.Operation, n)
	for i := range held {
		key := "triage/shared"
		if i%2 == 1 {
			key = "triage/other"
		}
		held[i] = e.heldSubmit(t, e.heldReply(e.heldConversationOpen(t), model.OperationID(fmt.Sprintf("c-%d", i)), "x", key)).Operation
	}
	cross(t)
	ops := make([]operations.Service, n)
	for i := range ops {
		ops[i] = e.heldOps(t)
	}
	errs := make([]error, n)
	meet := newRendezvous(n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			meet.wait()
			_, errs[i] = ops[i].Approve(e.ctx, heldDecide(held[i]))
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("approval %d: %v", i, err)
		}
	}
	for _, key := range []string{"triage/shared", "triage/other"} {
		if tr := e.heldTrust(t, key); tr.Clean != n/2 || tr.Approvals != n/2 {
			t.Fatalf("%s = %+v, want %d: an increment was lost", key, tr, n/2)
		}
	}
}

func heldEventsOnce(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.heldConversationOpen(t)
	held := e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	e.heldSubmit(t, e.heldReply(conv, "p-1", "hello", "triage/reply"))
	e.heldSubmit(t, e.heldReply(conv, "p-2", "dup", "triage/reply"))
	dying := e.heldSubmit(t, e.heldReply(e.heldConversationOpen(t), "p-3", "late", "triage/late"))
	cross(t)
	var payload model.HeldOperationPayload
	evs := heldEventsOf(t, e, model.EventOperationHeld)
	if len(evs) != 2 {
		t.Fatalf("operation.held events = %d, want 2: not for a replay or a duplicate", len(evs))
	}
	if err := json.Unmarshal(evs[0].Payload.Data, &payload); err != nil || payload.Operation != held.Operation.Ref ||
		payload.Proposer != heldPlugin || payload.Decision != "" || evs[0].Audience.Visibility != model.Internal {
		t.Fatalf("held event = %+v, %+v, %v", evs[0], payload, err)
	}
	if _, err := e.heldOps(t).Approve(e.ctx, heldDecide(held.Operation)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.heldOps(t).Approve(e.ctx, heldDecide(held.Operation)); err != nil {
		t.Fatal(err)
	}
	cross(t)
	evs = heldEventsOf(t, e, model.EventOperationDecided)
	if len(evs) != 1 {
		t.Fatalf("operation.decided events = %d, want 1: not for a replay", len(evs))
	}
	payload = model.HeldOperationPayload{}
	if err := json.Unmarshal(evs[0].Payload.Data, &payload); err != nil || payload.Decision != model.HeldApproved ||
		payload.DecidedBy != heldReviewer || payload.Operation != held.Operation.Ref {
		t.Fatalf("decided event = %+v, %v", payload, err)
	}
	e.advance(t, 2*heldTTL)
	for range 2 {
		if rep := e.sweepOnce(t, e.sweepEngine(t, nil)); rep.Expired > 1 {
			t.Fatalf("sweep = %+v", rep)
		}
		cross(t)
	}
	evs = heldEventsOf(t, e, model.EventOperationExpired)
	if len(evs) != 1 {
		t.Fatalf("operation.expired events = %d, want 1", len(evs))
	}
	payload = model.HeldOperationPayload{}
	if err := json.Unmarshal(evs[0].Payload.Data, &payload); err != nil || payload.Decision != model.HeldExpired || payload.Operation != dying.Operation.Ref {
		t.Fatalf("expired event = %+v, %v", payload, err)
	}
	if tr := e.heldTrust(t, "triage/reply"); tr.Approvals != 1 {
		t.Fatalf("trust = %+v", tr)
	}
	if _, err := e.heldOps(t).Trust(e.ctx, e.channel, "triage/late"); !model.IsCode(err, model.NotFound) {
		t.Fatalf("an expiry moved a counter: %v", err)
	}
}
