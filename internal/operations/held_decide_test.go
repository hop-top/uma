package operations_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// replyProposal holds a reply to the rig's conversation with the stored
// intent a worker executes, so an approval can be followed to the provider.
func replyProposal(r *enginetest.Rig, id model.OperationID, text string) operations.Submission {
	s := proposal(r, id, "fp-"+string(id))
	data, _ := json.Marshal(operations.ReplyIntent{
		Conversation: r.Conv.Ref, Content: textContent(text), Audience: operations.ReplyAudience(),
	})
	s.Intent = model.Extension{Name: operations.IntentReply, Version: 1, Data: data}
	return s
}

func textContent(text string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
}

func replyArgs(r *enginetest.Rig, text string) model.Extension {
	data, _ := json.Marshal(operations.ReplyIntent{
		Conversation: r.Conv.Ref, Content: textContent(text), Audience: operations.ReplyAudience(),
	})
	return model.Extension{Name: operations.IntentReply, Version: 1, Data: data}
}

func decide(op model.Operation) model.DecideHeld {
	return model.DecideHeld{Call: model.Call{Correlation: "d"}, Operation: op.Ref, ExpectedRevision: op.Revision, Note: "n"}
}

func sentTexts(r *enginetest.Rig) []string {
	var out []string
	for _, s := range r.F.ProviderSends() {
		out = append(out, s.Content.Parts[0].Text)
	}
	return out
}

func requireHeld(t *testing.T, r *enginetest.Rig, ref model.OperationRef) {
	t.Helper()
	if got := r.Op(ref); got.State != model.Held || got.Held.Decision != "" {
		t.Fatalf("operation = %s %+v, want still held and undecided", got.State, got.Held)
	}
}

func TestApproveAppliesUnderOriginalKey(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation

	got, err := r.C.Operations().Approve(r.Ctx, decide(held))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	op := got.Operation
	if op.Ref != held.Ref || op.State != model.Queued || got.Replayed || op.Revision == held.Revision ||
		*op.Key != *held.Key || op.Held.Decision != model.HeldApproved || op.Held.DecidedBy != enginetest.Actor ||
		op.Held.DecidedAt == nil || op.Held.DecisionNote != "n" || op.Held.Proposer != plugin ||
		string(op.Held.ProposedFingerprint) != "fp-p-1" {
		t.Fatalf("approved = %+v replayed=%v", op, got.Replayed)
	}
	if got.Decision != model.HeldApproved || got.Correction || got.TrustKey != "triage/reply" || got.TrustLabels["kind"] != "reply" {
		t.Fatalf("trust hook = %+v", got)
	}
	// The proposer finds the same operation under its own key.
	found, err := c.Operations().Lookup(r.Ctx, model.OperationLookup{
		Call: model.Call{Correlation: "l"}, Kind: model.ActionMessageReply, Target: r.Conv.Ref.Ref(), ID: "p-1",
	})
	if err != nil || found.Ref != held.Ref || found.State != model.Queued {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("work items = %d, want 1", n)
	}
	if sent := sentTexts(r); len(sent) != 1 || sent[0] != "hello" {
		t.Fatalf("provider sends = %q", sent)
	}
}

func TestDoubleApproveReturnsSameOperationReplayed(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	first, err := r.C.Operations().Approve(r.Ctx, decide(held))
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.C.Operations().Approve(r.Ctx, decide(held))
	if err != nil {
		t.Fatalf("second Approve: %v", err)
	}
	if !again.Replayed || again.Operation.Ref != first.Operation.Ref || again.Operation.Revision != first.Operation.Revision ||
		again.Decision != model.HeldApproved {
		t.Fatalf("second approve = %+v, want the first, replayed", again)
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("work items = %d, want exactly 1", n)
	}
	// A replay of the proposal itself returns the operation, not a second one.
	rep := propose(t, r, c, replyProposal(r, "p-1", "hello"))
	if !rep.Replayed || rep.Operation.Ref != held.Ref {
		t.Fatalf("proposal replay = %+v", rep)
	}
	// A different decision of a decided operation is a conflict.
	_, err = r.C.Operations().Reject(r.Ctx, decide(held))
	requireCode(t, err, model.Conflict, "already decided")
}

