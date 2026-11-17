package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// socketRig is an instance of a Socket Mode channel wired to the fake
// workspace, its receiver started by run.
type socketRig struct {
	f     *fakeSlack
	a     *fakeAccount
	x     *Instance
	creds *credentials
	sink  *sinkRecorder

	mu    sync.Mutex
	waits []time.Duration
}

func newSocketRig(t *testing.T, opts Options, mod ...func(*Config)) *socketRig {
	t.Helper()
	f := newFakeSlack(func() time.Time { return testNow })
	t.Cleanup(f.close)
	a := f.account("acct1", testTeam, testBotUser, testToken)
	a.appToken = testAppToken
	cfg := testConfig()
	cfg.APIURL, cfg.Receive = f.apiURL("acct1"), ReceiveSocketMode
	for _, m := range mod {
		m(&cfg)
	}
	creds := newCredentials()
	inst, err := NewFactory(opts).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "slack",
	}, adapter.Dependencies{Credentials: creds, HTTP: f.client(), Clock: &fixedClock{testNow}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r := &socketRig{f: f, a: a, x: inst.(*Instance), creds: creds, sink: &sinkRecorder{}}
	r.x.sock = socketTiming{hello: 2 * time.Second, ping: time.Hour, pong: time.Second, write: AckDeadline}
	r.x.wait = func(ctx context.Context, d time.Duration) error {
		r.mu.Lock()
		r.waits = append(r.waits, d)
		r.mu.Unlock()
		return ctx.Err()
	}
	return r
}

// run starts the receiver; stop cancels it and returns what Run returned,
// and ended reports Run's result once it returned on its own.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func (r *socketRig) run(t *testing.T) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	run := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { run.done <- r.x.Run(ctx, r.sink) }()
	t.Cleanup(func() { _ = run.stop(t) })
	return run
}

