package relay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

func ref(id string) model.ExternalRef { return model.ProviderMessageRef(testChannel, id) }

func TestSendPostsOneJSONMessage(t *testing.T) {
	r := newSendRig(t, nil)
	out, err := r.inst.Send(t.Context(), sendReq("hello"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != ref("gw-1") {
		t.Fatalf("outcome %+v", out)
	}
	reqs := r.fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	q := reqs[0]
	if q.Method != http.MethodPost || q.Path != "/out" {
		t.Errorf("%s %s", q.Method, q.Path)
	}
	h := q.Header
	if h.Get("Authorization") != "Bearer "+callbackSecret {
		t.Errorf("Authorization %q", h.Get("Authorization"))
	}
	if h.Get("Content-Type") != "application/json" || h.Get("Accept") != "application/json" {
		t.Errorf("content negotiation %q %q", h.Get("Content-Type"), h.Get("Accept"))
	}
	// The key the gateway may dedupe on: stable for the operation, so a
	// resend of the same operation is recognizable.
	if h.Get("Idempotency-Key") != string(testOp) {
		t.Errorf("Idempotency-Key %q", h.Get("Idempotency-Key"))
	}
	want := map[string]any{
		"medium": "sms", "to": "+15550100001", "body": "hello",
		"operation_id": string(testOp), "attempt_id": string(testAttempt),
	}
	for k, v := range want {
		if q.Body[k] != v {
			t.Errorf("%s = %v, want %v (body %s)", k, q.Body[k], v, q.Raw)
		}
	}
	if _, ok := q.Body["in_reply_to"]; ok {
		t.Errorf("in_reply_to present without a target: %s", q.Raw)
	}
}

func TestSendWhatsAppMedium(t *testing.T) {
	r := newSendRig(t, func(c *relay.Config) { c.Medium = relay.MediumWhatsApp })
	req := sendReq("hi")
	req.Route.Namespace = relay.NamespaceWhatsApp
	if _, err := r.inst.Send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.requests()[0].Body["medium"]; got != "whatsapp" {
		t.Errorf("medium %v", got)
	}
}

func TestSendResolvesTheCallbackCredentialPerCall(t *testing.T) {
	r := newSendRig(t, nil)
	for range 2 {
		if _, err := r.inst.Send(t.Context(), sendReq("x")); err != nil {
			t.Fatal(err)
		}
	}
	reqs := r.creds.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d credential requests, want one per call", len(reqs))
	}
	for _, q := range reqs {
		if q.Purpose != relay.PurposeCallback || q.Channel != testChannel || q.Reference != "relay-account" {
			t.Errorf("request %+v", q)
		}
	}
}

func TestSendHeaderAuth(t *testing.T) {
	r := newSendRig(t, func(c *relay.Config) {
		c.Callback.Auth, c.Callback.Header = relay.AuthHeader, "X-Relay-Token"
	})
	if _, err := r.inst.Send(t.Context(), sendReq("x")); err != nil {
		t.Fatal(err)
	}
	h := r.fake.requests()[0].Header
	if h.Get("X-Relay-Token") != callbackSecret || h.Get("Authorization") != "" {
		t.Errorf("headers %v", h)
	}
}

func TestSendNamesTheMessageItAnswers(t *testing.T) {
	r := newSendRig(t, nil)
	req := sendReq("re")
	target := ref("m-1")
	req.Target = &target
	if _, err := r.inst.Send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.requests()[0].Body["in_reply_to"]; got != "m-1" {
		t.Errorf("in_reply_to %v", got)
	}
	// Another channel's message is not this relay's to name.
	other := model.ProviderMessageRef(model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0a2"}, "x")
	req.Target = &other
	if _, err := r.inst.Send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.fake.requests()[1].Body["in_reply_to"]; ok {
		t.Error("a foreign message id reached the relay")
	}
}

func TestSendJoinsTextParts(t *testing.T) {
	r := newSendRig(t, nil)
	req := sendReq("")
	req.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "a"}, {Kind: model.PartText, Text: "b"}}}
	if _, err := r.inst.Send(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.requests()[0].Body["body"]; got != "a\nb" {
		t.Errorf("body %q", got)
	}
}

