package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/companions/bot"
	"hop.top/uma/examples/internal/botpage"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store/memory"
)

const tenant model.TenantID = "demo"

const (
	site     = "support"
	helpText = "Commands: /help. Anything else is echoed back."
)

var (
	// service runs the runtime and configures the channel; helper is the
	// bot. Both act in the demo tenant.
	service = model.Principal{Tenant: tenant, Actor: "demo-service"}
	helper  = model.Principal{Tenant: tenant, Actor: "helper-bot"}
)

// demoOptions are test seams; the zero value is the example as shipped.
type demoOptions struct {
	deny func(policy.Authorization) bool
}

// demo is one engine with one widget channel and one bot.
type demo struct {
	engine  uma.Engine
	widget  *widget.Factory
	bot     *bot.Bot
	handler http.Handler
}

// newDemo composes the engine, configures the widget channel and its inbox,
// mounts the widget and opens the bot's subscription. Nothing runs until
// Run.
//
// Ownership. The application owns who acts (the principals), what they may
// do (the authorizer), the credentials, the store, the channel and inbox
// configuration and the widget's visitor verifier: everything below that
// this function wires is the host's decision. uma owns the operations, the
// ledger of what happened and the delivery of queued replies. The bot
// companion owns dispatch and acknowledges an event only after its handlers
// ran. serve (main.go) supervises the parts while they run.
func newDemo(ctx context.Context, o demoOptions) (*demo, error) {
	// The application's widget verifier. Anonymous visitors: the widget
	// gives each a random visitor ID. A real host verifies its own login
	// here and returns AssuranceVerified.
	w, err := widget.New(widget.Options{Verifier: widget.VerifierFunc(
		func(*http.Request, model.ChannelRef) (widget.Visitor, error) {
			return widget.Visitor{Assurance: widget.AssuranceAnonymous}, nil
		})})
	if err != nil {
		return nil, err
	}
	engine, err := uma.New(uma.Config{
		// Durable=false: conversations, subscriptions and queued replies
		// are lost on restart. Use a durable store outside a demo.
		Store: memory.New(memory.Options{}),
		// The application's authorizer. Allows everything. A real host
		// decides per principal and resource.
		Authorizer: policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
			if o.deny != nil && o.deny(a) {
				return policy.Decision{Allow: false, ReasonCode: "demo-deny"}, nil
			}
			return policy.Decision{Allow: true, ReasonCode: "demo"}, nil
		}),
		Adapters: []adapter.Factory{w},
		// The application's credential port.
		Credentials: newSessionKey(),
		Clock:       ports.SystemClock{},
		IDs:         ports.NewUUIDv7(ports.SystemClock{}),
	})
	if err != nil {
		return nil, err
	}
	d := &demo{engine: engine, widget: w}
	if err := d.setup(ctx); err != nil {
		w.Close()
		return nil, errors.Join(err, engine.Close(ctx))
	}
	return d, nil
}

func (d *demo) setup(ctx context.Context) error {
	admin, err := d.engine.For(service)
	if err != nil {
		return err
	}
	// The application configures its channel and inbox through uma's
	// configuration operations; uma records each as a ledgered operation.
	cfg := admin.Configuration()
	acc, err := cfg.ConfigureChannel(ctx, model.ConfigureChannel{
		Command: command("configure-channel"),
		Channel: model.Channel{
			Ref: model.ChannelRef{Tenant: tenant}, Type: widget.Type,
			Configuration: widget.Configuration(site), CredentialRef: "session-key",
		},
	})
	if err != nil {
		return err
	}
	channel, err := acc.Operation.Result.AsChannel()
	if err != nil {
		return err
	}
	// Per room: one conversation per browser session.
	if _, err := cfg.ConfigureInbox(ctx, model.ConfigureInbox{
		Command: command("configure-inbox"),
		Inbox: model.Inbox{
			Ref: model.InboxRef{Tenant: tenant}, Channel: channel, Name: site,
			Destinations: []string{site}, Selection: model.Selection{Mode: model.ModePerRoom},
		},
	}); err != nil {
		return err
	}

	// uma owns the receiver: it verifies and records what the widget posts.
	// The application owns the routes it mounts it on.
	receiver, err := d.engine.Runtime().Webhook(ctx, runtimeCall(), channel)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("POST /widget/", receiver) // session, messages, actions, submissions
	mux.Handle("GET /widget/events", d.widget.Events(channel))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(botpage.Page)
	})
	d.handler = mux

	// The bot companion reads through a subscription the application opens
	// for its own principal.
	client, err := d.engine.For(helper)
	if err != nil {
		return err
	}
	opened, err := client.Events().Open(ctx, model.OpenSubscription{
		Command: command("open-subscription"), Consumer: "helper",
		Filter: model.Filter{Kinds: []string{model.EventMessageReceived, model.EventInteractionReceived}},
		Start:  model.StartBeginning, PayloadMajor: model.PayloadVersion1,
	})
	if err != nil {
		return err
	}
	d.bot, err = bot.New(bot.Config{
		Client: client, Self: helper.Actor, Subscription: opened.Subscription.Ref,
		Wait: 10 * time.Second,
		// The companion dispatches events to these handlers and acknowledges
		// each event after they ran. The page sends "/help" and the Help
		// button as interactions, which the command and action routes answer
		// alike. Messages fall to the fallback echo.
		Commands: map[string]bot.Handler{"help": help},
		Actions:  map[string]bot.Handler{"help": help},
		Fallback: reply(func(s string) string { return "echo: " + s }),
	})
	return err
}

