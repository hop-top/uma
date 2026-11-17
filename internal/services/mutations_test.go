package services_test

import (
	"context"
	"testing"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// An edit on a channel whose adapter declares edit is accepted as queued:
// one operation work item, the message untouched, no event on it.
func TestEditQueuesOnDeclaredCapability(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	before := len(w.Events())
	a, err := w.edit(w.target, w.cmd(w.target.Revision), "corrected")
	if err != nil {
		t.Fatal(err)
	}
	if a.Operation.State != model.Queued || a.Replayed || a.Operation.Pending != nil {
		t.Fatalf("acceptance = %+v, want queued, fresh, without pending", a)
	}
	if got := w.message(w.target.Ref); got.Revision != w.target.Revision || got.Content.Parts[0].Text != w.target.Content.Parts[0].Text {
		t.Fatalf("message changed at claim: %+v", got)
	}
	if n := len(w.Events()); n != before {
		t.Fatalf("claim emitted %d events, want none", n-before)
	}
	if works := w.Claim("w"); len(works) != 1 || works[0].Resource != a.Operation.Ref.Ref() {
		t.Fatalf("work = %+v, want one item on the operation", works)
	}
	rec := w.record(a.Operation.Ref)
	if rec.Key.Kind != model.ActionMessageEdit || rec.Key.Target != w.target.Ref.Ref() || rec.Command.Name != operations.IntentEdit {
		t.Fatalf("key %+v intent %s", rec.Key, rec.Command.Name)
	}
}

// Each mutation needs its own capability: on the rig's send-only adapter
// every one is unsupported and nothing is queued.
func TestEditNeedsEditCapability(t *testing.T) {
	w := newMutationWorld(t)
	_, err := w.edit(w.target, w.cmd(w.target.Revision), "x")
	requireCode(t, err, model.Unsupported, "does not declare edit")
	_, err = w.remove(w.target, w.cmd(w.target.Revision))
	requireCode(t, err, model.Unsupported, "does not declare delete")
	_, err = w.react(w.target, w.cmd(""), "👍", false)
	requireCode(t, err, model.Unsupported, "does not declare react")
	w.requireNothingQueued(t)
}

// Edit and Delete compare ExpectedRevision with the message's at claim.
func TestEditExpectedRevisionMismatchConflicts(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	_, err := w.edit(w.target, w.cmd("stale"), "x")
	requireCode(t, err, model.Conflict, "message revision changed")
	_, err = w.remove(w.target, w.cmd("stale"))
	requireCode(t, err, model.Conflict, "message revision changed")
	w.requireNothingQueued(t)
}

// React compares ExpectedRevision only when set.
func TestReactExpectedRevisionComparedWhenSet(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	_, err := w.react(w.target, w.cmd("stale"), "👍", false)
	requireCode(t, err, model.Conflict, "message revision changed")
	if _, err := w.react(w.target, w.cmd(""), "👍", false); err != nil {
		t.Fatalf("react without expected revision: %v", err)
	}
	if _, err := w.react(w.target, w.cmd(w.target.Revision), "🎉", false); err != nil {
		t.Fatalf("react at the current revision: %v", err)
	}
}

// A deleted or redacted target conflicts for every mutation.
func TestDeleteOfRedactedMessageConflicts(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	for _, c := range []struct {
		m    model.Message
		want string
	}{{w.redacted, "message redacted"}, {w.deleted, "message deleted"}} {
		_, err := w.remove(c.m, w.cmd(c.m.Revision))
		requireCode(t, err, model.Conflict, c.want)
		_, err = w.edit(c.m, w.cmd(c.m.Revision), "x")
		requireCode(t, err, model.Conflict, c.want)
		_, err = w.react(c.m, w.cmd(""), "👍", false)
		requireCode(t, err, model.Conflict, c.want)
	}
	w.requireNothingQueued(t)
}

// A repeated react with the same payload replays the stored operation,
// also after the message's revision moved: replay is compared before any
// precheck.
func TestReactReplaysSamePayload(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	cmd := w.cmd(w.target.Revision)
	first, err := w.react(w.target, cmd, "👍", false)
	if err != nil {
		t.Fatal(err)
	}
	moved := w.message(w.target.Ref)
	moved.Revision = "m2"
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error { return tx.PutMessage(ctx, moved, w.target.Revision) })
	again, err := w.react(w.target, cmd, "👍", false)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("replay = %+v, %v", again, err)
	}
}

