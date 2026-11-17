package main

import (
	"context"
	"net"
	"net/http"
	"regexp"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/examples/internal/bottest"
	"hop.top/uma/model"
)

// target starts the example as main does: the app served on a loopback
// listener.
func target(t *testing.T, o bottest.Options) *bottest.Running {
	t.Helper()
	r, _ := startApp(t, o)
	return r
}

// startApp is target, also returning the engine for tests that act as the
// bot's own client.
func startApp(t *testing.T, o bottest.Options) (*bottest.Running, uma.Engine) {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	t.Cleanup(stop) // the suite stops the host first; this only guards a failed start
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a, err := newApp(ctx, options{deny: o.Deny})
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	done := make(chan error, 1) // always receives: Running waits on it
	go func() { done <- a.run(ctx, ln) }()
	r := bottest.NewRunning("http://"+ln.Addr().String(), done)
	r.Stop = stop
	r.StopBot = func() { _ = a.engine.Close(context.Background()) }
	r.StopWorker = func() { _ = a.engine.Runtime().Drain(context.Background(), runtimeCall) }
	r.StopServer = func() { _ = ln.Close() }
	r.HelpText = helpText
	return r, a.engine
}

// TestContract runs the contract every bot example satisfies.
func TestContract(t *testing.T) { bottest.Suite(t, target) }

// TestReplyPayloadConflict pins payload-conflict detection at the engine,
// through the bot's own authorized client: the widget only dedupes by
// identity, so the conflict is uma's Messages.Reply contract.
func TestReplyPayloadConflict(t *testing.T) {
	run, engine := startApp(t, bottest.Options{})
	t.Cleanup(func() { run.Stop(); _ = run.Wait(t, 15*time.Second) })
	var s widget.SessionResponse
	if code := bottest.Post(t, run.URL, "session", "", struct{}{}, &s); code != http.StatusOK {
		t.Fatalf("session = %d", code)
	}
	events := bottest.Stream(t, run.URL, s.Token)
	var accepted widget.MessageResponse
	if code := bottest.Post(t, run.URL, "messages", s.Token, widget.MessageRequest{ID: "m-1", Text: "hello"}, &accepted); code != http.StatusAccepted {
		t.Fatalf("message = %d", code)
	}
	// The bot's echo commits in this conversation; wait for it so the
	// helper's replies do not race it.
	if ev := bottest.Next(t, events, "echo of hello"); ev.Type != widget.EventMessage {
		t.Fatalf("got %+v, want the echo", ev)
	}
	client, err := engine.For(helper)
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.Conversations().List(t.Context(), model.ListRequest{Call: model.Call{Correlation: "find-conversation"}, Page: model.PageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("conversations = %+v, %v, want the visitor's one conversation", page, err)
	}
	bottest.AssertReplyPayloadConflict(t, client, page.Items[0].Ref)
}

// draftNumber is the counter in the stand-in generator's text.
var draftNumber = regexp.MustCompile(`^draft #(\d+) on "x"$`)

// TestDraftDuplicateAddsNoResponseAndNewCommandRegenerates: the generator
// differs on every call, a duplicate command adds no second response, and a
// new command generates again. The widget catches the duplicate before it
// reaches the bot, so this does not exercise the recorded text; reuse on a
// rerun of the same turn is pinned by TestChooseGeneratesOncePerKey.
func TestDraftDuplicateAddsNoResponseAndNewCommandRegenerates(t *testing.T) {
	run := target(t, bottest.Options{})
	var s widget.SessionResponse
	if code := bottest.Post(t, run.URL, "session", "", struct{}{}, &s); code != http.StatusOK {
		t.Fatalf("session = %d", code)
	}
	events := bottest.Stream(t, run.URL, s.Token)
	draft := func(id string) widget.AcceptedInteraction {
		t.Helper()
		var sent widget.AcceptedInteraction
		if code := bottest.Post(t, run.URL, "actions", s.Token, widget.ActionRequest{ID: id, Kind: model.InteractionCommand, Command: "draft", Arguments: "x"}, &sent); code != http.StatusAccepted {
			t.Fatalf("draft %s = %d, want 202", id, code)
		}
		return sent
	}
	answer := func(sent widget.AcceptedInteraction) string {
		t.Helper()
		ev := bottest.Next(t, events, "draft response")
		if ev.Type != widget.EventInteractionResponse || ev.Interaction != sent.Interaction || len(ev.Parts) != 1 {
			t.Fatalf("got %+v, want one text response to interaction %q", ev, sent.Interaction)
		}
		if !draftNumber.MatchString(ev.Parts[0].Text) {
			t.Fatalf("response text = %q, want a draft", ev.Parts[0].Text)
		}
		return ev.Parts[0].Text
	}

	first := draft("d-1")
	text := answer(first)

	// The same ID again is a duplicate: no second interaction, no second response.
	if again := draft("d-1"); !again.Duplicate || again.Interaction != first.Interaction {
		t.Fatalf("retried draft = %+v, want a duplicate of %+v", again, first)
	}
	select {
	case ev := <-events:
		t.Fatalf("duplicate draft produced %+v, want nothing", ev)
	case <-time.After(500 * time.Millisecond):
	}

	// A new command generates again.
	if next := answer(draft("d-2")); next == text {
		t.Fatalf("second command answered %q again, want a new draft", next)
	}
}
