package relay_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// withLookup configures the rig's callback with a lookup on the fake relay.
func withLookup(mutate func(*relay.Config)) func(*relay.Config) {
	return func(c *relay.Config) {
		c.Callback.LookupURL = strings.TrimSuffix(c.Callback.URL, "/out") + "/lookup"
		if mutate != nil {
			mutate(c)
		}
	}
}

func reconcileReq() adapter.ReconcileRequest {
	s := sendReq("hello")
	return adapter.ReconcileRequest{Effect: s.Effect, Capability: model.CapabilitySend, Content: s.Content, AttemptedAt: testNow}
}

func TestReconcileAsksTheLookupByOperation(t *testing.T) {
	r := newSendRig(t, withLookup(nil))
	r.fake.script(jsonAnswer(`{"status":"sent","message_id":"gw-9"}`))
	out, err := r.inst.Reconcile(t.Context(), reconcileReq())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if out.State != model.ProviderAccepted || len(out.References) != 1 || out.References[0] != ref("gw-9") {
		t.Fatalf("outcome %+v", out)
	}
	reqs := r.fake.requests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	q := reqs[0]
	u, err := url.Parse(q.Path)
	if err != nil {
		t.Fatal(err)
	}
	if q.Method != http.MethodGet || u.Path != "/lookup" {
		t.Errorf("%s %s", q.Method, q.Path)
	}
	// The operation is the key: the callback's Idempotency-Key, named by
	// one query parameter and nothing else.
	if got := u.Query(); len(got) != 1 || got.Get(relay.LookupQuery) != string(testOp) {
		t.Errorf("query %v", got)
	}
	if len(q.Raw) != 0 {
		t.Errorf("a lookup carries no body: %q", q.Raw)
	}
	h := q.Header
	if h.Get("Authorization") != "Bearer "+callbackSecret || h.Get("Accept") != "application/json" {
		t.Errorf("headers %v", h)
	}
	if creds := r.creds.requests(); len(creds) != 1 || creds[0].Purpose != relay.PurposeCallback || creds[0].Channel != testChannel {
		t.Errorf("credential requests %+v", creds)
	}
}

func TestReconcileHeaderAuth(t *testing.T) {
	r := newSendRig(t, withLookup(func(c *relay.Config) {
		c.Callback.Auth, c.Callback.Header = relay.AuthHeader, "X-Relay-Token"
	}))
	r.fake.script(jsonAnswer(`{"status":"sent"}`))
	if _, err := r.inst.Reconcile(t.Context(), reconcileReq()); err != nil {
		t.Fatal(err)
	}
	h := r.fake.requests()[0].Header
	if h.Get("X-Relay-Token") != callbackSecret || h.Get("Authorization") != "" {
		t.Errorf("headers %v", h)
	}
}

func TestReconcileOfAStartedConversation(t *testing.T) {
	r := newSendRig(t, withLookup(nil))
	r.fake.script(jsonAnswer(`{"status":"sent","message_id":"gw-2"}`))
	req := reconcileReq()
	req.Capability = model.CapabilityStartConversation
	if out, err := r.inst.Reconcile(t.Context(), req); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}

