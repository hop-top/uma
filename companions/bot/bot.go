// Package bot is a companion dispatcher for bot hosts: it consumes one
// event subscription through an actor-bound uma.Client, routes each event
// to a host handler, and acknowledges a batch only after every handler of
// the batch settled.
//
// The package uses uma's public API only. The host owns the process: it
// opens the subscription (Events.Open with an explicit start), builds the
// client for the bot's principal, supplies the watch store and calls Run or
// Step. A Bot starts no goroutine of its own outside Step, and every
// goroutine Step starts has returned when Step returns.
//
// Package hop.top/uma/companions/bot/host is optional support for a host that
// wants the subscription prepared, the components supervised and the routes
// composed for it; this package does not depend on it.
//
// # Routing
//
// A message.received event is routed by declared precedence: Direct when
// IsDirect reports the conversation as a direct exchange, then Watched when
// the bot watches the conversation, then Mention when IsMention reports the
// message as addressed to the bot, then the first Pattern whose expression
// matches the message text. A message no other route claims goes to Fallback
// when set. An interaction.received event is routed by declared precedence
// too: the Commands handler for the interaction's command name, else the
// Actions handler for its action ID, else the
// Submissions handler for the name of its form schema, else the Kinds
// handler of interaction.received. An interaction that carries a command,
// an action and form fields (typed options, form state) takes the first
// route that has a handler for it. IsAddressed, when set, drops the
// interactions of other bots before any of these. Other event kinds route
// through Kinds. An event no route claims is ignored and acknowledged with
// its batch.
//
// Events the bot itself caused (Event.Actor is Self) and messages whose
// Origin names Self, the correlated echo of the bot's own operation, are
// ignored: a bot never answers itself. A message from another actor on the
// same account is not an echo.
//
// # Delivery and replay
//
// Delivery is at least once. A batch is acknowledged after its handlers
// return; until then the subscription redelivers it, after a crash or a
// retryable failure, to this or a restarted dispatcher. Turn.Reply and
// Turn.OperationID derive operation IDs from the bot, the event ID, the
// handler and the position of the call within the turn, so a rerun handler
// resubmits the same operations and uma replays them instead of sending
// twice. The replayed command must carry the same payload: a handler whose
// output can change between runs (a model call, a clock) must persist its
// decision and reuse it. Side effects outside uma are the host's to make
// replay-safe.
//
// # Answering interactions
//
// An interaction turn carries Turn.Interaction and Turn.Conversation and no
// message. The provider acknowledged the interaction when uma held it
// durably, so the response is deferred: the handler answers within the
// interaction's respond window with Turn.Respond (the initial response) and
// Turn.FollowUp, or replies publicly with Turn.Reply, which only correlates
// the reply with the interaction (Reply.InResponseTo) and is not a response.
// Respond and FollowUp take the response mode and audience explicitly, as
// Interactions.Respond does, and expire with the interaction's respond
// window, or Config.RespondWindow after the interaction was recorded when
// the provider states none; the expiry is a function of the stored
// interaction, never of the clock, so a redelivered turn resubmits the same
// request.
//
// The operation IDs of a redelivered turn are those of the first run, so uma
// replays an accepted response instead of answering twice, also to a
// restarted dispatcher. uma accepts one initial response per interaction:
// when another worker won it, Respond returns an error wrapping
// ErrResponseClaimed and the event is Ignored, never retried. A response
// after the window is model.DeadlineExpired, which drops the event. A
// response that lost to a concurrent unit (model.Transient) is resubmitted
// in place, like a reply.
//
// Watches are bot interest state in a host-supplied store, separate from
// subscription checkpoints and from provider subscriptions. Removing a
// watch stops routing only: it never stops ingestion or deletes history.
package bot

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Handler runs one routed event. Its error decides the event's outcome; see
// Classify.
type Handler func(ctx context.Context, t *Turn) error

