# Hosting a bot

`companions/bot/host` is the layer between your application and the bot
companion. It prepares the bot's subscription, supervises the parts of a bot
host and composes their HTTP routes. It decides nothing for you: principals,
authorization, credentials and storage stay yours. It uses uma's public API
only and knows no provider.

Two runnable examples show it:

- [`examples/bot`](../examples/bot) is the quickstart, written over the host
  package.
- [`examples/botexplicit`](../examples/botexplicit) is the reference
  composition: the same bot with the underlying calls and its own supervision.
  Read it to see what the host package does for you.

## Start a bot

You need an engine, the bot's client, a prepared subscription and a place to
serve. This is the shape of `examples/bot`; `botdemo` holds the example's
stand-ins for a demo authorizer, a session key and an already-provisioned
channel, and `w` is the widget factory.

```go
engine, err := uma.New(uma.Config{
	Store:       memory.New(memory.Options{}),     // nothing survives a restart
	Authorizer:  botdemo.LocalDemoAuthorizer(nil), // demo only: a real host decides per principal
	Adapters:    []adapter.Factory{w},
	Credentials: botdemo.NewSessionKey(),
	Clock:       ports.SystemClock{},
	IDs:         ports.NewUUIDv7(ports.SystemClock{}),
})
if err != nil {
	return err
}
defer engine.Close(context.WithoutCancel(ctx)) // the application owns the engine
client, err := engine.For(helper)              // helper is the bot's model.Principal
if err != nil {
	return err
}
prep, err := host.Prepare(ctx, host.PrepareConfig{
	Client: client, Principal: helper, Consumer: "support-helper",
	Filter: model.Filter{Kinds: []string{model.EventMessageReceived, model.EventInteractionReceived}},
	Intent: host.Bootstrap(model.StartBeginning, ""), // first run; a restart uses host.Resume(marker)
})
if err != nil {
	return err
}
// Persist prep.Marker here, before anything consumes.
h, err := host.New(ctx, host.Config{
	Client: client, Principal: helper, Runtime: engine.Runtime(), RuntimeCall: runtimeCall,
	Worker:   uma.WorkerOptions{Owner: "demo-worker", Limit: 10, Poll: 100 * time.Millisecond},
	Prepared: prep,
	Bot: bot.Config{
		Wait:     10 * time.Second,
		Commands: map[string]bot.Handler{"help": help},
		Fallback: echo, // any message no other route claimed
	},
	Bindings: []host.Binding{{Pattern: "POST /widget/", Channel: channel}},
	Routes:   []host.Route{{Pattern: "GET /widget/events", Handler: w.Events(channel)}},
})
if err != nil {
	return err
}
srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second}
return h.Run(ctx, host.Options{}, host.Server(srv, ln), // ln: the listener you bound
	host.Closer("widget", func(context.Context) error { w.Close(); return nil }))
```

What to notice:

- `New` starts nothing. `Run` starts the delivery worker, the bot and the
  extra components you pass, and returns when the context ends or any of them
  stops.
- `Bot` carries routes and limits only. `Client`, `Self` and `Subscription`
  are set by the host from `Client`, `Principal` and `Prepared`; set any of
  them in `Bot` and `New` returns a `model.Validation` error.
- `Principal` is the bot's. `RuntimeCall.Principal` is the runtime's, and it
  is never taken from the bot's: the actor that runs the runtime must be the
  one that provisioned the channel.
- `Client` must be bound to `Principal`. A `uma.Client` does not report its
  principal, so `New` cannot check that.
- Handlers use `Turn.ReplyText(ctx, s)` for a text reply and
  `bot.TextContent(s)` where content is needed, for example with
  `Turn.Respond`. A handler that returns has had its reply accepted by uma; the
  worker delivers it afterwards.

## Restart without losing your place

`Prepare` opens the bot's subscription once and returns a `Marker`. Persist
the marker (it is plain JSON) before anything consumes. On the next start
pass it back:

```go
// First run, no marker stored:
prep, err := host.Prepare(ctx, cfg(host.Bootstrap(model.StartBeginning, "")))
// Later runs:
prep, err := host.Prepare(ctx, cfg(host.Resume(marker)))
```

