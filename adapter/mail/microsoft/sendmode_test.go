package microsoft

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// directEnv is an opened direct-mode instance over a fake sendMail Graph.
type directEnv struct {
	g     *fakeSendMail
	inst  *instance
	creds *testCreds
}

func newDirectEnv(t *testing.T, cfg Config, script ...sendAnswer) *directEnv {
	t.Helper()
	g := newFakeSendMail(t, script...)
	creds := &testCreds{}
	cfg.SendMode = SendModeDirect
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := NewFactory(Options{Endpoint: g.endpoint()}).Open(t.Context(), model.Channel{
		Ref: testChan, Type: Type, CredentialRef: "cred-1",
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
	}, adapter.Dependencies{Credentials: creds, HTTP: g.srv.Client()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return &directEnv{g: g, inst: inst.(*instance), creds: creds}
}

func (e *directEnv) send(t *testing.T, r model.ExternalRef, text string) (adapter.Outcome, error) {
	t.Helper()
	return e.inst.Send(t.Context(), adapter.SendRequest{Effect: effect(r, "a1"), Content: textContent(text)})
}

func TestDirectSendPostsSendMailWithoutADraft(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox})
	out, err := e.send(t, route("peer@example.test", ""), "your code is 123456")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v; want provider_accepted", out, err)
	}
	calls := e.g.seen()
	if len(calls) != 1 {
		t.Fatalf("requests = %+v, want exactly one (no draft, no send call)", calls)
	}
	c := calls[0]
	if c.method != "POST" || c.path != "/v1.0/users/"+testMailbox+"/sendMail" {
		t.Fatalf("request = %s %s", c.method, c.path)
	}
	if c.contentType != "application/json" || c.auth != "Bearer tok-1" {
		t.Fatalf("headers: content-type %q auth %q", c.contentType, c.auth)
	}
	b := decodeSendMail(t, c)
	if got := wireAddrs(t, b.Message, "toRecipients"); !slices.Equal(got, []string{"peer@example.test"}) {
		t.Fatalf("to = %v", got)
	}
	body, _ := b.Message["body"].(map[string]any)
	if body["contentType"] != "text" || body["content"] != "your code is 123456" {
		t.Fatalf("body = %v", body)
	}
	if b.SaveToSentItems == nil || *b.SaveToSentItems {
		t.Fatalf("saveToSentItems = %v, want an explicit false", b.SaveToSentItems)
	}
	// Graph answers 202 with no ID: the outcome has no provider reference.
	if len(out.References) != 0 {
		t.Fatalf("references = %+v, want none", out.References)
	}
}

func TestDirectSendSaveToSentItemsIsConfigurable(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox, SaveToSentItems: true})
	if _, err := e.send(t, route("peer@example.test", ""), "hi"); err != nil {
		t.Fatal(err)
	}
	b := decodeSendMail(t, e.g.seen()[0])
	if b.SaveToSentItems == nil || !*b.SaveToSentItems {
		t.Fatalf("saveToSentItems = %v, want true", b.SaveToSentItems)
	}
}

func TestDirectSendWithoutMailboxUsesMe(t *testing.T) {
	e := newDirectEnv(t, Config{})
	if _, err := e.send(t, route("peer@example.test", ""), "hi"); err != nil {
		t.Fatal(err)
	}
	if c := e.g.seen()[0]; c.path != "/v1.0/me/sendMail" {
		t.Fatalf("path = %s, want /v1.0/me/sendMail", c.path)
	}
}

func TestDirectSendCarriesCcAndBcc(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox})
	_, err := e.inst.Send(t.Context(), adapter.SendRequest{
		Effect: effect(route("peer@example.test", ""), "a1"), Content: textContent("hi"),
		Recipients: &model.ReplyRecipients{Cc: []string{"cc@example.test"}, Bcc: []string{"bcc@example.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := decodeSendMail(t, e.g.seen()[0])
	if got := wireAddrs(t, b.Message, "ccRecipients"); !slices.Equal(got, []string{"cc@example.test"}) {
		t.Fatalf("cc = %v", got)
	}
	if got := wireAddrs(t, b.Message, "bccRecipients"); !slices.Equal(got, []string{"bcc@example.test"}) {
		t.Fatalf("bcc = %v", got)
	}
}

func TestDirectSendRefusesAReplyInAThread(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox})
	out, err := e.send(t, route("peer@example.test", "m1"), "hi")
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != model.Unsupported {
		t.Fatalf("Send = %+v, %v; want rejected unsupported", out, err)
	}
	if n := len(e.g.seen()); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

func TestDirectSendRefreshesTheTokenOnceOn401(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox}, sendAnswer{status: http.StatusUnauthorized})
	out, err := e.send(t, route("peer@example.test", ""), "hi")
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	calls := e.g.seen()
	if len(calls) != 2 || calls[0].auth != "Bearer tok-1" || calls[1].auth != "Bearer tok-fresh" {
		t.Fatalf("calls = %+v, want tok-1 then tok-fresh", calls)
	}
	reqs := e.creds.graphRequests()
	if len(reqs) != 2 || reqs[0].Fresh || !reqs[1].Fresh {
		t.Fatalf("credential requests = %+v, want one plain then one Fresh", reqs)
	}
}

