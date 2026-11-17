// Package host is optional hosting support for the bot companion: it
// prepares a subscription, supervises the components of a bot host and
// composes HTTP routes. It uses the standard library, companions/bot and
// uma's public API only; it knows no provider.
//
// # Preparing a subscription
//
// Prepare opens the bot's subscription once and returns a Marker for the
// host to persist before consumption starts. Bootstrap and Resume are
// separate, explicit intents: a missing marker is never taken as a first
// run. The marker records the exact Open request. Resume sends that request
// again, unchanged: uma replays a request it recorded, so a result that is
// not a replay means the operation record is gone (a replaced store, one
// restored from before the Open, or an operation record purged past its
// retention horizon) and Prepare returns *SubscriptionContinuityLost before
// anything is polled, dispatched or acknowledged.
//
// The guarantee is narrow. The marker records the uma operation that
// accepted the Open request, and Prepare requires Resume to get back a
// replay of that operation. It detects that uma does not remember the
// operation; it does not prove that history or the checkpoint are unchanged,
// and a purged operation record is a false positive. Replayed alone is not
// enough: the first lost Resume's Open records the operation in the replaced
// store, so a repeat replays, but of that new operation, not the marker's.
// The check follows Open, so it prevents consumption, not recreation: the
// replacement subscription stays and another process may use it. Prepare
// deletes nothing and never changes the marker. Recovery is a host decision:
// adopt the replacement as it is (Bootstrap with the identical request
// returns the open record unchanged and a new marker holding the
// replacement's operation; persist it), create a fresh consumer at the
// position wanted, or restore state. A different Start does not move an
// existing subscription. A subscription ref is derived from tenant, actor
// and consumer, so it is never evidence of continuity.
//
// A Resume of a StartCheckpoint marker against a replaced store can fail
// inside Open with the engine's own error instead of
// *SubscriptionContinuityLost; it fails closed, nothing is consumed.
//
// # Observed behaviour
//
// Observed on the memory store with StartTail, the same tenant, actor and
// consumer throughout; the subscription ref is identical in every case:
//
//	open                                          Replayed
//	first request                                 false
//	same request, same engine                     true
//	same request, new engine, same store          true
//	same request, new engine, fresh store         false (true on repeat)
//	same request after Events.Close, same store   true
//	request with another Start (other operation)  false
//
// A fresh store is the case Prepare reports; the repeat is told apart by the
// operation ref. A replay skips execution, so nothing is reopened: after
// Events.Close, Prepare with the same request succeeds (undetected) and the
// first Poll fails with Conflict "subscription closed". Reopening needs a
// different request and operation, not Prepare with this marker.
//
// # Supervising components
//
// Run starts every Component under one context. The first component to
// return, with an error or without one, ends the group: a bot that stopped
// must not leave a server answering nobody, so a nil return while the parent
// context is live is reported as ErrComponentExited. Run then cancels the
// shared context, calls each Shutdown in reverse order of registration
// (including that of a component whose Run already returned) and waits for
// the Runs, all within Options.StopTimeout. The result joins every failure,
// each named by component. What the supervisor's cancel made a component
// return is not blamed, and its error is not reported. A parent context that
// was cancelled or reached its deadline is a clean stop: the cancellation is
// not reported, and Run returns nil unless a component or a Shutdown failed
// or the stop overran StopTimeout.
//
// Run starts and stops only the components it was given. It never closes an
// engine, a store or a server the application owns.
//
// Both phases are bounded. The Shutdowns run one after another in a single
// goroutine; a Shutdown still running when StopTimeout passes is named in the
// result and abandoned, and so is the goroutine, which keeps running in the
// background: Shutdowns registered before the stuck one may not have run yet
// when Run returns. A Run that ignores cancellation is likewise named and
// abandoned. Both leak by design, because Go cannot stop a goroutine. A
// Shutdown should honour the context it is given, which carries the stop
// deadline.
//
// # Composing routes
//
// Mux serves receiver Bindings and application Routes on one handler.
// Both are explicit: a channel is served only at the pattern its Binding
// names, and no path is inferred. Every pattern and handler is validated
// before the handler is returned; an empty, malformed or conflicting pattern
// and a nil handler are model.Validation errors naming the pattern, never
// panics. A failed Webhook resolution is returned wrapped, naming its
// pattern. Mux owns no server: mount the result in the server the
// application runs.
//
// # Composing a host
//
// New builds the parts of a bot host from one Config without starting any of
// them: the bot, the delivery worker as a Component and the handler Mux
// returns. The application supplies every piece of policy: the bot's
// principal, the runtime's principal in RuntimeCall (never taken from the
// bot's), the worker's Owner and the Prepared subscription whose Marker must
// name the bot's principal. Client is bound to that principal, which New
// cannot verify. Bot carries routes only: Client, Self and Subscription are
// set by the host and must be zero. An invalid Config is a model.Validation
// error and nothing is built or resolved. Components lists the worker, then
// the bot; Host.Run adds extra components after them, so the extras stop
// first, then the bot, and the worker drains the runtime last.
package host
