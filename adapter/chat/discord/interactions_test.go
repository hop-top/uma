package discord

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"hop.top/uma/model"
)

// signed is one request to the Interactions Endpoint URL, signed as
// Discord signs: Ed25519 over the timestamp followed by the body
// (https://docs.discord.com/developers/interactions/overview).
type signed struct {
	body    []byte
	at      time.Time
	key     ed25519.PrivateKey
	headers map[string]string
	method  string
}

func interactionRequest(v any) signed {
	b, _ := json.Marshal(v)
	return signed{body: b, at: testNow, key: testKey, method: http.MethodPost}
}

func signBody(key ed25519.PrivateKey, ts string, body []byte) string {
	return hex.EncodeToString(ed25519.Sign(key, append([]byte(ts), body...)))
}

func (s signed) send(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(s.method, "/discord", bytes.NewReader(s.body))
	req.Header.Set("Content-Type", "application/json")
	ts := strconv.FormatInt(s.at.Unix(), 10)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, signBody(s.key, ts, s.body))
	for k, v := range s.headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// interactionPayload is an interaction of typ in a guild channel by
// testPerson.
func interactionPayload(id string, typ int, data map[string]any, extra map[string]any) map[string]any {
	p := map[string]any{
		"id": id, "application_id": testApp, "type": typ, "token": "aW50ZXJhY3Rpb24-tok_" + id, "version": 1,
		"guild_id": testGuild, "channel_id": testRoom, "channel": map[string]any{"id": testRoom, "type": 0},
		"member": map[string]any{"user": map[string]any{"id": testPerson, "username": "person"}}, "data": data,
	}
	for k, v := range extra {
		if v == nil {
			delete(p, k)
		} else {
			p[k] = v
		}
	}
	return p
}

func TestInteractionsEndpointSignature(t *testing.T) {
	r := newRig(t)
	h := r.x.Handler(&sinkRecorder{})
	ping := map[string]any{"id": "1500000000000000001", "application_id": testApp, "type": 1, "token": "t", "version": 1}
	ok := interactionRequest(ping).send(h)
	if ok.Code != 200 || ok.Body.String() != `{"type":1}` || ok.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("PING = %d %q", ok.Code, ok.Body.String())
	}
	_, other, _ := ed25519.GenerateKey(nil)
	for name, s := range map[string]signed{
		"wrong key": func() signed { s := interactionRequest(ping); s.key = other; return s }(),
		"stale":     func() signed { s := interactionRequest(ping); s.at = testNow.Add(-6 * time.Minute); return s }(),
		"future":    func() signed { s := interactionRequest(ping); s.at = testNow.Add(6 * time.Minute); return s }(),
		"no signature": func() signed {
			s := interactionRequest(ping)
			s.headers = map[string]string{HeaderSignature: ""}
			return s
		}(),
		"not hex": func() signed {
			s := interactionRequest(ping)
			s.headers = map[string]string{HeaderSignature: "zz"}
			return s
		}(),
		"no timestamp": func() signed {
			s := interactionRequest(ping)
			s.headers = map[string]string{HeaderTimestamp: ""}
			return s
		}(),
		"other timestamp": func() signed {
			s := interactionRequest(ping)
			s.headers = map[string]string{HeaderTimestamp: "1760000000"}
			return s
		}(),
	} {
		if rec := s.send(h); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rec.Code)
		}
	}
	// Inside the window either way is fresh.
	s := interactionRequest(ping)
	s.at = testNow.Add(-4 * time.Minute)
	if rec := s.send(h); rec.Code != 200 {
		t.Fatalf("4 minutes old: %d", rec.Code)
	}
	// A body changed after signing does not verify.
	s = interactionRequest(ping)
	ts := strconv.FormatInt(s.at.Unix(), 10)
	s.headers = map[string]string{HeaderSignature: signBody(testKey, ts, s.body)}
	s.body = append(s.body, ' ')
	if rec := s.send(h); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: %d", rec.Code)
	}
	s = interactionRequest(ping)
	s.method = http.MethodGet
	if rec := s.send(h); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
	// The public key is resolved per request; none is 503.
	r.creds.mu.Lock()
	r.creds.err = model.NewError(model.Unavailable, "vault down")
	r.creds.mu.Unlock()
	if rec := interactionRequest(ping).send(h); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no key: %d", rec.Code)
	}
	if rec := interactionRequest(ping).send(newRig(t, func(c *Config) { c.Receive = ReceiveGateway }).x.Handler(&sinkRecorder{})); rec.Code != http.StatusNotFound {
		t.Fatalf("gateway-only channel: %d", rec.Code)
	}
}

