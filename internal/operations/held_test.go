package operations_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

const plugin model.ActorID = "plugin:triage"

func pluginClient(t *testing.T, r *enginetest.Rig, actor model.ActorID) enginetest.Client {
	t.Helper()
	c, err := enginetest.For(r.E, model.Principal{Tenant: tenantA, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func proposal(r *enginetest.Rig, id model.OperationID, fp string) operations.Submission {
	conv := r.Conv.Ref.Ref()
	exp := r.Clock.Now().Add(time.Hour).UTC()
	return operations.Submission{
		Kind: model.ActionMessageReply, Target: conv, Owner: conv,
		Command:     model.Command{Call: model.Call{Correlation: "c"}, Operation: id},
		Fingerprint: []byte(fp),
		Intent:      model.Extension{Name: "test.intent", Version: 1, Data: []byte(`{"text":"hi"}`)},
		Hold: &operations.Hold{
			Reason:      "first contact",
			Preview:     &model.Extension{Name: "test.preview", Version: 1, Data: []byte(`{"would":"send hi"}`)},
			OnBehalfOf:  "user-9",
			TrustKey:    "triage/reply",
			TrustLabels: map[string]string{"kind": "reply"},
			ExpiresAt:   &exp,
		},
	}
}

func propose(t *testing.T, r *enginetest.Rig, c enginetest.Client, s operations.Submission) model.Acceptance {
	t.Helper()
	a, err := operations.Submit(r.Ctx, c.Client, s)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return a
}

func listHeld(t *testing.T, r *enginetest.Rig, c enginetest.Client) []model.Operation {
	t.Helper()
	p, err := services.ListOperations(r.Ctx, c.Client, model.ListRequest{Call: model.Call{Correlation: "list"}, Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: 50}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return p.Items
}

func TestHeldSubmitStoresProposalWithoutWork(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	a := propose(t, r, c, proposal(r, "p-1", "fp-1"))
	op := a.Operation
	if op.State != model.Held || a.Replayed || a.AlreadyHeld {
		t.Fatalf("acceptance = %+v", a)
	}
	h := op.Held
	if h == nil || h.Proposer != plugin || h.OnBehalfOf != "user-9" || h.Reason != "first contact" ||
		h.TrustKey != "triage/reply" || h.TrustLabels["kind"] != "reply" || h.Preview == nil ||
		string(h.Preview.Data) != `{"would":"send hi"}` || h.ExpiresAt == nil || string(h.ProposedFingerprint) != "fp-1" ||
		h.Decision != "" || h.DecidedBy != "" || h.DecidedAt != nil {
		t.Fatalf("held block = %+v", h)
	}
	if n := r.Step("w"); n != 0 {
		t.Fatalf("a held operation ran %d work items", n)
	}
	if len(r.Evidence(op.Ref)) != 0 {
		t.Fatal("held operation has attempt evidence")
	}
	got, err := c.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: op.Ref})
	if err != nil || got.State != model.Held || got.Held == nil || got.Held.Proposer != plugin {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	found, err := c.Operations().Lookup(r.Ctx, model.OperationLookup{
		Call: model.Call{Correlation: "l"}, Kind: model.ActionMessageReply, Target: r.Conv.Ref.Ref(), ID: "p-1",
	})
	if err != nil || found.Ref != op.Ref || found.State != model.Held {
		t.Fatalf("Lookup = %+v, %v", found, err)
	}
	if l := listHeld(t, r, c); len(l) != 1 || l[0].Ref != op.Ref || l[0].Held == nil {
		t.Fatalf("List held = %+v", l)
	}
	if l := listHeld(t, r, r.C); len(l) != 1 {
		t.Fatalf("person's List held = %d, want the held operation visible to a reader of its target", len(l))
	}
}

func TestHeldReplayReturnsHeldOperation(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	first := propose(t, r, c, proposal(r, "p-1", "fp-1"))
	again := propose(t, r, c, proposal(r, "p-1", "fp-1"))
	if !again.Replayed || again.AlreadyHeld || again.Operation.Ref != first.Operation.Ref || again.Operation.State != model.Held {
		t.Fatalf("replay = %+v", again)
	}
	if l := listHeld(t, r, c); len(l) != 1 {
		t.Fatalf("held operations = %d, want 1", len(l))
	}
	_, err := operations.Submit(r.Ctx, c.Client, proposal(r, "p-1", "fp-other"))
	requireCode(t, err, model.Conflict, "")
}

func TestHeldDuplicateGuard(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	first := propose(t, r, c, proposal(r, "p-1", "fp-1"))
	second := propose(t, r, c, proposal(r, "p-2", "fp-2"))
	if !second.AlreadyHeld || !second.Replayed || second.Operation.Ref != first.Operation.Ref {
		t.Fatalf("second proposal = %+v, want the held operation marked already held", second)
	}
	if l := listHeld(t, r, c); len(l) != 1 {
		t.Fatalf("held operations = %d, want 1", len(l))
	}
	// Another plugin proposing the same kind on the same target is guarded too.
	other := propose(t, r, pluginClient(t, r, "plugin:other"), proposal(r, "p-1", "fp-3"))
	if !other.AlreadyHeld || other.Operation.Ref != first.Operation.Ref {
		t.Fatalf("other plugin = %+v", other)
	}
	// A different kind or target is not a duplicate.
	note := proposal(r, "p-3", "fp-4")
	note.Kind = model.ActionMessageNote
	if a := propose(t, r, c, note); a.AlreadyHeld || a.Operation.Ref == first.Operation.Ref {
		t.Fatalf("different kind = %+v", a)
	}
	inbox := proposal(r, "p-4", "fp-5")
	inbox.Target, inbox.Owner = r.Conv.Ref.Inbox.Ref(), r.Conv.Ref.Inbox.Ref()
	if a := propose(t, r, c, inbox); a.AlreadyHeld {
		t.Fatalf("different target = %+v", a)
	}
}

func TestHeldRefusedForPersonActor(t *testing.T) {
	r := newRig(t)
	_, err := operations.Submit(r.Ctx, r.C.Client, proposal(r, "p-1", "fp-1"))
	requireCode(t, err, model.Validation, "")
	if l := listHeld(t, r, r.C); len(l) != 0 {
		t.Fatalf("a person's operation was held: %+v", l)
	}
	if _, err := r.C.Operations().Lookup(r.Ctx, model.OperationLookup{
		Call: model.Call{Correlation: "l"}, Kind: model.ActionMessageReply, Target: r.Conv.Ref.Ref(), ID: "p-1",
	}); err == nil {
		t.Fatal("refused proposal claimed its key")
	}
}

func TestSubmitWithoutHoldQueues(t *testing.T) {
	r := newRig(t)
	s := proposal(r, "p-1", "fp-1")
	s.Hold = nil
	a := propose(t, r, r.C, s)
	if a.Operation.State != model.Queued || a.Operation.Held != nil || a.AlreadyHeld {
		t.Fatalf("acceptance = %+v", a)
	}
}

// denyReads allows everything except the listed read actions for one actor.
type denyReads struct {
	actor   model.ActorID
	actions []string
}

func (p denyReads) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	if a.Principal.Actor == p.actor && slices.Contains(p.actions, a.Action) {
		return policy.Decision{Allow: false}, nil
	}
	return policy.Decision{Allow: true}, nil
}

// A held operation is the draft content of its target: a principal that may
// read the operation but not its target gets not found, never a stub.
func TestHeldVisibilityFollowsTargetRead(t *testing.T) {
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: "blind", actions: []string{model.ActionConversationRead}}
	})
	prop := pluginClient(t, r, plugin)
	held := propose(t, r, prop, proposal(r, "p-1", "fp-1"))
	plain := proposal(r, "p-9", "fp-9")
	plain.Hold = nil
	plain.Kind = model.ActionMessageNote
	queued := propose(t, r, prop, plain)

	blind := pluginClient(t, r, "blind")
	_, err := blind.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: held.Operation.Ref})
	requireCode(t, err, model.NotFound, "resource not found")
	_, unknown := blind.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: model.OperationRef{Tenant: tenantA, ID: inboxTwo}})
	if err.Error() != unknown.Error() {
		t.Fatalf("held-denied error %q differs from unknown-operation error %q", err, unknown)
	}
	if got, err := blind.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: queued.Operation.Ref}); err != nil || got.State != model.Queued {
		t.Fatalf("a non-held operation stays readable by operation.read: %+v, %v", got, err)
	}
	if l := listHeld(t, r, blind); len(l) != 0 {
		t.Fatalf("List showed %d held operations to a principal that cannot read the target", len(l))
	}
	all, err := services.ListOperations(r.Ctx, blind.Client, model.ListRequest{Call: model.Call{Correlation: "list"}, Page: model.PageRequest{Limit: 50}})
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range all.Items {
		if op.Ref == held.Operation.Ref {
			t.Fatal("unfiltered List showed the held operation")
		}
	}
	if l := listHeld(t, r, prop); len(l) != 1 {
		t.Fatalf("a reader of the target sees %d held operations, want 1", len(l))
	}
}