func TestRevisionCompareAndSwapOnEveryDecision(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	stale := decide(held)
	stale.ExpectedRevision = "not-the-revision"
	ops := r.C.Operations()
	_, err := ops.Approve(r.Ctx, stale)
	requireCode(t, err, model.Conflict, "")
	requireReason(t, err, model.StaleRevision)
	_, err = ops.Reject(r.Ctx, stale)
	requireCode(t, err, model.Conflict, "")
	requireReason(t, err, model.StaleRevision)
	_, err = ops.Edit(r.Ctx, model.EditHeld{DecideHeld: stale, Args: replyArgs(r, "x")})
	requireCode(t, err, model.Conflict, "")
	requireReason(t, err, model.StaleRevision)
	requireHeld(t, r, held.Ref)

	none := decide(held)
	none.ExpectedRevision = ""
	_, err = ops.Approve(r.Ctx, none)
	requireCode(t, err, model.Validation, "")
}

func TestDecisionRefusedWhenTargetAdvancedUnlessForced(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	if held.Held.TargetRevision != r.Conv.Revision {
		t.Fatalf("target revision at hold = %q, want the conversation's %q", held.Held.TargetRevision, r.Conv.Revision)
	}
	advanceConversation(t, r)
	ops := r.C.Operations()
	_, err := ops.Approve(r.Ctx, decide(held))
	requireCode(t, err, model.Conflict, "")
	requireReason(t, err, model.StaleTarget)
	_, err = ops.Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: replyArgs(r, "x")})
	requireCode(t, err, model.Conflict, "")
	requireReason(t, err, model.StaleTarget)
	requireHeld(t, r, held.Ref)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("a refused decision queued %d work items", n)
	}

	forced := decide(held)
	forced.Force = true
	got, err := ops.Approve(r.Ctx, forced)
	if err != nil || got.Operation.State != model.Queued {
		t.Fatalf("forced Approve = %+v, %v", got, err)
	}
}

func advanceConversation(t *testing.T, r *enginetest.Rig) {
	t.Helper()
	err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		conv, err := tx.GetConversation(r.Ctx, r.Conv.Ref)
		if err != nil {
			return err
		}
		prev := conv.Revision
		if conv.Revision, err = operations.NewRevision(r.Ctx, r.E); err != nil {
			return err
		}
		return tx.PutConversation(r.Ctx, conv, prev)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEditKeepsLineageAndCountsAsCorrection(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	held := propose(t, r, c, replyProposal(r, "p-1", "hello")).Operation
	args := replyArgs(r, "hello, edited")

	got, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: args})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	op, h := got.Operation, got.Operation.Held
	if op.Ref != held.Ref || *op.Key != *held.Key || op.State != model.Queued || op.Revision == held.Revision ||
		h.Decision != model.HeldEdited || h.DecidedBy != enginetest.Actor || string(h.ProposedFingerprint) != "fp-p-1" ||
		h.EditedArgs == nil || string(h.EditedArgs.Data) != string(args.Data) || len(h.Edits) != 1 {
		t.Fatalf("edited = %+v %+v", op, h)
	}
	e := h.Edits[0]
	if e.By != enginetest.Actor || e.At.IsZero() || string(e.After.Data) != string(args.Data) ||
		!strings.Contains(string(e.Before.Data), `"hello"`) || strings.Contains(string(e.Before.Data), "edited") {
		t.Fatalf("edit record = %+v", e)
	}
	if !got.Correction || got.Decision != model.HeldEdited || got.TrustKey != "triage/reply" {
		t.Fatalf("trust hook = %+v", got)
	}

	// A replay of the original proposal returns the operation: it compares
	// against the proposed fingerprint, never the edited arguments.
	rep, err := operations.Submit(r.Ctx, c.Client, replyProposal(r, "p-1", "hello"))
	if err != nil || !rep.Replayed || rep.Operation.Ref != held.Ref || rep.Operation.State != model.Queued {
		t.Fatalf("proposal replay after edit = %+v, %v", rep, err)
	}
	// The same edit again is the first, replayed; another is a conflict.
	again, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: args})
	if err != nil || !again.Replayed || again.Operation.Revision != op.Revision || len(again.Operation.Held.Edits) != 1 {
		t.Fatalf("repeated edit = %+v, %v", again, err)
	}
	_, err = r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: replyArgs(r, "other")})
	requireCode(t, err, model.Conflict, "already decided")

	// The worker applies the edited arguments, not the proposed ones.
	if n := r.Step("w"); n != 1 {
		t.Fatalf("work items = %d, want 1", n)
	}
	if sent := sentTexts(r); len(sent) != 1 || sent[0] != "hello, edited" {
		t.Fatalf("provider sends = %q, want the edited text", sent)
	}
}

