package github_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

func updateAction(data string) model.Extension {
	return model.Extension{Name: github.ExtensionUpdate, Version: github.ExtensionVersion, Data: json.RawMessage(data)}
}

func updateReq(attempt model.ID, data string) adapter.ExecuteRequest {
	return adapter.ExecuteRequest{
		Effect: adapter.Effect{Operation: opA, Attempt: attempt, Route: issueRoute("7"), Audience: model.Audience{Visibility: model.Internal}},
		Action: updateAction(data),
	}
}

func (r *actRig) update(data string) (adapter.Outcome, error) {
	return r.inst.(adapter.ExtensionExecutor).Execute(r.t.Context(), updateReq(tryOne, data))
}

func (r *actRig) reconcileUpdate(data string) (adapter.Outcome, error) {
	q := updateReq(tryOne, data)
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{
		Effect: q.Effect, Capability: model.CapabilityExtensionExecute, Action: q.Action,
	})
}

func TestDescriptorDeclaresUpdateEffect(t *testing.T) {
	f := github.NewFactory(github.Options{})
	if d := f.Describe(); !slices.Contains(d.Capabilities, model.CapabilityExtensionExecute) {
		t.Fatalf("capabilities %v lack extension_execute", d.Capabilities)
	}
	defs := f.Extensions()
	if len(defs) != 1 {
		t.Fatalf("Extensions = %+v, want the update effect", defs)
	}
	d := defs[0]
	if d.Name != "github.issues.update" || d.Version != 1 || d.Kind != model.ExtensionKindEffect ||
		d.RequiredCapability != model.CapabilityExtensionExecute || d.AuthorizeAs != model.ActionWorkItemAct ||
		d.InputSchema.Name != "github.issues.update.input" || d.InputSchema.Version != 1 {
		t.Fatalf("update definition = %+v", d)
	}
}

// The factory refuses an update's fields without I/O: each refusal names
// the field, and an action of another name or version is not judged.
func TestValidateUpdateAction(t *testing.T) {
	f := github.NewFactory(github.Options{})
	aud := model.Audience{Visibility: model.Internal}
	for _, data := range []string{
		`{"labels":["bug"]}`, `{"labels":[]}`, `{"assignees":["octocat","hub-ot_1"]}`, `{"assignees":[]}`,
		`{"milestone":3}`, `{"milestone":null}`, `{"assignees":["octocat"],"labels":["bug","triage"],"milestone":1}`,
		`{"labels":["bug"],"conformance_unknown_field":{"kept":[1]}}`,
	} {
		if issues := f.ValidateAction(aud, updateAction(data)); len(issues) != 0 {
			t.Errorf("ValidateAction(%s) = %+v, want none", data, issues)
		}
	}
	for _, c := range []struct{ data, field string }{
		{``, "data"}, {`{}`, "data"}, {`[]`, "data"}, {`"x"`, "data"},
		{`{"conformance_unknown_field":1}`, "data"},
		{`{"labels":"bug"}`, "data.labels"}, {`{"labels":null}`, "data.labels"}, {`{"labels":[""]}`, "data.labels"},
		{`{"labels":[" "]}`, "data.labels"}, {`{"labels":["bug","Bug"]}`, "data.labels"}, {`{"labels":[1]}`, "data.labels"},
		{`{"assignees":"octocat"}`, "data.assignees"}, {`{"assignees":["octo cat"]}`, "data.assignees"},
		{`{"assignees":[""]}`, "data.assignees"}, {`{"assignees":["octocat","Octocat"]}`, "data.assignees"},
		{`{"assignees":["` + strings.Repeat("a", 40) + `"]}`, "data.assignees"},
		{`{"milestone":0}`, "data.milestone"}, {`{"milestone":-1}`, "data.milestone"}, {`{"milestone":"3"}`, "data.milestone"},
		{`{"milestone":1.5}`, "data.milestone"}, {`{"milestone":{}}`, "data.milestone"},
		{`{"title":"x","labels":["bug"]}`, "data.title"}, {`{"body":"x","labels":["bug"]}`, "data.body"},
		{`{"state":"closed","labels":["bug"]}`, "data.state"}, {`{"state_reason":"completed","labels":["bug"]}`, "data.state_reason"},
		{`{"duplicate_issue_id":4,"labels":["bug"]}`, "data.duplicate_issue_id"}, {`{"assignee":"octocat"}`, "data.assignee"},
	} {
		issues := f.ValidateAction(aud, updateAction(c.data))
		if len(issues) != 1 || issues[0].Field != c.field || issues[0].Code != string(model.Validation) || issues[0].Message == "" {
			t.Errorf("ValidateAction(%s) = %+v, want one validation issue on %s", c.data, issues, c.field)
		}
	}
	other := updateAction(`{}`)
	other.Version = 2
	if issues := f.ValidateAction(aud, other); issues != nil {
		t.Errorf("ValidateAction(version 2) = %+v, want no judgement", issues)
	}
	other = model.Extension{Name: "github.issues.other", Version: 1, Data: json.RawMessage(`{}`)}
	if issues := f.ValidateAction(aud, other); issues != nil {
		t.Errorf("ValidateAction(other name) = %+v, want no judgement", issues)
	}
}