`Bootstrap` and `Resume` are separate intents on purpose. The zero intent is
refused, and a missing marker is never taken as a first run: you decide, so a
lost marker cannot silently start the bot over from the beginning of the log.
`Resume` also checks the marker against the config: another tenant, actor or
consumer, a changed filter (a changed filter needs a new consumer) or a
different payload major is a `model.Validation` error.

The marker records the exact Open request. `Resume` sends it again, unchanged;
uma replays a request it recorded. If it does not, uma holds no record of the
operation the marker names, and `Prepare` returns `*SubscriptionContinuityLost`
before anything is polled, dispatched or acknowledged:

```go
var lost *host.SubscriptionContinuityLost
if errors.As(err, &lost) { // errors.Is(err, host.ErrSubscriptionContinuityLost) also matches
	// a host decision, below
}
```

### What the check guarantees

The guarantee is narrow:

- The marker records the uma operation that accepted the Open request, and
  `Resume` requires a replay of that operation. It detects that uma does not
  remember the operation. It does not prove that history or the checkpoint
  are unchanged.
- A store without the operation's record is detected: a replaced store, or one
  restored from a backup taken before the Open. A backup taken after the Open
  holds the record and passes (recovery option 3).
- An operation record purged past its retention horizon is a false positive:
  the store is the same, and the check still reports a loss.
- `Events.Close` is not detected. After it, `Resume` with the same marker
  succeeds, and the first poll fails with a `Conflict` ("subscription closed").
  Reopening needs a different request and operation, not `Prepare` with this
  marker.
- A subscription ref is derived from tenant, actor and consumer, so it is
  never evidence of continuity. Two stores give the same ref.
- A `StartCheckpoint` marker resumed against a replaced store can fail inside
  `Open` with the engine's own error instead of `*SubscriptionContinuityLost`.
  It fails closed: nothing is consumed.
- The check follows `Open`, so it prevents consumption, not recreation. The
  replacement subscription stays in the new store, and another process may use
  it. `Prepare` deletes nothing and never changes the marker.

### When continuity is lost

Recovery is your decision. There are three options:

1. **Adopt the replacement.** Call `Prepare` with `Bootstrap` and the
   identical request. It returns the replacement's open record unchanged and a
   new marker; persist it. The bot continues from the replacement's checkpoint;
   the old subscription's checkpoint is not recovered.
2. **Create a fresh consumer at the position you want.** A new `Consumer`
   name with a `Start` you choose (`model.StartTail`, `StartBeginning` or
   `StartCheckpoint` with a cursor). A different `Start` does not move an
   existing subscription, so it needs a different consumer.
3. **Restore the state.** Put the lost store (or its backup) back and resume
   with the same marker.

Do not retry `Resume` with the same marker against the same replaced store
expecting a different answer. The first attempt's Open recorded a new
operation in that store, so a repeat replays, but of that new operation, not
the marker's, and `Prepare` still reports the loss.

The behaviours behind the check, observed on the memory store with the same
tenant, actor and consumer throughout (the ref is identical in every case):

| Open | Replayed |
| --- | --- |
| first request | false |
| same request, same engine | true |
| same request, new engine, same store | true |
| same request, new engine, fresh store | false (true on repeat) |
| same request after `Events.Close`, same store | true |
| request with another `Start` | false |

## Stop cleanly

`Run` (and `Host.Run`) supervise components: a name, a `Run` and an optional
bounded `Shutdown`. `Host.Components` lists the worker, then the bot; extras
registered after them stop first:

1. Extras, in reverse order of registration. Register a `Closer` for the
   widget after the server, so the event streams end before the server drains.
2. The bot.
3. The worker. Its `Shutdown` drains the runtime, so work already claimed
   finishes.

The rules:

- The first component to return, with an error or without one, ends the
  group. A bot that stopped must not leave a server answering nobody, so a
  nil return while the parent context is live is reported as
  `host.ErrComponentExited`.
- Run then cancels the shared context, calls each `Shutdown` in reverse order
  of registration (including that of a component whose `Run` already
  returned) and waits for the `Run`s.
