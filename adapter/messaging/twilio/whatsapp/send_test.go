package whatsapp_test

import (
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/whatsapp"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func sender(t *testing.T, fake *fakeTwilio, creds ports.Credentials, p adapter.ProtocolState) adapter.Sender {
	t.Helper()
	return open(t, fake, hookConfig(), adapter.Dependencies{Credentials: creds, Protocol: p}).(adapter.Sender)
}

func TestSendFreeFormRequest(t *testing.T) {
	fake := newFakeTwilio(t)
	creds := &tokenHost{}
	out, err := sender(t, fake, creds, nil).Send(t.Context(), sendReq(model.Part{Kind: model.PartText, Text: "line 1"}, model.Part{Kind: model.PartText, Text: "line 2"}))
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	calls := fake.calls(hookAccount)
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	f := calls[0].Form
	if f.Get("To") != "whatsapp:"+userNumber || f.Get("From") != "whatsapp:"+senderNumber || f.Get("Body") != "line 1\nline 2" ||
		f.Has("ContentSid") || f.Has("ContentVariables") {
		t.Fatalf("form = %v", f)
	}
	if out.References[0] != model.ProviderMessageRef(testChannel, calls[0].SID) {
		t.Fatalf("references %v, want the created SID %s", out.References, calls[0].SID)
	}
	cb, err := url.Parse(f.Get("StatusCallback"))
	if err != nil || cb.Scheme+"://"+cb.Host+cb.Path != hookURL ||
		cb.Query().Get("operation") != string(testOp.ID) || cb.Query().Get("attempt") != "00000000-0000-7000-8000-0000000007c1" {
		t.Fatalf("status callback %q", f.Get("StatusCallback"))
	}
	reqs := creds.requests()
	if len(reqs) != 1 || reqs[0].Purpose != whatsapp.PurposeREST || reqs[0].Channel != testChannel {
		t.Fatalf("credential requests %+v", reqs)
	}
	if a := reqs[0].Accept; !slices.Equal(a, []ports.CredentialKind{ports.KindSecret, ports.KindPassword}) {
		t.Fatalf("accepted kinds %v, want secret then password", a)
	}
}

// A template goes out as ContentSid and ContentVariables, without Body
// (https://www.twilio.com/docs/content/send-templates-created-with-the-content-template-builder).
func TestSendTemplateRequest(t *testing.T) {
	fake := newFakeTwilio(t)
	out, err := sender(t, fake, &tokenHost{}, nil).Send(t.Context(), sendReq(helloTemplate.Part()))
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	f := fake.calls(hookAccount)[0].Form
	if f.Get("ContentSid") != helloTemplate.ContentSID || f.Get("ContentVariables") != `{"1":"Ada"}` || f.Has("Body") ||
		f.Get("To") != "whatsapp:"+userNumber || f.Get("StatusCallback") == "" {
		t.Fatalf("form = %v", f)
	}
}

func TestSendThroughMessagingService(t *testing.T) {
	fake := newFakeTwilio(t)
	cfg := whatsapp.Config{AccountSID: hookAccount, MessagingServiceSID: "MG00000000000000000000000000000001"}
	s := open(t, fake, cfg, adapter.Dependencies{Credentials: &tokenHost{}}).(adapter.Sender)
	if _, err := s.Send(t.Context(), sendReq(helloTemplate.Part())); err != nil {
		t.Fatal(err)
	}
	f := fake.calls(hookAccount)[0].Form
	if f.Get("MessagingServiceSid") != cfg.MessagingServiceSID || f.Has("From") || f.Has("StatusCallback") {
		t.Fatalf("form = %v", f)
	}
}