// help answers a /help command or a press of the Help button with the help
// text. The provider was acknowledged when uma recorded the interaction, so
// the answer is a deferred response to it, to everyone, and a redelivered
// event replays the same response instead of answering twice.
func help(ctx context.Context, t *bot.Turn) error {
	_, err := t.Respond(ctx, model.ResponseModeMessage, model.Audience{Visibility: model.Public}, bot.TextContent(helpText))
	return err
}

// reply turns a text function into a bot handler. The answer depends on
// the message alone, so a redelivered event replays the same reply.
func reply(answer func(string) string) bot.Handler {
	return func(ctx context.Context, t *bot.Turn) error {
		_, err := t.Reply(ctx, bot.TextContent(answer(bot.Text(t.Message.Content))))
		return err
	}
}

// Handler serves the chat page and the widget's receiver and event stream.
func (d *demo) Handler() http.Handler { return d.handler }

// Run dispatches the bot (the companion) and delivers its replies (uma's
// delivery worker) until ctx ends, then drains the worker. If the bot or the worker stops on its own,
// Run stops the other, drains and returns the failure.
func (d *demo) Run(ctx context.Context) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// drained is set before Run's own Drain. A stop that beats the worker's
	// start (a signal during startup) drains first, and the runtime refuses
	// the Run that registers after it with Unavailable: that refusal is
	// the stop, not a failure. A drain from elsewhere still fails the worker.
	var drained atomic.Bool
	worker := make(chan error, 1)
	go func() {
		err := d.engine.Runtime().Run(context.WithoutCancel(ctx), runtimeCall(),
			uma.WorkerOptions{Owner: "demo-worker", Limit: 10, Poll: 100 * time.Millisecond})
		if err != nil && drained.Load() && isDrainingRefusal(err) {
			err = nil
		}
		worker <- err
	}()
	ran := make(chan error, 1)
	go func() { ran <- d.bot.Run(ctx) }()

	var botErr, workerErr error
	workerDone := false
	select {
	case botErr = <-ran:
	case workerErr = <-worker:
		workerDone = true
		if workerErr == nil && parent.Err() == nil {
			workerErr = errors.New("delivery worker stopped on its own")
		}
		cancel()
		botErr = <-ran
	}
	if errors.Is(botErr, context.Canceled) {
		botErr = nil
	}
	cancel()
	drained.Store(true)
	drainCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer stop()
	drainErr := d.engine.Runtime().Drain(drainCtx, runtimeCall())
	if !workerDone {
		workerErr = <-worker
	}
	return errors.Join(botErr, drainErr, workerErr)
}

// isDrainingRefusal reports whether err is the refusal the runtime gives a
// Run that registers on a drained tenant: Unavailable, not retryable, with
// the engine's message. A different Unavailable error is a real failure.
func isDrainingRefusal(err error) bool {
	var me *model.Error
	return errors.As(err, &me) && me.Code == model.Unavailable &&
		me.Retry == model.NoRetry && me.SafeMessage == "runtime draining"
}

// Close ends the visitors' event streams, then closes the engine.
func (d *demo) Close(ctx context.Context) error {
	d.widget.Close()
	return d.engine.Close(ctx)
}

func command(op model.OperationID) model.Command {
	return model.Command{Call: model.Call{Correlation: model.CorrelationID(op)}, Operation: op}
}

func runtimeCall() uma.RuntimeCall {
	return uma.RuntimeCall{Principal: service, Call: model.Call{Correlation: "demo-runtime"}}
}

// sessionKey is the host's credential port: the widget's session signing
// key, random per process, so restarting ends every visitor session.
type sessionKey []byte

func newSessionKey() sessionKey { return sessionKey(rand.Text() + rand.Text()) }

func (k sessionKey) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	if r.Purpose != widget.CredentialPurpose || !r.Accepts(ports.KindSecret) {
		return nil, model.NewError(model.Unsupported, "no credential for "+r.Purpose)
	}
	return ports.Secret{Bytes: k}, nil
}