func TestEditArgsKeepTheIntentNameAndVersion(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	bad := model.Extension{Name: "other.intent", Version: 1, Data: []byte(`{}`)}
	_, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: bad})
	requireCode(t, err, model.Validation, "args")
	_, err = r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: model.Extension{Name: operations.IntentReply, Version: 1}})
	requireCode(t, err, model.Validation, "args.data")
	requireHeld(t, r, held.Ref)
}

func TestRejectAppliesNothing(t *testing.T) {
	const intent = "test.apply.reject"
	var applied int
	operations.RegisterApplier(intent, func(context.Context, *engine.Engine, store.Tx, model.Operation, store.OperationKey, model.Extension, model.Revision) error {
		applied++
		return nil
	})
	t.Cleanup(func() { operations.UnregisterApplier(intent) })
	r := newRig(t)
	s := replyProposal(r, "p-1", "hello")
	s.Intent.Name = intent
	held := propose(t, r, pluginClient(t, r, plugin), s).Operation

	d := decide(held)
	d.Note = "wrong tone"
	got, err := r.C.Operations().Reject(r.Ctx, d)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	op, h := got.Operation, got.Operation.Held
	if op.State != model.Rejected || op.Failure != nil || h.Decision != model.HeldRejected || h.DecisionNote != "wrong tone" ||
		h.DecidedBy != enginetest.Actor || !got.Correction || got.Decision != model.HeldRejected || got.TrustKey != "triage/reply" {
		t.Fatalf("rejected = %+v %+v correction=%v", op, h, got.Correction)
	}
	if applied != 0 {
		t.Fatal("a rejection ran the applier")
	}
	if n := r.Step("w"); n != 0 || len(r.F.ProviderSends()) != 0 || len(r.Evidence(op.Ref)) != 0 {
		t.Fatalf("a rejection queued work or reached the provider (work %d)", n)
	}
	again, err := r.C.Operations().Reject(r.Ctx, d)
	if err != nil || !again.Replayed || again.Operation.Revision != op.Revision {
		t.Fatalf("second reject = %+v, %v", again, err)
	}
	_, err = r.C.Operations().Approve(r.Ctx, d)
	requireCode(t, err, model.Conflict, "already decided")
	if !operations.Settled(model.Rejected) {
		t.Fatal("rejected must be final")
	}
}

// The references an operation names when held are the ones its approval
// writes under: the applier gets them, and the original proposer's key.
func TestApprovalAppliesUnderResultIDsNamedAtHold(t *testing.T) {
	const intent = "test.apply.ids"
	var gotOp model.Operation
	var gotKey store.OperationKey
	var gotIntent model.Extension
	operations.RegisterApplier(intent, func(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, in model.Extension, rev model.Revision) error {
		gotOp, gotKey, gotIntent = op, key, in
		now := e.Clock().Now().UTC()
		return tx.PutMessage(ctx, model.Message{
			Ref: *op.ResultMessage, Content: textContent("applied"), Audience: model.Audience{Visibility: model.Public},
			OccurredAt: now, RecordedAt: now, Revision: rev, Origin: &model.Origin{Actor: key.Caller, Operation: op.Ref},
		}, "")
	})
	operations.RegisterValidator(intent, func(context.Context, store.Reader, store.OperationKey, model.Extension, model.Extension) error {
		return nil
	})
	t.Cleanup(func() { operations.UnregisterApplier(intent); operations.UnregisterValidator(intent) })
	r := newRig(t)
	msg := model.MessageRef{Conversation: r.Conv.Ref, ID: "00000005-0000-7000-8000-0000000000aa"}
	res := msg.Ref()
	s := replyProposal(r, "p-1", "hello")
	s.Intent.Name, s.Result, s.ResultMessage = intent, &res, &msg
	held := propose(t, r, pluginClient(t, r, plugin), s).Operation
	if _, err := r.C.Messages().Get(r.Ctx, model.Lookup[model.MessageRef]{Call: model.Call{Correlation: "g"}, Ref: msg}); err == nil {
		t.Fatal("the named message exists before approval")
	}

	args := model.Extension{Name: intent, Version: 1, Data: []byte(`{"v":2}`)}
	got, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: args})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if gotOp.ResultMessage == nil || *gotOp.ResultMessage != msg || gotOp.Ref != held.Ref || gotKey.Caller != plugin ||
		string(gotIntent.Data) != `{"v":2}` {
		t.Fatalf("applier saw op=%+v key=%+v intent=%s", gotOp, gotKey, gotIntent.Data)
	}
	if got.Operation.ResultMessage == nil || *got.Operation.ResultMessage != msg || got.Operation.Result == nil || *got.Operation.Result != res {
		t.Fatalf("result after approval = %+v", got.Operation)
	}
	if m := r.Message(msg); m.Origin == nil || m.Origin.Operation != held.Ref {
		t.Fatalf("applied message = %+v", m)
	}
}

