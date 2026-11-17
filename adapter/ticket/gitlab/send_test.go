package gitlab_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func (r *rig) send(attempt model.ID, text string) (adapter.Outcome, error) {
	return r.inst.(adapter.Sender).Send(r.t.Context(), sendReq(attempt, text))
}

func (r *rig) reconcile(attempt model.ID) (adapter.Outcome, error) {
	return r.inst.(adapter.Reconciler).Reconcile(r.t.Context(), adapter.ReconcileRequest{Effect: sendReq(attempt, "").Effect})
}

func TestSendPostsNoteWithMarker(t *testing.T) {
	r := newRig(t)
	out, err := r.send(tryOne, "Thanks, looking into it.")
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	calls := r.gl.calls("a")
	if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/api/v4/projects/700000001/issues/7/notes" {
		t.Fatalf("calls = %+v, want one POST of the issue's notes", calls)
	}
	if calls[0].Auth != "Bearer "+apiToken {
		t.Fatalf("Authorization = %q, want the bearer token", calls[0].Auth)
	}
	var req struct{ Body string }
	_ = json.Unmarshal([]byte(calls[0].Body), &req)
	want := "Thanks, looking into it.\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryOne) + " -->"
	if req.Body != want {
		t.Fatalf("body = %q, want %q", req.Body, want)
	}
	if out.References[0] != noteRef(calls[0].Note) {
		t.Fatalf("reference = %v, want note %d", out.References[0], calls[0].Note)
	}
}

// GitLab runs and removes any note line starting with a quick action; a
// reply must never change the issue.
func TestSendEscapesQuickActions(t *testing.T) {
	r := newRig(t)
	if _, err := r.send(tryOne, "Closing the loop:\n/close\n  /assign @someone\nsee /tmp/log and http://x/y"); err != nil {
		t.Fatal(err)
	}
	var req struct{ Body string }
	_ = json.Unmarshal([]byte(r.gl.calls("a")[0].Body), &req)
	text, _, _ := strings.Cut(req.Body, "\n\n<!--")
	want := "Closing the loop:\n\\/close\n  \\/assign @someone\nsee /tmp/log and http://x/y"
	if text != want {
		t.Fatalf("body text = %q, want %q", text, want)
	}
}

func TestSendRefusals(t *testing.T) {
	r := newRig(t)
	disabled := r.open(gitlab.Config{APIURL: r.gl.apiURL("a")})
	_, err := disabled.(adapter.Sender).Send(t.Context(), sendReq(tryOne, "x"))
	wantCode(t, err, model.Disabled)

	req := sendReq(tryOne, "x")
	req.Audience = model.Audience{Visibility: model.Internal}
	_, err = r.inst.(adapter.Sender).Send(t.Context(), req)
	wantCode(t, err, model.Unsupported)

	req = sendReq(tryOne, "x")
	req.Content = model.Content{Parts: []model.Part{{Kind: model.PartStructured}}}
	_, err = r.inst.(adapter.Sender).Send(t.Context(), req)
	wantCode(t, err, model.Unsupported)

	req = sendReq(tryOne, strings.Repeat("x", 1_000_001))
	_, err = r.inst.(adapter.Sender).Send(t.Context(), req)
	wantCode(t, err, model.Validation)

	req = sendReq(tryOne, "x")
	req.Route.ParentID = sandboxPath // a path, not the numeric project ID
	_, err = r.inst.(adapter.Sender).Send(t.Context(), req)
	wantCode(t, err, model.Validation)

	if n := len(r.gl.calls("a")); n != 0 {
		t.Fatalf("%d provider calls for refused sends, want none", n)
	}
}

func TestSendAnswers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ans     answer
		state   model.DeliveryState
		code    model.ErrorCode
		binding string
		wait    time.Duration
	}{
		{name: "400", ans: statusAnswer(400, `{"error":"body is missing"}`, nil), state: model.Rejected, code: model.Validation},
		{name: "403", ans: statusAnswer(403, `{"message":"403 Forbidden"}`, nil), code: model.Unauthorized},
		{name: "404", ans: statusAnswer(404, notFound, nil), state: model.Rejected, code: model.NotFound, binding: model.WorkItemBindingInaccessible},
		{name: "405 moved project", ans: statusAnswer(405, `{"message":"Non GET methods are not allowed for moved projects"}`, nil), state: model.Rejected, code: model.NotFound, binding: model.WorkItemBindingInaccessible},
		{name: "429 retry-after", ans: statusAnswer(429, "Retry later", map[string]string{"Retry-After": "30"}), code: model.RateLimited, wait: 30 * time.Second},
		{name: "429 reset epoch", ans: statusAnswer(429, "Retry later", map[string]string{"RateLimit-Reset": "1791720090"}), code: model.RateLimited, wait: 90 * time.Second},
		{name: "429 nothing", ans: statusAnswer(429, "Retry later", nil), code: model.RateLimited, wait: time.Minute},
		{name: "500", ans: statusAnswer(500, `{"message":"500 Internal Server Error"}`, nil), code: model.OutcomeUnknown},
		{name: "answer lost", ans: answerLost, code: model.OutcomeUnknown},
		{name: "request lost", ans: requestLost, code: model.OutcomeUnknown},
		{name: "unreachable", ans: unreachable, code: model.Transient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t)
			r.gl.respond("a", tc.ans)
			out, err := r.send(tryOne, "x")
			if tc.state != "" {
				if err != nil || out.State != tc.state || out.Failure == nil || out.Failure.Code != tc.code || out.Binding != tc.binding {
					t.Fatalf("Send = %+v, %v; want %s %s binding %q", out, err, tc.state, tc.code, tc.binding)
				}
				return
			}
			me := wantCode(t, err, tc.code)
			if tc.wait != 0 && me.RetryAfter != tc.wait {
				t.Fatalf("RetryAfter = %v, want %v", me.RetryAfter, tc.wait)
			}
		})
	}
}

