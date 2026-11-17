package github_test

import (
	"path/filepath"
	"testing"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
)

var updateCassettes = filepath.Join("testdata", "cassettes", "github", "update")

// TestRecordedUpdates runs the update effect on the sandbox issue: labels
// it, reconciles the update (applied) and one the issue does not hold (not
// applied), clears the labels and reconciles that, so the issue is left
// without labels, as it started; then updates issue 999999, which does not
// exist, to record GitHub's 404. Assignees and the milestone are not
// recorded: the sandbox names no login or milestone, so they are covered
// against the local server only. Checks report with Errorf so the clearing
// update always runs once a label is set.
func TestRecordedUpdates(t *testing.T) {
	inst := recordedChannel(t, updateCassettes, github.Config{}, portstest.NewFixedClock(t0), func(gh *fakeGitHub) {
		gh.setIssue("a", sandboxRepo, sandboxIssue, sandboxAccount, "open", nil, t0)
	})
	execute := func(what, data string) adapter.Outcome {
		t.Helper()
		q := updateReq(tryOne, data)
		q.Route = sandboxRoute()
		out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), q)
		if err != nil {
			t.Errorf("%s = %v", what, err)
		}
		return out
	}
	reconcile := func(what, data string) adapter.Outcome {
		t.Helper()
		q := updateReq(tryOne, data)
		q.Route = sandboxRoute()
		out, err := inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{
			Effect: q.Effect, Capability: model.CapabilityExtensionExecute, Action: q.Action,
		})
		if err != nil {
			t.Errorf("%s = %v", what, err)
		}
		return out
	}
	applied := func(what string, o adapter.Outcome) {
		t.Helper()
		if o.State != model.ProviderAccepted || o.Failure != nil {
			t.Errorf("%s = %+v; want provider_accepted", what, o)
		}
	}

	applied("Execute label", execute("Execute label", `{"labels":["uma-recorded"]}`))
	applied("Reconcile label", reconcile("Reconcile label", `{"labels":["uma-recorded"]}`))
	if o := reconcile("Reconcile other label", `{"labels":["uma-other"]}`); o.State != model.Failed || o.Failure == nil || o.Failure.Code != model.NotFound {
		t.Errorf("Reconcile of a label the issue does not hold = %+v; want failed not_found", o)
	}
	applied("Execute clear", execute("Execute clear", `{"labels":[]}`))
	applied("Reconcile clear", reconcile("Reconcile clear", `{"labels":[]}`))

	missing := updateReq(tryOne, `{"labels":["uma-recorded"]}`)
	missing.Route = issueRoute("999999")
	out, err := inst.(adapter.ExtensionExecutor).Execute(t.Context(), missing)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.NotFound || out.Binding != model.WorkItemBindingInaccessible {
		t.Errorf("missing issue = %+v, %v; want rejected not_found, binding inaccessible", out, err)
	}
}
