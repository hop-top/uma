package jira_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func TestSendPostsADFWithEchoProperty(t *testing.T) {
	r := newRig(t)
	out, err := r.send(tryOne, "Thanks, looking into it.\nWill report back.\n\nRegards")
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	cs := r.j.commentsOf(r.a, issueID)
	if len(cs) != 1 || out.References[0] != commentRef(cs[0].ID) {
		t.Fatalf("comments %+v, outcome %+v", cs, out)
	}
	var doc struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
		Content []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(cs[0].Body, &doc); err != nil {
		t.Fatal(err)
	}
	// Two paragraphs; the newline inside the first is a hard break.
	if doc.Type != "doc" || doc.Version != 1 || len(doc.Content) != 2 || doc.Content[0].Type != "paragraph" ||
		len(doc.Content[0].Content) != 3 || doc.Content[0].Content[1].Type != "hardBreak" || doc.Content[1].Content[0].Text != "Regards" {
		t.Fatalf("ADF body = %s", cs[0].Body)
	}
	var echo struct{ Operation, Attempt model.ID }
	if err := json.Unmarshal(cs[0].Props[jira.EchoPropertyKey], &echo); err != nil || echo.Operation != opA.ID || echo.Attempt != tryOne {
		t.Fatalf("echo property = %s (%v)", cs[0].Props[jira.EchoPropertyKey], err)
	}
	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@example.com:api-token")); r.a.auth != want {
		t.Fatalf("Authorization = %q, want %q", r.a.auth, want)
	}
	if p := r.posts(); len(p) != 1 || p[0].Path != "/rest/api/3/issue/"+issueID+"/comment" {
		t.Fatalf("posts = %+v", p)
	}
}

func TestSendRefusals(t *testing.T) {
	r := newRig(t)
	nonText := model.Content{Parts: []model.Part{{Kind: model.PartStructured}}}
	long := text(strings.Repeat("é", 32768))
	for name, tc := range map[string]struct {
		req  adapter.SendRequest
		want model.ErrorCode
	}{
		"internal audience": {adapter.SendRequest{Effect: func() adapter.Effect {
			e := effect(tryOne)
			e.Audience = model.Audience{Visibility: model.Internal}
			return e
		}(), Content: text("x")}, model.Unsupported},
		"non-text part":  {adapter.SendRequest{Effect: effect(tryOne), Content: nonText}, model.Unsupported},
		"no text":        {adapter.SendRequest{Effect: effect(tryOne), Content: model.Content{}}, model.Validation},
		"blank text":     {adapter.SendRequest{Effect: effect(tryOne), Content: text("\n\n")}, model.Validation},
		"over the limit": {adapter.SendRequest{Effect: effect(tryOne), Content: long}, model.Validation},
		"foreign route": {adapter.SendRequest{Effect: func() adapter.Effect {
			e := effect(tryOne)
			e.Route.Namespace = "other"
			return e
		}(), Content: text("x")}, model.Validation},
		"key as route": {adapter.SendRequest{Effect: func() adapter.Effect {
			e := effect(tryOne)
			e.Route.ID = "SBX-1"
			return e
		}(), Content: text("x")}, model.Validation},
	} {
		_, err := r.inst.(adapter.Sender).Send(t.Context(), tc.req)
		if !model.IsCode(err, tc.want) {
			t.Errorf("%s: err = %v, want %s", name, err, tc.want)
		}
	}
	if n := len(r.j.callsOf(r.a)); n != 0 {
		t.Fatalf("refused sends made %d calls", n)
	}
	// A 32767 character comment is at the limit and goes out.
	if out, err := r.inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: effect(tryOne), Content: text(strings.Repeat("é", 32767))}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("limit-sized send = %+v, %v", out, err)
	}
	if !utf8.ValidString(string(r.j.commentsOf(r.a, issueID)[0].Body)) {
		t.Fatal("body is not valid UTF-8")
	}
}

