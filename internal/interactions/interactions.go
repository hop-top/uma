// Package interactions implements the Interactions service: reading the
// interaction records ingress stores, answering them, recipient-bound
// ephemeral replies and expiring operational signals, plus the executor
// plans that deliver responses and ephemeral operations through
// Runtime.Run. Stream lifecycle is the Streams service (package streams).
//
// A response (Respond) is an outbound operation on the interaction's
// conversation route. Its initial response is claimed atomically with the
// operation, so one wins; follow-ups need the adapter's respond_follow_up.
// The provider's response token lives in the interaction's storage and is
// read only by the effector at the attempt, into the adapter's effect; it
// is never in the operation, its evidence, an event or an error. An
// attempt past the interaction's respond window, or after its redaction,
// makes no provider call.
//
// An ephemeral reply is an outbound operation to one stored participant of
// the conversation. It is never stored as a message, never falls back to a
// public reply or a direct message, and is not attempted after its
// deadline. Naming an interaction it is that interaction's follow-up
// response to its actor; naming none it reads no interaction state.
//
// A signal, such as typing, is sent once, synchronously, before it expires.
// It creates no operation, event or message and is never replayed, and
// reads no interaction state.
package interactions

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
)

// Service is the Interactions service of one client.
type Service struct{ c *engine.Client }

// New returns the Interactions service bound to c.
func New(c *engine.Client) Service { return Service{c} }

// submit hands s to the operation ledger, masking a not-found store answer.
func submit(ctx context.Context, c *engine.Client, s operations.Submission) (model.Acceptance, error) {
	a, err := operations.Submit(ctx, c, s)
	return a, route.NotFound(err)
}

func expiredError() error {
	return &model.Error{Code: model.DeadlineExpired, SafeMessage: "deadline already passed", Retry: model.NoRetry}
}