func TestSendRefusesBeforeAnyRequest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*adapter.SendRequest)
		code   model.ErrorCode
	}{
		{"no content", func(r *adapter.SendRequest) { r.Content = model.Content{} }, model.Validation},
		{"blank text", func(r *adapter.SendRequest) { r.Content.Parts[0].Text = " \n" }, model.Validation},
		{"too large", func(r *adapter.SendRequest) { r.Content.Parts[0].Text = strings.Repeat("x", relay.MaxCallbackBytes+1) }, model.Validation},
		{"structured part", func(r *adapter.SendRequest) {
			r.Content.Parts = append(r.Content.Parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: "a.b.c", Version: 1, Data: []byte(`{}`)}})
		}, model.Unsupported},
		{"attachment part", func(r *adapter.SendRequest) { r.Content.Parts[0] = model.Part{Kind: model.PartAttachment} }, model.Unsupported},
		{"internal audience", func(r *adapter.SendRequest) { r.Audience.Visibility = model.Internal }, model.Unsupported},
		{"other channel route", func(r *adapter.SendRequest) {
			r.Route.Channel = model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0a2"}
		}, model.Validation},
		{"other namespace route", func(r *adapter.SendRequest) { r.Route.Namespace = relay.NamespaceWhatsApp }, model.Validation},
		{"other kind route", func(r *adapter.SendRequest) { r.Route.Kind = "user" }, model.Validation},
		{"empty route", func(r *adapter.SendRequest) { r.Route.ID = "" }, model.Validation},
		{"no attempt", func(r *adapter.SendRequest) { r.Attempt = "" }, model.Validation},
		{"no operation", func(r *adapter.SendRequest) { r.Operation = model.OperationRef{} }, model.Validation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newSendRig(t, nil)
			req := sendReq("hi")
			tt.mutate(&req)
			_, err := r.inst.Send(t.Context(), req)
			if model.CodeOf(err) != tt.code {
				t.Fatalf("error %v, want %s", err, tt.code)
			}
			if len(r.fake.requests()) != 0 || len(r.creds.requests()) != 0 {
				t.Errorf("%d requests, %d credential lookups before a refusal", len(r.fake.requests()), len(r.creds.requests()))
			}
		})
	}
}

func TestSendWithoutCallbackOrClient(t *testing.T) {
	f := newFakeRelay(t)
	noCallback := open(t, f.factory(0), baseConfig(), adapter.Dependencies{Credentials: newCreds(), HTTP: plainClient()})
	if _, err := noCallback.Send(t.Context(), sendReq("x")); model.CodeOf(err) != model.Disabled {
		t.Errorf("no callback: %v", err)
	}
	noClient := open(t, f.factory(0), f.config(), adapter.Dependencies{Credentials: newCreds()})
	if _, err := noClient.Send(t.Context(), sendReq("x")); model.CodeOf(err) != model.Unavailable {
		t.Errorf("no client: %v", err)
	}
	noCreds := open(t, f.factory(0), f.config(), adapter.Dependencies{HTTP: plainClient()})
	if _, err := noCreds.Send(t.Context(), sendReq("x")); model.CodeOf(err) != model.Unavailable {
		t.Errorf("no credentials port: %v", err)
	}
	closed := open(t, f.factory(0), f.config(), adapter.Dependencies{Credentials: newCreds(), HTTP: plainClient()})
	_ = closed.Close(t.Context())
	if _, err := closed.Send(t.Context(), sendReq("x")); model.CodeOf(err) != model.Unavailable {
		t.Errorf("closed: %v", err)
	}
	if len(f.requests()) != 0 {
		t.Errorf("%d requests", len(f.requests()))
	}
}

func TestSendCredentialFailuresMakeNoRequest(t *testing.T) {
	for name, set := range map[string]func(*creds){
		"port error":       func(c *creds) { c.fail = model.NewError(model.Unauthorized, "consent revoked") },
		"empty secret":     func(c *creds) { c.set(relay.PurposeCallback, secretOf("")) },
		"oauth token":      func(c *creds) { c.set(relay.PurposeCallback, oauthOf("tok")) },
		"header injection": func(c *creds) { c.set(relay.PurposeCallback, secretOf("abc\r\nX-Evil: 1")) },
		"space in secret":  func(c *creds) { c.set(relay.PurposeCallback, secretOf("abc def")) },
		"non-ascii secret": func(c *creds) { c.set(relay.PurposeCallback, secretOf("abc\u00e9")) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newSendRig(t, nil)
			set(r.creds)
			if _, err := r.inst.Send(t.Context(), sendReq("x")); err == nil {
				t.Fatal("sent without a usable credential")
			}
			if len(r.fake.requests()) != 0 {
				t.Fatal("a request left without a credential")
			}
		})
	}
}