// Free-form messages go out only inside the 24-hour window a verified
// inbound message opens; templates always go out.
func TestServiceWindow(t *testing.T) {
	text := model.Part{Kind: model.PartText, Text: "hi"}
	cases := map[string]struct {
		last     time.Duration // before testNow; 0 = never
		part     model.Part
		wantSent bool
	}{
		"free-form, no inbound recorded": {0, text, true},
		"free-form, inbound 1h ago":      {time.Hour, text, true},
		"free-form, inbound 23h59m ago":  {24*time.Hour - time.Minute, text, true},
		"free-form, inbound exactly 24h": {24 * time.Hour, text, false},
		"free-form, inbound 25h ago":     {25 * time.Hour, text, false},
		"template, no inbound ever":      {0, helloTemplate.Part(), true},
		"template, inbound 25h ago":      {25 * time.Hour, helloTemplate.Part(), true},
		"template, inbound 1h ago":       {time.Hour, helloTemplate.Part(), true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeTwilio(t)
			p := newProtocol()
			if c.last > 0 {
				p.setLastInbound(userNumber, testNow.Add(-c.last))
			}
			creds := &tokenHost{}
			out, err := sender(t, fake, creds, p).Send(t.Context(), sendReq(c.part))
			if err != nil {
				t.Fatal(err)
			}
			sent := len(fake.calls(hookAccount)) == 1
			if sent != c.wantSent {
				t.Fatalf("sent = %v, want %v (outcome %+v)", sent, c.wantSent, out)
			}
			if c.wantSent {
				if out.State != model.ProviderAccepted {
					t.Fatalf("outcome %+v", out)
				}
				return
			}
			if out.State != model.Rejected || out.Failure == nil || out.Failure.Retry != model.NoRetry || !strings.Contains(out.Failure.SafeMessage, "template") {
				t.Fatalf("outcome %+v, want rejected asking for a template", out)
			}
			if len(creds.requests()) != 0 {
				t.Fatal("credential resolved for a message refused before any call")
			}
		})
	}
	t.Run("no window state: Twilio decides", func(t *testing.T) {
		fake := newFakeTwilio(t)
		if out, err := sender(t, fake, &tokenHost{}, nil).Send(t.Context(), sendReq()); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Send = %+v, %v", out, err)
		}
	})
	t.Run("Twilio refuses with 63016", func(t *testing.T) {
		fake := newFakeTwilio(t)
		fake.respond(hookAccount, conformance.Response{Outcome: outcomeWindowClosed})
		out, err := sender(t, fake, &tokenHost{}, nil).Send(t.Context(), sendReq())
		if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Retry != model.NoRetry || !strings.Contains(out.Failure.SafeMessage, "template") {
			t.Fatalf("Send = %+v, %v; want rejected asking for a template", out, err)
		}
	})
	t.Run("window state unavailable", func(t *testing.T) {
		fake := newFakeTwilio(t)
		p := newProtocol()
		p.fail = model.NewError(model.Unavailable, "down")
		_, err := sender(t, fake, &tokenHost{}, p).Send(t.Context(), sendReq())
		if model.CodeOf(err) != model.Unavailable || len(fake.calls(hookAccount)) != 0 {
			t.Fatalf("err = %v, calls %d; want unavailable, no call", err, len(fake.calls(hookAccount)))
		}
	})
}

// A request Twilio received but never answered may have created the
// message: outcome_unknown, never failed or transient.
func TestSendTimeoutAfterSendIsOutcomeUnknown(t *testing.T) {
	fake := newFakeTwilio(t)
	fake.respond(hookAccount, conformance.Response{Outcome: outcomeHang})
	inst, err := whatsapp.NewFactory(whatsapp.Options{BaseURL: fake.srv.URL, Timeout: 200 * time.Millisecond}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: model.TypeTwilioWhatsapp, Configuration: hookConfig().Extension(), CredentialRef: "twilio", Revision: "ch1",
	}, adapter.Dependencies{HTTP: fake.client(), Credentials: &tokenHost{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inst.(adapter.Sender).Send(t.Context(), sendReq()); model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("Send after timeout = %v, want outcome_unknown", err)
	}
	if calls := fake.calls(hookAccount); len(calls) != 1 {
		t.Fatalf("calls = %d, want the one request that timed out", len(calls))
	}
}

func TestSendOutcomes(t *testing.T) {
	for _, c := range []struct {
		name  string
		resp  conformance.Response
		state model.DeliveryState
		code  model.ErrorCode
		after time.Duration
	}{
		{"429 rate limited with Retry-After", conformance.Response{Outcome: conformance.OutcomeRateLimited, RetryAfter: 30 * time.Second}, "", model.RateLimited, 30 * time.Second},
		{"refused before send transient", conformance.Response{Outcome: conformance.OutcomeTransient}, "", model.Transient, 0},
		{"500 after send unknown", conformance.Response{Outcome: outcomeStatus(500)}, "", model.OutcomeUnknown, 0},
		{"502 after send unknown", conformance.Response{Outcome: outcomeStatus(502)}, "", model.OutcomeUnknown, 0},
		{"503 after send unknown", conformance.Response{Outcome: outcomeStatus(503)}, "", model.OutcomeUnknown, 0},
		{"504 after send unknown", conformance.Response{Outcome: outcomeStatus(504)}, "", model.OutcomeUnknown, 0},
		{"401 unauthorized", conformance.Response{Outcome: outcomeStatus(401)}, "", model.Unauthorized, 0},
		{"400 rejected", conformance.Response{Outcome: conformance.OutcomeRejected}, model.Rejected, model.Permanent, 0},
		{"answer lost", conformance.Response{Outcome: conformance.OutcomeAnswerLost}, "", model.OutcomeUnknown, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeTwilio(t)
			fake.respond(hookAccount, c.resp)
			out, err := sender(t, fake, &tokenHost{}, nil).Send(t.Context(), sendReq())
			f := out.Failure
			if c.state == "" {
				me, ok := err.(*model.Error)
				if !ok {
					t.Fatalf("Send = %+v, %v; want error %s", out, err, c.code)
				}
				f = me
			} else if err != nil || out.State != c.state {
				t.Fatalf("Send = %+v, %v; want state %s", out, err, c.state)
			}
			if f == nil || f.Code != c.code || f.RetryAfter != c.after {
				t.Fatalf("failure %+v, want %s after %v", f, c.code, c.after)
			}
		})
	}
}