func TestReconcileAnswers(t *testing.T) {
	type want struct {
		state model.DeliveryState // set: an outcome
		code  model.ErrorCode     // the outcome's failure, or the error
		ref   string              // the reference of an accepted outcome
		retry model.RetryAdvice   // of an error
	}
	accepted := func(id string) want { return want{state: model.ProviderAccepted, ref: id} }
	notFound := want{state: model.Failed, code: model.NotFound}
	unknown := want{state: model.Unknown, code: model.OutcomeUnknown}
	transient := want{code: model.Transient, retry: model.RetrySameOperation}
	permanent := want{code: model.Permanent, retry: model.NoRetry}
	answer := func(code int, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if code >= 300 && code < 400 {
				w.Header().Set("Location", "http://elsewhere.invalid/x")
			}
			w.WriteHeader(code)
			_, _ = io.WriteString(w, body)
		}
	}
	tests := []struct {
		name   string
		mutate func(*relay.Config)
		answer http.HandlerFunc
		want   want
	}{
		{"sent with an id", nil, answer(200, `{"status":"sent","message_id":"gw-7"}`), accepted("gw-7")},
		{"sent under a configured key", func(c *relay.Config) { c.Callback.MessageID = []string{"sid"} },
			answer(200, `{"status":"sent","sid":"S-1","message_id":"other"}`), accepted("S-1")},
		{"sent without an id: the attempt names it", nil, answer(200, `{"status":"sent"}`), accepted(string(testAttempt))},
		{"sent with an unusable id", nil, answer(200, `{"status":"sent","message_id":42}`), accepted(string(testAttempt))},
		{"sent, other 2xx", nil, answer(203, `{"status":"sent","message_id":"gw-8"}`), accepted("gw-8")},
		{"not found", nil, answer(200, `{"status":"not_found"}`), notFound},
		{"not found as 404", nil, answer(404, `{"status":"not_found"}`), notFound},
		// Only an explicit answer is evidence: anything else proves nothing.
		{"404 without the status", nil, answer(404, `not found`), unknown},
		{"404 claiming sent", nil, answer(404, `{"status":"sent"}`), unknown},
		{"no status", nil, answer(200, `{"message_id":"gw-1"}`), unknown},
		{"status not a string", nil, answer(200, `{"status":true}`), unknown},
		{"status null", nil, answer(200, `{"status":null}`), unknown},
		{"other status", nil, answer(200, `{"status":"pending"}`), unknown},
		{"status in other case", nil, answer(200, `{"status":"SENT"}`), unknown},
		{"not json", nil, answer(200, `sent`), unknown},
		{"json array", nil, answer(200, `["sent"]`), unknown},
		{"empty", nil, status(http.StatusNoContent), unknown},
		{"answer over the read bound", nil, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"status":"not_found","pad":"`+strings.Repeat("x", relay.MaxAnswerBytes)+`"}`)
		}, unknown},
		// What was read parses, but the answer went on: it is not read whole.
		{"valid prefix over the read bound", nil, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"status":"not_found"}`+strings.Repeat(" ", relay.MaxAnswerBytes))
		}, unknown},
		{"answer at the read bound", nil, func(w http.ResponseWriter, _ *http.Request) {
			body := `{"status":"not_found"}`
			_, _ = io.WriteString(w, body+strings.Repeat(" ", relay.MaxAnswerBytes-len(body)))
		}, notFound},
		{"401", nil, answer(401, ``), want{code: model.Unauthorized, retry: model.NoRetry}},
		{"403", nil, answer(403, ``), want{code: model.Unauthorized, retry: model.NoRetry}},
		{"429", nil, answer(429, ``), want{code: model.RateLimited, retry: model.RetrySameOperation}},
		{"408", nil, answer(408, ``), transient},
		{"500", nil, answer(500, `{"status":"not_found"}`), transient},
		{"502", nil, answer(502, ``), transient},
		{"503", nil, answer(503, ``), transient},
		{"301", nil, answer(301, ``), permanent},
		{"307", nil, answer(307, ``), permanent},
		{"400", nil, answer(400, ``), permanent},
		{"410", nil, answer(410, `{"status":"not_found"}`), permanent},
		{"422", nil, answer(422, ``), permanent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newSendRig(t, withLookup(tt.mutate))
			r.fake.script(tt.answer)
			out, err := r.inst.Reconcile(t.Context(), reconcileReq())
			if tt.want.state == "" {
				var me *model.Error
				if !errors.As(err, &me) || me.Code != tt.want.code || me.Retry != tt.want.retry {
					t.Fatalf("outcome %+v err %v, want error %s (%s)", out, err, tt.want.code, tt.want.retry)
				}
				return
			}
			if err != nil || out.State != tt.want.state {
				t.Fatalf("outcome %+v err %v, want %s", out, err, tt.want.state)
			}
			switch {
			case tt.want.ref != "":
				if len(out.References) != 1 || out.References[0] != ref(tt.want.ref) || out.Failure != nil {
					t.Errorf("outcome %+v, want reference %s", out, tt.want.ref)
				}
			case out.Failure == nil || out.Failure.Code != tt.want.code || len(out.References) != 0:
				t.Errorf("outcome %+v, want failure %s and no reference", out, tt.want.code)
			}
		})
	}
}

func TestReconcileRateLimitStatesItsWait(t *testing.T) {
	r := newSendRig(t, withLookup(nil))
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := r.inst.Reconcile(t.Context(), reconcileReq())
	var me *model.Error
	if !errors.As(err, &me) || me.RetryAfter != 9*time.Second {
		t.Fatalf("error %v", err)
	}
}

