package linear_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	linear "hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func TestSendCreatesCommentWithDerivedIDAndMarker(t *testing.T) {
	r := newRig(t)
	out, err := r.send(tryOne)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	calls := r.lin.callsOf("a")
	if len(calls) != 1 || calls[0].Op != "commentCreate" || calls[0].Auth != apiKey {
		t.Fatalf("calls = %+v, want one commentCreate with the bare API key", calls)
	}
	in := calls[0].Vars["input"].(map[string]any)
	id := in["id"].(string)
	if in["issueId"] != issueA || out.References[0] != commentRef(chanA, id) {
		t.Fatalf("input %v, ref %v", in, out.References)
	}
	if body := in["body"].(string); !strings.HasPrefix(body, "Thanks, looking into it.\n\n<!-- uma:"+string(opA.ID)+":"+string(tryOne)) {
		t.Fatalf("body %q: want the text then the attempt marker", body)
	}
	// The ID is a version 4 UUID, stable per attempt and distinct across them.
	if len(id) != 36 || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
		t.Fatalf("comment id %q is not a v4 UUID", id)
	}
	other, _ := r.send(tryTwo)
	if other.References[0] == out.References[0] {
		t.Fatal("two attempts derived one comment id")
	}
}

func TestSendSendsOAuthTokenAsBearer(t *testing.T) {
	r := newRig(t)
	r.creds.kind = ports.KindOAuth2
	if _, err := r.send(tryOne); err != nil {
		t.Fatal(err)
	}
	if got := r.lin.callsOf("a")[0].Auth; got != "Bearer "+apiKey {
		t.Fatalf("Authorization = %q, want Bearer <token>", got)
	}
}

func TestSendRefusals(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		mutate func(*adapter.SendRequest)
		code   model.ErrorCode
	}{
		"internal":      {func(q *adapter.SendRequest) { q.Audience.Visibility = model.Internal }, model.Unsupported},
		"attachment":    {func(q *adapter.SendRequest) { q.Content.Parts = []model.Part{{Kind: model.PartAttachment}} }, model.Unsupported},
		"empty":         {func(q *adapter.SendRequest) { q.Content = model.Content{} }, model.Validation},
		"foreign route": {func(q *adapter.SendRequest) { q.Route.Namespace = "other" }, model.Validation},
		"bad issue id":  {func(q *adapter.SendRequest) { q.Route.ID = "ENG-7" }, model.Validation},
	} {
		q := sendReq(tryOne, "hi")
		c.mutate(&q)
		if _, err := r.inst.(adapter.Sender).Send(t.Context(), q); !model.IsCode(err, c.code) {
			t.Errorf("%s: err = %v, want %s", name, err, c.code)
		}
	}
	if n := len(r.lin.callsOf("a")); n != 0 {
		t.Fatalf("%d provider calls for refused sends", n)
	}
}

func TestRepliesDisabled(t *testing.T) {
	r := newRig(t)
	inst := r.open(linear.Config{APIURL: r.lin.apiURL("a")})
	if _, err := inst.(adapter.Sender).Send(t.Context(), sendReq(tryOne, "hi")); !model.IsCode(err, model.Disabled) {
		t.Fatalf("Send = %v, want disabled", err)
	}
	st, _ := inst.Capabilities(t.Context())
	for _, s := range st {
		switch s.Name {
		case model.CapabilitySend, model.CapabilityEdit, model.CapabilityDelete:
			if s.Available || s.Reason != model.Disabled {
				t.Errorf("%s = %+v, want unavailable, disabled", s.Name, s)
			}
		default:
			if !s.Available {
				t.Errorf("%s unavailable without replies", s.Name)
			}
		}
	}
}

// A lost answer is reconciled by reading the comment the attempt names.
func TestSendAnswerLostReconcilesByID(t *testing.T) {
	r := newRig(t)
	r.lin.respond("a", answerLost)
	if _, err := r.send(tryOne); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatalf("Send = %v, want outcome_unknown", err)
	}
	found, err := r.reconcile(tryOne)
	if err != nil || found.State != model.ProviderAccepted || len(found.References) != 1 {
		t.Fatalf("Reconcile = %+v, %v; want the stored comment", found, err)
	}
	if n := len(r.lin.commentsOn("a", issueA)); n != 1 {
		t.Fatalf("%d comments, want 1", n)
	}
	// The request never arrived: nothing is recorded, a retry is safe.
	r.lin.respond("a", requestLost)
	if _, err := r.send(tryTwo); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatalf("Send = %v, want outcome_unknown", err)
	}
	none, err := r.reconcile(tryTwo)
	if err != nil || none.State != model.Failed || none.Failure == nil || none.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile = %+v, %v; want failed not_found", none, err)
	}
}

