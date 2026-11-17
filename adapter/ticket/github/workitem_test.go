package github_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// actRig is a channel without replies over one seeded open issue, #7 of
// the sandbox repository, opened by user 5000001.
type actRig struct {
	*sendRig
	inst adapter.Instance
}

func newActRig(t *testing.T, cfg github.Config) *actRig {
	t.Helper()
	r := newSendRig(t)
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "open", nil, t0.Add(-time.Hour))
	cfg.APIURL = r.gh.apiURL("a")
	return &actRig{sendRig: r, inst: r.open(cfg)}
}

func actReq(attempt model.ID, status, reason string) adapter.WorkItemActRequest {
	return adapter.WorkItemActRequest{
		Effect: adapter.Effect{Operation: opA, Attempt: attempt, Route: issueRoute("7"), Audience: model.Audience{Visibility: model.Internal}},
		Status: status, Reason: reason,
	}
}

func (r *actRig) act(status, reason string) (adapter.WorkItemActOutcome, error) {
	return r.inst.(adapter.WorkItemActor).ActWorkItem(r.t.Context(), actReq(tryOne, status, reason))
}

func (r *actRig) reconcileAct(status, reason string) (adapter.Outcome, error) {
	q := actReq(tryOne, status, reason)
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{Effect: q.Effect, Status: q.Status, Reason: q.Reason})
}

func (r *actRig) patches() []fakeCall {
	var out []fakeCall
	for _, c := range r.gh.calls("a") {
		if c.Method == http.MethodPatch {
			out = append(out, c)
		}
	}
	return out
}

// patchBody decodes a PATCH body into its fields.
func patchBody(t *testing.T, c fakeCall) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(c.Body), &m); err != nil {
		t.Fatalf("PATCH body %q: %v", c.Body, err)
	}
	return m
}

func TestDescriptorDeclaresIssueActs(t *testing.T) {
	d := github.NewFactory(github.Options{}).Describe()
	if !slices.Contains(d.Capabilities, model.CapabilityWorkItemAct) {
		t.Fatalf("capabilities %v lack work_item_act", d.Capabilities)
	}
	want := []adapter.WorkItemTransition{
		{To: model.WorkItemClosed, From: []string{model.WorkItemOpen}, Reasons: []string{"completed", "not_planned"}},
		{To: model.WorkItemOpen, From: []string{model.WorkItemClosed}, Reasons: []string{"reopened"}},
	}
	if !slices.EqualFunc(d.WorkItemActs, want, func(a, b adapter.WorkItemTransition) bool {
		return a.To == b.To && slices.Equal(a.From, b.From) && slices.Equal(a.Reasons, b.Reasons)
	}) {
		t.Fatalf("work item acts = %+v, want close {completed, not_planned} from open and reopen {reopened} from closed", d.WorkItemActs)
	}
}

// Close sends the PATCH with the state and the reason, the default
// (completed) sent explicitly, and reports the issue state GitHub returned
// with its updated_at as source revision and the channel's local fields.
func TestCloseSendsStateReason(t *testing.T) {
	for _, tc := range []struct{ reason, sent string }{{"", "completed"}, {"completed", "completed"}, {"not_planned", "not_planned"}} {
		t.Run(tc.sent+"/"+tc.reason, func(t *testing.T) {
			local := []string{model.WorkItemFieldAssignees}
			r := newActRig(t, github.Config{LocalFields: local})
			out, err := r.act(model.WorkItemClosed, tc.reason)
			if err != nil || out.Outcome.State != model.Delivered || out.Failure != nil {
				t.Fatalf("ActWorkItem = %+v, %v; want delivered", out, err)
			}
			p := r.patches()
			if len(p) != 1 || p[0].Path != "/repos/example/uma-sandbox/issues/7" || p[0].Auth != "Bearer "+apiToken {
				t.Fatalf("patches = %+v, want one on the issue", p)
			}
			if b := patchBody(t, p[0]); len(b) != 2 || b["state"] != "closed" || b["state_reason"] != tc.sent {
				t.Fatalf("PATCH body = %v, want state closed and state_reason %s", b, tc.sent)
			}
			s := out.State
			if s == nil || s.Status != model.WorkItemClosed || s.RawStatus != "closed:"+tc.sent || s.Subject != "Export fails on large files" ||
				s.Requester != userRef("5000001") || s.Binding != model.WorkItemBindingActive || !slices.Equal(s.LocalFields, local) {
				t.Fatalf("state = %+v", s)
			}
			if err := s.Validate(); err != nil {
				t.Fatal(err)
			}
			if out.SourceRevision != "2026-10-11T12:00:00Z" || !out.OccurredAt.Equal(t0) {
				t.Fatalf("revision %q at %v, want the answer's updated_at", out.SourceRevision, out.OccurredAt)
			}
		})
	}
}

