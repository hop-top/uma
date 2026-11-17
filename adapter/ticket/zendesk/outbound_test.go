package zendesk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/zendesk"
	"hop.top/uma/model"
)

// openAgainst opens an instance whose API calls go to handler, or fail
// with dialErr when set.
func openAgainst(t *testing.T, handler http.HandlerFunc, dialErr error) *zendesk.Instance {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := srv.Client()
	if dialErr != nil {
		client = &http.Client{Transport: &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) { return nil, dialErr }}}
	}
	f := zendesk.NewFactory(zendesk.WithBaseURL(srv.URL), zendesk.WithHTTPClient(client))
	cfg := zendesk.Config{Subdomain: hostSub, Auth: zendesk.AuthAPIToken, Email: "agent@acme.example", WebhookToleranceSeconds: 300}
	inst, err := f.Open(t.Context(), model.Channel{Ref: hostChannel, Type: zendesk.Type, Configuration: cfg.Extension(), CredentialRef: "zendesk"},
		adapter.Dependencies{Credentials: credentials{}})
	if err != nil {
		t.Fatal(err)
	}
	return inst.(*zendesk.Instance)
}

func sendReq() adapter.SendRequest {
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: model.OperationRef{Tenant: hostTenant, ID: "00000ad0-0000-7000-8000-0000000000e1"},
			Attempt:   "00000ad0-0000-7000-8000-0000000000e2",
			Route:     route(hostChannel, "100"), Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "On it"}}},
	}
}