func TestSendAcceptsAPasswordCredential(t *testing.T) {
	r := newSendRig(t, nil)
	r.creds.set(relay.PurposeCallback, passwordOf("relay", callbackSecret))
	if _, err := r.inst.Send(t.Context(), sendReq("x")); err != nil {
		t.Fatal(err)
	}
	if got := r.fake.requests()[0].Header.Get("Authorization"); got != "Bearer "+callbackSecret {
		t.Errorf("Authorization %q", got)
	}
}

func TestSendReferenceFromTheAnswer(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*relay.Config)
		answer http.HandlerFunc
		want   model.ExternalRef
	}{
		{"configured key", func(c *relay.Config) { c.Callback.MessageID = []string{"id", "sid"} },
			jsonAnswer(`{"sid":"S-7"}`), ref("S-7")},
		{"first key present wins", func(c *relay.Config) { c.Callback.MessageID = []string{"id", "sid"} },
			jsonAnswer(`{"sid":"S-7","id":"I-1"}`), ref("I-1")},
		{"no id: the attempt names it", nil, jsonAnswer(`{"ok":true}`), ref(string(testAttempt))},
		{"empty body", nil, status(http.StatusNoContent), ref(string(testAttempt))},
		{"not json", nil, textAnswer("queued"), ref(string(testAttempt))},
		{"id not a string", nil, jsonAnswer(`{"message_id":42}`), ref(string(testAttempt))},
		{"id with control characters", nil, jsonAnswer("{\"message_id\":\"a\\u0000b\"}"), ref(string(testAttempt))},
		{"id too long", nil, jsonAnswer(`{"message_id":"` + strings.Repeat("m", 300) + `"}`), ref(string(testAttempt))},
		{"id past the read bound", nil, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"pad":"`+strings.Repeat("x", relay.MaxAnswerBytes)+`","message_id":"late"}`)
		}, ref(string(testAttempt))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newSendRig(t, tt.mutate)
			r.fake.script(tt.answer)
			out, err := r.inst.Send(t.Context(), sendReq("x"))
			if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != tt.want {
				t.Fatalf("outcome %+v err %v, want %+v", out, err, tt.want)
			}
		})
	}
}

func TestSendStatusMapping(t *testing.T) {
	type want struct {
		state model.DeliveryState // set: an outcome
		code  model.ErrorCode     // set: an error
		retry model.RetryAdvice
	}
	accepted := want{state: model.ProviderAccepted}
	rejected := want{state: model.Rejected}
	unknown := want{code: model.OutcomeUnknown}
	tests := []struct {
		status int
		want   want
	}{
		{200, accepted}, {201, accepted}, {202, accepted}, {204, accepted},
		{400, rejected}, {404, rejected}, {410, rejected}, {413, rejected}, {422, rejected},
		{301, rejected}, {302, rejected}, {307, rejected}, {308, rejected},
		{401, want{code: model.Unauthorized, retry: model.NoRetry}},
		{403, want{code: model.Unauthorized, retry: model.NoRetry}},
		{429, want{code: model.RateLimited, retry: model.RetrySameOperation}},
		{408, unknown}, {409, unknown}, {500, unknown}, {502, unknown}, {503, unknown}, {504, unknown},
	}
	for _, tt := range tests {
		t.Run(http.StatusText(tt.status), func(t *testing.T) {
			r := newSendRig(t, nil)
			r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
				if tt.status >= 300 && tt.status < 400 {
					w.Header().Set("Location", "http://elsewhere.invalid/x")
				}
				w.WriteHeader(tt.status)
			})
			out, err := r.inst.Send(t.Context(), sendReq("x"))
			switch {
			case tt.want.code != "":
				var me *model.Error
				if !errors.As(err, &me) || me.Code != tt.want.code || (tt.want.retry != "" && me.Retry != tt.want.retry) {
					t.Fatalf("error %v, want %s", err, tt.want.code)
				}
			case err != nil || out.State != tt.want.state:
				t.Fatalf("outcome %+v err %v, want %s", out, err, tt.want.state)
			}
			if tt.want.state == model.Rejected && (out.Failure == nil || out.Failure.Retry != model.NoRetry) {
				t.Errorf("rejection %+v must be final", out.Failure)
			}
			if tt.status/100 == 3 && !strings.Contains(out.Failure.Error(), "redirect") {
				t.Errorf("failure %q does not say why", out.Failure)
			}
			if tt.want.state == model.ProviderAccepted && len(out.References) != 1 {
				t.Errorf("references %+v", out.References)
			}
		})
	}
}

func TestSendRateLimitStatesItsWait(t *testing.T) {
	r := newSendRig(t, nil)
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := r.inst.Send(t.Context(), sendReq("x"))
	var me *model.Error
	if !errors.As(err, &me) || me.RetryAfter != 7*time.Second {
		t.Fatalf("error %v", err)
	}
}

// Nothing the relay answers with, and no credential, reaches an error.
func TestSendErrorsCarryNeitherAnswerNorSecret(t *testing.T) {
	r := newSendRig(t, nil)
	r.fake.script(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom "+req.Header.Get("Authorization")+" internal-host.corp") // #nosec G705 -- fake provider echoing a leak probe
	})
	_, err := r.inst.Send(t.Context(), sendReq("x"))
	for _, leak := range []string{callbackSecret, "boom", "internal-host", "127.0.0.1"} {
		if err != nil && strings.Contains(err.Error(), leak) {
			t.Errorf("error %q discloses %q", err, leak)
		}
	}
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, "bad number "+callbackSecret)
	})
	out, _ := r.inst.Send(t.Context(), sendReq("x"))
	if out.Failure == nil || strings.Contains(out.Failure.Error(), "bad number") || strings.Contains(out.Failure.Error(), callbackSecret) {
		t.Errorf("failure %+v", out.Failure)
	}
}

func TestSendTransportFailures(t *testing.T) {
	t.Run("nothing sent: transient", func(t *testing.T) {
		r := newSendRig(t, nil)
		r.fake.srv.Close() // the dial is refused
		_, err := r.inst.Send(t.Context(), sendReq("x"))
		if model.CodeOf(err) != model.Transient {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("answer lost: unknown", func(t *testing.T) {
		r := newSendRig(t, nil)
		r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		})
		_, err := r.inst.Send(t.Context(), sendReq("x"))
		if model.CodeOf(err) != model.OutcomeUnknown {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("no answer in time: unknown", func(t *testing.T) {
		f := newFakeRelay(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.script(func(http.ResponseWriter, *http.Request) { <-release })
		inst := open(t, f.factory(50*time.Millisecond), f.config(), adapter.Dependencies{Credentials: newCreds(), HTTP: plainClient()})
		start := time.Now()
		_, err := inst.Send(t.Context(), sendReq("x"))
		if model.CodeOf(err) != model.OutcomeUnknown {
			t.Fatalf("error %v", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("took %v: the timeout is not bounding the call", time.Since(start))
		}
	})
	t.Run("caller gives up", func(t *testing.T) {
		r := newSendRig(t, nil)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := r.inst.Send(ctx, sendReq("x"))
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error %v", err)
		}
	})
}

// A credential goes to the configured URL and nowhere else.
func TestSendNeverFollowsARedirect(t *testing.T) {
	elsewhere := newFakeRelay(t)
	r := newSendRig(t, nil)
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", elsewhere.srv.URL+"/steal")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	out, err := r.inst.Send(t.Context(), sendReq("x"))
	if err != nil || out.State != model.Rejected {
		t.Fatalf("outcome %+v err %v", out, err)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("the redirect target received %d requests: %+v", n, elsewhere.requests())
	}
}

// A host client that follows redirects itself cannot be stopped from
// sending, but its answer is never taken for the callback's.
func TestSendDoesNotAcceptAnAnswerFromAnotherURL(t *testing.T) {
	elsewhere := newFakeRelay(t)
	f := newFakeRelay(t)
	f.script(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/other", http.StatusTemporaryRedirect)
	})
	inst := open(t, f.factory(0), f.config(), adapter.Dependencies{Credentials: newCreds(), HTTP: &http.Client{}})
	_, err := inst.Send(t.Context(), sendReq("x"))
	if model.CodeOf(err) != model.OutcomeUnknown {
		t.Fatalf("error %v: an answer from another URL must not settle the attempt", err)
	}
}

func TestSendAnswerReadIsBounded(t *testing.T) {
	r := newSendRig(t, nil)
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := []byte(strings.Repeat("x", 64<<10))
		for range 160 { // 10 MiB offered
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	if out, err := r.inst.Send(t.Context(), sendReq("x")); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}

func TestOpenRefusesACallbackTheHostDoesNotAllow(t *testing.T) {
	f := newFakeRelay(t)
	cfg := f.config()
	cfg.Callback.URL = "http://evil.example.net/out"
	if _, err := f.factory(0).Open(t.Context(), channelOf(cfg), adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
		t.Fatalf("Open: %v", err)
	}
}