// Reopen sends state open with reason reopened, explicit when defaulted;
// the raw status keeps the reason on the open state.
func TestReopenSendsReopened(t *testing.T) {
	for _, reason := range []string{"", "reopened"} {
		r := newActRig(t, github.Config{})
		r.gh.setIssue("a", sandboxRepo, 7, 5000001, "closed", "not_planned", t0.Add(-time.Hour))
		out, err := r.act(model.WorkItemOpen, reason)
		if err != nil || out.Outcome.State != model.Delivered {
			t.Fatalf("ActWorkItem(%q) = %+v, %v", reason, out, err)
		}
		if b := patchBody(t, r.patches()[0]); b["state"] != "open" || b["state_reason"] != "reopened" {
			t.Fatalf("PATCH body = %v, want state open, state_reason reopened", b)
		}
		if s := out.State; s == nil || s.Status != model.WorkItemOpen || s.RawStatus != "open:reopened" {
			t.Fatalf("state = %+v, want open:reopened", s)
		}
	}
}

// The PATCH is sent even when the issue already holds the requested state:
// the local snapshot may be stale, and GitHub applies it idempotently.
func TestActAlwaysSendsThePatch(t *testing.T) {
	r := newActRig(t, github.Config{})
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "closed", "completed", t0.Add(-time.Hour))
	if out, err := r.act(model.WorkItemClosed, ""); err != nil || out.Outcome.State != model.Delivered {
		t.Fatalf("ActWorkItem = %+v, %v", out, err)
	}
	if n := len(r.patches()); n != 1 || len(r.gh.calls("a")) != 1 {
		t.Fatalf("calls = %+v, want exactly one PATCH and no read", r.gh.calls("a"))
	}
}

// Statuses and reasons GitHub has no transition for are refused before any
// call: pending and resolved, duplicate, and reasons of the other state.
func TestResolvedRejectedUnsupported(t *testing.T) {
	r := newActRig(t, github.Config{})
	for _, tc := range [][2]string{
		{model.WorkItemResolved, ""}, {model.WorkItemPending, ""}, {model.WorkItemClosed, "duplicate"},
		{model.WorkItemClosed, "reopened"}, {model.WorkItemOpen, "completed"}, {model.WorkItemOpen, "not_planned"},
	} {
		_, err := r.act(tc[0], tc[1])
		if me := wantCode(t, err, model.Unsupported); me.Retry != model.NoRetry {
			t.Fatalf("%v: error %+v, want final", tc, me)
		}
	}
	foreign := actReq(tryOne, model.WorkItemClosed, "")
	foreign.Route.ParentID = "example/../admin"
	if _, err := r.inst.(adapter.WorkItemActor).ActWorkItem(t.Context(), foreign); model.CodeOf(err) != model.Validation {
		t.Fatalf("path in repository = %v, want validation", err)
	}
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
}

// GitHub's refusals map as for comments, with a refused write a rejected
// outcome: 403 unauthorized, 404 and 410 not found naming the binding
// state, 422 validation; a rate limit dispatches nothing more; a lost
// answer is outcome unknown, reconciled before any retry.
func TestForbiddenCloseRejected(t *testing.T) {
	reset := t0.Add(17 * time.Minute)
	for _, tc := range []struct {
		name    string
		answer  answer
		state   model.DeliveryState
		binding string
		code    model.ErrorCode
		wait    time.Duration
	}{
		{name: "403 forbidden", answer: statusAnswer(403, `{"message":"Resource not accessible by integration","documentation_url":"`+issueDocsURL+`","status":"403"}`, nil), state: model.Rejected, code: model.Unauthorized},
		{name: "404 inaccessible", answer: statusAnswer(404, `{"message":"Not Found","documentation_url":"`+issueDocsURL+`","status":"404"}`, nil), state: model.Rejected, binding: model.WorkItemBindingInaccessible, code: model.NotFound},
		{name: "410 deleted", answer: statusAnswer(410, `{"message":"This issue was deleted","documentation_url":"`+issueDocsURL+`","status":"410"}`, nil), state: model.Rejected, binding: model.WorkItemBindingDeleted, code: model.NotFound},
		{name: "422 invalid", answer: statusAnswer(422, `{"message":"Validation Failed","documentation_url":"`+issueDocsURL+`","status":"422"}`, nil), state: model.Rejected, code: model.Validation},
		{name: "403 rate limit", answer: statusAnswer(403, `{"message":"API rate limit exceeded for installation ID 1."}`, map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": itoa(reset.Unix()),
		}), code: model.RateLimited, wait: 17 * time.Minute},
		{name: "500 unknown", answer: statusAnswer(500, `{"message":"Server Error"}`, nil), code: model.OutcomeUnknown},
		{name: "answer lost", answer: answerLost, code: model.OutcomeUnknown},
		{name: "unreachable", answer: unreachable, code: model.Transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newActRig(t, github.Config{})
			r.gh.respond("a", tc.answer)
			out, err := r.act(model.WorkItemClosed, "not_planned")
			if out.State != nil {
				t.Fatalf("state reported on a failure: %+v", out)
			}
			if tc.state != "" {
				if err != nil || out.Outcome.State != tc.state || out.Binding != tc.binding || out.Failure == nil || out.Failure.Code != tc.code || out.Failure.Retry != model.NoRetry {
					t.Fatalf("ActWorkItem = %+v, %v; want %s %s", out, err, tc.state, tc.code)
				}
			} else if me := wantCode(t, err, tc.code); me.RetryAfter != tc.wait {
				t.Fatalf("error = %+v, want wait %v", me, tc.wait)
			}
			if n := len(r.patches()); n != 1 {
				t.Fatalf("patches = %d, want 1", n)
			}
		})
	}
}