// Every provider answer maps to an outcome that retries only what Zendesk
// provably did not apply.
func TestSendOutcomeMapping(t *testing.T) {
	answer := func(code int, header map[string]string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"error":"x"}`)
		}
	}
	resetAfterWrite := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}
	for _, c := range []struct {
		name    string
		handler http.HandlerFunc
		dial    error
		state   model.DeliveryState // set for an outcome
		code    model.ErrorCode
		after   time.Duration
	}{
		{name: "429 rate limited", handler: answer(429, map[string]string{"Retry-After": "93"}), code: model.RateLimited, after: 93 * time.Second},
		{name: "dial failure", handler: answer(200, nil), dial: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, code: model.Transient},
		{name: "503", handler: answer(503, map[string]string{"Retry-After": "10"}), code: model.OutcomeUnknown},
		{name: "500", handler: answer(500, nil), code: model.OutcomeUnknown},
		{name: "502", handler: answer(502, nil), code: model.OutcomeUnknown},
		{name: "409 collision", handler: answer(409, nil), code: model.OutcomeUnknown},
		{name: "reset after write", handler: resetAfterWrite, code: model.OutcomeUnknown},
		{name: "unreadable 200", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{") }, code: model.OutcomeUnknown},
		{name: "401", handler: answer(401, nil), code: model.Unauthorized},
		{name: "403", handler: answer(403, nil), code: model.Unauthorized},
		{name: "404 deleted or inaccessible", handler: answer(404, nil), state: model.Rejected, code: model.NotFound},
		{name: "422", handler: answer(422, nil), state: model.Rejected, code: model.Permanent},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, err := openAgainst(t, c.handler, c.dial).Send(t.Context(), sendReq())
			if c.state != "" {
				if err != nil || out.State != c.state || out.Failure == nil || out.Failure.Code != c.code || out.Failure.Retry != model.NoRetry {
					t.Fatalf("Send = %+v, %v; want %s %s without retry", out, err, c.state, c.code)
				}
				return
			}
			var me *model.Error
			if !errors.As(err, &me) || me.Code != c.code {
				t.Fatalf("Send = %+v, %v; want error %s", out, err, c.code)
			}
			if me.RetryAfter != c.after {
				t.Fatalf("Retry-After = %v, want %v", me.RetryAfter, c.after)
			}
		})
	}
}

// A reply goes out as a public comment carrying the attempt's marker.
func TestSendRequestShape(t *testing.T) {
	var got struct {
		method, path, auth string
		body               map[string]map[string]json.RawMessage
	}
	inst := openAgainst(t, func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path, got.auth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got.body)
		_, _ = io.WriteString(w, `{"audit":{"id":206091192546,"events":[{"body":"On it","id":206091192547,"type":"Comment","public":true}]},"ticket":{"id":100,"status":"open"}}`)
	}, nil)
	out, err := inst.Send(t.Context(), sendReq())
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	if out.References[0] != model.ProviderMessageRef(hostChannel, "206091192547") {
		t.Fatalf("references = %v, want the comment's provider message reference first", out.References)
	}
	if got.method != http.MethodPut || got.path != "/api/v2/tickets/100" || !strings.HasPrefix(got.auth, "Basic ") {
		t.Fatalf("request %s %s auth %q", got.method, got.path, got.auth)
	}
	var comment struct {
		Body   string `json:"body"`
		Public *bool  `json:"public"`
	}
	_ = json.Unmarshal(got.body["ticket"]["comment"], &comment)
	if comment.Body != "On it" || comment.Public == nil || !*comment.Public {
		t.Fatalf("comment = %+v, want a public comment", comment)
	}
	var marker map[string]string
	_ = json.Unmarshal(got.body["ticket"]["metadata"], &marker)
	if marker["uma_operation"] != "00000ad0-0000-7000-8000-0000000000e1" || marker["uma_attempt"] != "00000ad0-0000-7000-8000-0000000000e2" || marker["uma_effect"] != "reply" {
		t.Fatalf("metadata = %v, want the attempt's marker", marker)
	}
	// A non-public reply never reaches Zendesk.
	req := sendReq()
	req.Audience = model.Audience{Visibility: model.Internal}
	got.method = ""
	if out, err := inst.Send(t.Context(), req); err != nil || out.State != model.Rejected || got.method != "" {
		t.Fatalf("non-public Send = %+v, %v (call %q); want rejected without a call", out, err, got.method)
	}
}

// An update Zendesk may have applied is reconciled before any second
// write: a 5xx is outcome unknown, the worker reads the audits, and writes
// again only when no audit holds the attempt.
func TestNoSecondWriteBeforeReconcile(t *testing.T) {
	for _, c := range []struct {
		name    string
		applied bool
		writes  int
	}{
		{"503 not applied", false, 2},
		{"500 applied", true, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHost(t)
			conv := seedTicket(t, h)
			code := http.StatusServiceUnavailable
			if c.applied {
				code = http.StatusInternalServerError
			}
			h.fake.failNext(h.acct, code, c.applied)
			ref := h.reply(conv.Ref, "op-5xx", "We are on it")
			h.work()
			if op := h.op(ref); op.State != model.Unknown || op.Failure == nil || op.Failure.Retry != model.ReconcileFirst {
				t.Fatalf("operation after %d = %+v, want outcome_unknown, reconcile first", code, op)
			}
			if n := h.fake.writesOf(h.acct); n != 1 {
				t.Fatalf("ticket updates = %d before reconciling, want 1", n)
			}
			h.clock.Advance(time.Hour)
			h.work()
			if op := h.op(ref); op.State != model.ProviderAccepted {
				t.Fatalf("operation after reconciling = %+v, want provider_accepted", op)
			}
			var seq []model.Capability
			for _, call := range h.fake.callsOf(h.acct) {
				seq = append(seq, call.Capability)
			}
			want := []model.Capability{model.CapabilitySend, model.CapabilityReconcile}
			if !c.applied {
				want = append(want, model.CapabilitySend)
			}
			if !equalCaps(seq, want) || h.fake.writesOf(h.acct) != c.writes {
				t.Fatalf("provider calls %v, writes %d; want %v and %d writes", seq, h.fake.writesOf(h.acct), want, c.writes)
			}
		})
	}
}

func equalCaps(a, b []model.Capability) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ adapter.ActionValidator = (*zendesk.Factory)(nil)

// The factory refuses the fields Execute refuses, without I/O, so Execute
// and ValidateAction share one set of rules.
func TestValidateAction(t *testing.T) {
	pub := model.Audience{Visibility: model.Public}
	act := func(name, data string) model.Extension {
		return model.Extension{Name: name, Version: zendesk.ExtensionVersion, Data: json.RawMessage(data)}
	}
	f := zendesk.NewFactory()
	for _, tc := range []struct {
		name  string
		aud   model.Audience
		x     model.Extension
		field string
	}{
		{"note ok", staff, act(zendesk.ExtensionInternalNote, `{"body":"hi","x_future":1}`), ""},
		{"note html body ok", staff, act(zendesk.ExtensionInternalNote, `{"html_body":"<b>hi</b>"}`), ""},
		{"note public audience", pub, act(zendesk.ExtensionInternalNote, `{"body":"hi"}`), "audience"},
		{"note public flag", staff, act(zendesk.ExtensionInternalNote, `{"body":"hi","public":true}`), "data.public"},
		{"note no body", staff, act(zendesk.ExtensionInternalNote, `{}`), "data.body"},
		{"note empty body", staff, act(zendesk.ExtensionInternalNote, `{"body":""}`), "data.body"},
		{"note not an object", staff, act(zendesk.ExtensionInternalNote, `[1]`), "data"},
		{"update ok", pub, act(zendesk.ExtensionUpdate, `{"status":"solved","tags":["vip"]}`), ""},
		{"update no field", staff, act(zendesk.ExtensionUpdate, `{}`), "data"},
		{"update comment", staff, act(zendesk.ExtensionUpdate, `{"comment":{"body":"x"}}`), "data.comment"},
		{"update metadata", staff, act(zendesk.ExtensionUpdate, `{"metadata":{}}`), "data.metadata"},
		{"update safe_update", staff, act(zendesk.ExtensionUpdate, `{"safe_update":true}`), "data.safe_update"},
		{"update updated_stamp", staff, act(zendesk.ExtensionUpdate, `{"updated_stamp":"x"}`), "data.updated_stamp"},
		{"update null data", staff, act(zendesk.ExtensionUpdate, `null`), "data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := f.ValidateAction(tc.aud, tc.x)
			if tc.field == "" {
				if len(issues) != 0 {
					t.Fatalf("issues = %+v, want none", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].Field != tc.field || issues[0].Code != string(model.Validation) || issues[0].Message == "" {
				t.Fatalf("issues = %+v, want one %s issue on %s", issues, model.Validation, tc.field)
			}
		})
	}
}

// Execute keeps validating the stored action at the attempt, whatever the
// call checked: an action stored before a rule tightened never reaches
// Zendesk.
func TestExecuteValidatesAtTheAttempt(t *testing.T) {
	inst := openAgainst(t, func(http.ResponseWriter, *http.Request) {
		t.Error("provider called for an invalid action")
	}, nil)
	req := adapter.ExecuteRequest{
		Effect: sendReq().Effect,
		Action: model.Extension{Name: zendesk.ExtensionUpdate, Version: zendesk.ExtensionVersion, Data: json.RawMessage(`{}`)},
	}
	out, err := inst.Execute(t.Context(), req)
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Validation {
		t.Fatalf("Execute = %+v, %v; want a rejected validation outcome", out, err)
	}
}