func TestSendDisabledWithoutReplies(t *testing.T) {
	r := newRig(t)
	inst := r.open(jira.Config{SiteURL: "https://" + host})
	_, err := inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: effect(tryOne), Content: text("x")})
	if !model.IsCode(err, model.Disabled) {
		t.Fatalf("err = %v, want disabled", err)
	}
	caps, _ := inst.Capabilities(t.Context())
	for _, c := range caps {
		want := c.Name == model.CapabilityReceiveHTTP || c.Name == model.CapabilityRefetch
		if c.Available != want || (!want && c.Reason != model.Disabled) {
			t.Errorf("capability %s = %+v", c.Name, c)
		}
	}
}

func TestSendFailureMapping(t *testing.T) {
	for name, tc := range map[string]struct {
		script  answer
		state   model.DeliveryState
		code    model.ErrorCode
		isError bool
		wait    time.Duration
	}{
		"bad request":                     {script: answer{status: http.StatusBadRequest}, state: model.Rejected, code: model.Validation},
		"too large":                       {script: answer{status: http.StatusRequestEntityTooLarge}, state: model.Rejected, code: model.Validation},
		"forbidden":                       {script: answer{status: http.StatusForbidden}, code: model.Unauthorized, isError: true},
		"rate limited":                    {script: answer{status: http.StatusTooManyRequests, retry: "7"}, code: model.RateLimited, isError: true, wait: 7 * time.Second},
		"rate no header":                  {script: answer{status: http.StatusTooManyRequests}, code: model.RateLimited, isError: true, wait: time.Minute},
		"server error":                    {script: answer{status: http.StatusInternalServerError}, code: model.OutcomeUnknown, isError: true},
		"answer lost":                     {script: answer{status: http.StatusBadGateway, applied: true}, code: model.OutcomeUnknown, isError: true},
		"dial failure":                    {script: answer{dial: true}, code: model.Transient, isError: true},
		"connection lost after the write": {script: answer{reset: true}, code: model.OutcomeUnknown, isError: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.j.respond(r.a, tc.script)
			out, err := r.send(tryOne, "hello")
			if tc.isError {
				me, ok := err.(*model.Error)
				if !ok || me.Code != tc.code || (tc.wait != 0 && me.RetryAfter != tc.wait) {
					t.Fatalf("err = %#v, want %s retry %s", err, tc.code, tc.wait)
				}
				return
			}
			if err != nil || out.State != tc.state || out.Failure == nil || out.Failure.Code != tc.code {
				t.Fatalf("out = %+v, %v", out, err)
			}
		})
	}
}

func TestSendMissingIssueIsInaccessible(t *testing.T) {
	r := newRig(t)
	e := effect(tryOne)
	e.Route = route("99999")
	out, err := r.inst.(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: e, Content: text("x")})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound || out.Binding != model.WorkItemBindingInaccessible {
		t.Fatalf("out = %+v, %v; want rejected not_found, binding inaccessible", out, err)
	}
}

// A 401 re-resolves the credential once with Fresh set; nothing was
// applied, so the request is repeated.
func TestUnauthorizedRetriesOnceWithFreshCredential(t *testing.T) {
	r := newRig(t)
	r.j.respond(r.a, answer{status: http.StatusUnauthorized})
	out, err := r.send(tryOne, "hello")
	if err != nil || out.State != model.ProviderAccepted || r.creds.fresh() != 1 {
		t.Fatalf("out = %+v, %v, fresh = %d", out, err, r.creds.fresh())
	}
	r.j.respond(r.a, answer{status: http.StatusUnauthorized}, answer{status: http.StatusUnauthorized})
	if _, err := r.send(tryTwo, "again"); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("persistent 401: err = %v, want unauthorized", err)
	}
}