- All of it is bounded by `Options.StopTimeout` (10 seconds when zero; negative
  is invalid). A `Shutdown` or a `Run` still going when it passes is named in
  the result and abandoned, and keeps running in the background, because Go
  cannot stop a goroutine. Write a `Shutdown` that honours the context it is
  given.
- The result joins every failure, each named by component. A component that
  stopped because of the cancellation is not blamed, and its error is not
  reported.
- A parent context that was cancelled, or reached its deadline, is a clean
  stop: the cancellation is not reported, and `Run` returns nil unless a
  component or a `Shutdown` failed or the stop overran `StopTimeout`.
- A `host.Worker` remembers its own `Shutdown`: draining is permanent for the
  tenant on that runtime, so build a fresh one (call `Components` again) for
  each `Run`.

`Run` starts and stops only the components it was given. It never closes your
engine, your store, your widget (unless you register a `Closer` for it) or
your own server. `host.Server` and `host.ListenAndServe` stop the
`http.Server` they run with `Shutdown`; closing the engine, as in the example,
is yours, after `Run` returns:

```go
err := h.Run(ctx, host.Options{StopTimeout: 15 * time.Second}, host.Server(srv, ln))
return errors.Join(err, engine.Close(context.WithoutCancel(ctx)))
```

## Mount in your own server

`Host.Handler()` serves the `Bindings` and `Routes` you gave `New`. Hand it
to the server you run, with `host.Server(srv, ln)` or
`host.ListenAndServe(addr, h.Handler(), onListen)`, or mount it in a server you
manage yourself. `host.Mux` builds the same handler without a host.

The rules are explicit and checked:

- A `Binding` mounts a receiver: `Pattern` is an `http.ServeMux` pattern such
  as `POST /widget/`, and `Channel` is resolved into the receiver's handler
  through `Runtime.Webhook`. A `Route` mounts a handler of yours.
- Nothing is inferred. A channel is served only where a `Binding` says so, and
  there is no default route.
- Every pattern and handler is validated before the handler is returned. An
  empty, malformed or conflicting pattern, or a nil handler, is a
  `model.Validation` error naming the pattern. Mux never panics.
- A receiver that cannot be resolved is returned wrapped, naming its pattern.
- Mux starts nothing and owns no server.

## Generated replies

A handler that reruns (the event is redelivered after a crash between sending
and acknowledging) resubmits the same operation IDs, and uma replays them
instead of sending twice. That works only when the rerun carries the same
payload. A handler whose output changes between runs, such as a model call or
a clock, would send different content under the same ID, and uma refuses that
as a payload conflict.

So a stable operation ID is not enough on its own. Record the choice before
sending it, keyed by the turn's operation ID, and reuse it on every rerun:

```go
func (a *app) draft(ctx context.Context, t *bot.Turn) error {
	topic := t.Interaction.Arguments
	text := a.decisions.choose(string(t.OperationID("draft")), func() string { return a.generate(topic) })
	_, err := t.Respond(ctx, model.ResponseModeMessage, model.Audience{Visibility: model.Public}, bot.TextContent(text))
	return err
}
```

`decisions.choose` returns the recorded text for the key and generates and
records one first when there is none. The write must complete before the
outbound operation is submitted. The example keeps it in memory like its
store; a durable host keeps it next to its uma store. Side effects outside uma
are yours to make replay-safe in the same way.

## What each layer owns

| Layer | Owns |
| --- | --- |
| Application | Principals (the bot's, the runtime's, the worker's owner), authorization, credentials, storage, the listener and server, persisting the marker, the recovery decision when continuity is lost, recording generated content before it is sent |
| Bot host layer (`companions/bot`, `companions/bot/host`) | Routing events to handlers, acknowledging after handlers ran, preparing the subscription and checking continuity, supervising components with bounded shutdown, validating routes |
| uma | Operations (identity, replay, conflicts), the event log and checkpoints, enforcing the application's authorization, delivery of queued replies through the worker |

The host layer checks and reports; it does not decide. It never picks a
recovery, never persists the marker, and never supplies a principal.

See also: [interactions](interactions.md) for commands, buttons and forms;
[compatibility](compatibility.md) for what is frozen (this package is not).
