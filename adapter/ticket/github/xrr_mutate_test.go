package github_test

import (
	"path/filepath"
	"testing"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

var mutateCassettes = filepath.Join("testdata", "cassettes", "github", "mutate")

// tryThree is an attempt no call carried.
const tryThree = model.ID("00000000-0000-7000-8000-00000000b0b3")

// TestRecordedMutations runs one comment through its life on the sandbox
// issue: posted, edited, reacted to, reconciled at every step, reaction
// removed, deleted; then the refusals of GitHub for a comment that is gone.
// It creates one comment and deletes it itself, so the issue is left as it
// was; the checks report with Errorf so the deletion always runs once the
// comment exists.
//
// The sandbox holds comments of the recording account only, so the edit of
// "another user's comment" is the same call as the edit of the account's
// own: the adapter makes no authorship check, and GitHub's answer for a
// comment the token may not edit (403) is covered against the local server
// shaped like GitHub's answers.
func TestRecordedMutations(t *testing.T) {
	inst := recordedChannel(t, mutateCassettes, github.Config{Replies: true, AccountID: sandboxAccount}, portstest.NewFixedClock(t0), func(*fakeGitHub) {})
	sent := sendReq(tryOne, "Recorded mutation target.")
	sent.Route = sandboxRoute()
	out, err := inst.(adapter.Sender).Send(t.Context(), sent)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	comment := out.References[0]
	eff := func(attempt model.ID) adapter.Effect {
		e := mutEffect(attempt, &comment)
		e.Route = sandboxRoute()
		return e
	}
	reconcile := func(c model.Capability, attempt model.ID, content model.Content, emoji string, remove bool) adapter.Outcome {
		t.Helper()
		o, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff(attempt), Capability: c, Content: content, Emoji: emoji, Remove: remove})
		if err != nil {
			t.Errorf("Reconcile %s = %v", c, err)
		}
		return o
	}
	applied := func(what string, o adapter.Outcome, err error) {
		t.Helper()
		if err != nil || o.State != model.ProviderAccepted {
			t.Errorf("%s = %+v, %v; want provider_accepted", what, o, err)
		}
	}
	rejected := func(what string, o adapter.Outcome, err error, code model.ErrorCode) {
		t.Helper()
		if err != nil || o.State != model.Rejected || o.Failure == nil || o.Failure.Code != code || o.Binding != "" {
			t.Errorf("%s = %+v, %v; want rejected %s", what, o, err, code)
		}
	}
	notApplied := func(what string, o adapter.Outcome) {
		t.Helper()
		if o.State != model.Failed || o.Failure == nil || o.Failure.Code != model.NotFound {
			t.Errorf("%s = %+v; want failed not_found", what, o)
		}
	}

	// Edit, then find the edit by its marker; another attempt left none.
	edited := textOf("Recorded edit.")
	o, err := inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: eff(tryTwo), Content: edited})
	applied("Edit", o, err)
	applied("Reconcile edit", reconcile(model.CapabilityEdit, tryTwo, edited, "", false), nil)
	notApplied("Reconcile edit of another attempt", reconcile(model.CapabilityEdit, tryThree, edited, "", false))

	// React, find the reaction, remove it, find it gone, remove it again.
	o, err = inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff(tryOne), Emoji: "👍"})
	applied("React", o, err)
	if len(o.References) != 1 || o.References[0].Kind != github.RefKindReaction {
		t.Errorf("React references = %+v, want the reaction", o.References)
	} else if f := reconcile(model.CapabilityReact, tryOne, model.Content{}, "👍", false); f.State != model.ProviderAccepted || len(f.References) != 1 || f.References[0] != o.References[0] {
		t.Errorf("Reconcile react = %+v, want the reaction %+v", f, o.References[0])
	}
	notApplied("Reconcile removal of a held reaction", reconcile(model.CapabilityReact, tryOne, model.Content{}, "👍", true))
	o, err = inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff(tryTwo), Emoji: "👍", Remove: true})
	applied("React remove", o, err)
	applied("Reconcile removal", reconcile(model.CapabilityReact, tryTwo, model.Content{}, "👍", true), nil)
	o, err = inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff(tryThree), Emoji: "👍", Remove: true})
	applied("React remove of an absent reaction", o, err)

	// Delete: present before, gone after.
	notApplied("Reconcile delete of a present comment", reconcile(model.CapabilityDelete, tryOne, model.Content{}, "", false))
	o, err = inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: eff(tryOne)})
	applied("Delete", o, err)
	applied("Reconcile delete", reconcile(model.CapabilityDelete, tryOne, model.Content{}, "", false), nil)

	// GitHub's answers for a comment that is gone.
	if f := reconcile(model.CapabilityEdit, tryTwo, edited, "", false); f.State != model.Rejected || f.Failure == nil || f.Failure.Code != model.NotFound {
		t.Errorf("Reconcile edit of a deleted comment = %+v, want rejected not_found", f)
	}
	o, err = inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: eff(tryThree), Content: edited})
	rejected("Edit of a deleted comment", o, err, model.NotFound)
	o, err = inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: eff(tryThree)})
	rejected("Delete of a deleted comment", o, err, model.NotFound)
	o, err = inst.(adapter.Reactor).React(t.Context(), adapter.ReactRequest{Effect: eff(tryThree), Emoji: "👍", Remove: true})
	rejected("React remove on a deleted comment", o, err, model.NotFound)
}