func TestExpiredIsSettledHeldIsNot(t *testing.T) {
	if !operations.Settled(model.Expired) || operations.Settled(model.Held) {
		t.Fatalf("Settled(expired) = %v, Settled(held) = %v", operations.Settled(model.Expired), operations.Settled(model.Held))
	}
}

// The duplicate guard answers with the held operation only to a principal
// that may read it. A proposer without the target's read right learns
// nothing of the held proposal, its preview or its reason.
func TestHeldDuplicateGuardHidesHeldBlockFromProposerWithoutTargetRead(t *testing.T) {
	const blindActor model.ActorID = "plugin:blind"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: blindActor, actions: []string{model.ActionConversationRead}}
	})
	first := propose(t, r, pluginClient(t, r, plugin), proposal(r, "p-1", "fp-1"))
	blind := pluginClient(t, r, blindActor)

	a, err := operations.Submit(r.Ctx, blind.Client, proposal(r, "b-1", "fp-b"))
	requireCode(t, err, model.Conflict, "operation already held")
	if a.Operation.Ref != (model.OperationRef{}) || a.Operation.Held != nil || a.AlreadyHeld || a.Replayed {
		t.Fatalf("refused duplicate returned a record: %+v", a)
	}
	for _, leak := range []string{"first contact", "send hi", "triage/reply", "user-9", string(first.Operation.Ref.ID)} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q leaks %q", err, leak)
		}
	}
	if _, err := blind.Operations().Lookup(r.Ctx, model.OperationLookup{
		Call: model.Call{Correlation: "l"}, Kind: model.ActionMessageReply, Target: r.Conv.Ref.Ref(), ID: "b-1",
	}); err == nil {
		t.Fatal("refused proposal claimed its key")
	}
	if l := listHeld(t, r, pluginClient(t, r, plugin)); len(l) != 1 {
		t.Fatalf("held operations = %d, want 1", len(l))
	}
}