func TestSendFailureMapping(t *testing.T) {
	rateBody := `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`
	for name, c := range map[string]struct {
		ans   answer
		code  model.ErrorCode
		state model.DeliveryState
		wait  time.Duration
	}{
		"429 retry-after":      {statusAnswer(429, rateBody, map[string]string{"Retry-After": "42"}), model.RateLimited, "", 42 * time.Second},
		"400 ratelimited code": {statusAnswer(400, rateBody, map[string]string{"X-RateLimit-Requests-Remaining": "0", "X-RateLimit-Requests-Reset": "1791720090000"}), model.RateLimited, "", 90 * time.Second},
		"200 ratelimited":      {statusAnswer(200, rateBody, nil), model.RateLimited, "", time.Minute},
		"401":                  {statusAnswer(401, `{"errors":[{"message":"Authentication required","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`, nil), model.Unauthorized, "", 0},
		"403":                  {statusAnswer(403, `{"errors":[{"message":"Forbidden","extensions":{"type":"forbidden"}}]}`, nil), model.Unauthorized, model.Rejected, 0},
		"invalid input":        {statusAnswer(400, `{"errors":[{"message":"Argument Validation Error","extensions":{"type":"invalid input"}}]}`, nil), model.Validation, model.Rejected, 0},
		"issue gone":           {statusAnswer(400, `{"errors":[{"message":"Entity not found: Issue","extensions":{"type":"invalid input"}}]}`, nil), model.NotFound, model.Rejected, 0},
		"502":                  {statusAnswer(502, "<html>bad gateway</html>", nil), model.OutcomeUnknown, "", 0},
		"unreachable":          {unreachable, model.Transient, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.clock.Set(time.UnixMilli(1791720000000).UTC())
			r.lin.respond("a", c.ans)
			if c.ans.status == 401 {
				// Refused again after the fresh credential.
				r.lin.respond("a", c.ans)
			}
			out, err := r.send(tryOne)
			if c.state != "" {
				if err != nil || out.State != c.state || out.Failure == nil || out.Failure.Code != c.code {
					t.Fatalf("Send = %+v, %v; want %s %s", out, err, c.state, c.code)
				}
				if c.code == model.NotFound && out.Binding != model.WorkItemBindingInaccessible {
					t.Fatalf("binding = %q, want inaccessible", out.Binding)
				}
				return
			}
			me, ok := err.(*model.Error)
			if !ok || me.Code != c.code {
				t.Fatalf("Send = %+v, %v; want error %s", out, err, c.code)
			}
			if c.wait != 0 && me.RetryAfter != c.wait {
				t.Fatalf("retry after %v, want %v", me.RetryAfter, c.wait)
			}
		})
	}
}

// A 401 re-resolves the credential once with Fresh set.
func TestSendUnauthorizedRefreshesCredentialOnce(t *testing.T) {
	r := newRig(t)
	r.lin.respond("a", statusAnswer(http.StatusUnauthorized, `{"errors":[{"message":"x","extensions":{"code":"AUTHENTICATION_ERROR"}}]}`, nil))
	out, err := r.send(tryOne)
	if err != nil || out.State != model.ProviderAccepted || r.creds.fresh() != 1 {
		t.Fatalf("Send = %+v, %v with %d fresh resolutions, want accepted after one", out, err, r.creds.fresh())
	}
}