func TestDirectSendSecond401IsUnauthorizedAndNotRepeated(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox}, sendAnswer{status: 401}, sendAnswer{status: 401}, sendAnswer{})
	_, err := e.send(t, route("peer@example.test", ""), "hi")
	wantCode(t, err, model.Unauthorized)
	if n := len(e.g.seen()); n != 2 {
		t.Fatalf("requests = %d, want 2 (one refresh, no third)", n)
	}
}

func TestDirectSendThrottlingIsReportedNotWaitedOn(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox}, sendAnswer{status: 429, retryAfter: "7"})
	start := time.Now()
	out, err := e.send(t, route("peer@example.test", ""), "hi")
	if out.State == model.Rejected {
		t.Fatalf("429 mapped to rejected: %+v", out)
	}
	me := wantErr(t, err, model.RateLimited)
	if me.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v, want 7s", me.RetryAfter)
	}
	if time.Since(start) > 3*time.Second || len(e.g.seen()) != 1 {
		t.Fatalf("the adapter waited or retried: %d requests in %v", len(e.g.seen()), time.Since(start))
	}
}

func TestDirectSendAnswersByStatus(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer sendAnswer
		code   model.ErrorCode // of the error, when one is returned
		state  model.DeliveryState
	}{
		{"503 is transient, nothing was sent", sendAnswer{status: 503, retryAfter: "1"}, model.Transient, ""},
		{"500 may have sent: outcome unknown", sendAnswer{status: 500}, model.OutcomeUnknown, ""},
		{"502 may have sent: outcome unknown", sendAnswer{status: 502}, model.OutcomeUnknown, ""},
		{"lost answer: outcome unknown", sendAnswer{drop: true}, model.OutcomeUnknown, ""},
		{"400 is a final rejection", sendAnswer{status: 400}, "", model.Rejected},
		{"403 is unauthorized", sendAnswer{status: 403}, model.Unauthorized, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newDirectEnv(t, Config{Mailbox: testMailbox}, c.answer)
			out, err := e.send(t, route("peer@example.test", ""), "hi")
			if c.code != "" {
				wantCode(t, err, c.code)
			} else if err != nil || out.State != c.state {
				t.Fatalf("Send = %+v, %v; want %s", out, err, c.state)
			}
			if n := len(e.g.seen()); n != 1 {
				t.Fatalf("requests = %d, want 1: a direct send has no idempotency key and is never repeated", n)
			}
		})
	}
}

func TestDirectSendErrorsCarryNoContentAddressOrToken(t *testing.T) {
	secrets := []string{"SECRET-BODY-TEXT", "secret-recipient@leak.test", "tok-1", "tok-fresh"}
	for _, status := range []int{400, 401, 403, 404, 429, 500, 503} {
		e := newDirectEnv(t, Config{Mailbox: testMailbox}, sendAnswer{status: status}, sendAnswer{status: status})
		out, err := e.send(t, route("secret-recipient@leak.test", ""), "SECRET-BODY-TEXT")
		if err != nil {
			mustNotContain(t, "error", err.Error(), secrets...)
		}
		if out.Failure != nil {
			mustNotContain(t, "failure", out.Failure.Error(), secrets...)
		}
	}
}

func TestDirectModeReportsOnlySendCapabilities(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox})
	st, err := e.inst.Capabilities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range st {
		want := s.Name == model.CapabilitySend || s.Name == model.CapabilityStartConversation
		if s.Available != want {
			t.Errorf("%s available = %v, want %v", s.Name, s.Available, want)
		}
		if !s.Available && s.Reason != model.Unsupported {
			t.Errorf("%s reason = %q, want unsupported", s.Name, s.Reason)
		}
	}
}