// Without a lookup, reconcile is disabled: nothing is resolved or asked.
func TestReconcileWithoutLookupMakesNoRequest(t *testing.T) {
	r := newSendRig(t, nil)
	_, err := r.inst.Reconcile(t.Context(), reconcileReq())
	var me *model.Error
	if !errors.As(err, &me) || me.Code != model.Disabled || me.Retry != model.NoRetry {
		t.Fatalf("error %v, want disabled", err)
	}
	noCallback := open(t, r.fake.factory(0), baseConfig(), adapter.Dependencies{Credentials: r.creds, HTTP: plainClient()})
	if _, err := noCallback.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Disabled {
		t.Errorf("no callback: %v", err)
	}
	if len(r.fake.requests()) != 0 || len(r.creds.requests()) != 0 {
		t.Errorf("%d requests, %d credential lookups without a lookup", len(r.fake.requests()), len(r.creds.requests()))
	}
}

func TestReconcileRefusesBeforeAnyRequest(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*adapter.ReconcileRequest)
		code   model.ErrorCode
	}{
		{"another capability", func(r *adapter.ReconcileRequest) { r.Capability = model.CapabilityEdit }, model.Unsupported},
		{"no capability", func(r *adapter.ReconcileRequest) { r.Capability = "" }, model.Unsupported},
		{"other channel route", func(r *adapter.ReconcileRequest) {
			r.Route.Channel = model.ChannelRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000c0a2"}
		}, model.Validation},
		{"other namespace route", func(r *adapter.ReconcileRequest) { r.Route.Namespace = relay.NamespaceWhatsApp }, model.Validation},
		{"empty route", func(r *adapter.ReconcileRequest) { r.Route.ID = "" }, model.Validation},
		{"no operation", func(r *adapter.ReconcileRequest) { r.Operation = model.OperationRef{} }, model.Validation},
		{"no attempt", func(r *adapter.ReconcileRequest) { r.Attempt = "" }, model.Validation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newSendRig(t, withLookup(nil))
			req := reconcileReq()
			tt.mutate(&req)
			if _, err := r.inst.Reconcile(t.Context(), req); model.CodeOf(err) != tt.code {
				t.Fatalf("error %v, want %s", err, tt.code)
			}
			if len(r.fake.requests()) != 0 || len(r.creds.requests()) != 0 {
				t.Errorf("%d requests, %d credential lookups before a refusal", len(r.fake.requests()), len(r.creds.requests()))
			}
		})
	}
}

func TestReconcileWithoutClientCredentialsOrWhenClosed(t *testing.T) {
	f := newFakeRelay(t)
	cfg := f.config()
	withLookup(nil)(&cfg)
	noClient := open(t, f.factory(0), cfg, adapter.Dependencies{Credentials: newCreds()})
	if _, err := noClient.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Unavailable {
		t.Errorf("no client: %v", err)
	}
	noCreds := open(t, f.factory(0), cfg, adapter.Dependencies{HTTP: plainClient()})
	if _, err := noCreds.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Unavailable {
		t.Errorf("no credentials port: %v", err)
	}
	closed := open(t, f.factory(0), cfg, adapter.Dependencies{Credentials: newCreds(), HTTP: plainClient()})
	_ = closed.Close(t.Context())
	if _, err := closed.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Unavailable {
		t.Errorf("closed: %v", err)
	}
	if len(f.requests()) != 0 {
		t.Errorf("%d requests", len(f.requests()))
	}
}

func TestReconcileCredentialFailuresMakeNoRequest(t *testing.T) {
	for name, set := range map[string]func(*creds){
		"port error":       func(c *creds) { c.fail = model.NewError(model.Unauthorized, "consent revoked") },
		"empty secret":     func(c *creds) { c.set(relay.PurposeCallback, secretOf("")) },
		"oauth token":      func(c *creds) { c.set(relay.PurposeCallback, oauthOf("tok")) },
		"header injection": func(c *creds) { c.set(relay.PurposeCallback, secretOf("abc\r\nX-Evil: 1")) },
	} {
		t.Run(name, func(t *testing.T) {
			r := newSendRig(t, withLookup(nil))
			set(r.creds)
			if _, err := r.inst.Reconcile(t.Context(), reconcileReq()); err == nil {
				t.Fatal("looked up without a usable credential")
			}
			if len(r.fake.requests()) != 0 {
				t.Fatal("a request left without a credential")
			}
		})
	}
}