// Pattern routes messages whose text Match matches. Name identifies the
// handler in operation IDs and results; it must be unique and stable across
// releases of the host, or redelivered events resubmit under new IDs.
type Pattern struct {
	Name   string
	Match  *regexp.Regexp
	Handle Handler
}

// Config composes a Bot. Client, Self and Subscription are required. Each
// route is optional; a route's predicate or store is required with it.
type Config struct {
	// Client is bound to the bot's principal.
	Client uma.Client
	// Self is the bot's actor: the actor of Client's principal.
	Self model.ActorID
	// Subscription is the bot's opened subscription.
	Subscription model.SubscriptionRef

	// Direct handles messages in conversations IsDirect accepts.
	Direct   Handler
	IsDirect func(model.Conversation) bool
	// Watched handles messages in conversations the bot watches in Watches.
	Watched Handler
	// Watches holds watch state; required by Watched, Turn.Watch and
	// Turn.Unwatch.
	Watches Watches
	// Mention handles messages IsMention reports as addressed to the bot.
	Mention   Handler
	IsMention func(model.Message) bool
	// Patterns handle messages by text, first match wins.
	Patterns []Pattern
	// Fallback handles a message no earlier route claimed: it runs after
	// Direct, Watched, Mention and Patterns, so it never changes their
	// precedence. It does not receive interactions or other event kinds.
	Fallback Handler
	// Commands, Actions and Submissions handle interaction.received events
	// by command name, action ID and submitted form schema name. See
	// Routing for their precedence.
	Commands    map[string]Handler
	Actions     map[string]Handler
	Submissions map[string]Handler
	// IsAddressed reports whether an interaction is for this bot. When set,
	// an interaction it rejects is ignored by every interaction route,
	// Kinds included: interactions of other bots in the same inbox are
	// not the bot's to answer. Nil addresses every interaction to the bot.
	IsAddressed func(model.Interaction) bool
	// RespondWindow bounds a response to an interaction whose provider
	// states no respond window (Interaction.RespondUntil zero): the
	// response expires this long after the interaction was recorded. It
	// has no effect when the provider states one. Default 15 minutes.
	RespondWindow time.Duration
	// Kinds handles events of other kinds by Event.Kind. message.received
	// routes through the message routes above and is not accepted here.
	// interaction.received is accepted as the fallback of the interaction
	// routes.
	Kinds map[string]Handler

	// Concurrency caps the handlers running at once. Events of one
	// conversation always run one at a time, in batch order. Default 1.
	Concurrency int
	// MaxAttempts caps the runs of an event a retryable failure keeps
	// unsettled, counted within this Bot; the last failure drops it.
	// Default 5.
	MaxAttempts int
	// Limit is Poll.Limit; zero takes the engine default.
	Limit int
	// Wait is Poll.Wait, the long-poll wait; zero polls without waiting.
	Wait time.Duration
	// Idle is Run's pause after a poll found nothing. Default 1s.
	Idle time.Duration
	// Backoff is Run's pause after a batch was left unacknowledged or a
	// poll failed retryably. Default 1s.
	Backoff time.Duration
	// Waiter paces Run's pauses; nil uses a cancellable stdlib timer.
	Waiter ports.Waiter
	// Observe, when set, receives every result in batch order, from the
	// goroutine running Step, after the batch's handlers returned.
	Observe func(Result)
}

// Defaults of the zero Config fields.
const (
	DefaultConcurrency = 1
	DefaultMaxAttempts = 5
	DefaultIdle        = time.Second
	DefaultBackoff     = time.Second

	DefaultRespondWindow = 15 * time.Minute
)

// UnitAttempts bounds Step's submissions of one poll, reply or
// acknowledgment that fails with model.Transient: a unit of work that lost
// to a concurrent unit, which the same request, under the same operation
// ID, may simply repeat. Other failures are never resubmitted in place.
const UnitAttempts = 4