func TestInteractionsEndpointRecordsAndDefers(t *testing.T) {
	r := newRig(t)
	r.f.thread("acct1", testRoom, testThread)
	sink := &sinkRecorder{}
	h := r.x.Handler(sink)
	component := interactionPayload("1500000000000000002", interactionComponent,
		map[string]any{"custom_id": "approve", "component_type": 2},
		map[string]any{"message": map[string]any{"id": "1200000000000000001", "channel_id": testRoom, "flags": 0}})
	command := interactionPayload("1500000000000000003", interactionCommand,
		map[string]any{"name": "ticket", "type": 1, "options": []any{map[string]any{"name": "close", "type": 1, "options": []any{map[string]any{"name": "reason", "type": 3, "value": "done"}}}}},
		map[string]any{"channel": map[string]any{"id": testThread, "type": 11, "parent_id": testRoom}, "channel_id": testThread})
	modal := interactionPayload("1500000000000000004", interactionModalSubmit,
		map[string]any{"custom_id": "Feedback Form", "components": []any{map[string]any{"type": 1, "components": []any{map[string]any{"type": 4, "custom_id": "text", "value": "great"}}}}},
		map[string]any{"member": nil, "guild_id": nil, "user": map[string]any{"id": testPerson}, "channel": map[string]any{"id": "1100000000000000099", "type": 1}, "channel_id": "1100000000000000099"})
	auto := interactionPayload("1500000000000000005", interactionAutocomplete, map[string]any{"name": "ticket"}, nil)
	for _, c := range []struct {
		p    map[string]any
		body string
	}{
		{component, `{"type":6}`}, {command, `{"type":5}`}, {modal, `{"type":5}`}, {auto, `{"data":{"choices":[]},"type":8}`},
	} {
		if rec := interactionRequest(c.p).send(h); rec.Code != 200 || rec.Body.String() != c.body {
			t.Fatalf("%v: %d %q, want %s", c.p["type"], rec.Code, rec.Body.String(), c.body)
		}
	}
	envs := sink.envelopes()
	if len(envs) != 3 {
		t.Fatalf("recorded %d, want the component, the command and the modal", len(envs))
	}
	act := envs[0].Interaction
	if envs[0].Kind != model.EnvelopeInteraction || act.Kind != model.InteractionAction || act.Action != "approve" || act.Message == nil || *act.Message != *msgRef(testRoom, "1200000000000000001") ||
		act.ResponseToken != "aW50ZXJhY3Rpb24-tok_1500000000000000002" || !act.ResponseExpiresAt.Equal(testNow.Add(ResponseWindow)) || !act.Deadline.Equal(testNow.Add(AckDeadline)) ||
		envs[0].Sender != r.x.userRef(testPerson) || envs[0].EventID != "interaction:1500000000000000002" {
		t.Fatalf("component = %+v %+v", envs[0], act)
	}
	cmd := envs[1].Interaction
	if cmd.Kind != model.InteractionCommand || cmd.Command != "ticket close" || cmd.Arguments != `{"reason":"done"}` || len(envs[1].References) != 2 || envs[1].References[1] != r.x.threadRef(testRoom, testThread) {
		t.Fatalf("command = %+v refs %+v", cmd, envs[1].References)
	}
	sub := envs[2].Interaction
	if sub.Kind != model.InteractionSubmission || sub.Fields.Name != "discord.chat.modal.feedback_form" || string(sub.Fields.Data) != `{"custom_id":"Feedback Form","values":{"text":"great"}}` {
		t.Fatalf("modal = %+v %s", sub, sub.Fields.Data)
	}
	// Redelivered, the same interaction ID: core sees a duplicate.
	again := interactionRequest(component).send(h)
	if again.Code != 200 || sink.envelopes()[3].EventID != envs[0].EventID {
		t.Fatal("redelivery has another interaction ID")
	}
	// Refusals: retryable 503, other 422; another application's 400.
	for code, want := range map[model.ErrorCode]int{model.Transient: 503, model.Validation: 422} {
		x := newRig(t)
		hh := x.x.Handler(&sinkRecorder{fail: func(model.Ingress) error { return model.NewError(code, "no") }})
		if rec := interactionRequest(component).send(hh); rec.Code != want {
			t.Fatalf("%s refusal: %d, want %d", code, rec.Code, want)
		}
	}
	foreign := interactionPayload("1500000000000000006", interactionCommand, map[string]any{"name": "x"}, map[string]any{"application_id": "1100000000000000777"})
	if rec := interactionRequest(foreign).send(h); rec.Code != http.StatusBadRequest {
		t.Fatalf("another application's interaction: %d", rec.Code)
	}
}

// Over the gateway, an interaction is recorded, then acknowledged through
// the callback endpoint with the same deferral.
func TestGatewayInteraction(t *testing.T) {
	r := gatewayRig(t)
	h := r.watch()
	sink := &sinkRecorder{}
	r.run(t, sink)
	c := r.acct.gw.nextConn(t)
	seq := c.dispatch("INTERACTION_CREATE", interactionPayload("1500000000000000002", interactionCommand, map[string]any{"name": "help"}, nil))
	h.wait(t, seq)
	if envs := sink.envelopes(); len(envs) != 1 || envs[0].Interaction.Command != "help" {
		t.Fatalf("recorded %+v", envs)
	}
	calls := r.callsOf(http.MethodPost, "interactions/1500000000000000002/")
	if len(calls) != 1 || calls[0].args(t)["type"] != float64(callbackDeferredMessage) || calls[0].Auth != "" {
		t.Fatalf("callbacks = %+v, want one deferral without the bot token", calls)
	}
	// Refused, no callback: Discord shows the user the failure.
	r2 := gatewayRig(t)
	h2 := r2.watch()
	r2.run(t, &sinkRecorder{fail: func(model.Ingress) error { return model.NewError(model.Validation, "no inbox") }})
	seq = r2.acct.gw.nextConn(t).dispatch("INTERACTION_CREATE", interactionPayload("1500000000000000003", interactionCommand, map[string]any{"name": "help"}, nil))
	h2.wait(t, seq)
	if calls := r2.callsOf(http.MethodPost, "interactions/"); len(calls) != 0 {
		t.Fatalf("callback after a refusal: %+v", calls)
	}
}

func TestModalSchema(t *testing.T) {
	for in, want := range map[string]string{"Feedback Form": "discord.chat.modal.feedback_form", "": "discord.chat.modal", "v2": "discord.chat.modal.id-v2", "__x__": "discord.chat.modal.x"} {
		if got := modalSchema(in); got != want {
			t.Errorf("modalSchema(%q) = %q, want %q", in, got, want)
		}
	}
}
