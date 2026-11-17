package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/examples/internal/bottest"
	"hop.top/uma/model"
)

// target starts the example as main does: the demo served on a loopback
// listener under serve's supervision.
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
	d, err := newDemo(ctx, demoOptions{deny: o.Deny})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, d, ln) }()
	r := bottest.NewRunning("http://"+ln.Addr().String(), done)
	r.Stop = stop
	r.StopBot = func() { _ = d.engine.Close(context.Background()) }
	r.StopWorker = func() { _ = d.engine.Runtime().Drain(context.Background(), runtimeCall()) }
	r.StopServer = func() { _ = ln.Close() }
	r.HelpText = helpText
	return r, d.engine
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

// TestRunStopsWhenWorkerStops ends the delivery worker from outside: Run
// must stop the bot too and return, not keep dispatching with no replies
// delivered.
func TestRunStopsWhenWorkerStops(t *testing.T) {
	d, err := newDemo(t.Context(), demoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	ran := make(chan error, 1)
	go func() { ran <- d.Run(t.Context()) }()
	if err := d.engine.Runtime().Drain(t.Context(), runtimeCall()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
	case <-time.After(10 * time.Second):
		t.Fatal("Run still going 10s after its worker stopped")
	}
}

// TestServeStopsWhenBotStops closes the engine under a serving host: the
// bot cannot continue, so serve must report it and stop the HTTP server,
// not keep serving a chat page nobody answers.
func TestServeStopsWhenBotStops(t *testing.T) {
	d, err := newDemo(t.Context(), demoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(t.Context(), d, ln) }()
	if err := d.engine.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("serve = nil, want the bot's failure")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve still going 15s after the bot stopped")
	}
	if c, err := net.Dial("tcp", ln.Addr().String()); err == nil {
		_ = c.Close()
		t.Error("HTTP server still accepting after serve returned")
	}
}
