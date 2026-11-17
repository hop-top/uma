package whatsapp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
)

func fakeWithAccount(t *testing.T) *fakeMeta {
	t.Helper()
	f := newFakeMeta(t)
	if a := f.account(testChannel); a.pnid != testPNID {
		t.Fatalf("fake phone number ID %s, want %s", a.pnid, testPNID)
	}
	return f
}

// The documented send request: POST /{version}/{phone-number-id}/messages,
// bearer token, messaging_product, recipient_type, to, type, text.body;
// a reply carries context.message_id; the operation rides in
// biz_opaque_callback_data.
func TestSendTextRequestShape(t *testing.T) {
	f := fakeWithAccount(t)
	c := newCreds()
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: c})
	e := effect("00000000-0000-7000-8000-0000000000e1")
	target := model.ProviderMessageRef(testChannel, "wamid.inbound-1")
	e.Target = &target
	out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: e, Content: textContent("Yes, in blue.")})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0].Namespace != model.ProviderMessageNamespace || !strings.HasPrefix(out.References[0].ID, "wamid.") {
		t.Fatalf("outcome %+v, want provider_accepted with the wamid", out)
	}
	reqs := f.recordedRequests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.Path != "/v24.0/"+testPNID+"/messages" || r.Auth != "Bearer EAAG-access-token" {
		t.Errorf("path %s auth %q", r.Path, r.Auth)
	}
	b := r.Body
	if b["messaging_product"] != "whatsapp" || b["recipient_type"] != "individual" || b["to"] != testUser || b["type"] != "text" {
		t.Errorf("body %v", b)
	}
	if text, _ := b["text"].(map[string]any); text["body"] != "Yes, in blue." {
		t.Errorf("text %v", b["text"])
	}
	if ctx, _ := b["context"].(map[string]any); ctx["message_id"] != "wamid.inbound-1" {
		t.Errorf("context %v", b["context"])
	}
	cb, _ := b["biz_opaque_callback_data"].(string)
	op, at, ok := whatsapp.ParseCallbackData(cb)
	if !ok || op != e.Operation.ID || at != e.Attempt || len(cb) > whatsapp.MaxCallbackData {
		t.Errorf("callback data %q, want the attempt within %d bytes", cb, whatsapp.MaxCallbackData)
	}
	reqsC := c.requests()
	if len(reqsC) != 1 || reqsC[0].Purpose != whatsapp.PurposeGraph || reqsC[0].Channel != testChannel || reqsC[0].Reference != "meta-app" {
		t.Errorf("credential requests %+v", reqsC)
	}
}

func TestSendTemplateRequestShape(t *testing.T) {
	f := fakeWithAccount(t)
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	tpl := `{"name":"order_update","language":{"code":"en_US"},"components":[{"type":"body","parameters":[{"type":"text","text":"42"}]}]}`
	if _, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op-tpl"), Content: templateContent(tpl)}); err != nil {
		t.Fatal(err)
	}
	b := f.recordedRequests()[0].Body
	tplBody, _ := b["template"].(map[string]any)
	if b["type"] != "template" || tplBody["name"] != "order_update" || b["text"] != nil {
		t.Fatalf("body %v", b)
	}
	if lang, _ := tplBody["language"].(map[string]any); lang["code"] != "en_US" {
		t.Fatalf("language %v", tplBody["language"])
	}
}

// Content the Cloud API path does not express is refused before any call.
func TestSendRefusesUnsupportedContentWithoutCall(t *testing.T) {
	f := fakeWithAccount(t)
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	att := model.Content{Parts: []model.Part{{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{}}}}
	other := model.Extension{Name: "org.example.card", Version: 1}
	for name, c := range map[string]model.Content{
		"two parts":         {Parts: []model.Part{{Kind: model.PartText, Text: "a"}, {Kind: model.PartText, Text: "b"}}},
		"attachment":        att,
		"unknown structure": {Parts: []model.Part{{Kind: model.PartStructured, Structured: &other}}},
		"template no name":  templateContent(`{"language":{"code":"en_US"}}`),
		"template array":    templateContent(`[]`),
	} {
		_, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: c})
		if code := model.CodeOf(err); code != model.Unsupported && code != model.Validation {
			t.Errorf("%s: err = %v, want unsupported or validation", name, err)
		}
	}
	e := effect("op")
	e.Route.Channel = model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0b2"}
	if _, err := inst.Send(t.Context(), adapter.SendRequest{Effect: e, Content: textContent("x")}); model.CodeOf(err) != model.Validation {
		t.Errorf("foreign route: err = %v", err)
	}
	if n := len(f.recordedRequests()); n != 0 {
		t.Fatalf("provider requests = %d, want none", n)
	}
}

