package sms_test

import (
	"context"
	"net/url"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/twilio/sms"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

var (
	sendChannel = model.ChannelRef{Tenant: "twilio-send", ID: "00000000-0000-7000-8000-0000000004a0"}
	sendRoute   = model.ExternalRef{Channel: sendChannel, Namespace: sms.Namespace, Kind: sms.RouteKind, ID: "+15558675310"}
	sendOp      = model.OperationRef{Tenant: "twilio-send", ID: "00000000-0000-7000-8000-0000000004b1"}
)

func openSender(t *testing.T, fake *fakeTwilio, timeout time.Duration, creds ports.Credentials) adapter.Sender {
	t.Helper()
	opts := sms.Options{Timeout: timeout}
	deps := adapter.Dependencies{Credentials: creds, Clock: portstest.NewFixedClock(time.Unix(1_760_000_000, 0))}
	if fake != nil {
		deps.HTTP, opts.BaseURL = fake.client(), fake.srv.URL
	}
	inst, err := sms.NewFactory(opts).Open(t.Context(), model.Channel{
		Ref: sendChannel, Type: model.TypeTwilioSms, Configuration: hookConfig().Extension(), CredentialRef: "twilio", Revision: "ch1",
	}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inst.Close(context.Background()) })
	return inst.(adapter.Sender)
}

func sendReq(parts ...model.Part) adapter.SendRequest {
	if len(parts) == 0 {
		parts = []model.Part{{Kind: model.PartText, Text: "hello"}}
	}
	return adapter.SendRequest{
		Effect: adapter.Effect{
			Operation: sendOp, Attempt: "00000000-0000-7000-8000-0000000004c1",
			Route: sendRoute, Audience: model.Audience{Visibility: model.Public},
		},
		Content: model.Content{Parts: parts},
	}
}

func TestSendRequest(t *testing.T) {
	fake := newFakeTwilio(t)
	creds := &tokenHost{}
	s := openSender(t, fake, 5*time.Second, creds)
	out, err := s.Send(t.Context(), sendReq(model.Part{Kind: model.PartText, Text: "line 1"}, model.Part{Kind: model.PartText, Text: "line 2"}))
	if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
		t.Fatalf("Send = %+v, %v", out, err)
	}
	calls := fake.calls(hookAccount)
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	f := calls[0].Form
	if f.Get("To") != "+15558675310" || f.Get("From") != senderNumber || f.Get("Body") != "line 1\nline 2" {
		t.Fatalf("form = %v", f)
	}
	if out.References[0] != model.ProviderMessageRef(sendChannel, calls[0].SID) {
		t.Fatalf("references %v, want the created SID %s", out.References, calls[0].SID)
	}
	cb, err := url.Parse(f.Get("StatusCallback"))
	if err != nil || cb.Scheme+"://"+cb.Host+cb.Path != hookURL ||
		cb.Query().Get("operation") != string(sendOp.ID) || cb.Query().Get("attempt") != "00000000-0000-7000-8000-0000000004c1" {
		t.Fatalf("status callback %q", f.Get("StatusCallback"))
	}
	if len(creds.reqs) != 1 || creds.reqs[0].Purpose != sms.PurposeREST || creds.reqs[0].Channel != sendChannel {
		t.Fatalf("credential requests %+v", creds.reqs)
	}
	// The auth token first: the channel needs it anyway to verify webhooks;
	// an API key SID and secret as a Password second.
	if a := creds.reqs[0].Accept; !slices.Equal(a, []ports.CredentialKind{ports.KindSecret, ports.KindPassword}) {
		t.Fatalf("accepted kinds %v, want secret then password", a)
	}
}

