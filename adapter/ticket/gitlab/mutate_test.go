package gitlab_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) target(id string) adapter.Effect {
	e := effect(tryOne, issueRoute("7"))
	ref := model.ProviderMessageRef(chanA, id)
	e.Target = &ref
	return e
}

func (r *rig) edit(id, text string) (adapter.Outcome, error) {
	return r.inst.(adapter.Editor).Edit(r.t.Context(), adapter.EditRequest{Effect: r.target(id), Content: textOf(text)})
}

func (r *rig) del(id string) (adapter.Outcome, error) {
	return r.inst.(adapter.Deleter).Delete(r.t.Context(), adapter.DeleteRequest{Effect: r.target(id)})
}

func (r *rig) reconcileMutation(id string, c model.Capability) (adapter.Outcome, error) {
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{Effect: r.target(id), Capability: c})
}

func TestEditNote(t *testing.T) {
	r := newRig(t)
	n := r.gl.seed("a", sandboxProject, 7, 5000001, "first", t0)
	id := "note:" + strconv.FormatInt(n.ID, 10)
	out, err := r.edit(id, "second")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	c := r.gl.calls("a")[0]
	if c.Method != http.MethodPut || c.Path != "/api/v4/projects/700000001/issues/7/notes/"+strconv.FormatInt(n.ID, 10) {
		t.Fatalf("call = %+v, want PUT of the note", c)
	}
	got, _ := r.gl.note("a", n.ID)
	if got.Body != "second\n\n<!-- uma:"+string(opA.ID)+":"+string(tryOne)+" -->" {
		t.Fatalf("stored body = %q", got.Body)
	}
}

func TestEditDescription(t *testing.T) {
	r := newRig(t)
	out, err := r.edit("issue:3000007", "New description.")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	c := r.gl.calls("a")[0]
	var req map[string]string
	_ = json.Unmarshal([]byte(c.Body), &req)
	if c.Method != http.MethodPut || c.Path != "/api/v4/projects/700000001/issues/7" || req["description"] == "" || req["body"] != "" {
		t.Fatalf("call = %+v, want PUT of the issue's description", c)
	}
}

func TestMutationRefusals(t *testing.T) {
	r := newRig(t)
	for name, fn := range map[string]func() (adapter.Outcome, error){
		"internal edit":      func() (adapter.Outcome, error) { return internalEdit(r) },
		"delete description": func() (adapter.Outcome, error) { return r.del("issue:3000007") },
		"non-text edit": func() (adapter.Outcome, error) {
			return r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{
				Effect: r.target("note:1"), Content: model.Content{Parts: []model.Part{{Kind: model.PartStructured}}},
			})
		},
	} {
		out, err := fn()
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Unsupported {
			t.Errorf("%s = %+v, %v; want rejected unsupported", name, out, err)
		}
	}
	for _, id := range []string{"comment:1", "note:", "note:0", "note:01", "issue:x", "1"} {
		_, err := r.edit(id, "x")
		wantCode(t, err, model.Validation)
	}
	if n := len(r.gl.calls("a")); n != 0 {
		t.Fatalf("%d provider calls for refused mutations, want none", n)
	}
}

func internalEdit(r *rig) (adapter.Outcome, error) {
	e := r.target("note:1")
	e.Audience = model.Audience{Visibility: model.Internal}
	return r.inst.(adapter.Editor).Edit(r.t.Context(), adapter.EditRequest{Effect: e, Content: textOf("x")})
}

func TestDeleteNote(t *testing.T) {
	r := newRig(t)
	n := r.gl.seed("a", sandboxProject, 7, 5000001, "gone", t0)
	id := "note:" + strconv.FormatInt(n.ID, 10)
	out, err := r.del(id)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if _, ok := r.gl.note("a", n.ID); ok {
		t.Fatal("note still stored")
	}
	// A note that is gone: rejected not_found, never retried, no binding.
	out, err = r.del(id)
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound || out.Binding != "" {
		t.Fatalf("Delete of a gone note = %+v, %v", out, err)
	}
}

func TestMutationAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		ans  answer
		code model.ErrorCode
		err  bool
	}{
		{name: "403", ans: statusAnswer(403, `{"message":"403 Forbidden"}`, nil), code: model.Unauthorized},
		{name: "400", ans: statusAnswer(400, `{"error":"body is missing"}`, nil), code: model.Validation},
		{name: "429", ans: statusAnswer(429, "Retry later", map[string]string{"Retry-After": "5"}), code: model.RateLimited, err: true},
		{name: "500", ans: statusAnswer(500, "", nil), code: model.OutcomeUnknown, err: true},
		{name: "lost", ans: answerLost, code: model.OutcomeUnknown, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			n := r.gl.seed("a", sandboxProject, 7, 5000001, "x", t0)
			r.gl.respond("a", tc.ans)
			out, err := r.edit("note:"+strconv.FormatInt(n.ID, 10), "y")
			if tc.err {
				wantCode(t, err, tc.code)
				return
			}
			if err != nil || out.State != model.Rejected || out.Failure.Code != tc.code {
				t.Fatalf("Edit = %+v, %v; want rejected %s", out, err, tc.code)
			}
		})
	}
}

// Any note the token may change is changed: GitLab's 403 is the answer for
// one it may not.
func TestEditOthersNote(t *testing.T) {
	r := newRig(t)
	n := r.gl.seed("a", sandboxProject, 7, 5000001, "theirs", t0)
	if out, err := r.edit("note:"+strconv.FormatInt(n.ID, 10), "mine"); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit of another user's note = %+v, %v", out, err)
	}
	r.gl.account("a", botID).noOwnership = true
	out, err := r.edit("note:"+strconv.FormatInt(n.ID, 10), "again")
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Unauthorized {
		t.Fatalf("Edit without rights = %+v, %v; want rejected unauthorized", out, err)
	}
}

func TestReconcileEditByMarker(t *testing.T) {
	r := newRig(t)
	n := r.gl.seed("a", sandboxProject, 7, 5000001, "first", t0)
	id := "note:" + strconv.FormatInt(n.ID, 10)
	r.gl.respond("a", answerLost)
	_, err := r.edit(id, "second")
	wantCode(t, err, model.OutcomeUnknown)
	if out, err := r.reconcileMutation(id, model.CapabilityEdit); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of an applied edit = %+v, %v", out, err)
	}
	// The same attempt's marker is not another attempt's.
	e := r.target(id)
	e.Attempt = tryTwo
	out, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityEdit})
	if err != nil || out.State != model.Failed || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile of another attempt = %+v, %v; want failed not_found", out, err)
	}
	// Not applied: the request never arrived.
	r.gl.respond("a", requestLost)
	_, _ = r.edit(id, "third")
	e.Attempt = tryOne
	if out, err = r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: e, Capability: model.CapabilityEdit}); err != nil || out.State != model.ProviderAccepted {
		// tryOne's marker is still the body from the first edit.
		t.Fatalf("Reconcile = %+v, %v", out, err)
	}
	if out, err = r.reconcileMutation("note:999", model.CapabilityEdit); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile of a gone note = %+v, %v; want rejected not_found", out, err)
	}
}

func TestReconcileEditOfDescription(t *testing.T) {
	r := newRig(t)
	r.gl.respond("a", answerLost)
	_, err := r.edit("issue:3000007", "d")
	wantCode(t, err, model.OutcomeUnknown)
	if out, err := r.reconcileMutation("issue:3000007", model.CapabilityEdit); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile = %+v, %v", out, err)
	}
}

func TestReconcileDelete(t *testing.T) {
	r := newRig(t)
	n := r.gl.seed("a", sandboxProject, 7, 5000001, "x", t0)
	id := "note:" + strconv.FormatInt(n.ID, 10)
	if out, err := r.reconcileMutation(id, model.CapabilityDelete); err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile of a note still there = %+v, %v; want failed", out, err)
	}
	r.gl.respond("a", answerLost)
	_, err := r.del(id)
	wantCode(t, err, model.OutcomeUnknown)
	if out, err := r.reconcileMutation(id, model.CapabilityDelete); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile of a deleted note = %+v, %v; want applied", out, err)
	}
	if out, err := r.reconcileMutation("issue:3000007", model.CapabilityDelete); err != nil || out.State != model.Rejected {
		t.Fatalf("Reconcile of a description delete = %+v, %v", out, err)
	}
	_, err = r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: r.target(id), Capability: model.CapabilityReact})
	wantCode(t, err, model.Unsupported)
}
