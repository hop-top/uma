// Package bottest is the contract every bot example satisfies: one suite,
// driven through the browser-facing HTTP surface, run against each example
// by its own tests. An example plugs in by providing a Target.
package bottest

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// Options are the seams a Target must honour for the suite to steer it.
type Options struct {
	// Deny makes the example's authorizer refuse the matching authorization.
	Deny func(policy.Authorization) bool
}

// Running is one started example host.
type Running struct {
	URL        string // base URL of the example's own server
	Stop       func() // cancel the host's context: a requested shutdown
	StopBot    func() // end the bot from outside (engine closed under it)
	StopWorker func() // end the delivery worker from outside (Runtime.Drain)
	StopServer func() // close the server's listener from outside
	HelpText   string // what the example answers to /help and the Help button

	stopped chan struct{} // closed once the host has returned
	err     error         // the host's result; valid after stopped closes
}

// NewRunning wraps the host's result channel (buffered, one value). The
// first value received is the host's result for every later Wait.
func NewRunning(url string, result <-chan error) *Running {
	r := &Running{URL: url, stopped: make(chan struct{})}
	go func() {
		r.err = <-result
		close(r.stopped)
	}()
	return r
}

// Stopped reports, without blocking, whether the host has returned.
func (r *Running) Stopped() bool {
	select {
	case <-r.stopped:
		return true
	default:
		return false
	}
}

// Wait returns the host's result, failing the test if it has not stopped
// within d. It may be called repeatedly; later calls return the same error.
func (r *Running) Wait(t *testing.T, d time.Duration) error {
	t.Helper()
	select {
	case <-r.stopped:
		return r.err
	case <-time.After(d):
		t.Fatalf("host still running %s after it should have stopped", d)
	}
	return nil
}

// Target starts one example host with the given options and returns it. It
// fails the test if the host cannot start.
type Target func(t *testing.T, o Options) *Running

// Suite runs the contract against the example start produces. Each
// subtest starts its own host and, when it ends, stops it and waits for it
// without judging its result: failure subtests inject failures on purpose.
func Suite(t *testing.T, start Target) {
	t.Run("Reply", func(t *testing.T) { reply(t, begin(t, start, Options{})) })
	t.Run("HelpButton", func(t *testing.T) { helpButton(t, begin(t, start, Options{})) })
	t.Run("HelpCommand", func(t *testing.T) { helpCommand(t, begin(t, start, Options{})) })
	t.Run("DeniedAuthorization", func(t *testing.T) { deniedAuthorization(t, start) })
	t.Run("DuplicateMessageIDIsNotASecondMessage", func(t *testing.T) { duplicateMessageID(t, start) })
	t.Run("ComponentFailure", func(t *testing.T) { componentFailure(t, start) })
	t.Run("CleanShutdown", func(t *testing.T) {
		run := begin(t, start, Options{})
		run.Stop()
		if err := run.Wait(t, 15*time.Second); err != nil {
			t.Fatalf("host stopped with %v after a requested shutdown, want nil", err)
		}
	})
	t.Run("CleanShutdownAfterTraffic", func(t *testing.T) {
		run := begin(t, start, Options{})
		echo(t, run)
		run.Stop()
		if err := run.Wait(t, 15*time.Second); err != nil {
			t.Fatalf("host stopped with %v after traffic and a requested shutdown, want nil", err)
		}
	})
}

// begin starts a host and registers its teardown.
func begin(t *testing.T, start Target, o Options) *Running {
	t.Helper()
	run := start(t, o)
	t.Cleanup(func() {
		run.Stop()
		_ = run.Wait(t, 15*time.Second)
	})
	return run
}

// session opens a visitor session and its event stream.
func session(t *testing.T, run *Running) (widget.SessionResponse, <-chan widget.Event) {
	t.Helper()
	var s widget.SessionResponse
	if code := Post(t, run.URL, "session", "", struct{}{}, &s); code != http.StatusOK {
		t.Fatalf("session = %d", code)
	}
	if s.Assurance != widget.AssuranceAnonymous || s.Visitor == "" {
		t.Fatalf("session = %+v, want an anonymous visitor", s)
	}
	return s, Stream(t, run.URL, s.Token)
}