// An update is one PATCH of the issue with the action's fields, unknown
// ones included; GitHub replaces each set sent. Its reconciliation reads
// the issue: every requested field held is applied, anything else not.
func TestUpdatePatchesIssueFields(t *testing.T) {
	r := newActRig(t, github.Config{})
	out, err := r.update(`{"assignees":["octocat"],"labels":["bug","triage"],"milestone":3,"conformance_unknown_field":{"kept":true}}`)
	if err != nil || out.State != model.ProviderAccepted || out.Failure != nil || len(out.References) != 0 {
		t.Fatalf("Execute = %+v, %v; want provider_accepted", out, err)
	}
	p := r.patches()
	if len(p) != 1 || p[0].Path != "/repos/example/uma-sandbox/issues/7" || !p[0].Applied || p[0].Auth != "Bearer "+apiToken {
		t.Fatalf("patches = %+v, want one applied PATCH of issue 7 with the bearer token", p)
	}
	body := patchBody(t, p[0])
	want := map[string]any{
		"assignees": []any{"octocat"}, "labels": []any{"bug", "triage"}, "milestone": float64(3),
		"conformance_unknown_field": map[string]any{"kept": true},
	}
	if len(body) != len(want) {
		t.Fatalf("PATCH body %v, want %v", body, want)
	}
	for k, v := range want {
		if got, _ := json.Marshal(body[k]); string(got) != mustJSON(t, v) {
			t.Fatalf("PATCH body %s = %s, want %s", k, got, mustJSON(t, v))
		}
	}
	assignees, labels, milestone := r.gh.issueFields("a", sandboxRepo+"#7")
	if !slices.Equal(assignees, []string{"octocat"}) || !slices.Equal(labels, []string{"bug", "triage"}) || milestone != 3 {
		t.Fatalf("issue holds assignees %v labels %v milestone %d", assignees, labels, milestone)
	}

	for _, data := range []string{`{"labels":["Triage","BUG"]}`, `{"assignees":["OctoCat"]}`, `{"milestone":3}`, `{"assignees":["octocat"],"labels":["bug","triage"],"milestone":3}`} {
		if out, err := r.reconcileUpdate(data); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Reconcile(%s) = %+v, %v; want provider_accepted: the issue holds the fields", data, out, err)
		}
	}
	for _, data := range []string{`{"labels":["bug"]}`, `{"labels":[]}`, `{"assignees":[]}`, `{"assignees":["hubot"]}`, `{"milestone":4}`, `{"milestone":null}`, `{"labels":["bug","triage"],"milestone":null}`} {
		out, err := r.reconcileUpdate(data)
		if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound {
			t.Fatalf("Reconcile(%s) = %+v, %v; want failed not_found: the issue does not hold the fields", data, out, err)
		}
	}
	if n := len(r.patches()); n != 1 {
		t.Fatalf("patches after reconciliation = %d, want 1: reconciliation only reads", n)
	}

	// Empty arrays and a null milestone clear the sets.
	if out, err := r.update(`{"assignees":[],"labels":[],"milestone":null}`); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("clearing Execute = %+v, %v", out, err)
	}
	if body := r.patches()[1].Body; !strings.Contains(body, `"milestone":null`) || !strings.Contains(body, `"labels":[]`) || !strings.Contains(body, `"assignees":[]`) {
		t.Fatalf("clearing PATCH body %s, want empty arrays and a null milestone", body)
	}
	assignees, labels, milestone = r.gh.issueFields("a", sandboxRepo+"#7")
	if len(assignees) != 0 || len(labels) != 0 || milestone != 0 {
		t.Fatalf("issue holds assignees %v labels %v milestone %d after clearing", assignees, labels, milestone)
	}
	if out, err := r.reconcileUpdate(`{"assignees":[],"labels":[],"milestone":null}`); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile(cleared) = %+v, %v; want provider_accepted", out, err)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A field uma owns for the channel (local_fields) is refused before any