// answer is one scripted Graph API answer.
type answer struct {
	status int
	body   string
	header map[string]string
}

func errBody(code int) string {
	return fmt.Sprintf(`{"error":{"message":"(#%d) x","type":"OAuthException","code":%d,"error_data":{"messaging_product":"whatsapp","details":"x"},"fbtrace_id":"Az"}}`, code, code)
}

func answering(t *testing.T, a answer) (*whatsapp.Instance, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.ReadAll(r.Body)
		for k, v := range a.header {
			w.Header().Set(k, v)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(srv.Close)
	f := whatsapp.NewFactory(whatsapp.Options{BaseURL: srv.URL})
	return openInstance(t, f, adapter.Dependencies{HTTP: srv.Client(), Credentials: newCreds()}), &calls
}

// Graph API error codes map to outcomes: refusals rejected, rate limits
// rate_limited with the stated wait, refused tokens and permissions
// unauthorized, temporary faults transient.
func TestSendErrorOutcomes(t *testing.T) {
	type want struct {
		state      model.DeliveryState // set for an outcome
		code       model.ErrorCode
		retryAfter time.Duration
	}
	usage := `{"102290129340398":[{"type":"whatsapp_business_management","call_count":100,"total_cputime":1,"total_time":1,"estimated_time_to_regain_access":3}]}`
	cases := map[string]struct {
		a    answer
		want want
	}{
		"undeliverable 131026":    {answer{400, errBody(131026), nil}, want{model.Rejected, model.Permanent, 0}},
		"invalid parameter 100":   {answer{400, errBody(100), nil}, want{model.Rejected, model.Permanent, 0}},
		"template missing 132001": {answer{400, errBody(132001), nil}, want{model.Rejected, model.Permanent, 0}},
		"window closed 131047":    {answer{400, errBody(131047), nil}, want{model.Rejected, model.Permanent, 0}},
		"ecosystem 131049":        {answer{400, errBody(131049), nil}, want{model.Rejected, model.Permanent, 0}},
		"throughput 130429":       {answer{400, errBody(130429), map[string]string{"Retry-After": "60"}}, want{"", model.RateLimited, time.Minute}},
		"app rate 4 usage header": {answer{429, errBody(4), map[string]string{"X-Business-Use-Case-Usage": usage}}, want{"", model.RateLimited, 3 * time.Minute}},
		"waba rate 80007":         {answer{429, errBody(80007), nil}, want{"", model.RateLimited, 0}},
		"pair rate 131056":        {answer{400, errBody(131056), nil}, want{"", model.RateLimited, 0}},
		"expired token 190":       {answer{401, errBody(190), nil}, want{"", model.Unauthorized, 0}},
		"auth 0":                  {answer{401, errBody(0), nil}, want{"", model.Unauthorized, 0}},
		"permission 10":           {answer{403, errBody(10), nil}, want{"", model.Unauthorized, 0}},
		"permission 200":          {answer{403, errBody(200), nil}, want{"", model.Unauthorized, 0}},
		"access denied 131005":    {answer{403, errBody(131005), nil}, want{"", model.Unauthorized, 0}},
		// Server faults answered after the request was sent may mean the
		// message went out: never transient.
		"unavailable 2":             {answer{503, errBody(2), nil}, want{"", model.OutcomeUnknown, 0}},
		"unknown 131000":            {answer{500, errBody(131000), nil}, want{"", model.OutcomeUnknown, 0}},
		"unknown 131000 as 400":     {answer{400, errBody(131000), nil}, want{"", model.OutcomeUnknown, 0}},
		"service 131016":            {answer{503, errBody(131016), nil}, want{"", model.OutcomeUnknown, 0}},
		"maintenance 131057":        {answer{400, errBody(131057), nil}, want{"", model.OutcomeUnknown, 0}},
		"server 133004":             {answer{503, errBody(133004), nil}, want{"", model.OutcomeUnknown, 0}},
		"invalid request 1 as 500":  {answer{500, errBody(1), nil}, want{"", model.OutcomeUnknown, 0}},
		"unknown code as 500":       {answer{500, errBody(987654), nil}, want{"", model.OutcomeUnknown, 0}},
		"rate limit as 503":         {answer{503, errBody(130429), map[string]string{"Retry-After": "30"}}, want{"", model.RateLimited, 30 * time.Second}},
		"gateway without body":      {answer{502, "<html/>", nil}, want{"", model.OutcomeUnknown, 0}},
		"429 without body":          {answer{429, "", map[string]string{"Retry-After": "5"}}, want{"", model.RateLimited, 5 * time.Second}},
		"400 without body":          {answer{400, "nope", nil}, want{model.Rejected, model.Permanent, 0}},
		"200 without message id":    {answer{200, `{"messaging_product":"whatsapp","messages":[]}`, nil}, want{"", model.OutcomeUnknown, 0}},
		"held for quality":          {answer{200, `{"messaging_product":"whatsapp","contacts":[{"input":"1","wa_id":"1"}],"messages":[{"id":"wamid.x","message_status":"held_for_quality_assessment"}]}`, nil}, want{model.Queued, "", 0}},
		"accepted without a status": {answer{200, `{"messaging_product":"whatsapp","contacts":[{"input":"1","wa_id":"1"}],"messages":[{"id":"wamid.x"}]}`, nil}, want{model.ProviderAccepted, "", 0}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			inst, _ := answering(t, c.a)
			out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
			if c.want.state != "" {
				if err != nil || out.State != c.want.state {
					t.Fatalf("Send = %+v, %v; want state %s", out, err, c.want.state)
				}
				if c.want.code != "" && (out.Failure == nil || out.Failure.Code != c.want.code || out.Failure.Retry != model.NoRetry) {
					t.Fatalf("failure %+v, want %s without retry", out.Failure, c.want.code)
				}
				return
			}
			var me *model.Error
			if !errors.As(err, &me) || me.Code != c.want.code {
				t.Fatalf("Send = %+v, %v; want error %s", out, err, c.want.code)
			}
			if me.RetryAfter != c.want.retryAfter {
				t.Fatalf("RetryAfter = %v, want %v", me.RetryAfter, c.want.retryAfter)
			}
			if c.want.code == model.Unauthorized && me.Retry != model.NoRetry {
				t.Fatalf("auth error retry = %s, want none", me.Retry)
			}
			if strings.Contains(me.Error(), "EAAG") || strings.Contains(me.Error(), "(#") {
				t.Fatalf("error %q leaks provider text or credentials", me.Error())
			}
		})
	}
}

