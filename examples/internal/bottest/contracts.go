package bottest

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// deniedAuthorization refuses the bot's replies and nothing else: the
// visitor hears nothing, the host keeps running, and an interaction
// response, a different action, is still delivered.
func deniedAuthorization(t *testing.T, start Target) {
	run := begin(t, start, Options{Deny: func(a policy.Authorization) bool {
		return a.Action == model.ActionMessageReply && a.Principal.Actor == "helper-bot"
	}})
	s, events := session(t, run)
	var accepted widget.MessageResponse
	if code := Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: "m-1", Text: "hello"}, &accepted); code != http.StatusAccepted {
		t.Fatalf("message = %d", code)
	}
	// No reply: the bot's reply was refused, and nothing substituted a permissive path.
	select {
	case ev := <-events:
		t.Fatalf("got %+v, want no reply after a denied authorization", ev)
	case <-time.After(700 * time.Millisecond):
	}
	if run.Stopped() {
		t.Fatal("host stopped on a denied reply")
	}
	// The denial is per action: the interaction response is still allowed.
	var pressed widget.AcceptedInteraction
	if code := Post(t, run.URL, "actions", s.Token, widget.ActionRequest{ID: "help-1", Action: "help"}, &pressed); code != http.StatusAccepted {
		t.Fatalf("action = %d", code)
	}
	if ev := Next(t, events, "help response"); ev.Type != widget.EventInteractionResponse {
		t.Fatalf("got %+v, want the help response", ev)
	}
}

// duplicateMessageID pins the widget's side of replay: it dedupes a visitor
// message by identity and never compares payloads, so the same ID with
// other text is an accepted duplicate that creates no second message.
// Payload conflict is uma's, see AssertReplyPayloadConflict.
func duplicateMessageID(t *testing.T, start Target) {
	run := begin(t, start, Options{})
	s, events := session(t, run)
	var first, second widget.MessageResponse
	if code := Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: "m-1", Text: "a"}, &first); code != http.StatusAccepted {
		t.Fatalf("first = %d", code)
	}
	Next(t, events, "reply to the first message")
	if code := Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: "m-1", Text: "b"}, &second); code != http.StatusAccepted || !second.Duplicate {
		t.Fatalf("same ID, other text = %d %+v, want a 202 duplicate (the widget dedupes by identity, not payload)", code, second)
	}
	select {
	case ev := <-events:
		t.Fatalf("got %+v, want no second reply for a duplicate", ev)
	case <-time.After(500 * time.Millisecond):
	}
}

// componentFailure ends one part of a running host from outside, once it is
// in steady state and has served traffic: the host must stop and report the
// failure, not limp on without it, and stop serving.
func componentFailure(t *testing.T, start Target) {
	for name, stop := range map[string]func(*Running){
		"bot":    func(r *Running) { r.StopBot() },
		"worker": func(r *Running) { r.StopWorker() },
		"server": func(r *Running) { r.StopServer() },
	} {
		t.Run(name, func(t *testing.T) {
			run := begin(t, start, Options{})
			echo(t, run) // steady state: a session, a stream, one delivered reply
			stop(run)
			if err := run.Wait(t, 15*time.Second); err == nil { // fails the test if the host keeps running
				t.Errorf("the %s stopped under a running host: want a failure, not a clean stop", name)
			}
			if c, err := net.Dial("tcp", strings.TrimPrefix(run.URL, "http://")); err == nil {
				_ = c.Close()
				t.Errorf("server still accepting after the %s stopped", name)
			}
		})
	}
}

// echo opens a session and its stream, posts one message and waits for the
// reply: the host is serving, dispatching and delivering.
func echo(t *testing.T, run *Running) {
	t.Helper()
	s, events := session(t, run)
	var accepted widget.MessageResponse
	if code := Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: "m-1", Text: "ready"}, &accepted); code != http.StatusAccepted {
		t.Fatalf("message = %d", code)
	}
	if ev := Next(t, events, "reply to ready"); ev.Type != widget.EventMessage || len(ev.Parts) != 1 || ev.Parts[0].Text != "echo: ready" {
		t.Fatalf("got %+v, want the echo", ev)
	}
}

// replyAttempts bounds the submissions of one reply that loses to a
// concurrent unit in its conversation: the bot's own echo commits there too.
const replyAttempts = 5

// transientUnit reports a unit of work that lost to a concurrent unit:
// model.Transient with same-operation retry advice, the failure uma says may
// simply be resubmitted under the same operation ID.
func transientUnit(err error) bool {
	var e *model.Error
	if !errors.As(err, &e) || e.Code != model.Transient {
		return false
	}
	retry := e.Retry
	if retry == "" {
		retry = e.Code.DefaultRetry()
	}
	return retry == model.RetrySameOperation
}

// AssertReplyPayloadConflict sends the same reply operation twice through
// the bot's authorized client, with different content: the first is
// accepted, the second is model.Conflict. Operation IDs stay stable and a
// changed payload is never mistaken for an identical replay.
//
// The conversation is shared with the bot, whose own reply may commit while
// one of these is in flight; that one loses as model.Transient. Each reply
// is then resubmitted unchanged, same request and same operation ID, up to
// replyAttempts times in all, and only an exhausted budget or any other
// error fails the test. Acceptance, replay and conflict are asserted as
// strictly as without the resubmission.
func AssertReplyPayloadConflict(t *testing.T, c uma.Client, conv model.ConversationRef) {
	t.Helper()
	reply := func(text string) (acc model.Acceptance, err error) {
		for attempt := 1; attempt <= replyAttempts; attempt++ {
			acc, err = c.Messages().Reply(t.Context(), model.Reply{
				Command:      model.Command{Call: model.Call{Correlation: "conflict"}, Operation: "conflict-1"},
				Conversation: conv, Content: bot.TextContent(text),
			})
			if !transientUnit(err) {
				return acc, err
			}
			time.Sleep(time.Duration(attempt) * time.Millisecond)
		}
		return acc, err
	}
	if _, err := reply("one"); err != nil {
		t.Fatalf("first reply: %v", err)
	}
	if acc, err := reply("one"); err != nil || !acc.Replayed {
		t.Fatalf("identical replay = %+v, %v, want a replay", acc, err)
	}
	if _, err := reply("two"); model.CodeOf(err) != model.Conflict {
		t.Fatalf("same operation, other content: err = %v, want conflict", err)
	}
}