// The same operation ID with another emoji, another remove flag or
// another expected revision conflicts; so does an edit with other content.
func TestReactDifferentPayloadConflicts(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	cmd := w.cmd("")
	if _, err := w.react(w.target, cmd, "👍", false); err != nil {
		t.Fatal(err)
	}
	_, err := w.react(w.target, cmd, "👎", false)
	requireCode(t, err, model.Conflict, "different payload")
	_, err = w.react(w.target, cmd, "👍", true)
	requireCode(t, err, model.Conflict, "different payload")
	other := cmd
	other.ExpectedRevision = w.target.Revision
	_, err = w.react(w.target, other, "👍", false)
	requireCode(t, err, model.Conflict, "different payload")
	ecmd := w.cmd(w.target.Revision)
	if _, err := w.edit(w.target, ecmd, "one"); err != nil {
		t.Fatal(err)
	}
	_, err = w.edit(w.target, ecmd, "two")
	requireCode(t, err, model.Conflict, "different payload")
	dcmd := w.cmd(w.target.Revision)
	if _, err := w.remove(w.target, dcmd); err != nil {
		t.Fatal(err)
	}
	dcmd.ExpectedRevision = "other"
	_, err = w.remove(w.target, dcmd)
	requireCode(t, err, model.Conflict, "different payload")
}

// The edit fingerprint covers the message's audience: a retry after the
// audience narrowed is a different command and conflicts, and a retry under
// the unchanged audience replays.
func TestEditRetryAfterAudienceNarrowedConflicts(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	cmd := w.cmd(w.target.Revision)
	first, err := w.edit(w.target, cmd, "corrected")
	if err != nil {
		t.Fatal(err)
	}
	again, err := w.edit(w.target, cmd, "corrected")
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
		t.Fatalf("retry under the same audience = %+v, %v; want replay", again, err)
	}

	// Narrowing bumps the revision; the retry still names the old one, so
	// only the audience differs from the first call.
	narrowed := w.message(w.target.Ref)
	narrowed.Audience, narrowed.Revision = model.Audience{Visibility: model.Internal}, "m2"
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error { return tx.PutMessage(ctx, narrowed, w.target.Revision) })
	_, err = w.edit(w.target, cmd, "corrected")
	requireCode(t, err, model.Conflict, "different payload")
}

// A message of another tenant, or one named under another inbox's or
// conversation's reference, is not found, as a missing one is.
func TestEditForeignMessageNotFound(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	misplaced := w.otherInbox
	misplaced.Ref.Conversation = w.Conv.Ref
	underOtherInbox := w.target
	underOtherInbox.Ref.Conversation = w.otherInbox.Ref.Conversation
	missing := w.target
	missing.Ref.ID = "00000004-0000-7000-8000-0000000000ff"
	for name, m := range map[string]model.Message{
		"another inbox": underOtherInbox, "another tenant": w.otherTenant, "another conversation": misplaced, "missing": missing,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := w.edit(m, w.cmd("m1"), "x")
			requireCode(t, err, model.NotFound, "resource not found")
		})
	}
	w.requireNothingQueued(t)
}

// Authorization is the mutation's own action on the message under its
// audience at ExpectedRevision; a denial queues nothing.
func TestEditUnauthorizedActorDenied(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	w.policy.denyAction(model.ActionMessageEdit)
	_, err := w.edit(w.target, w.cmd(w.target.Revision), "x")
	requireCode(t, err, model.Unauthorized, "denied by policy")
	a, ok := w.policy.last(model.ActionMessageEdit)
	if !ok || a.Resource != w.target.Ref.Ref() || a.Audience != w.target.Audience || a.Revision != w.target.Revision {
		t.Fatalf("authorization = %+v, want message.edit on the message under its audience and revision", a)
	}
	w.requireNothingQueued(t)
}

// A message uma did not author (no Origin) is a target when policy
// allows: the provider account decides at execution.
func TestEditOfInboundMessageIsAccepted(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	if w.target.Origin != nil {
		t.Fatal("fixture target must be inbound")
	}
	if a, err := w.edit(w.target, w.cmd(w.target.Revision), "x"); err != nil || a.Operation.State != model.Queued {
		t.Fatalf("edit of an inbound message = %+v, %v", a, err)
	}
}