// A request that was sent but got no answer is outcome unknown; one that
// never left the host is transient.
func TestSendTransportOutcomes(t *testing.T) {
	t.Run("connection dropped", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			conn, _, _ := w.(http.Hijacker).Hijack()
			_ = conn.Close()
		}))
		defer srv.Close()
		inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{BaseURL: srv.URL}), adapter.Dependencies{HTTP: srv.Client(), Credentials: newCreds()})
		_, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
		if model.CodeOf(err) != model.OutcomeUnknown {
			t.Fatalf("err = %v, want outcome_unknown", err)
		}
	})
	t.Run("client timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		hc := srv.Client()
		hc.Timeout = 50 * time.Millisecond
		inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{BaseURL: srv.URL}), adapter.Dependencies{HTTP: hc, Credentials: newCreds()})
		_, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
		if model.CodeOf(err) != model.OutcomeUnknown {
			t.Fatalf("err = %v, want outcome_unknown", err)
		}
	})
	t.Run("context deadline", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
		defer srv.Close()
		defer close(release)
		inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{BaseURL: srv.URL}), adapter.Dependencies{HTTP: srv.Client(), Credentials: newCreds()})
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		_, err := inst.Send(ctx, adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
		// The engine records a cancelled call as outcome unknown.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the deadline", err)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		base := "http://" + l.Addr().String()
		_ = l.Close()
		inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{BaseURL: base}), adapter.Dependencies{HTTP: http.DefaultClient, Credentials: newCreds()})
		_, err = inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
		if model.CodeOf(err) != model.Transient {
			t.Fatalf("err = %v, want transient", err)
		}
	})
}