func TestCredentialKinds(t *testing.T) {
	r := newRig(t)
	send := func(cfg jira.Config, cred ports.Credential) error {
		r.creds.cred = cred
		_, err := r.open(cfg).(adapter.Sender).Send(t.Context(), adapter.SendRequest{Effect: effect(tryOne), Content: text("x")})
		return err
	}
	basicOf := func(s string) string { return "Basic " + base64.StdEncoding.EncodeToString([]byte(s)) }

	cfg := replyConfig()
	cfg.Email = "bot@example.com"
	if err := send(cfg, ports.Secret{Bytes: []byte("tok")}); err != nil || r.a.auth != basicOf("bot@example.com:tok") {
		t.Fatalf("secret with email: %v, %q", err, r.a.auth)
	}
	if err := send(replyConfig(), ports.Secret{Bytes: []byte("tok")}); !model.IsCode(err, model.Validation) {
		t.Fatalf("bare secret without email: %v, want validation", err)
	}
	if err := send(replyConfig(), ports.OAuth2{AccessToken: "oauth-token"}); !model.IsCode(err, model.Validation) { // #nosec G101 -- fixture value
		t.Fatalf("OAuth 2.0 without cloud_id: %v, want validation", err)
	}
	if err := send(replyConfig(), ports.Password{User: "bot@example.com"}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("password without secret: %v, want unauthorized", err)
	}
	if err := send(replyConfig(), ports.Secret{Bytes: []byte("tok"), ExpiresAt: t0.Add(-time.Hour)}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("expired: %v", err)
	}

	// OAuth 2.0 (3LO): the API gateway, bearer.
	gw := r.j.account("api.atlassian.com")
	r.j.putIssue(gw, issueID, "SBX-1", "new")
	cfg = replyConfig()
	cfg.CloudID = "1324a887-45db-1bf4-1e99-ef0ff456d421"
	if err := send(cfg, ports.OAuth2{AccessToken: "oauth-token"}); err != nil || gw.auth != "Bearer oauth-token" { // #nosec G101 -- fixture value
		t.Fatalf("OAuth 2.0: %v, %q", err, gw.auth)
	}
	if c := r.j.callsOf(gw); len(c) != 1 || c[0].Path != "/ex/jira/"+cfg.CloudID+"/rest/api/3/issue/"+issueID+"/comment" {
		t.Fatalf("gateway calls = %+v", c)
	}
}

func TestEditAndDeleteComment(t *testing.T) {
	r := newRig(t)
	if _, err := r.send(tryOne, "first"); err != nil {
		t.Fatal(err)
	}
	id := r.j.commentsOf(r.a, issueID)[0].ID

	// Edit: the body is replaced and the echo property names the new attempt.
	out, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: r.target(tryTwo, id), Content: text("second")})
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	c := r.j.commentsOf(r.a, issueID)[0]
	if adfText(c.Body) != "second" || !strings.Contains(string(c.Props[jira.EchoPropertyKey]), string(tryTwo)) {
		t.Fatalf("comment after edit: %s %s", c.Body, c.Props[jira.EchoPropertyKey])
	}

	// Another author's comment is edited too: no authorship check.
	r.j.putComment(r.a, &fakeComment{ID: "20001", Issue: issueID, Author: "someone-else", Body: adfOf("theirs"), Created: t0, Updated: t0, Props: map[string]json.RawMessage{}})
	if out, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: r.target(tryOne, "20001"), Content: text("moderated")}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit of another author's comment = %+v, %v", out, err)
	}

	out, err = r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: r.target(tryOne, id)})
	if err != nil || out.State != model.ProviderAccepted || len(r.j.commentsOf(r.a, issueID)) != 1 {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
}

