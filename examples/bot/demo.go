package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/companions/bot"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/examples/internal/botdemo"
	"hop.top/uma/examples/internal/botpage"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store/memory"
)

const helpText = "Commands: /help, /draft <topic>. Anything else is echoed back."

var (
	// service runs the runtime; helper is the bot. Both act in the demo tenant.
	service = model.Principal{Tenant: botdemo.Tenant, Actor: botdemo.ServiceActor}
	helper  = model.Principal{Tenant: botdemo.Tenant, Actor: "helper-bot"}

	runtimeCall = uma.RuntimeCall{Principal: service, Call: model.Call{Correlation: "demo-runtime"}}
)

// options are test seams; the zero value is the example as shipped.
type options struct {
	deny func(policy.Authorization) bool
}

// app is one engine with one widget channel and one bot host.
type app struct {
	engine uma.Engine
	widget *widget.Factory
	host   *host.Host

	// decisions and n serve the draft command: n stands in for a model's
	// nondeterminism.
	decisions *decisions
	n         atomic.Int64
}

// newApp composes the engine, subscribes the bot and builds the host.
// Nothing runs until run.
func newApp(ctx context.Context, o options) (*app, error) {
	// Anonymous visitors: the widget gives each a random visitor ID. A real
	// host verifies its own login here and returns AssuranceVerified.
	w, err := widget.New(widget.Options{Verifier: widget.VerifierFunc(
		func(*http.Request, model.ChannelRef) (widget.Visitor, error) {
			return widget.Visitor{Assurance: widget.AssuranceAnonymous}, nil
		})})
	if err != nil {
		return nil, err
	}
	engine, err := uma.New(uma.Config{ // the engine config, shown here rather than hidden
		Store:       memory.New(memory.Options{}),        // memory storage: nothing survives a restart
		Authorizer:  botdemo.LocalDemoAuthorizer(o.deny), // permissive demo authorization; a real host decides per principal
		Adapters:    []adapter.Factory{w},
		Credentials: botdemo.NewSessionKey(),
		Clock:       ports.SystemClock{},
		IDs:         ports.NewUUIDv7(ports.SystemClock{}),
	})
	if err != nil {
		w.Close()
		return nil, err
	}
	a := &app{decisions: newDecisions()}
	// failed releases what was opened, then reports err.
	failed := func(err error) (*app, error) {
		w.Close()
		return nil, errors.Join(err, engine.Close(context.WithoutCancel(ctx)))
	}

	channel, err := botdemo.Provision(ctx, engine) // stand-in for an existing deployment
	if err != nil {
		return failed(err)
	}
	client, err := engine.For(helper)
	if err != nil {
		return failed(err)
	}
	prep, err := host.Prepare(ctx, host.PrepareConfig{
		Client: client, Principal: helper, Consumer: "support-helper",
		Filter: model.Filter{Kinds: []string{model.EventMessageReceived, model.EventInteractionReceived}},
		// The memory store keeps nothing across restarts, so every start is a
		// first run. A durable host loads its saved marker and uses
		// host.Resume(marker) instead.
		Intent: host.Bootstrap(model.StartBeginning, ""),
	})
	if err != nil {
		return failed(err)
	}
	// A durable host persists prep.Marker here, before consuming.

	h, err := host.New(ctx, host.Config{
		Client: client, Principal: helper, Runtime: engine.Runtime(), RuntimeCall: runtimeCall,
		Worker:   uma.WorkerOptions{Owner: "demo-worker", Limit: 10, Poll: 100 * time.Millisecond},
		Prepared: prep,
		Bot: bot.Config{
			Wait: 10 * time.Second,
			// The page sends "/help" and the Help button as interactions,
			// which the command and action routes answer alike. Messages
			// fall to the fallback echo.
			Commands: map[string]bot.Handler{"help": help, "draft": a.draft},
			Actions:  map[string]bot.Handler{"help": help},
			Fallback: echo,
		},
		Bindings: []host.Binding{{Pattern: "POST /widget/", Channel: channel}},
		Routes: []host.Route{
			{Pattern: "GET /widget/events", Handler: w.Events(channel)},
			{Pattern: "GET /{$}", Handler: http.HandlerFunc(page)},
		},
	})
	if err != nil {
		return failed(err)
	}
	a.engine, a.widget, a.host = engine, w, h
	return a, nil
}

// run serves the app on ln, which the caller bound, and dispatches the bot
// and delivers its replies until ctx ends or any part stops. The app owns
// the engine and the widget, so run closes them; host.Run never does.
func (a *app) run(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: a.host.Handler(), ReadHeaderTimeout: 5 * time.Second}
	err := a.host.Run(ctx, host.Options{},
		host.Server(srv, ln),
		// Registered after the server, so it closes first: the event streams
		// end before the server drains.
		host.Closer("widget", func(context.Context) error { a.widget.Close(); return nil }))
	// The Closer closes the widget first on a normal stop; closing it here
	// too (it is idempotent) covers a Run that failed before starting.
	a.widget.Close()
	return errors.Join(err, a.engine.Close(context.WithoutCancel(ctx)))
}

// help answers a /help command or a press of the Help button with the help
// text, as a deferred response to the interaction, to everyone.
func help(ctx context.Context, t *bot.Turn) error {
	_, err := t.Respond(ctx, model.ResponseModeMessage, model.Audience{Visibility: model.Public}, bot.TextContent(helpText))
	return err
}

// draft answers /draft <topic> with generated text, as a response to the
// command. A model call differs from run to run, so the text is recorded under
// the turn's operation ID before it is sent. A rerun of the turn (the event is
// redelivered, say after a crash between sending and acknowledging) finds the
// record and sends the same text, which replays the response; generating
// again would send different content under the same operation ID, and uma
// refuses that as a payload conflict.
func (a *app) draft(ctx context.Context, t *bot.Turn) error {
	topic := t.Interaction.Arguments
	text := a.decisions.choose(string(t.OperationID("draft")), func() string { return a.generate(topic) })
	_, err := t.Respond(ctx, model.ResponseModeMessage, model.Audience{Visibility: model.Public}, bot.TextContent(text))
	return err
}

// generate stands in for a model: every call returns a different text.
func (a *app) generate(topic string) string {
	return fmt.Sprintf("draft #%d on %q", a.n.Add(1), topic)
}

// echo answers any other message with its text. The answer depends on the
// message alone, so a redelivered event replays the same reply.
func echo(ctx context.Context, t *bot.Turn) error {
	_, err := t.ReplyText(ctx, "echo: "+bot.Text(t.Message.Content))
	return err
}

func page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(botpage.Page)
}