// Bot dispatches one subscription. Step and Run must not be called
// concurrently on one Bot; to share a subscription, run several Bots: the
// subscription's lease hands each batch to one of them.
type Bot struct {
	cfg      Config
	patterns []Pattern
	kinds    map[string]Handler

	commands, actions, submissions map[string]Handler

	mu       sync.Mutex
	settled  map[model.ID]Result
	attempts map[model.ID]int
}

// New validates cfg and returns a Bot. It performs no I/O.
func New(cfg Config) (*Bot, error) {
	if err := validate(cfg); err != nil {
		return nil, err
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = DefaultMaxAttempts
	}
	if cfg.Idle == 0 {
		cfg.Idle = DefaultIdle
	}
	if cfg.Backoff == 0 {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.RespondWindow == 0 {
		cfg.RespondWindow = DefaultRespondWindow
	}
	if cfg.Waiter == nil {
		cfg.Waiter = timerWaiter{}
	}
	return &Bot{
		cfg: cfg, patterns: append([]Pattern(nil), cfg.Patterns...), kinds: copyRoutes(cfg.Kinds),
		commands: copyRoutes(cfg.Commands), actions: copyRoutes(cfg.Actions), submissions: copyRoutes(cfg.Submissions),
		settled: map[model.ID]Result{}, attempts: map[model.ID]int{},
	}, nil
}

func validate(cfg Config) error {
	invalid := func(msg string) error { return model.NewError(model.Validation, "bot: "+msg) }
	switch {
	case cfg.Client == nil:
		return invalid("client required")
	case cfg.Self.Validate() != nil:
		return invalid("self actor required")
	case cfg.Subscription.Validate() != nil:
		return invalid("subscription required")
	case cfg.Direct != nil && cfg.IsDirect == nil:
		return invalid("direct route requires IsDirect")
	case cfg.Mention != nil && cfg.IsMention == nil:
		return invalid("mention route requires IsMention")
	case cfg.Watched != nil && cfg.Watches == nil:
		return invalid("watched route requires a watch store")
	case cfg.Concurrency < 0, cfg.MaxAttempts < 0, cfg.Limit < 0, cfg.Wait < 0, cfg.Idle < 0, cfg.Backoff < 0, cfg.RespondWindow < 0:
		return invalid("negative limit or duration")
	}
	names := map[string]bool{}
	for _, p := range cfg.Patterns {
		switch {
		case p.Name == "" || p.Match == nil || p.Handle == nil:
			return invalid("pattern requires a name, an expression and a handler")
		case names[p.Name]:
			return invalid("duplicate pattern " + p.Name)
		}
		names[p.Name] = true
	}
	for name, table := range map[string]map[string]Handler{
		"command": cfg.Commands, "action": cfg.Actions, "submission": cfg.Submissions,
	} {
		for k, h := range table {
			if k == "" || h == nil {
				return invalid(name + " route requires a name and a handler")
			}
		}
	}
	for k, h := range cfg.Kinds {
		switch {
		case k == model.EventMessageReceived:
			return invalid("message.received routes through the message routes")
		case k == "" || h == nil:
			return invalid("kind handler requires a kind and a handler")
		}
	}
	return nil
}

func copyRoutes(in map[string]Handler) map[string]Handler {
	out := make(map[string]Handler, len(in))
	for k, h := range in {
		out[k] = h
	}
	return out
}

// Report is the result of one Step.
type Report struct {
	// Results holds one result per delivered event, in batch order.
	Results []Result
	// Acked reports that this Step acknowledged the batch.
	Acked bool
}

// Retried reports whether an event of the batch was left unsettled.
func (r Report) Retried() bool {
	for _, x := range r.Results {
		if x.Outcome == Retried {
			return true
		}
	}
	return false
}

// Run polls and dispatches until ctx ends or a step fails permanently. It
// pauses Idle after an empty poll and Backoff after a batch left
// unacknowledged or a retryable poll failure. On cancellation it stops
// polling, waits for running handlers, leaves an unsettled batch for
// redelivery and returns ctx's error.
func (b *Bot) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, err := b.Step(ctx)
		var pause time.Duration
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && retrySame(err):
			pause = b.cfg.Backoff
		case err != nil:
			return err
		case r.Retried():
			pause = b.cfg.Backoff
		case len(r.Results) == 0 && !r.Acked:
			pause = b.cfg.Idle
		}
		if pause > 0 {
			if err := b.cfg.Waiter.Wait(ctx, pause); err != nil {
				return err
			}
		}
	}
}