// With window state, a free-form message is sent only inside the 24-hour
// customer service window; outside it only a template goes out.
func TestServiceWindow(t *testing.T) {
	cases := map[string]struct {
		last     time.Duration // before testNow; 0 = never
		content  model.Content
		wantSent bool
	}{
		"text, no inbound recorded": {0, textContent("hi"), true},
		"text, inbound 1h ago":      {time.Hour, textContent("hi"), true},
		"text, inbound 23h59m ago":  {24*time.Hour - time.Minute, textContent("hi"), true},
		"text, inbound exactly 24h": {24 * time.Hour, textContent("hi"), false},
		"text, inbound 25h ago":     {25 * time.Hour, textContent("hi"), false},
		"template, no inbound ever": {0, templateContent(helloTemplate), true},
		"template, inbound 25h ago": {25 * time.Hour, templateContent(helloTemplate), true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := fakeWithAccount(t)
			p := newProtocol()
			if c.last > 0 {
				p.setLastInbound(testUser, testNow.Add(-c.last))
			}
			cr := newCreds()
			inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: cr, Protocol: p})
			out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: c.content})
			if err != nil {
				t.Fatal(err)
			}
			sent := len(f.recordedRequests()) == 1
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
			if len(cr.requests()) != 0 {
				t.Fatal("credential resolved for a message refused before any call")
			}
		})
	}
	t.Run("no window state: Meta decides", func(t *testing.T) {
		f := fakeWithAccount(t)
		inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
		if out, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("Send = %+v, %v", out, err)
		}
	})
	t.Run("window state unavailable", func(t *testing.T) {
		f := fakeWithAccount(t)
		p := newProtocol()
		p.fail = model.NewError(model.Unavailable, "down")
		inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds(), Protocol: p})
		_, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
		if model.CodeOf(err) != model.Unavailable || len(f.recordedRequests()) != 0 {
			t.Fatalf("err = %v, requests %d; want unavailable, no call", err, len(f.recordedRequests()))
		}
	})
}

func TestOpenValidatesConfiguration(t *testing.T) {
	f := whatsapp.NewFactory(whatsapp.Options{})
	for name, cfg := range map[string]model.Extension{
		"other schema":  {Name: "meta.messenger.config", Version: 1, Data: []byte(`{"phone_number_id":"1"}`)},
		"no number":     whatsapp.Config{}.Extension(),
		"not digits":    whatsapp.Config{PhoneNumberID: "+1555"}.Extension(),
		"bad version":   whatsapp.Config{PhoneNumberID: "1", APIVersion: "24"}.Extension(),
		"unknown field": {Name: whatsapp.ConfigSchema, Version: 1, Data: []byte(`{"phone_number_id":"1","base_url":"https://evil.example"}`)},
	} {
		if len(f.Validate(cfg)) == 0 {
			t.Errorf("%s: Validate accepted", name)
		}
	}
	if issues := f.Validate(whatsapp.Config{PhoneNumberID: testPNID, APIVersion: "v23.0"}.Extension()); len(issues) != 0 {
		t.Errorf("valid config: %+v", issues)
	}
	ch := channel(testPNID)
	ch.Type = model.TypeTwilioWhatsapp
	if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); model.CodeOf(err) != model.Validation {
		t.Errorf("Open of another type = %v", err)
	}
}

func TestSendWithoutHTTPClientMakesNoCall(t *testing.T) {
	inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{}), adapter.Dependencies{Credentials: newCreds()})
	_, err := inst.Send(t.Context(), adapter.SendRequest{Effect: effect("op"), Content: textContent("hi")})
	if model.CodeOf(err) != model.Unavailable {
		t.Fatalf("err = %v", err)
	}
}

// Every send carries its attempt in biz_opaque_callback_data; a send with
// no attempt carries none rather than a made-up one.
func TestSendCarriesAttemptPerAttempt(t *testing.T) {
	f := fakeWithAccount(t)
	inst := openInstance(t, f.factory(), adapter.Dependencies{HTTP: f.client(), Credentials: newCreds()})
	for _, at := range []model.ID{"00000000-0000-7000-8000-0000000000a1", "00000000-0000-7000-8000-0000000000a2", ""} {
		e := effect("00000000-0000-7000-8000-0000000000e1")
		e.Attempt = at
		if _, err := inst.Send(t.Context(), adapter.SendRequest{Effect: e, Content: textContent("hi")}); err != nil {
			t.Fatal(err)
		}
	}
	reqs := f.recordedRequests()
	for i, want := range []model.ID{"00000000-0000-7000-8000-0000000000a1", "00000000-0000-7000-8000-0000000000a2"} {
		_, at, ok := whatsapp.ParseCallbackData(reqs[i].Body["biz_opaque_callback_data"].(string))
		if !ok || at != want {
			t.Errorf("send %d carries attempt %q, want %q", i, at, want)
		}
	}
	if v, ok := reqs[2].Body["biz_opaque_callback_data"]; ok {
		t.Errorf("send without an attempt carries %v", v)
	}
}