// A request Twilio received but never answered may have created the
// message: outcome_unknown, never failed or transient.
func TestSendTimeoutAfterSendIsOutcomeUnknown(t *testing.T) {
	fake := newFakeTwilio(t)
	fake.respond(hookAccount, conformance.Response{Outcome: outcomeHang})
	s := openSender(t, fake, 200*time.Millisecond, &tokenHost{})
	_, err := s.Send(t.Context(), sendReq())
	if model.CodeOf(err) != model.OutcomeUnknown {
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
		{"429 transient with Retry-After", conformance.Response{Outcome: conformance.OutcomeRateLimited, RetryAfter: 30 * time.Second}, "", model.RateLimited, 30 * time.Second},
		{"refused before send transient", conformance.Response{Outcome: conformance.OutcomeTransient}, "", model.Transient, 0},
		{"500 after send unknown", conformance.Response{Outcome: outcomeStatus(500)}, "", model.OutcomeUnknown, 0},
		{"502 after send unknown", conformance.Response{Outcome: outcomeStatus(502)}, "", model.OutcomeUnknown, 0},
		{"503 after send unknown", conformance.Response{Outcome: outcomeStatus(503)}, "", model.OutcomeUnknown, 0},
		{"504 after send unknown", conformance.Response{Outcome: outcomeStatus(504)}, "", model.OutcomeUnknown, 0},
		{"400 rejected", conformance.Response{Outcome: conformance.OutcomeRejected}, model.Rejected, model.Permanent, 0},
		{"answer lost", conformance.Response{Outcome: conformance.OutcomeAnswerLost}, "", model.OutcomeUnknown, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake := newFakeTwilio(t)
			fake.respond(hookAccount, c.resp)
			out, err := openSender(t, fake, 5*time.Second, &tokenHost{}).Send(t.Context(), sendReq())
			f := out.Failure
			if c.state == "" {
				var me *model.Error
				if model.CodeOf(err) == "" {
					t.Fatalf("Send = %+v, %v; want error %s", out, err, c.code)
				}
				me = err.(*model.Error)
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
	s := openSender(t, fake, time.Second, creds)
	otherChannel := sendRoute
	otherChannel.Channel.ID = "00000000-0000-7000-8000-0000000004a9"
	otherNS := sendRoute
	otherNS.Namespace = "twilio.whatsapp"
	private := sendReq()
	private.Audience.Visibility = model.Internal
	for name, c := range map[string]struct {
		req  adapter.SendRequest
		code model.ErrorCode
	}{
		"route of another channel":   {func() adapter.SendRequest { r := sendReq(); r.Route = otherChannel; return r }(), model.Validation},
		"route of another namespace": {func() adapter.SendRequest { r := sendReq(); r.Route = otherNS; return r }(), model.Validation},
		"internal audience":          {private, model.Unsupported},
		"attachment part":            {sendReq(model.Part{Kind: model.PartAttachment}), model.Unsupported},
		"no content":                 {adapter.SendRequest{Effect: sendReq().Effect}, model.Validation},
	} {
		if _, err := s.Send(t.Context(), c.req); model.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	if n := len(fake.calls(hookAccount)); n != 0 || len(creds.reqs) != 0 {
		t.Fatalf("calls = %d, credential requests = %d; want none", n, len(creds.reqs))
	}
	if _, err := openSender(t, nil, time.Second, creds).Send(t.Context(), sendReq()); model.CodeOf(err) != model.Unavailable {
		t.Fatalf("Send without an HTTP client = %v, want unavailable", err)
	}
}

func TestValidate(t *testing.T) {
	f := sms.NewFactory(sms.Options{})
	if issues := f.Validate(hookConfig().Extension()); len(issues) != 0 {
		t.Fatalf("valid config: %+v", issues)
	}
	svc := sms.Config{AccountSID: hookAccount, MessagingServiceSID: "MG00000000000000000000000000000001"}
	if issues := f.Validate(svc.Extension()); len(issues) != 0 {
		t.Fatalf("service config: %+v", issues)
	}
	for name, e := range map[string]model.Extension{
		"schema":      {Name: "other.schema.config", Version: 1},
		"version":     {Name: sms.ConfigSchema, Version: 2},
		"unknown key": {Name: sms.ConfigSchema, Version: 1, Data: []byte(`{"account_sid":"` + hookAccount + `","from":"+1","api_base_url":"https://evil"}`)},
		"account":     sms.Config{AccountSID: "AC123", From: "+1"}.Extension(),
		"no sender":   sms.Config{AccountSID: hookAccount}.Extension(),
		"service sid": sms.Config{AccountSID: hookAccount, MessagingServiceSID: "MGx"}.Extension(),
		"url query":   sms.Config{AccountSID: hookAccount, From: "+1", WebhookURL: "https://h.example.com/x?y=1"}.Extension(),
		"url creds":   sms.Config{AccountSID: hookAccount, From: "+1", WebhookURL: "https://u:p@h.example.com/x"}.Extension(), // #nosec G101 -- the case under test
		"url scheme":  sms.Config{AccountSID: hookAccount, From: "+1", WebhookURL: "ftp://h.example.com/x"}.Extension(),
	} {
		if issues := f.Validate(e); len(issues) == 0 {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.Open(t.Context(), model.Channel{Ref: sendChannel, Type: model.TypeTwilioWhatsapp, Configuration: hookConfig().Extension()}, adapter.Dependencies{}); model.CodeOf(err) != model.Validation {
		t.Errorf("Open of another type = %v", err)
	}
}