func (run *running) stop(t *testing.T) error {
	t.Helper()
	run.cancel()
	select {
	case err := <-run.done:
		run.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	return nil
}

// ended waits for Run to return on its own.
func (run *running) ended(t *testing.T) error {
	t.Helper()
	select {
	case err := <-run.done:
		run.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	return nil
}

// running reports whether Run is still going after d.
func (run *running) still(d time.Duration) bool {
	select {
	case err := <-run.done:
		run.done <- err
		return false
	case <-time.After(d):
		return true
	}
}

func eventPayload(eventID, user, text, ts string) map[string]any {
	return callback(eventID, messageEvent(user, text, ts, nil))
}

func commandPayload() map[string]any {
	return map[string]any{
		"token": "deprecated", "team_id": testTeam, "team_domain": "acme", "channel_id": testRoom, "channel_name": "ops",
		"user_id": testPerson, "user_name": "ann", "command": "/deploy", "text": "api to prod --force",
		"api_app_id": testApp, "response_url": hookURL, "trigger_id": "13345224609.738474920.8088930838d88f008e0",
		"is_enterprise_install": "false",
	}
}

func TestSocketModeDeliversEventsAndAcknowledgesAfterTheRecord(t *testing.T) {
	r := newSocketRig(t, Options{})
	// The sink holds the first Accept until released: the acknowledgement
	// must wait for the record.
	release := make(chan struct{})
	var once, released sync.Once
	free := func() { released.Do(func() { close(release) }) }
	r.sink.fail = func(model.Ingress) error {
		once.Do(func() { <-release })
		return nil
	}
	r.run(t)
	t.Cleanup(free) // registered after run's, so it runs first
	c := r.a.nextConn(t)
	id := c.envelope(t, "events_api", eventPayload("Ev01", testPerson, "hello there", testTS), false)
	c.noAck(t, 200*time.Millisecond)
	free()
	ack := c.ack(t)
	if ack.EnvelopeID != id || ack.Payload != nil {
		t.Fatalf("ack = %+v, want envelope %s without payload", ack, id)
	}
	e := wantOne(t, r.sink)
	if e.Kind != model.EnvelopeMessage || textOf(e) != "hello there" || e.Sender != r.x.userRef(testPerson) {
		t.Errorf("envelope %+v", e)
	}
	// Without a sink holding it, an envelope is acknowledged well inside
	// the deadline.
	c.envelope(t, "events_api", eventPayload("Ev02", testPerson, "second", "1700000000.000200"), false)
	if ack := c.ack(t); ack.lag >= AckDeadline {
		t.Errorf("acknowledged after %v, want within %v", ack.lag, AckDeadline)
	}
	opens := slices.DeleteFunc(r.f.calls("acct1"), func(c fakeCall) bool { return c.Method != "apps.connections.open" })
	if len(opens) != 1 || opens[0].Auth != "Bearer "+testAppToken {
		t.Errorf("apps.connections.open calls %+v, want one with the app-level token", opens)
	}
	for _, call := range r.f.calls("acct1") {
		if strings.Contains(call.Auth, testToken) {
			t.Errorf("%s carried the bot token", call.Method)
		}
	}
	if slices.ContainsFunc(r.creds.requests, func(q ports.CredentialRequest) bool { return q.Purpose == CredentialPurposeSigning }) {
		t.Error("the signing secret was resolved: Socket Mode envelopes are not signed")
	}
}

func TestSocketModeRoutesInteractionsAndCommandsThroughTheirParsers(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.run(t)
	c := r.a.nextConn(t)
	c.envelope(t, "interactive", buttonPayload(), true)
	if ack := c.ack(t); ack.Payload != nil {
		t.Errorf("button ack carries %s, want none", ack.Payload)
	}
	c.envelope(t, "slash_commands", commandPayload(), true)
	if ack := c.ack(t); ack.Payload != nil {
		t.Errorf("command ack carries %s, want none: it would be shown to the user", ack.Payload)
	}
	got := r.sink.envelopes()
	if len(got) != 2 {
		t.Fatalf("envelopes %+v, want the button and the command", got)
	}
	b, cmd := got[0].Interaction, got[1].Interaction
	if b.Kind != model.InteractionAction || b.Action != "approve" || b.Arguments != "deploy-42" || b.ResponseToken != hookTok {
		t.Errorf("button = %+v", b)
	}
	if cmd.Kind != model.InteractionCommand || cmd.Command != "deploy" || cmd.ID != "command:13345224609.738474920.8088930838d88f008e0" || cmd.ResponseToken != hookTok {
		t.Errorf("command = %+v", cmd)
	}
	// The deadline runs from the envelope's arrival, the host's clock.
	if !b.Deadline.Equal(testNow.Add(AckDeadline)) || !cmd.Deadline.Equal(testNow.Add(AckDeadline)) {
		t.Errorf("deadlines %v %v, want %v", b.Deadline, cmd.Deadline, testNow.Add(AckDeadline))
	}
}

func TestSocketModeAnswersInsideTheAcknowledgement(t *testing.T) {
	r := newSocketRig(t, Options{}, func(c *Config) { c.EvaluateInteractions = true })
	opts := optionsExt(t, map[string]any{"options": []any{opt("Prod", "prod")}})
	r.sink.eval = func(in model.InteractionInput) (model.InteractionDecision, error) {
		if in.Kind == model.InteractionOptions {
			return model.InteractionDecision{Options: opts}, nil
		}
		return model.InteractionDecision{FieldErrors: map[string]string{"b": "too short"}}, nil
	}
	r.run(t)
	c := r.a.nextConn(t)
	c.envelope(t, "interactive", suggestionPayload(), true)
	var got map[string]any
	if ack := c.ack(t); json.Unmarshal(ack.Payload, &got) != nil || len(got["options"].([]any)) != 1 {
		t.Errorf("options ack payload %s, want the validator's options", ack.Payload)
	}
	c.envelope(t, "interactive", submissionPayload(), true)
	got = nil
	if ack := c.ack(t); json.Unmarshal(ack.Payload, &got) != nil || got["response_action"] != "errors" {
		t.Errorf("submission ack payload %s, want response_action errors", ack.Payload)
	}
	wantNone(t, r.sink)
	// An envelope that does not take a payload is acknowledged bare.
	c.envelope(t, "interactive", suggestionPayload(), false)
	if ack := c.ack(t); ack.Payload != nil {
		t.Errorf("ack carries %s, though the envelope accepts no payload", ack.Payload)
	}
}

func TestSocketModeWithholdsTheAcknowledgementOfARefusal(t *testing.T) {
	t.Run("final refusal keeps the connection", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.sink.fail = func(in model.Ingress) error {
			if textOf(in.Envelope) == "refused" {
				return model.NewError(model.Validation, "no conversation for it")
			}
			return nil
		}
		run := r.run(t)
		c := r.a.nextConn(t)
		c.envelope(t, "events_api", eventPayload("Ev01", testPerson, "refused", testTS), false)
		c.noAck(t, 200*time.Millisecond)
		id := c.envelope(t, "events_api", eventPayload("Ev02", testPerson, "fine", "1700000000.000200"), false)
		if ack := c.ack(t); ack.EnvelopeID != id {
			t.Errorf("ack %+v, want the second envelope's", ack)
		}
		if !run.still(50 * time.Millisecond) {
			t.Fatalf("Run ended: %v", run.ended(t))
		}
	})
	for _, c := range []struct {
		code model.ErrorCode
		want model.ErrorCode
	}{{model.Unavailable, model.Unavailable}, {model.Unauthorized, model.Unauthorized}} {
		t.Run(string(c.code)+" ends the connection", func(t *testing.T) {
			r := newSocketRig(t, Options{})
			r.sink.fail = func(model.Ingress) error { return model.NewError(c.code, "sink says no") }
			run := r.run(t)
			conn := r.a.nextConn(t)
			conn.envelope(t, "events_api", eventPayload("Ev01", testPerson, "hi", testTS), false)
			if err := run.ended(t); !model.IsCode(err, c.want) {
				t.Fatalf("Run = %v, want %s", err, c.want)
			}
			conn.noAck(t, 50*time.Millisecond)
			if !conn.closed(time.Second) {
				t.Error("connection left open after Run returned")
			}
		})
	}
}

func TestSocketModeRefreshesWithoutReturning(t *testing.T) {
	for _, reason := range []string{"refresh_requested", "warning"} {
		t.Run(reason, func(t *testing.T) {
			r := newSocketRig(t, Options{})
			run := r.run(t)
			c1 := r.a.nextConn(t)
			c1.disconnect(t, reason)
			c2 := r.a.nextConn(t)
			if !c1.closed(2 * time.Second) {
				t.Error("replaced connection still open")
			}
			c2.envelope(t, "events_api", eventPayload("Ev01", testPerson, "after refresh", testTS), false)
			c2.ack(t)
			if !run.still(50 * time.Millisecond) {
				t.Fatalf("Run returned across a refresh: %v", run.ended(t))
			}
			if n := len(r.sink.envelopes()); n != 1 {
				t.Errorf("%d envelopes, want 1", n)
			}
			// A second refresh works the same.
			c2.disconnect(t, reason)
			c3 := r.a.nextConn(t)
			c3.envelope(t, "events_api", eventPayload("Ev02", testPerson, "again", "1700000000.000200"), false)
			c3.ack(t)
		})
	}
}

func TestSocketModeOldConnectionStillDeliversDuringARefresh(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.a.noHello = true // the replacement stays pending until the test says hello
	r.run(t)
	c1 := r.a.nextConn(t)
	if err := c1.send(map[string]any{"type": "hello", "num_connections": 1}); err != nil {
		t.Fatal(err)
	}
	c1.disconnect(t, "warning")
	c2 := r.a.nextConn(t)
	id := c1.envelope(t, "events_api", eventPayload("Ev01", testPerson, "late on the old one", testTS), false)
	if ack := c1.ack(t); ack.EnvelopeID != id {
		t.Errorf("ack %+v", ack)
	}
	if c1.closed(50 * time.Millisecond) {
		t.Fatal("old connection closed before the new one said hello")
	}
	if err := c2.send(map[string]any{"type": "hello", "num_connections": 2}); err != nil {
		t.Fatal(err)
	}
	if !c1.closed(2 * time.Second) {
		t.Error("old connection still open after the new one's hello")
	}
}

func TestSocketModeOldConnectionClosingDuringARefreshIsNoFailure(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.a.noHello = true
	run := r.run(t)
	c1 := r.a.nextConn(t)
	if err := c1.send(map[string]any{"type": "hello", "num_connections": 1}); err != nil {
		t.Fatal(err)
	}
	c1.disconnect(t, "refresh_requested")
	c2 := r.a.nextConn(t)
	// Slack closes the old connection before the new one said hello.
	_ = c1.ws.CloseNow()
	if !run.still(100 * time.Millisecond) {
		t.Fatalf("Run returned when the refreshed connection closed: %v", run.ended(t))
	}
	if err := c2.send(map[string]any{"type": "hello", "num_connections": 1}); err != nil {
		t.Fatal(err)
	}
	c2.envelope(t, "events_api", eventPayload("Ev01", testPerson, "on the new one", testTS), false)
	c2.ack(t)
}

func TestSocketModeDisconnects(t *testing.T) {
	t.Run("link_disabled is disabled", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		run := r.run(t)
		r.a.nextConn(t).disconnect(t, "link_disabled")
		if err := run.ended(t); !model.IsCode(err, model.Disabled) || retryable(err) {
			t.Fatalf("Run = %v, want final disabled", err)
		}
		r.a.noConn(t, 50*time.Millisecond)
	})
	t.Run("another reason reconnects through the engine", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		run := r.run(t)
		r.a.nextConn(t).disconnect(t, "too_many_websockets")
		if err := run.ended(t); !model.IsCode(err, model.Unavailable) || !retryable(err) || !strings.Contains(err.Error(), "too_many_websockets") {
			t.Fatalf("Run = %v, want retryable unavailable naming the reason", err)
		}
	})
	t.Run("a closed connection reconnects through the engine", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		run := r.run(t)
		_ = r.a.nextConn(t).ws.CloseNow()
		if err := run.ended(t); !model.IsCode(err, model.Unavailable) || !retryable(err) {
			t.Fatalf("Run = %v, want retryable unavailable", err)
		}
	})
	t.Run("no hello", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.a.noHello = true
		r.x.sock.hello = 100 * time.Millisecond
		run := r.run(t)
		r.a.nextConn(t)
		if err := run.ended(t); !model.IsCode(err, model.Unavailable) || !retryable(err) {
			t.Fatalf("Run = %v, want retryable unavailable", err)
		}
	})
	t.Run("no pong", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.a.deafUntil = make(chan struct{})
		t.Cleanup(func() { close(r.a.deafUntil) })
		r.x.sock.ping, r.x.sock.pong = 20*time.Millisecond, 50*time.Millisecond
		run := r.run(t)
		r.a.nextConn(t)
		if err := run.ended(t); !model.IsCode(err, model.Unavailable) || !retryable(err) {
			t.Fatalf("Run = %v, want retryable unavailable", err)
		}
	})
}