func TestSendRefusesBeforeAnyCall(t *testing.T) {
	fake := newFakeTwilio(t)
	creds := &tokenHost{}
	s := sender(t, fake, creds, nil)
	route := func(mut func(*model.ExternalRef)) adapter.SendRequest {
		r := sendReq()
		mut(&r.Route)
		return r
	}
	private := sendReq()
	private.Audience.Visibility = model.Internal
	badTemplate := func(data string) model.Part {
		return model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: whatsapp.TemplateSchema, Version: 1, Data: []byte(data)}}
	}
	for name, c := range map[string]struct {
		req  adapter.SendRequest
		code model.ErrorCode
	}{
		"route of another channel":   {route(func(r *model.ExternalRef) { r.Channel.ID = "00000000-0000-7000-8000-0000000007a9" }), model.Validation},
		"route of another namespace": {route(func(r *model.ExternalRef) { r.Namespace = "twilio.sms" }), model.Validation},
		"prefixed route":             {route(func(r *model.ExternalRef) { r.ID = "whatsapp:" + userNumber }), model.Validation},
		"internal audience":          {private, model.Unsupported},
		"attachment part":            {sendReq(model.Part{Kind: model.PartAttachment}), model.Unsupported},
		"other structured part":      {sendReq(model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: "x.other", Version: 1}}), model.Unsupported},
		"template plus text":         {sendReq(helloTemplate.Part(), model.Part{Kind: model.PartText, Text: "x"}), model.Unsupported},
		"template without SID":       {sendReq(badTemplate(`{"content_variables":{"1":"x"}}`)), model.Validation},
		"template with SM SID":       {sendReq(badTemplate(`{"content_sid":"SM00000000000000000000000000000001"}`)), model.Validation},
		"template with number vars":  {sendReq(badTemplate(`{"content_sid":"HX00000000000000000000000000000001","content_variables":{"1":2}}`)), model.Validation},
		"template unknown field":     {sendReq(badTemplate(`{"content_sid":"HX00000000000000000000000000000001","body":"x"}`)), model.Validation},
		"empty text":                 {sendReq(model.Part{Kind: model.PartText}), model.Validation},
		"no content":                 {adapter.SendRequest{Effect: sendReq().Effect}, model.Validation},
	} {
		if _, err := s.Send(t.Context(), c.req); model.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if n := len(fake.calls(hookAccount)); n != 0 || len(creds.requests()) != 0 {
		t.Fatalf("calls = %d, credential requests = %d; want none", n, len(creds.requests()))
	}
	if _, err := sender(t, nil, creds, nil).Send(t.Context(), sendReq()); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("Send without an HTTP client = %v, want unavailable", err)
	}
}

func TestValidate(t *testing.T) {
	f := whatsapp.NewFactory(whatsapp.Options{})
	if issues := f.Validate(hookConfig().Extension()); len(issues) != 0 {
		t.Fatalf("valid config: %+v", issues)
	}
	svc := whatsapp.Config{AccountSID: hookAccount, MessagingServiceSID: "MG00000000000000000000000000000001"}
	if issues := f.Validate(svc.Extension()); len(issues) != 0 {
		t.Fatalf("service config: %+v", issues)
	}
	for name, e := range map[string]model.Extension{
		"schema":          {Name: "twilio.sms.config", Version: 1, Data: hookConfig().Extension().Data},
		"version":         {Name: whatsapp.ConfigSchema, Version: 2},
		"unknown key":     {Name: whatsapp.ConfigSchema, Version: 1, Data: []byte(`{"account_sid":"` + hookAccount + `","from":"+14155238886","api_base_url":"https://evil"}`)},
		"account":         whatsapp.Config{AccountSID: "AC123", From: senderNumber}.Extension(),
		"no sender":       whatsapp.Config{AccountSID: hookAccount}.Extension(),
		"prefixed sender": whatsapp.Config{AccountSID: hookAccount, From: "whatsapp:" + senderNumber}.Extension(),
		"sender digits":   whatsapp.Config{AccountSID: hookAccount, From: "+1415abc"}.Extension(),
		"service sid":     whatsapp.Config{AccountSID: hookAccount, MessagingServiceSID: "MGx"}.Extension(),
		"url query":       whatsapp.Config{AccountSID: hookAccount, From: senderNumber, WebhookURL: "https://h.example.com/x?y=1"}.Extension(),
		"url creds":       whatsapp.Config{AccountSID: hookAccount, From: senderNumber, WebhookURL: "https://u:p@h.example.com/x"}.Extension(), // #nosec G101 -- the case under test
		"url scheme":      whatsapp.Config{AccountSID: hookAccount, From: senderNumber, WebhookURL: "ftp://h.example.com/x"}.Extension(),
	} {
		if issues := f.Validate(e); len(issues) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.Open(t.Context(), model.Channel{Ref: testChannel, Type: model.TypeTwilioSms, Configuration: hookConfig().Extension()}, adapter.Dependencies{}); model.CodeOf(err) != model.Validation {
		t.Errorf("Open of another type = %v", err)
	}
}