// Step polls one batch, runs its handlers and acknowledges it when every
// event settled. A poll or acknowledgment that lost to a concurrent unit is
// resubmitted up to UnitAttempts times. Any other Poll failure, or a failed
// acknowledgment, is returned; an
// acknowledgment lost to another poller of the subscription, or to lease
// expiry, is not an error: Acked is false and the next poll decides.
func (b *Bot) Step(ctx context.Context) (Report, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var batch model.EventBatch
	err := b.resubmit(ctx, func() (err error) {
		batch, err = b.cfg.Client.Events().Poll(ctx, model.Poll{
			Call: model.Call{Correlation: "bot-poll"}, Subscription: b.cfg.Subscription,
			Limit: b.cfg.Limit, Wait: b.cfg.Wait,
		})
		return err
	})
	if err != nil {
		return Report{}, err
	}
	if batch.AckToken == "" {
		return Report{}, nil
	}
	results := b.dispatch(ctx, batch.Events)
	b.remember(results)
	if b.cfg.Observe != nil {
		for _, r := range results {
			b.cfg.Observe(r)
		}
	}
	r := Report{Results: results}
	if r.Retried() {
		return r, nil
	}
	ack := model.Acknowledge{
		Command:      model.Command{Call: model.Call{Correlation: "bot-ack"}, Operation: ackOperation(batch.AckToken)},
		Subscription: b.cfg.Subscription, Token: batch.AckToken,
	}
	err = b.resubmit(ctx, func() error {
		_, err := b.cfg.Client.Events().Ack(ctx, ack)
		return err
	})
	switch {
	case err == nil:
		clear(b.settled)
		clear(b.attempts)
		r.Acked = true
		return r, nil
	case lostAck(err):
		return r, nil
	}
	return r, err
}

// resubmit runs submit, repeating it up to UnitAttempts times in all while
// it fails with model.Transient and same-operation retry advice, pausing
// one more millisecond before each repeat. Each repeat is the same
// request: a poll, or a command under the same operation ID, which uma
// replays if an earlier attempt committed after all.
func (b *Bot) resubmit(ctx context.Context, submit func() error) error {
	for attempt := 1; ; attempt++ {
		err := submit()
		if attempt == UnitAttempts || !transientUnit(err) || ctx.Err() != nil {
			return err
		}
		if b.cfg.Waiter.Wait(ctx, time.Duration(attempt)*time.Millisecond) != nil {
			return err
		}
	}
}

// lostAck reports an acknowledgment refused because the token no longer
// binds the outstanding batch: another poller acknowledged it or the lease
// ended. A closed subscription is a conflict too, but not retryable.
func lostAck(err error) bool {
	var e *model.Error
	return errors.As(err, &e) && e.Code == model.Conflict && advice(e) != model.NoRetry
}

// remember keeps, for a redelivery of the same batch, the results of the
// events that settled and the attempt counts of those that did not; state
// of events outside the batch is dropped.
func (b *Bot) remember(results []Result) {
	settled := make(map[model.ID]Result, len(results))
	attempts := make(map[model.ID]int, len(results))
	for _, r := range results {
		if r.Outcome == Retried {
			attempts[r.Event.ID] = r.Attempt
		} else {
			settled[r.Event.ID] = r
		}
	}
	b.settled, b.attempts = settled, attempts
}

type timerWaiter struct{}

func (timerWaiter) Wait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