func TestMutationRefusals(t *testing.T) {
	r := newRig(t)
	calls := func() int { return len(r.j.callsOf(r.a)) }
	edit := func(e adapter.Effect) (adapter.Outcome, error) {
		return r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: e, Content: text("x")})
	}

	// The description is not editable or deletable: rejected, no call.
	desc := effect(tryOne)
	ref := model.ProviderMessageRef(chanA, "issue:"+issueID)
	desc.Target = &ref
	if out, err := edit(desc); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("edit description = %+v, %v", out, err)
	}
	if out, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: desc}); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("delete description = %+v, %v", out, err)
	}
	// A non-public edit is rejected before any call; malformed targets are errors.
	internal := r.target(tryOne, "20001")
	internal.Audience = model.Audience{Visibility: model.Internal}
	if out, err := edit(internal); err != nil || out.State != model.Rejected || out.Failure.Code != model.Unsupported {
		t.Fatalf("internal edit = %+v, %v", out, err)
	}
	for _, id := range []string{"", "comment:abc", "comment:0", "20001"} {
		e := effect(tryOne)
		bad := model.ProviderMessageRef(chanA, id)
		e.Target = &bad
		if _, err := edit(e); !model.IsCode(err, model.Validation) {
			t.Errorf("target %q: err = %v, want validation", id, err)
		}
	}
	if _, err := edit(effect(tryOne)); !model.IsCode(err, model.Validation) {
		t.Errorf("no target: err = %v, want validation", err)
	}
	if calls() != 0 {
		t.Fatalf("refusals made %d calls", calls())
	}

	// A comment that is gone, or a refusal by Jira, is a final rejection with no binding.
	out, err := edit(r.target(tryOne, "99999"))
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound || out.Binding != "" {
		t.Fatalf("edit of a missing comment = %+v, %v", out, err)
	}
	r.j.putComment(r.a, &fakeComment{ID: "20002", Issue: issueID, Author: "x", Body: adfOf("t"), Created: t0, Updated: t0, Props: map[string]json.RawMessage{}})
	for status, code := range map[int]model.ErrorCode{http.StatusForbidden: model.Unauthorized, http.StatusBadRequest: model.Validation, http.StatusMethodNotAllowed: model.Unauthorized} {
		r.j.respond(r.a, answer{status: status})
		if out, err := edit(r.target(tryOne, "20002")); err != nil || out.State != model.Rejected || out.Failure.Code != code {
			t.Errorf("status %d: %+v, %v; want rejected %s", status, out, err, code)
		}
	}
	// A lost answer after the write is outcome unknown.
	r.j.respond(r.a, answer{status: http.StatusBadGateway, applied: true})
	if _, err := edit(r.target(tryTwo, "20002")); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatalf("lost answer: err = %v, want outcome_unknown", err)
	}
}

func TestReconcileReply(t *testing.T) {
	r := newRig(t)
	// The answer is lost after the comment was created: reconcile finds it.
	r.j.respond(r.a, answer{status: http.StatusBadGateway, applied: true})
	if _, err := r.send(tryOne, "hello"); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatalf("lost answer: %v", err)
	}
	made := r.j.commentsOf(r.a, issueID)[0]
	// A newer comment by another account carrying the same property is not
	// uma's: it is listed first and must be passed over.
	r.j.putComment(r.a, &fakeComment{ID: "20009", Issue: issueID, Author: "someone-else", Body: adfOf("copy"), Created: t0, Updated: t0,
		Props: map[string]json.RawMessage{jira.EchoPropertyKey: json.RawMessage(`{"operation":"` + string(opA.ID) + `","attempt":"` + string(tryOne) + `"}`)}})
	out, err := r.reconcile(tryOne)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != commentRef(made.ID) {
		t.Fatalf("reconcile = %+v, %v; want the created comment", out, err)
	}
	// Another attempt of the operation was never made: failed, so a retry is safe.
	out, err = r.reconcile(tryTwo)
	if err != nil || out.State != model.Failed || out.Failure.Code != model.NotFound {
		t.Fatalf("reconcile of an unmade attempt = %+v, %v", out, err)
	}
	// Comments older than the window are not looked at.
	r.clock.Set(t0.Add(jira.ReconcileWindow + 48*time.Hour))
	if out, err := r.reconcile(tryOne); err != nil || out.State != model.Failed {
		t.Fatalf("reconcile past the window = %+v, %v", out, err)
	}
}