// Reconcile reads the issue: it holds the requested state and reason
// (the default when none was requested) → delivered; otherwise the act
// was not applied → failed, so a retry is safe. It needs no replies.
func TestReconcileComparesRequestedAct(t *testing.T) {
	r := newActRig(t, github.Config{})
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "closed", "not_planned", t0)
	for _, tc := range []struct {
		status, reason string
		applied        bool
	}{
		{model.WorkItemClosed, "not_planned", true},
		{model.WorkItemClosed, "", false},
		{model.WorkItemClosed, "completed", false},
		{model.WorkItemOpen, "", false},
	} {
		out, err := r.reconcileAct(tc.status, tc.reason)
		switch {
		case err != nil:
			t.Fatalf("Reconcile(%s %q) = %v", tc.status, tc.reason, err)
		case tc.applied && out.State != model.Delivered:
			t.Fatalf("Reconcile(%s %q) = %+v, want delivered", tc.status, tc.reason, out)
		case !tc.applied && (out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound):
			t.Fatalf("Reconcile(%s %q) = %+v, want failed not_found", tc.status, tc.reason, out)
		}
	}
	r.gh.setIssue("a", sandboxRepo, 7, 5000001, "open", "reopened", t0)
	if out, err := r.reconcileAct(model.WorkItemOpen, ""); err != nil || out.State != model.Delivered {
		t.Fatalf("Reconcile(reopen) = %+v, %v; want delivered", out, err)
	}
	for _, c := range r.gh.calls("a") {
		if c.Method != http.MethodGet || c.Path != "/repos/example/uma-sandbox/issues/7" {
			t.Fatalf("call %+v, want issue reads only", c)
		}
	}
	missing := r.open(github.Config{APIURL: r.gh.apiURL("missing")})
	q := actReq(tryOne, model.WorkItemClosed, "")
	_, err := missing.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: q.Effect, Status: q.Status})
	wantCode(t, err, model.NotFound)
}

// Without replies, work item acts and their reconciliation stay
// available; reconciling a comment does not.
func TestActsWithoutReplies(t *testing.T) {
	r := newActRig(t, github.Config{})
	st, err := r.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []model.Capability{model.CapabilityWorkItemAct, model.CapabilityReconcile} {
		if !slices.ContainsFunc(st, func(s model.CapabilityStatus) bool { return s.Name == c && s.Available }) {
			t.Fatalf("capabilities %+v: %s unavailable", st, c)
		}
	}
	comment := adapter.ReconcileRequest{Effect: sendReq(tryOne, "").Effect}
	if _, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), comment); model.CodeOf(err) != model.Disabled {
		t.Fatalf("comment Reconcile with replies disabled = %v, want disabled", err)
	}
}

// A reopened issue keeps GitHub's reason on the open state.
func TestReopenedWebhookKeepsReason(t *testing.T) {
	h := newHookRig(t, github.Config{})
	envs := h.post("issues", "issues-reopened", "d-reopened")
	if len(envs) != 1 || envs[0].WorkItem == nil {
		t.Fatalf("reopened = %+v", envs)
	}
	if w := envs[0].WorkItem; w.Status != model.WorkItemOpen || w.RawStatus != "open:reopened" || w.Validate() != nil {
		t.Fatalf("reopened state = %+v, want open with raw status open:reopened", w)
	}
}

// The channel's own close, reported back by webhook, is an ordinary state
// observation: it carries no echo of an operation, so it never settles
// one; only the PATCH answer or reconciliation does.
func TestOwnCloseWebhookDoesNotSettle(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	own := edited(t, "issues-closed", func(ev map[string]any) {
		ev["sender"].(map[string]any)["id"] = botID
		ev["issue"].(map[string]any)["state_reason"] = "not_planned"
	})
	if code, body := h.serve(hookRequest("issues", "d-own-close", own)); code != http.StatusOK {
		t.Fatalf("delivery = %d %q", code, body)
	}
	envs := h.sink.envelopes()
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeWorkItem || envs[0].WorkItem.RawStatus != "closed:not_planned" {
		t.Fatalf("own close = %+v, want one work item observation", envs)
	}
	for _, ref := range envs[0].References {
		if ref.Namespace != github.Namespace || ref.Kind != model.RefKindWorkItem {
			t.Fatalf("own close references %+v, want the work item only", envs[0].References)
		}
	}
}