func TestSocketModePingsKeepALiveConnection(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.x.sock.ping, r.x.sock.pong = 10*time.Millisecond, time.Second
	run := r.run(t)
	c := r.a.nextConn(t)
	if !run.still(200 * time.Millisecond) {
		t.Fatalf("Run ended while the peer answered pings: %v", run.ended(t))
	}
	c.envelope(t, "events_api", eventPayload("Ev01", testPerson, "hi", testTS), false)
	c.ack(t)
}

func TestSocketModeCredentialAndOpenFailures(t *testing.T) {
	t.Run("credential port refuses", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.creds.err = model.NewError(model.NotFound, "no such credential")
		run := r.run(t)
		if err := run.ended(t); !model.IsCode(err, model.NotFound) {
			t.Fatalf("Run = %v, want the port's refusal", err)
		}
		r.a.noConn(t, 50*time.Millisecond)
	})
	t.Run("token refused, resolved fresh once", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.a.appToken = "xapp-other"
		run := r.run(t)
		if err := run.ended(t); !model.IsCode(err, model.Unauthorized) || retryable(err) {
			t.Fatalf("Run = %v, want final unauthorized", err)
		}
		var fresh []bool
		for _, q := range r.creds.requests {
			if q.Purpose == CredentialPurposeSocket {
				fresh = append(fresh, q.Fresh)
			}
		}
		if !slices.Equal(fresh, []bool{false, true}) {
			t.Errorf("app-level token resolved %v, want once cached, once fresh", fresh)
		}
	})
	t.Run("rate limit is waited out", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.a.opens = []answer{rateLimitedFor(7 * time.Second)}
		r.x.wait = func(ctx context.Context, d time.Duration) error {
			r.mu.Lock()
			r.waits = append(r.waits, d)
			r.mu.Unlock()
			return nil
		}
		r.run(t)
		r.a.nextConn(t)
		r.mu.Lock()
		defer r.mu.Unlock()
		if !slices.Equal(r.waits, []time.Duration{7 * time.Second}) {
			t.Errorf("waits %v, want the Retry-After", r.waits)
		}
	})
	t.Run("server error reconnects through the engine", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.a.opens = []answer{failWith("internal_error")}
		if err := r.run(t).ended(t); !retryable(err) {
			t.Fatalf("Run = %v, want retryable", err)
		}
	})
	t.Run("an answer without a wss URL is refused before any dial", func(t *testing.T) {
		r := newSocketRig(t, Options{})
		r.a.socketURL = "https://" + fakeSocketHost + "/link/?ticket=acct1.1"
		if err := r.run(t).ended(t); !model.IsCode(err, model.Permanent) {
			t.Fatalf("Run = %v, want permanent", err)
		}
		r.a.noConn(t, 50*time.Millisecond)
	})
}