// call, at the attempt and at its reconciliation: assignees when uma owns
// the assignees, labels and the milestone when it owns the fields.
func TestUpdateRefusesFieldsUmaOwns(t *testing.T) {
	for _, c := range []struct {
		local           []string
		refused, passed []string
	}{
		{[]string{model.WorkItemFieldAssignees}, []string{`{"assignees":["octocat"]}`, `{"assignees":[],"labels":["bug"]}`}, []string{`{"labels":["bug"]}`, `{"milestone":null}`}},
		{[]string{model.WorkItemFieldFields}, []string{`{"labels":["bug"]}`, `{"milestone":2}`, `{"assignees":["octocat"],"milestone":null}`}, []string{`{"assignees":["octocat"]}`}},
		{[]string{model.WorkItemFieldStatus, model.WorkItemFieldSubject}, nil, []string{`{"assignees":["octocat"],"labels":["bug"],"milestone":1}`}},
	} {
		r := newActRig(t, github.Config{LocalFields: c.local})
		for _, data := range c.refused {
			out, err := r.update(data)
			if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation || out.Failure.Retry != model.NoRetry {
				t.Errorf("local %v: Execute(%s) = %+v, %v; want rejected validation", c.local, data, out, err)
			}
			if out, err := r.reconcileUpdate(data); err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
				t.Errorf("local %v: Reconcile(%s) = %+v, %v; want rejected validation", c.local, data, out, err)
			}
		}
		if n := len(r.gh.calls("a")); n != 0 {
			t.Errorf("local %v: provider calls = %d before any accepted update, want none", c.local, n)
		}
		for _, data := range c.passed {
			if out, err := r.update(data); err != nil || out.State != model.ProviderAccepted {
				t.Errorf("local %v: Execute(%s) = %+v, %v; want provider_accepted", c.local, data, out, err)
			}
		}
	}
}

// Fields the adapter refuses are a final rejection at the attempt, before
// any call: the guard for an action stored before a rule tightened. An
// action of another version or name is rejected too.
func TestUpdateRefusesInvalidActionAtAttempt(t *testing.T) {
	r := newActRig(t, github.Config{})
	for _, c := range []struct {
		data string
		code model.ErrorCode
	}{
		{`{}`, model.Validation}, {`{"state":"closed"}`, model.Validation}, {`{"milestone":"x"}`, model.Validation},
	} {
		out, err := r.update(c.data)
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != c.code || out.Failure.Retry != model.NoRetry {
			t.Errorf("Execute(%s) = %+v, %v; want rejected %s", c.data, out, err, c.code)
		}
	}
	q := updateReq(tryOne, `{"labels":["bug"]}`)
	q.Action.Version = 2
	if out, err := r.inst.(adapter.ExtensionExecutor).Execute(t.Context(), q); err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.SchemaMismatch {
		t.Errorf("Execute(version 2) = %+v, %v; want rejected schema_mismatch", out, err)
	}
	q = updateReq(tryOne, `{"labels":["bug"]}`)
	q.Action.Name = "github.issues.other"
	if out, err := r.inst.(adapter.ExtensionExecutor).Execute(t.Context(), q); err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Unsupported {
		t.Errorf("Execute(other name) = %+v, %v; want rejected unsupported", out, err)
	}
	q = updateReq(tryOne, `{"labels":["bug"]}`)
	q.Route = model.ExternalRef{Channel: chanA, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: "7", ParentID: "not-a-repo"}
	if _, err := r.inst.(adapter.ExtensionExecutor).Execute(t.Context(), q); model.CodeOf(err) != model.Validation {
		t.Errorf("Execute(bad route) = %v, want validation", err)
	}
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
}

