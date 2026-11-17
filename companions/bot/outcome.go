package bot

import (
	"context"
	"errors"

	"hop.top/uma/model"
)

// Outcome is the settled state of one delivered event.
type Outcome string

const (
	// Handled: a handler ran and returned nil.
	Handled Outcome = "handled"
	// Ignored: no route claimed the event, it is the bot's own effect, or
	// another worker won the interaction's initial response.
	Ignored Outcome = "ignored"
	// Dropped: the event failed permanently, or still failed after
	// MaxAttempts runs. It is acknowledged with its batch and not retried.
	Dropped Outcome = "dropped"
	// Retried: the event failed retryably. Its batch stays unacknowledged
	// and is redelivered; later events of its conversation wait for it.
	Retried Outcome = "retried"
)

// Route names the route that claimed an event; empty when none did.
type Route string

const (
	RouteDirect   Route = "direct"
	RouteWatched  Route = "watched"
	RouteMention  Route = "mention"
	RoutePattern  Route = "pattern"
	RouteFallback Route = "fallback"
	RouteKind     Route = "kind"

	RouteCommand    Route = "command"
	RouteAction     Route = "action"
	RouteSubmission Route = "submission"
)

// Result is the outcome of one delivered event.
type Result struct {
	Event model.Event
	Route Route
	// Handler is the handler identity used in operation IDs: the route
	// name, "pattern/<name>", "command/<name>", "action/<id>",
	// "submission/<schema>" or "kind/<event kind>".
	Handler string
	Outcome Outcome
	// Attempt counts this Bot's runs of the event, starting at 1. A
	// settled event redelivered in the same batch keeps its result.
	Attempt int
	// Err is the failure behind a Dropped or Retried outcome; nil for an
	// Ignored one.
	Err error
}

type dropError struct{ err error }

func (e *dropError) Error() string { return e.err.Error() }
func (e *dropError) Unwrap() error { return e.err }

type retryError struct{ err error }

func (e *retryError) Error() string { return e.err.Error() }
func (e *retryError) Unwrap() error { return e.err }

// Drop marks err as permanent: the event is dropped and acknowledged.
func Drop(err error) error {
	if err == nil {
		return nil
	}
	return &dropError{err}
}

// Retry marks err as retryable whatever its code: the batch is redelivered.
func Retry(err error) error {
	if err == nil {
		return nil
	}
	return &retryError{err}
}

// Classify returns the outcome of a handler error. nil is Handled. An error
// marked with Drop or Retry takes that outcome. An error wrapping
// ErrResponseClaimed, the initial response another worker won, is Ignored:
// nothing is left to do and nothing to retry. Otherwise a *model.Error
// whose retry advice is model.NoRetry (validation, authorization, not
// found, unsupported, permanent) is Dropped, and every other error,
// context cancellation, model.RetrySameOperation and model.ReconcileFirst
// advice and errors without a code included, is Retried: the operation IDs
// of the rerun replay what already happened.
func Classify(err error) Outcome {
	if err == nil {
		return Handled
	}
	var d *dropError
	var r *retryError
	var e *model.Error
	switch {
	case errors.As(err, &d):
		return Dropped
	case errors.As(err, &r):
		return Retried
	case errors.Is(err, ErrResponseClaimed):
		return Ignored
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return Retried
	case errors.As(err, &e) && advice(e) == model.NoRetry:
		return Dropped
	}
	return Retried
}

// advice returns e's retry advice, defaulting to its code's.
func advice(e *model.Error) model.RetryAdvice {
	if e.Retry != "" {
		return e.Retry
	}
	return e.Code.DefaultRetry()
}

// retrySame reports a failure the same request may simply be repeated
// after: model.RetrySameOperation advice.
func retrySame(err error) bool {
	var e *model.Error
	return errors.As(err, &e) && advice(e) == model.RetrySameOperation
}

// transientUnit reports a unit of work that lost to a concurrent unit:
// model.Transient with same-operation retry advice.
func transientUnit(err error) bool {
	var e *model.Error
	return errors.As(err, &e) && e.Code == model.Transient && advice(e) == model.RetrySameOperation
}