// Nothing the lookup answers with, and no credential, reaches an error or
// a failure.
func TestReconcileErrorsCarryNeitherAnswerNorSecret(t *testing.T) {
	echo := func(code int) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(code)
			_, _ = io.WriteString(w, `{"status":"boom `+req.Header.Get("Authorization")+` internal-host.corp","message_id":"`+callbackSecret+`"}`) // #nosec G705 -- fake provider echoing a leak probe
		}
	}
	for _, code := range []int{200, 302, 400, 401, 404, 429, 500} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			r := newSendRig(t, withLookup(nil))
			r.fake.script(echo(code))
			out, err := r.inst.Reconcile(t.Context(), reconcileReq())
			var texts []string
			if err != nil {
				texts = append(texts, err.Error())
			}
			if out.Failure != nil {
				texts = append(texts, out.Failure.Error())
			}
			if len(texts) == 0 {
				t.Fatalf("outcome %+v: an echoing answer proved something", out)
			}
			for _, s := range texts {
				for _, leak := range []string{callbackSecret, "boom", "internal-host", "127.0.0.1", "/lookup", string(testOp)} {
					if strings.Contains(s, leak) {
						t.Errorf("%q discloses %q", s, leak)
					}
				}
			}
		})
	}
}

func TestReconcileTransportFailures(t *testing.T) {
	t.Run("refused dial: transient", func(t *testing.T) {
		r := newSendRig(t, withLookup(nil))
		r.fake.srv.Close()
		if _, err := r.inst.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Transient {
			t.Fatalf("error %v", err)
		}
	})
	// A lookup changes nothing, so a lost answer is safe to ask again.
	t.Run("answer lost: transient", func(t *testing.T) {
		r := newSendRig(t, withLookup(nil))
		r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				_ = conn.Close()
			}
		})
		if _, err := r.inst.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Transient {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("no answer in time: transient", func(t *testing.T) {
		f := newFakeRelay(t)
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		f.script(func(http.ResponseWriter, *http.Request) { <-release })
		cfg := f.config()
		withLookup(nil)(&cfg)
		inst := open(t, f.factory(50*time.Millisecond), cfg, adapter.Dependencies{Credentials: newCreds(), HTTP: plainClient()})
		start := time.Now()
		if _, err := inst.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Transient {
			t.Fatalf("error %v", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Errorf("took %v: the timeout is not bounding the lookup", time.Since(start))
		}
	})
	t.Run("caller gives up", func(t *testing.T) {
		r := newSendRig(t, withLookup(nil))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := r.inst.Reconcile(ctx, reconcileReq()); !errors.Is(err, context.Canceled) {
			t.Fatalf("error %v", err)
		}
	})
}

// The credential goes to the lookup URL and nowhere else.
func TestReconcileNeverFollowsARedirect(t *testing.T) {
	elsewhere := newFakeRelay(t)
	r := newSendRig(t, withLookup(nil))
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", elsewhere.srv.URL+"/steal")
		w.WriteHeader(http.StatusFound)
	})
	if _, err := r.inst.Reconcile(t.Context(), reconcileReq()); model.CodeOf(err) != model.Permanent {
		t.Fatalf("error %v", err)
	}
	if n := len(elsewhere.requests()); n != 0 {
		t.Fatalf("the redirect target received %d requests", n)
	}
}

// A host client that follows redirects itself cannot be stopped from
// asking, but its answer is never taken for the lookup's.
func TestReconcileDoesNotAcceptAnAnswerFromAnotherURL(t *testing.T) {
	elsewhere := newFakeRelay(t)
	elsewhere.script(jsonAnswer(`{"status":"not_found"}`))
	f := newFakeRelay(t)
	f.script(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.srv.URL+"/other", http.StatusTemporaryRedirect)
	})
	cfg := f.config()
	withLookup(nil)(&cfg)
	inst := open(t, f.factory(0), cfg, adapter.Dependencies{Credentials: newCreds(), HTTP: &http.Client{}})
	out, err := inst.Reconcile(t.Context(), reconcileReq())
	if model.CodeOf(err) != model.Permanent || out.State != "" {
		t.Fatalf("outcome %+v err %v: an answer from another URL must prove nothing", out, err)
	}
}

func TestReconcileAnswerReadIsBounded(t *testing.T) {
	r := newSendRig(t, withLookup(nil))
	r.fake.script(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"sent","pad":"`)
		chunk := []byte(strings.Repeat("x", 64<<10))
		for range 160 { // 10 MiB offered
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	})
	out, err := r.inst.Reconcile(t.Context(), reconcileReq())
	if err != nil || out.State != model.Unknown {
		t.Fatalf("outcome %+v err %v", out, err)
	}
}