func TestUnauthorizedDeciderDecidesNothing(t *testing.T) {
	const other model.ActorID = "reviewer-2"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: other, actions: []string{
			model.ActionOperationApprove, model.ActionOperationReject, model.ActionOperationEdit,
		}}
	})
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	denied := pluginClient(t, r, other)
	ops := denied.Operations()
	_, err := ops.Approve(r.Ctx, decide(held))
	requireCode(t, err, model.Unauthorized, "")
	_, err = ops.Reject(r.Ctx, decide(held))
	requireCode(t, err, model.Unauthorized, "")
	_, err = ops.Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: replyArgs(r, "x")})
	requireCode(t, err, model.Unauthorized, "")
	requireHeld(t, r, held.Ref)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("an unauthorized decision queued %d work items", n)
	}
	// Another principal is not blocked by that denial.
	if _, err := r.C.Operations().Approve(r.Ctx, decide(held)); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

// A decider that may not read the target learns nothing: not found, never
// unauthorized, as for any read of the held operation.
func TestDeciderWithoutTargetReadSeesNotFound(t *testing.T) {
	const blindActor model.ActorID = "blind"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: blindActor, actions: []string{model.ActionConversationRead}}
	})
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	blind := pluginClient(t, r, blindActor)
	_, err := blind.Operations().Approve(r.Ctx, decide(held))
	requireCode(t, err, model.NotFound, "resource not found")
	_, unknown := blind.Operations().Approve(r.Ctx, model.DecideHeld{
		Call: model.Call{Correlation: "d"}, Operation: model.OperationRef{Tenant: tenantA, ID: inboxTwo}, ExpectedRevision: "r",
	})
	if err.Error() != unknown.Error() {
		t.Fatalf("error %q differs from the unknown operation's %q", err, unknown)
	}
	requireHeld(t, r, held.Ref)
}

func TestDecisionOfOperationThatIsNotHeldIsNotFound(t *testing.T) {
	r := newRig(t)
	s := replyProposal(r, "p-1", "hello")
	s.Hold = nil
	queued := propose(t, r, r.C, s).Operation
	_, err := r.C.Operations().Approve(r.Ctx, decide(queued))
	requireCode(t, err, model.NotFound, "")
	if got := r.Op(queued.Ref); got.State != model.Queued {
		t.Fatalf("state = %s", got.State)
	}
}

func TestExpiredProposalCannotBeApprovedOrEdited(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	r.Clock.Advance(2 * time.Hour)
	_, err := r.C.Operations().Approve(r.Ctx, decide(held))
	requireCode(t, err, model.Conflict, "expired")
	_, err = r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: replyArgs(r, "x")})
	requireCode(t, err, model.Conflict, "expired")
	requireHeld(t, r, held.Ref)
	if got, err := r.C.Operations().Reject(r.Ctx, decide(held)); err != nil || got.Operation.State != model.Rejected {
		t.Fatalf("Reject past the deadline = %+v, %v", got, err)
	}
}