// Result and ResultMessage name the target of every mutation.
func TestMutationResultNamesTarget(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	for name, call := range map[string]func() (model.Acceptance, error){
		"edit":   func() (model.Acceptance, error) { return w.edit(w.target, w.cmd(w.target.Revision), "x") },
		"delete": func() (model.Acceptance, error) { return w.remove(w.target, w.cmd(w.target.Revision)) },
		"react":  func() (model.Acceptance, error) { return w.react(w.target, w.cmd(""), "👍", false) },
	} {
		a, err := call()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		op := a.Operation
		if op.Result == nil || *op.Result != w.target.Ref.Ref() || op.ResultMessage == nil || *op.ResultMessage != w.target.Ref {
			t.Fatalf("%s result = %v / %v, want the target", name, op.Result, op.ResultMessage)
		}
	}
}

// The stored intent holds the target, its audience, the base snapshot
// the claiming unit read and the wait bound.
func TestIntentHoldsBaseSnapshot(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	a, err := w.edit(w.edited, w.cmd(w.edited.Revision), "fixed")
	if err != nil {
		t.Fatal(err)
	}
	in, err := operations.DecodeEditIntent(w.record(a.Operation.Ref).Command)
	if err != nil {
		t.Fatal(err)
	}
	b := in.Base
	if in.Message != w.edited.Ref || in.Audience != w.edited.Audience || in.Content.Parts[0].Text != "fixed" {
		t.Fatalf("intent = %+v", in)
	}
	if b.Revision != w.edited.Revision || b.LastEdit == nil || *b.LastEdit.Operation != *w.edited.LastEdit.Operation ||
		!b.LastEdit.At.Equal(w.edited.LastEdit.At) || b.LastEdit.Actor != w.edited.LastEdit.Actor || b.Deleted || b.Redacted {
		t.Fatalf("base = %+v, want the edited message's edit state", b)
	}
	if want := w.Clock.Now().Add(model.DefaultStreamDeadline); !in.WaitUntil.Equal(want) {
		t.Fatalf("wait until %v, want %v", in.WaitUntil, want)
	}
	d, err := w.remove(w.target, w.cmd(w.target.Revision))
	if err != nil {
		t.Fatal(err)
	}
	if din, err := operations.DecodeDeleteIntent(w.record(d.Operation.Ref).Command); err != nil || din.Base.LastEdit != nil || din.Base.Revision != w.target.Revision {
		t.Fatalf("delete intent = %+v, %v", din, err)
	}
	r, err := w.react(w.target, w.cmd(""), "👍", true)
	if err != nil {
		t.Fatal(err)
	}
	if rin, err := operations.DecodeReactIntent(w.record(r.Operation.Ref).Command); err != nil || rin.Emoji != "👍" || !rin.Remove {
		t.Fatalf("react intent = %+v, %v", rin, err)
	}
}

// A message that is a live stream's provider message changes only through
// the stream: every mutation conflicts until the stream is terminal.
func TestMutationOfLiveStreamMessageConflicts(t *testing.T) {
	w := newMutationWorld(t, enginetest.WithMutations)
	st := model.Stream{
		Ref: model.StreamRef{Conversation: w.Conv.Ref, ID: "00000004-0000-7000-8000-0000000000aa"}, Mode: model.StreamEdit,
		State: model.StreamEnding, Audience: model.Audience{Visibility: model.Public},
		ProviderReferences: []model.ExternalRef{w.providerRef}, Revision: "s1",
	}
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error { return tx.PutStream(ctx, st, "") })
	_, err := w.edit(w.target, w.cmd(w.target.Revision), "x")
	requireCode(t, err, model.Conflict, "live stream")
	_, err = w.remove(w.target, w.cmd(w.target.Revision))
	requireCode(t, err, model.Conflict, "live stream")
	_, err = w.react(w.target, w.cmd(""), "👍", false)
	requireCode(t, err, model.Conflict, "live stream")
	w.requireNothingQueued(t)
	ended := st.Clone()
	ended.State, ended.Revision = model.StreamFinished, "s2"
	w.seed(tenantA, func(ctx context.Context, tx store.Tx) error { return tx.PutStream(ctx, ended, "s1") })
	if _, err := w.edit(w.target, w.cmd(w.target.Revision), "x"); err != nil {
		t.Fatalf("edit after the stream finished: %v", err)
	}
}