func TestReconcileMutations(t *testing.T) {
	r := newRig(t)
	if _, err := r.send(tryOne, "first"); err != nil {
		t.Fatal(err)
	}
	id := r.j.commentsOf(r.a, issueID)[0].ID
	rec := func(c model.Capability, attempt model.ID, cid string) (adapter.Outcome, error) {
		return r.inst.(adapter.Reconciler).Reconcile(t.Context(), adapter.ReconcileRequest{Effect: r.target(attempt, cid), Capability: c})
	}
	// Edit: applied only when the echo property names the attempt.
	r.j.respond(r.a, answer{status: http.StatusBadGateway, applied: true})
	if _, err := r.inst.(adapter.Editor).Edit(t.Context(), adapter.EditRequest{Effect: r.target(tryTwo, id), Content: text("second")}); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatal(err)
	}
	if out, err := rec(model.CapabilityEdit, tryTwo, id); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("edit applied: %+v, %v", out, err)
	}
	if out, err := rec(model.CapabilityEdit, model.ID("00000000-0000-7000-8000-00000000b0b3"), id); err != nil || out.State != model.Failed || out.Failure.Code != model.NotFound {
		t.Fatalf("edit not applied: %+v, %v", out, err)
	}
	if out, err := rec(model.CapabilityEdit, tryTwo, "99999"); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("edit of a missing comment: %+v, %v", out, err)
	}
	// Delete: applied once the comment is gone.
	if out, err := rec(model.CapabilityDelete, tryOne, id); err != nil || out.State != model.Failed {
		t.Fatalf("delete not applied: %+v, %v", out, err)
	}
	r.j.respond(r.a, answer{status: http.StatusBadGateway, applied: true})
	if _, err := r.inst.(adapter.Deleter).Delete(t.Context(), adapter.DeleteRequest{Effect: r.target(tryOne, id)}); !model.IsCode(err, model.OutcomeUnknown) {
		t.Fatal(err)
	}
	if out, err := rec(model.CapabilityDelete, tryOne, id); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("delete applied: %+v, %v", out, err)
	}
}

func TestRefetch(t *testing.T) {
	r := newRig(t)
	r.j.putComment(r.a, &fakeComment{ID: "20003", Issue: issueID, Author: "712020:a", Body: adfOf("current text"), Created: t0, Updated: t0.Add(time.Minute), Props: map[string]json.RawMessage{}})
	f := r.inst.(adapter.Refetcher)

	env, err := f.Refetch(t.Context(), adapter.RefetchRequest{Target: commentRef("20003"), Destination: "SBX", References: []model.ExternalRef{route(issueID)}})
	if err != nil || env.Kind != model.EnvelopeMessageEdited || env.MessageID != "comment:20003" || env.Content.Parts[0].Text != "current text" ||
		env.Sender.ID != "712020:a" || env.Destination != "SBX" || env.SourceRevision == "" {
		t.Fatalf("comment refetch = %+v, %v", env, err)
	}
	env, err = f.Refetch(t.Context(), adapter.RefetchRequest{Target: route(issueID), Destination: "SBX"})
	if err != nil || env.Kind != model.EnvelopeWorkItem || env.WorkItem == nil || env.WorkItem.Status != model.WorkItemOpen ||
		env.WorkItem.RawStatus != "To Do" || env.WorkItem.URL != "https://"+host+"/browse/SBX-1" || env.WorkItem.Validate() != nil {
		t.Fatalf("issue refetch = %+v, %v", env, err)
	}
	r.j.putIssue(r.a, issueID, "SBX-1", "done")
	env, _ = f.Refetch(t.Context(), adapter.RefetchRequest{Target: route(issueID), Destination: "SBX"})
	if env.WorkItem.Status != model.WorkItemClosed {
		t.Fatalf("done category = %q, want closed", env.WorkItem.Status)
	}

	// Missing resources and malformed targets.
	if _, err := f.Refetch(t.Context(), adapter.RefetchRequest{Target: commentRef("99999"), References: []model.ExternalRef{route(issueID)}}); !model.IsCode(err, model.NotFound) {
		t.Errorf("missing comment: %v", err)
	}
	if _, err := f.Refetch(t.Context(), adapter.RefetchRequest{Target: commentRef("20003")}); !model.IsCode(err, model.Validation) {
		t.Errorf("comment without a work item: %v", err)
	}
	if _, err := f.Refetch(t.Context(), adapter.RefetchRequest{Target: model.ProviderMessageRef(chanA, "issue:"+issueID), References: []model.ExternalRef{route(issueID)}}); !model.IsCode(err, model.Validation) {
		t.Errorf("description target: %v", err)
	}
}