func TestEditAndDeleteComment(t *testing.T) {
	r := newRig(t)
	sent, err := r.send(tryOne)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimPrefix(sent.References[0].ID, "comment:")
	edit := adapter.EditRequest{Effect: target(tryTwo, id), Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Fixed."}}}}
	if out, err := r.inst.(adapter.Editor).Edit(t.Context(), edit); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	c, _ := r.lin.comment("a", id)
	if !strings.HasPrefix(c.Body, "Fixed.\n\n<!-- uma:"+string(opA.ID)+":"+string(tryTwo)) {
		t.Fatalf("body %q", c.Body)
	}
	// Lost edit answers reconcile from the marker.
	rc := adapter.ReconcileRequest{Effect: edit.Effect, Capability: model.CapabilityEdit}
	if out, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), rc); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("reconcile edit = %+v, %v", out, err)
	}
	rc.Attempt = "00000000-0000-7000-8000-00000000b0b9"
	if out, _ := r.inst.(adapter.Reconciler).Reconcile(t.Context(), rc); out.State != model.Failed {
		t.Fatalf("reconcile of another attempt = %+v, want failed", out)
	}
	del := adapter.DeleteRequest{Effect: target(tryTwo, id)}
	drc := adapter.ReconcileRequest{Effect: del.Effect, Capability: model.CapabilityDelete}
	if out, _ := r.inst.(adapter.Reconciler).Reconcile(t.Context(), drc); out.State != model.Failed {
		t.Fatalf("reconcile of a present comment = %+v, want failed", out)
	}
	if out, err := r.inst.(adapter.Deleter).Delete(t.Context(), del); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	if out, err := r.inst.(adapter.Reconciler).Reconcile(t.Context(), drc); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("reconcile of a deleted comment = %+v, %v; want applied", out, err)
	}
	// The comment is gone: editing it is a final rejection, never a new comment.
	out, err := r.inst.(adapter.Editor).Edit(t.Context(), edit)
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("Edit of a deleted comment = %+v, %v; want rejected not_found", out, err)
	}
	if out, _ := r.inst.(adapter.Reconciler).Reconcile(t.Context(), rc); out.State != model.Rejected {
		t.Fatalf("reconcile edit of a deleted comment = %+v, want rejected", out)
	}
}

func TestMutationRefusals(t *testing.T) {
	r := newRig(t)
	desc := target(tryOne, "")
	ref := model.ProviderMessageRef(chanA, "issue:"+issueA)
	desc.Target = &ref
	del, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: desc})
	if err != nil || del.State != model.Rejected || del.Failure.Code != model.Unsupported {
		t.Fatalf("Delete of a description = %+v, %v", del, err)
	}
	bad := target(tryOne, "not-a-uuid")
	if _, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: bad}); !model.IsCode(err, model.Validation) {
		t.Fatalf("Delete = %v, want validation", err)
	}
	e := adapter.EditRequest{Effect: target(tryOne, botID), Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "x"}}}}
	e.Audience.Visibility = model.Internal
	if out, _ := r.inst.(adapter.Editor).Edit(t.Context(), e); out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("internal edit = %+v", out)
	}
	if n := len(r.lin.callsOf("a")); n != 0 {
		t.Fatalf("%d provider calls for refused mutations", n)
	}
}

func TestRefetch(t *testing.T) {
	r := newRig(t)
	r.lin.putIssue("a", fakeIssue{ID: issueB, Identifier: "ENG-8", Title: "Slow", Team: "OPS", StateType: "completed", StateName: "Done", Description: "Details", Creator: personID, Labels: []string{"bug", "api"}, Updated: t0})
	rf := r.inst.(adapter.Refetcher)
	env, err := rf.Refetch(t.Context(), adapter.RefetchRequest{Target: issueRoute(issueB)})
	if err != nil || env.Kind != model.EnvelopeWorkItem || env.Destination != "OPS" || env.WorkItem == nil {
		t.Fatalf("Refetch(work item) = %+v, %v", env, err)
	}
	s := env.WorkItem
	if s.Status != model.WorkItemClosed || s.RawStatus != "completed:Done" || s.Subject != "Slow" || len(s.Fields) != 1 || string(s.Fields[0].Data) != `["api","bug"]` || s.Validate() != nil {
		t.Fatalf("state = %+v", s)
	}
	r.lin.putComment("a", fakeComment{ID: botID, IssueID: issueB, Body: "Hello", UserID: personID, Created: t0})
	env, err = rf.Refetch(t.Context(), adapter.RefetchRequest{Target: commentRef(chanA, botID)})
	if err != nil || env.Kind != model.EnvelopeMessageEdited || env.MessageID != "comment:"+botID || env.Content.Parts[0].Text != "Hello" ||
		env.Destination != "OPS" || env.References[0] != issueRoute(issueB) || env.Sender.ID != personID {
		t.Fatalf("Refetch(comment) = %+v, %v", env, err)
	}
	ref := model.ProviderMessageRef(chanA, "issue:"+issueB)
	env, err = rf.Refetch(t.Context(), adapter.RefetchRequest{Target: ref, References: []model.ExternalRef{issueRoute(issueB)}})
	if err != nil || env.Content.Parts[0].Text != "Details" || env.Sender.ID != personID {
		t.Fatalf("Refetch(description) = %+v, %v", env, err)
	}
	if _, err := rf.Refetch(t.Context(), adapter.RefetchRequest{Target: issueRoute("3b9e6d12-4c57-4f0a-8d21-6a7b5c4d3eff")}); !model.IsCode(err, model.NotFound) {
		t.Fatalf("Refetch of a missing issue = %v, want not_found", err)
	}
}