func editRefused(t *testing.T, r *enginetest.Rig, held model.Operation, args model.Extension, code model.ErrorCode, msg string) {
	t.Helper()
	_, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: args})
	requireCode(t, err, code, msg)
	requireHeld(t, r, held.Ref)
	if n := r.Step("w"); n != 0 || len(r.F.ProviderSends()) != 0 {
		t.Fatalf("a refused edit queued %d work items", n)
	}
}

func replyData(t *testing.T, in operations.ReplyIntent) model.Extension {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return model.Extension{Name: operations.IntentReply, Version: 1, Data: data}
}

// Edited arguments cannot redirect an approved write: every target-bearing
// field must stay the claimed target.
func TestEditCannotRetarget(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	base := operations.ReplyIntent{Conversation: r.Conv.Ref, Content: textContent("x"), Audience: operations.ReplyAudience()}

	other := base
	other.Conversation.ID = "00000005-0000-7000-8000-0000000000bb"
	editRefused(t, r, held, replyData(t, other), model.Validation, "args")

	elsewhere := base
	elsewhere.Conversation.Inbox.ID = "00000005-0000-7000-8000-0000000000cc"
	editRefused(t, r, held, replyData(t, elsewhere), model.Validation, "args")

	parent := base
	parent.InReplyTo = &model.MessageRef{Conversation: other.Conversation, ID: "00000005-0000-7000-8000-0000000000dd"}
	editRefused(t, r, held, replyData(t, parent), model.Validation, "args")

	wide := base
	wide.Audience = model.Audience{Visibility: model.Internal}
	editRefused(t, r, held, replyData(t, wide), model.Validation, "args")

	empty := base
	empty.Content = model.Content{}
	editRefused(t, r, held, replyData(t, empty), model.Validation, "args")

	editRefused(t, r, held, model.Extension{Name: operations.IntentReply, Version: 1, Data: []byte(`not json`)}, model.Validation, "args")
}

// An intent that registered no validator cannot be edited: its arguments
// cannot be checked against the claimed target.
func TestEditRefusedWithoutValidator(t *testing.T) {
	const intent = "test.edit.novalidator"
	operations.RegisterApplier(intent, func(context.Context, *engine.Engine, store.Tx, model.Operation, store.OperationKey, model.Extension, model.Revision) error {
		return nil
	})
	t.Cleanup(func() { operations.UnregisterApplier(intent) })
	r := newRig(t)
	s := replyProposal(r, "p-1", "hello")
	s.Intent.Name = intent
	held := propose(t, r, pluginClient(t, r, plugin), s).Operation
	editRefused(t, r, held, model.Extension{Name: intent, Version: 1, Data: []byte(`{"v":2}`)}, model.Validation, "args")
	// Approval as proposed needs no validator.
	if _, err := r.C.Operations().Approve(r.Ctx, decide(held)); err != nil {
		t.Fatalf("Approve: %v", err)
	}
}

// Approve and edit apply the claimed action to the claimed target, so the
// decider needs that action too; reject applies nothing and needs only
// operation.reject.
func TestDeciderNeedsTheUnderlyingAction(t *testing.T) {
	const other model.ActorID = "reviewer-2"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: other, actions: []string{model.ActionMessageReply}}
	})
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	denied := pluginClient(t, r, other)
	_, err := denied.Operations().Approve(r.Ctx, decide(held))
	requireCode(t, err, model.Unauthorized, "")
	_, err = denied.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(held), Args: replyArgs(r, "x")})
	requireCode(t, err, model.Unauthorized, "")
	requireHeld(t, r, held.Ref)
	if n := r.Step("w"); n != 0 {
		t.Fatalf("an unauthorized decision queued %d work items", n)
	}
	if got, err := denied.Operations().Reject(r.Ctx, decide(held)); err != nil || got.Operation.State != model.Rejected {
		t.Fatalf("Reject = %+v, %v", got, err)
	}
}

// requireReason fails unless err carries exactly want, and no other reason.
func requireReason(t *testing.T, err error, want model.ErrorReason) {
	t.Helper()
	if !model.IsReason(err, want) {
		t.Fatalf("err %v: reason = %q, want %q", err, model.ReasonOf(err), want)
	}
	other := model.StaleTarget
	if want == other {
		other = model.StaleRevision
	}
	if model.IsReason(err, other) {
		t.Fatalf("err %v also matches %q", err, other)
	}
}