// reply drives the example as a browser does: the chat page, a visitor
// session, the event stream, then messages answered by the echo handler.
func reply(t *testing.T, run *Running) {
	s, events := session(t, run)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, run.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("EventSource")) {
		t.Fatalf("GET / = %d, want the chat page", res.StatusCode)
	}

	for _, tc := range []struct{ id, say, want string }{
		{"m-1", "hello", "echo: hello"},
		{"m-2", "again", "echo: again"},
	} {
		var accepted widget.MessageResponse
		if code := Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: tc.id, Text: tc.say}, &accepted); code != http.StatusAccepted {
			t.Fatalf("message %q = %d", tc.say, code)
		}
		ev := Next(t, events, "reply to "+tc.say)
		if ev.Type != widget.EventMessage || len(ev.Parts) != 1 || ev.Parts[0].Text != tc.want {
			t.Fatalf("after %q got %+v, want message %q", tc.say, ev, tc.want)
		}
	}
}

// helpButton presses the page's Help button: the action reaches the bot as
// an interaction, the help handler answers it, and the answer is an
// interaction response to that press, not a second chat message.
func helpButton(t *testing.T, run *Running) {
	requireHelpText(t, run)
	s, events := session(t, run)
	var pressed widget.AcceptedInteraction
	if code := Post(t, run.URL, "actions", s.Token, widget.ActionRequest{ID: "help-1", Action: "help"}, &pressed); code != http.StatusAccepted {
		t.Fatalf("action = %d, want 202", code)
	}
	ev := Next(t, events, "help response")
	if ev.Type != widget.EventInteractionResponse || ev.Mode != model.ResponseModeMessage || ev.Interaction != pressed.Interaction ||
		len(ev.Parts) != 1 || ev.Parts[0].Text != run.HelpText {
		t.Fatalf("got %+v, want the help text as a response to interaction %q", ev, pressed.Interaction)
	}

	// The press is answered once: a retry is a duplicate and adds nothing,
	// and the next thing the visitor hears is the next answer.
	var again widget.AcceptedInteraction
	if code := Post(t, run.URL, "actions", s.Token, widget.ActionRequest{ID: "help-1", Action: "help"}, &again); code != http.StatusAccepted || !again.Duplicate {
		t.Fatalf("retried action = %d %+v, want a duplicate", code, again)
	}
	var accepted widget.MessageResponse
	if code := Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: "m-1", Text: "hi"}, &accepted); code != http.StatusAccepted {
		t.Fatalf("message = %d", code)
	}
	if ev := Next(t, events, "echo"); ev.Type != widget.EventMessage || ev.Parts[0].Text != "echo: hi" {
		t.Fatalf("got %+v, want only the echo after the retried press", ev)
	}
}

// helpCommand posts what the page's slash parser sends for "/help": the
// command route answers like the button.
func helpCommand(t *testing.T, run *Running) {
	requireHelpText(t, run)
	s, events := session(t, run)
	var sent widget.AcceptedInteraction
	if code := Post(t, run.URL, "actions", s.Token, widget.ActionRequest{ID: "cmd-1", Kind: model.InteractionCommand, Command: "help"}, &sent); code != http.StatusAccepted {
		t.Fatalf("command = %d, want 202", code)
	}
	ev := Next(t, events, "help response")
	if ev.Type != widget.EventInteractionResponse || ev.Interaction != sent.Interaction || len(ev.Parts) != 1 || ev.Parts[0].Text != run.HelpText {
		t.Fatalf("got %+v, want the help text as a response to the command", ev)
	}
}

// requireHelpText fails if the target did not say what its help answer is.
func requireHelpText(t *testing.T, run *Running) {
	t.Helper()
	if run.HelpText == "" {
		t.Fatal("Running.HelpText is empty: the target must set the text its help answers with")
	}
}