func TestSendReResolvesAfter401(t *testing.T) {
	r := newRig(t)
	r.gl.respond("a", statusAnswer(401, `{"message":"401 Unauthorized"}`, nil))
	out, err := r.send(tryOne, "x")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v; want accepted after a fresh credential", out, err)
	}
	if r.creds.fresh() != 1 {
		t.Fatalf("fresh resolutions = %d, want 1", r.creds.fresh())
	}
	// Still 401: unauthorized, no third attempt.
	r.gl.respond("a", statusAnswer(401, `{}`, nil), statusAnswer(401, `{}`, nil))
	_, err = r.send(tryTwo, "x")
	wantCode(t, err, model.Unauthorized)
}

func TestSendUsesOAuthToken(t *testing.T) {
	r := newRig(t)
	r.creds.kind = ports.KindOAuth2
	if out, err := r.send(tryOne, "x"); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
}

func TestSendNeedsInjectedClient(t *testing.T) {
	r := newRig(t)
	inst, err := gitlab.NewFactory(gitlab.Options{}).Open(t.Context(), model.Channel{
		Ref: chanA, Type: gitlab.Type, Configuration: replyConfig(r.gl.apiURL("a")).Extension(), Revision: "ch1",
	}, adapter.Dependencies{Credentials: r.creds, Clock: r.clock})
	if err != nil {
		t.Fatal(err)
	}
	_, err = inst.(adapter.Sender).Send(t.Context(), sendReq(tryOne, "x"))
	wantCode(t, err, model.Unavailable)
}

func TestReconcileFindsSentNote(t *testing.T) {
	r := newRig(t)
	r.gl.respond("a", answerLost)
	_, err := r.send(tryOne, "x")
	wantCode(t, err, model.OutcomeUnknown)
	out, err := r.reconcile(tryOne)
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Reconcile = %+v, %v; want the lost note", out, err)
	}
	// An attempt that never reached GitLab is failed not_found: a retry is safe.
	out, err = r.reconcile(tryTwo)
	if err != nil || out.State != model.Failed || out.Failure == nil || out.Failure.Code != model.NotFound {
		t.Fatalf("Reconcile of an unsent attempt = %+v, %v", out, err)
	}
}

func TestReconcileIgnoresOthersAndStaleNotes(t *testing.T) {
	r := newRig(t)
	marked := "x\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryOne) + " -->"
	// The marker in a note by someone else, and by the account but older
	// than the window, is not the attempt's note.
	r.gl.seed("a", sandboxProject, 7, 5000001, marked, t0.Add(-time.Hour))
	r.gl.seed("a", sandboxProject, 7, botID, marked, t0.Add(-gitlab.ReconcileWindow-time.Hour))
	out, err := r.reconcile(tryOne)
	if err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile = %+v, %v; want failed", out, err)
	}
	// A system note by the account is activity, not a reply.
	r.gl.putNote("a", fakeNote{Issue: "700000001#7", Body: marked, Author: botID, System: true, Created: t0.Add(-time.Minute)})
	if out, err = r.reconcile(tryOne); err != nil || out.State != model.Failed {
		t.Fatalf("Reconcile with a system note = %+v, %v; want failed", out, err)
	}
	r.gl.seed("a", sandboxProject, 7, botID, marked, t0.Add(-time.Minute))
	if out, err = r.reconcile(tryOne); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile = %+v, %v; want the account's own marked note", out, err)
	}
}

func TestReconcilePagesAndIsInconclusiveWhenCutShort(t *testing.T) {
	r := newRig(t)
	marked := "x\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryOne) + " -->"
	r.gl.seed("a", sandboxProject, 7, botID, marked, t0.Add(-2*time.Hour))
	for i := range 230 {
		r.gl.seed("a", sandboxProject, 7, 5000001, "chatter", t0.Add(-time.Hour+time.Duration(i)*time.Second))
	}
	out, err := r.reconcile(tryOne)
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Reconcile over %d pages = %+v, %v; want found", 3, out, err)
	}
	calls := r.gl.calls("a")
	if len(calls) != 3 || !strings.Contains(calls[0].Query, "sort=desc") || !strings.Contains(calls[2].Query, "page=3") {
		t.Fatalf("calls = %+v, want three newest-first pages", calls)
	}
}

func TestReconcileFailuresAreErrors(t *testing.T) {
	r := newRig(t)
	r.gl.respond("a", statusAnswer(429, "Retry later", map[string]string{"Retry-After": "7"}))
	_, err := r.reconcile(tryOne)
	if me := wantCode(t, err, model.RateLimited); me.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v", me.RetryAfter)
	}
	r.gl.respond("a", statusAnswer(404, notFound, nil))
	_, err = r.reconcile(tryOne)
	wantCode(t, err, model.NotFound)
	r.gl.respond("a", statusAnswer(502, "", nil))
	_, err = r.reconcile(tryOne)
	wantCode(t, err, model.Transient)
}
