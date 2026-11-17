package gitlab_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
)

func actReq(status, reason string) adapter.WorkItemActRequest {
	return adapter.WorkItemActRequest{Effect: effect(tryOne, issueRoute("7")), Status: status, Reason: reason}
}

func (r *rig) act(status, reason string) (adapter.WorkItemActOutcome, error) {
	return r.inst.(adapter.WorkItemActor).ActWorkItem(r.t.Context(), actReq(status, reason))
}

func TestActClosesAndReopens(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct{ status, event, raw string }{
		{model.WorkItemClosed, "close", "closed"},
		{model.WorkItemOpen, "reopen", "opened"},
	} {
		out, err := r.act(tc.status, "")
		if err != nil || out.Outcome.State != model.Delivered || out.State == nil {
			t.Fatalf("act %s = %+v, %v", tc.status, out, err)
		}
		s := out.State
		if s.Status != tc.status || s.RawStatus != tc.raw || s.Binding != model.WorkItemBindingActive || s.Validate() != nil ||
			out.SourceRevision == "" || out.OccurredAt.IsZero() || s.Requester.ID != "5000001" {
			t.Fatalf("state = %+v (revision %q), want %s/%s", s, out.SourceRevision, tc.status, tc.raw)
		}
		calls := r.gl.calls("a")
		c := calls[len(calls)-1]
		var req map[string]string
		_ = json.Unmarshal([]byte(c.Body), &req)
		if c.Method != http.MethodPut || c.Path != "/api/v4/projects/700000001/issues/7" || req["state_event"] != tc.event || len(req) != 1 {
			t.Fatalf("call = %+v, want PUT state_event %s", c, tc.event)
		}
	}
}

func TestActRefusals(t *testing.T) {
	r := newRig(t)
	for _, tc := range []struct{ status, reason string }{
		{model.WorkItemClosed, "not_planned"}, {model.WorkItemClosed, "completed"}, {model.WorkItemOpen, "reopened"},
		{model.WorkItemPending, ""}, {model.WorkItemResolved, ""}, {"", ""},
	} {
		_, err := r.act(tc.status, tc.reason)
		wantCode(t, err, model.Unsupported)
	}
	if n := len(r.gl.calls("a")); n != 0 {
		t.Fatalf("%d provider calls for refused acts, want none", n)
	}
	// The descriptor declares only what the adapter sends.
	acts := gitlab.NewFactory(gitlab.Options{}).Describe().WorkItemActs
	if len(acts) != 2 || len(acts[0].Reasons) != 0 || len(acts[1].Reasons) != 0 {
		t.Fatalf("declared acts = %+v, want close and reopen without reasons", acts)
	}
}

func TestActAnswers(t *testing.T) {
	r := newRig(t)
	missing := actReq(model.WorkItemClosed, "")
	missing.Route = issueRoute("999")
	out, err := r.inst.(adapter.WorkItemActor).ActWorkItem(t.Context(), missing)
	if err != nil || out.Outcome.State != model.Rejected || out.Failure.Code != model.NotFound || out.Binding != model.WorkItemBindingInaccessible {
		t.Fatalf("missing issue = %+v, %v", out, err)
	}
	r.gl.respond("a", statusAnswer(403, `{"message":"403 Forbidden"}`, nil))
	out, err = r.act(model.WorkItemClosed, "")
	if err != nil || out.Outcome.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("403 = %+v, %v; want rejected unauthorized", out, err)
	}
	r.gl.respond("a", statusAnswer(429, "Retry later", map[string]string{"Retry-After": "3"}))
	_, err = r.act(model.WorkItemClosed, "")
	wantCode(t, err, model.RateLimited)
	r.gl.respond("a", answerLost)
	_, err = r.act(model.WorkItemClosed, "")
	wantCode(t, err, model.OutcomeUnknown)
}

func TestReconcileAct(t *testing.T) {
	r := newRig(t)
	rec := func(status string) model.DeliveryState {
		t.Helper()
		out, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(tryOne, issueRoute("7")), Status: status})
		if err != nil {
			t.Fatalf("Reconcile(%s) = %v", status, err)
		}
		return out.State
	}
	if st := rec(model.WorkItemClosed); st != model.Failed {
		t.Fatalf("reconcile close of an open issue = %s, want failed", st)
	}
	r.gl.respond("a", answerLost)
	_, err := r.act(model.WorkItemClosed, "")
	wantCode(t, err, model.OutcomeUnknown)
	if st := rec(model.WorkItemClosed); st != model.Delivered {
		t.Fatalf("reconcile close of a closed issue = %s, want delivered", st)
	}
	if st := rec(model.WorkItemOpen); st != model.Failed {
		t.Fatalf("reconcile reopen of a closed issue = %s, want failed", st)
	}
}
