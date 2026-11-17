package host

import (
	"context"
	"net/http"

	"hop.top/uma"
	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// Config is what the application supplies to New. Nothing in it is
// defaulted where it is policy: the bot's principal, the runtime's principal
// and the worker's owner are all stated by the application.
type Config struct {
	// Client is bound to Principal: it acts as the bot. A uma.Client does not
	// report the principal it is bound to, so New cannot check that, and
	// cannot detect a client bound to another actor.
	Client uma.Client
	// Principal is the bot's principal. Its actor becomes the bot's Self.
	Principal model.Principal

	// Runtime serves the receivers in Bindings and runs the delivery worker.
	Runtime uma.Runtime
	// RuntimeCall carries the principal the runtime acts as for receivers
	// and the worker. It is never defaulted, and in particular never taken
	// from Principal: the runtime's authority is not the bot's.
	RuntimeCall uma.RuntimeCall
	// Worker configures the delivery worker; Owner, the lease holder, is
	// required.
	Worker uma.WorkerOptions

	// Prepared is the outcome of Prepare for this principal. Its Marker
	// and Subscription must come from one Prepare call: New checks the
	// marker's tenant and actor against Principal and that the subscription
	// is set, nothing more. The subscription becomes the bot's.
	Prepared Prepared

	// Bot holds the bot's routes and limits only. Client, Self and
	// Subscription are set by the host from the fields above and must be
	// zero here.
	Bot bot.Config

	// Bindings mount receivers and Routes mount the application's own
	// handlers, on the Handler New returns. See Mux.
	Bindings []Binding
	Routes   []Route
}

// Host is a bot, a delivery worker and an HTTP handler composed from one
// Config. New starts none of them: Run does.
type Host struct {
	b    *bot.Bot
	sub  model.Subscription
	h    http.Handler
	rt   uma.Runtime
	call uma.RuntimeCall
	opts uma.WorkerOptions
}

// New composes a host. It checks the whole Config before it builds
// anything, so an invalid Config constructs no bot and resolves no receiver;
// every such failure is a model.Validation error. It then builds the bot and
// the handler, and returns their errors as they come. New opens no
// subscription (Prepare did), polls nothing, runs no worker and listens on
// nothing; the receivers it resolves are only mounted on the handler.
func New(ctx context.Context, cfg Config) (*Host, error) {
	invalid := func(msg string) (*Host, error) { return nil, model.NewError(model.Validation, "host: "+msg) }
	switch {
	case cfg.Client == nil:
		return invalid("client required")
	case cfg.Runtime == nil:
		return invalid("runtime required")
	case cfg.Principal.Validate() != nil:
		return invalid("bot principal required")
	case cfg.RuntimeCall.Principal.Validate() != nil:
		return invalid("runtime principal required: it is never defaulted")
	case cfg.Worker.Owner == "":
		return invalid("worker owner required")
	case cfg.Prepared.Subscription.Ref == (model.SubscriptionRef{}):
		return invalid("prepared subscription required")
	case cfg.Prepared.Marker.Tenant != cfg.Principal.Tenant || cfg.Prepared.Marker.Actor != cfg.Principal.Actor:
		return invalid("prepared subscription belongs to another principal")
	case cfg.Bot.Client != nil || cfg.Bot.Self != "" || cfg.Bot.Subscription != (model.SubscriptionRef{}):
		return invalid("Bot.Client, Self and Subscription are set by the host")
	}
	if err := checkRoutes(cfg.Bindings, cfg.Routes); err != nil {
		return nil, err
	}
	bc := cfg.Bot
	bc.Client, bc.Self, bc.Subscription = cfg.Client, cfg.Principal.Actor, cfg.Prepared.Subscription.Ref
	b, err := bot.New(bc)
	if err != nil {
		return nil, err
	}
	mux, err := Mux(ctx, cfg.Runtime, cfg.RuntimeCall, cfg.Bindings, cfg.Routes)
	if err != nil {
		return nil, err
	}
	return &Host{b: b, sub: cfg.Prepared.Subscription, h: mux, rt: cfg.Runtime, call: cfg.RuntimeCall, opts: cfg.Worker}, nil
}

// Bot is the dispatcher New built.
func (h *Host) Bot() *bot.Bot { return h.b }

// Subscription is the prepared subscription the bot consumes.
func (h *Host) Subscription() model.Subscription { return h.sub }

// Handler serves the Bindings and Routes. Mount it in the server the
// application runs, or hand it to ListenAndServe.
func (h *Host) Handler() http.Handler { return h.h }

// Components are the worker, then the bot, in registration order: Run stops
// components in reverse, so anything registered after them stops first and
// the worker, whose Shutdown drains the runtime, last. Each call returns a
// new worker component: use one list for one Run (see Worker).
func (h *Host) Components() []Component {
	return []Component{Worker(h.rt, h.call, h.opts), Bot(h.b)}
}

// Run supervises the worker, the bot and any extra components (a server, a
// widget to close) under Run's rules. Extras stop first, the worker last.
func (h *Host) Run(ctx context.Context, o Options, extra ...Component) error {
	return Run(ctx, o, append(h.Components(), extra...)...)
}