func TestSocketModeStopsWithItsContext(t *testing.T) {
	r := newSocketRig(t, Options{})
	run := r.run(t)
	c := r.a.nextConn(t)
	if err := run.stop(t); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want the cancellation", err)
	}
	if !c.closed(time.Second) {
		t.Error("connection left open after Run returned")
	}
}

func TestSocketModeDebugReconnects(t *testing.T) {
	r := newSocketRig(t, Options{DebugReconnects: true})
	r.run(t)
	if q := r.a.nextConn(t).query; !strings.HasSuffix(q, "&debug_reconnects=true") || !strings.HasPrefix(q, "ticket=acct1.") {
		t.Errorf("dialled with query %q, want the ticket and debug_reconnects=true", q)
	}
	r2 := newSocketRig(t, Options{})
	r2.run(t)
	if q := r2.a.nextConn(t).query; strings.Contains(q, "debug_reconnects") {
		t.Errorf("dialled with query %q without the option", q)
	}
}

func TestSocketURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"wss://wss-primary.slack.com/link/?ticket=1&app_id=A1": true,
		"ws://wss-primary.slack.com/link/":                     false,
		"https://wss-primary.slack.com/link/":                  false,
		"wss://user:pw@wss-primary.slack.com/link/":            false,
		"wss:///link/": false,
		"":             false,
	} {
		if _, got := socketURL(raw, false); got != ok {
			t.Errorf("socketURL(%q) ok = %v, want %v", raw, got, ok)
		}
	}
	if u, _ := socketURL("wss://h/link/", true); u != "wss://h/link/?debug_reconnects=true" {
		t.Errorf("debug URL %q", u)
	}
}

// retryable is the engine's rule for reconnecting a receiver: a typed
// failure whose code and advice both allow the same operation again.
func retryable(err error) bool {
	me, ok := errors.AsType[*model.Error](err)
	return ok && me.Code.DefaultRetry() == model.RetrySameOperation && (me.Retry == "" || me.Retry == model.RetrySameOperation)
}