// The proposer of a held operation replays its own proposal under the same
// key without the target's read right: the replay is not a duplicate.
func TestHeldReplayByProposerWithoutTargetReadStillReturnsHeldOperation(t *testing.T) {
	const blindActor model.ActorID = "plugin:blind"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: blindActor, actions: []string{model.ActionConversationRead}}
	})
	blind := pluginClient(t, r, blindActor)
	first := propose(t, r, blind, proposal(r, "b-1", "fp-b"))
	again := propose(t, r, blind, proposal(r, "b-1", "fp-b"))
	if !again.Replayed || again.AlreadyHeld || again.Operation.Ref != first.Operation.Ref || again.Operation.State != model.Held {
		t.Fatalf("replay = %+v", again)
	}
}

// Message redaction scrubs the held block's draft content like the
// message's: preview, edited arguments and each edit's before and after.
// Names, versions and the facts of the decision stay.
func TestRedactingMessageScrubsHeldContent(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	m := r.SeedMessage("draft")
	ref := m.Ref
	s := proposal(r, "p-1", "fp-1")
	s.Result, s.ResultMessage = func() *model.Ref { x := ref.Ref(); return &x }(), &ref
	op := propose(t, r, c, s).Operation
	secret := model.Extension{Name: "test.args", Version: 1, Data: []byte(`{"text":"secret"}`)}
	if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		rec, err := tx.Operation(r.Ctx, op.Ref)
		if err != nil {
			return err
		}
		next := rec.Operation.Clone()
		next.Held.EditedArgs = &secret
		next.Held.Edits = []model.HeldEdit{{By: "alice", At: r.Clock.Now().UTC(), Before: secret, After: secret}}
		next.Revision = "op-r2"
		return tx.UpdateOperation(r.Ctx, next, rec.Operation.Revision, store.Fence{})
	}); err != nil {
		t.Fatal(err)
	}
	m = r.Mutate(ref, func(m *model.Message) { m.Origin = &model.Origin{Actor: plugin, Operation: op.Ref} })
	rd := model.Redact{
		Command: model.Command{Call: model.Call{Correlation: "c"}, Operation: "redact-1", ExpectedRevision: m.Revision},
		Target:  ref.Ref(), Message: &ref, Reason: "erasure",
	}
	if _, err := r.C.Privacy().Redact(r.Ctx, rd); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	got := r.Op(op.Ref)
	h := got.Held
	if h == nil || h.Preview == nil || h.Preview.Name != "test.preview" || len(h.Preview.Data) != 0 ||
		h.EditedArgs == nil || h.EditedArgs.Name != "test.args" || len(h.EditedArgs.Data) != 0 ||
		len(h.Edits) != 1 || len(h.Edits[0].Before.Data) != 0 || len(h.Edits[0].After.Data) != 0 ||
		h.Edits[0].By != "alice" || h.Reason != "first contact" || h.TrustKey != "triage/reply" {
		t.Fatalf("held block after redaction = %+v", h)
	}
}
