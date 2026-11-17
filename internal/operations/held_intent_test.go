package operations_test

import (
	"encoding/json"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

func requireIntent(t *testing.T, where string, op model.Operation, want model.Extension) {
	t.Helper()
	h := op.Held
	if h == nil || h.Intent == nil || h.Intent.Name != want.Name || h.Intent.Version != want.Version ||
		string(h.Intent.Data) != string(want.Data) {
		t.Fatalf("%s: held intent = %+v, want %+v", where, h, want)
	}
}

// A reader of the target sees the stored intent on every path that shows a
// held operation.
func TestHeldIntentShownToReaderOfTarget(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	want := replyArgs(r, "hello")
	a := propose(t, r, c, replyProposal(r, "p-1", "hello"))
	requireIntent(t, "acceptance", a.Operation, want)
	got, err := r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: a.Operation.Ref})
	if err != nil {
		t.Fatal(err)
	}
	requireIntent(t, "Get", got, want)
	found, err := c.Operations().Lookup(r.Ctx, model.OperationLookup{
		Call: model.Call{Correlation: "l"}, Kind: model.ActionMessageReply, Target: r.Conv.Ref.Ref(), ID: "p-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	requireIntent(t, "Lookup", found, want)
	l := listHeld(t, r, r.C)
	if len(l) != 1 {
		t.Fatalf("List = %d", len(l))
	}
	requireIntent(t, "List", l[0], want)
}

// A caller that may not read the target does not see the operation at all,
// so never its intent: the rule that hides the preview.
func TestHeldIntentHiddenWithoutTargetRead(t *testing.T) {
	const blindActor model.ActorID = "blind"
	r := newRig(t, func(c *engine.Config, _ *enginetest.OutFactory) {
		c.Authorizer = denyReads{actor: blindActor, actions: []string{model.ActionConversationRead}}
	})
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	blind := pluginClient(t, r, blindActor)
	_, err := blind.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: held.Ref})
	requireCode(t, err, model.NotFound, "resource not found")
	for _, op := range listHeld(t, r, blind) {
		if op.Held != nil && op.Held.Intent != nil {
			t.Fatalf("blind List shows intent %+v", op.Held.Intent)
		}
	}
	if l := listHeld(t, r, blind); len(l) != 0 {
		t.Fatalf("blind List = %d, want none", len(l))
	}
}

// The intent is a copy: a draft edit of the returned value never reaches
// the stored operation.
func TestHeldIntentIsACopy(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	held.Held.Intent.Data[0] = 'X'
	if got := r.Op(held.Ref); got.Held.Intent.Data[0] != '{' {
		t.Fatalf("stored intent mutated: %s", got.Held.Intent.Data)
	}
}

// The returned intent, modified, is accepted as edit arguments.
func TestEditWithReturnedIntentPassesValidation(t *testing.T) {
	r := newRig(t)
	held := propose(t, r, pluginClient(t, r, plugin), replyProposal(r, "p-1", "hello")).Operation
	got, err := r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: held.Ref})
	if err != nil {
		t.Fatal(err)
	}
	in, err := operations.DecodeReplyIntent(*got.Held.Intent)
	if err != nil {
		t.Fatal(err)
	}
	in.Content = textContent("hello, revised")
	args := replyData(t, in)
	if _, err := r.C.Operations().Edit(r.Ctx, model.EditHeld{DecideHeld: decide(got), Args: args}); err != nil {
		t.Fatalf("Edit with the returned intent: %v", err)
	}
	if n := r.Step("w"); n != 1 {
		t.Fatalf("work items = %d", n)
	}
	if sent := sentTexts(r); len(sent) != 1 || sent[0] != "hello, revised" {
		t.Fatalf("sends = %q", sent)
	}
}

// An operation that was never held has no held block, so no intent.
func TestIntentOmittedWhenNotHeld(t *testing.T) {
	b, err := json.Marshal(model.HeldBlock{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["intent"]; ok {
		t.Fatalf("empty block carries intent: %s", b)
	}
}

// Redaction scrubs the intent's data like the preview's, keeping name and
// version.
func TestRedactingMessageScrubsHeldIntent(t *testing.T) {
	r := newRig(t)
	c := pluginClient(t, r, plugin)
	m := r.SeedMessage("draft")
	ref := m.Ref
	s := replyProposal(r, "p-1", "secret")
	s.Result, s.ResultMessage = func() *model.Ref { x := ref.Ref(); return &x }(), &ref
	op := propose(t, r, c, s).Operation
	m = r.Mutate(ref, func(m *model.Message) { m.Origin = &model.Origin{Actor: plugin, Operation: op.Ref} })
	rd := model.Redact{
		Command: model.Command{Call: model.Call{Correlation: "c"}, Operation: "redact-1", ExpectedRevision: m.Revision},
		Target:  ref.Ref(), Message: &ref, Reason: "erasure",
	}
	if _, err := r.C.Privacy().Redact(r.Ctx, rd); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	h := r.Op(op.Ref).Held
	if h == nil || h.Intent == nil || h.Intent.Name != operations.IntentReply || len(h.Intent.Data) != 0 {
		t.Fatalf("held intent after redaction = %+v", h)
	}
}