// GitHub's answers to an update map as an issue update's: a refusal is
// final, a gone issue names the binding it shows, a server error or a lost
// answer is outcome unknown, and a rate limit carries the stated wait.
func TestUpdateAnswers(t *testing.T) {
	for _, c := range []struct {
		name    string
		answer  answer
		state   model.DeliveryState
		code    model.ErrorCode
		binding string
		wait    time.Duration
	}{
		{"422", statusAnswer(http.StatusUnprocessableEntity, `{"message":"Validation Failed","errors":[{"resource":"Issue","field":"assignees","code":"invalid"}],"documentation_url":"`+issueDocsURL+`","status":"422"}`, nil), model.Rejected, model.Validation, "", 0},
		{"400", statusAnswer(http.StatusBadRequest, `{"message":"Problems parsing JSON","documentation_url":"`+issueDocsURL+`","status":"400"}`, nil), model.Rejected, model.Validation, "", 0},
		{"403", statusAnswer(http.StatusForbidden, `{"message":"Resource not accessible by integration","documentation_url":"`+issueDocsURL+`","status":"403"}`, nil), model.Rejected, model.Unauthorized, "", 0},
		{"404", statusAnswer(http.StatusNotFound, `{"message":"Not Found","documentation_url":"`+issueDocsURL+`","status":"404"}`, nil), model.Rejected, model.NotFound, model.WorkItemBindingInaccessible, 0},
		{"410", statusAnswer(http.StatusGone, `{"message":"This issue was deleted","documentation_url":"`+issueDocsURL+`","status":"410"}`, nil), model.Rejected, model.NotFound, model.WorkItemBindingDeleted, 0},
		{"401", statusAnswer(http.StatusUnauthorized, `{"message":"Bad credentials","documentation_url":"https://docs.github.com/rest","status":"401"}`, nil), "", model.Unauthorized, "", 0},
		{"429", statusAnswer(http.StatusTooManyRequests, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`, map[string]string{"Retry-After": "45"}), "", model.RateLimited, "", 45 * time.Second},
		{"500", statusAnswer(http.StatusInternalServerError, `{"message":"Server Error"}`, nil), "", model.OutcomeUnknown, "", 0},
		{"answer lost", answerLost, "", model.OutcomeUnknown, "", 0},
		{"request lost", requestLost, "", model.OutcomeUnknown, "", 0},
		{"unreachable", unreachable, "", model.Transient, "", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newActRig(t, github.Config{})
			if c.name == "401" {
				// Both resolutions of the credential are refused.
				r.gh.respond("a", c.answer, c.answer)
			} else {
				r.gh.respond("a", c.answer)
			}
			out, err := r.update(`{"labels":["bug"]}`)
			if c.state == "" {
				me := wantCode(t, err, c.code)
				if me.RetryAfter != c.wait || out.State != "" {
					t.Fatalf("Execute = %+v, %+v; want error %s with wait %s", out, me, c.code, c.wait)
				}
				return
			}
			if err != nil || out.State != c.state || out.Failure == nil || out.Failure.Code != c.code || out.Failure.Retry != model.NoRetry || out.Binding != c.binding {
				t.Fatalf("Execute = %+v, %v; want %s %s binding %q", out, err, c.state, c.code, c.binding)
			}
		})
	}
}

// A reconciliation that does not carry the attempt's action cannot
// compare: it is inconclusive, and reads nothing. One of an issue that is
// gone is the read's failure.
func TestUpdateReconcileNeedsTheAction(t *testing.T) {
	r := newActRig(t, github.Config{})
	eff := updateReq(tryOne, "").Effect
	out, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: eff, Capability: model.CapabilityExtensionExecute})
	if err != nil || out.State != model.Unknown {
		t.Fatalf("Reconcile without action = %+v, %v; want unknown", out, err)
	}
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("provider calls = %d, want none", n)
	}
	q := updateReq(tryOne, `{"labels":["bug"]}`)
	q.Route = issueRoute("999")
	_, err = r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: q.Effect, Capability: model.CapabilityExtensionExecute, Action: q.Action})
	wantCode(t, err, model.NotFound)
}