func TestDirectModeCannotReconcile(t *testing.T) {
	e := newDirectEnv(t, Config{Mailbox: testMailbox})
	_, err := e.inst.Reconcile(t.Context(), adapter.ReconcileRequest{Effect: effect(route("peer@example.test", ""), "a1"), Capability: model.CapabilitySend})
	wantCode(t, err, model.Unsupported)
	if n := len(e.g.seen()); n != 0 {
		t.Fatalf("requests = %d, want none", n)
	}
}

func TestPermissionsPerMode(t *testing.T) {
	if got := (Config{}).RequiredPermissions(); !slices.Equal(got, []string{"Mail.ReadWrite", "Mail.Send"}) {
		t.Errorf("default mode permissions = %v", got)
	}
	if got := (Config{SendMode: SendModeDraft}).RequiredPermissions(); !slices.Equal(got, []string{"Mail.ReadWrite", "Mail.Send"}) {
		t.Errorf("draft mode permissions = %v", got)
	}
	if got := (Config{SendMode: SendModeDirect}).RequiredPermissions(); !slices.Equal(got, []string{"Mail.Send"}) {
		t.Errorf("direct mode permissions = %v", got)
	}
	if !slices.Equal(RequiredPermissions, []string{"Mail.ReadWrite", "Mail.Send"}) {
		t.Errorf("RequiredPermissions changed: %v", RequiredPermissions)
	}
}

func TestDefaultFactoryDescriptorIsUnchanged(t *testing.T) {
	d := NewFactory(Options{}).Describe()
	if !slices.Equal(d.RequiredPermissions, []string{"Mail.ReadWrite", "Mail.Send"}) {
		t.Errorf("permissions = %v", d.RequiredPermissions)
	}
	for _, c := range []model.Capability{model.CapabilityReceive, model.CapabilityReconcile, model.CapabilitySend, model.CapabilityArchive} {
		if !slices.Contains(d.Capabilities, c) {
			t.Errorf("capability %s no longer declared", c)
		}
	}
	if d.ReconcileSettle != ReconcileSettle {
		t.Errorf("ReconcileSettle = %v", d.ReconcileSettle)
	}
}

func TestSendOnlyFactoryDeclaresOnlySendAndMailSend(t *testing.T) {
	d := NewFactory(Options{SendOnly: true}).Describe()
	if !slices.Equal(d.RequiredPermissions, []string{"Mail.Send"}) {
		t.Errorf("permissions = %v, want Mail.Send only", d.RequiredPermissions)
	}
	want := []model.Capability{model.CapabilitySend, model.CapabilityStartConversation}
	if !slices.Equal(d.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v", d.Capabilities, want)
	}
	if len(d.Events) != 0 || d.ReconcileSettle != 0 {
		t.Errorf("events %v settle %v, want none", d.Events, d.ReconcileSettle)
	}
}

func TestSendOnlyFactoryDefaultsToDirectAndRefusesDraft(t *testing.T) {
	f := NewFactory(Options{SendOnly: true})
	ext := func(c string) model.Extension {
		return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: json.RawMessage(c)}
	}
	if issues := f.Validate(ext(`{"mailbox":"desk@contoso.test"}`)); len(issues) != 0 {
		t.Errorf("empty send_mode on a send-only factory: %v", issues)
	}
	if issues := f.Validate(ext(`{"mailbox":"desk@contoso.test","send_mode":"draft"}`)); len(issues) == 0 {
		t.Error("draft mode on a send-only factory was accepted")
	}
	inst, err := f.Open(t.Context(), model.Channel{Ref: testChan, Type: Type, Configuration: ext(`{"mailbox":"desk@contoso.test"}`)},
		adapter.Dependencies{HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	if inst.(*instance).cfg.sendMode() != SendModeDirect {
		t.Error("send-only factory did not default to direct")
	}
}

func TestConfigValidatesSendFields(t *testing.T) {
	f := NewFactory(Options{})
	ext := func(c string) model.Extension {
		return model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: json.RawMessage(c)}
	}
	for c, ok := range map[string]bool{
		`{}`:                     true,
		`{"send_mode":"draft"}`:  true,
		`{"send_mode":"direct"}`: true,
		`{"send_mode":"direct","save_to_sent_items":true}`: true,
		`{"send_mode":"queue"}`:                            false,
		`{"save_to_sent_items":true}`:                      false, // draft mode always keeps the sent copy
		`{"send_mode":"draft","save_to_sent_items":true}`:  false,
	} {
		if got := len(f.Validate(ext(c))) == 0; got != ok {
			t.Errorf("Validate(%s) valid = %v, want %v", c, got, ok)
		}
	}
}
