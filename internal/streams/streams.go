// Package streams implements the Streams service: logical streaming
// responses, plus the executor plan that delivers stream operations
// through Runtime.Run.
//
// A stream is one logical response. Start, every update and the end are
// separate commands, each with its own operation ID in the ledger; updates
// carry consecutive sequences and the end names the last one, so a stale or
// reordered command conflicts instead of overwriting. Each command executes
// only after its predecessor's provider attempt has an answer, and an update
// overtaken by a later applied one is never sent. A predecessor whose
// outcome is unknown is waited for only while the runtime will still
// reconcile it, and only until the stream deadline, which then ends the
// stream partial; when nothing can reconcile it (no reconciler, or
// reconcile unavailable for good on the channel) the next update
// supersedes it, and an end whose final update is unknown re-asserts that
// update's content.
package streams

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/route"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Service is the Streams service of one client.
type Service struct{ c *engine.Client }

// New returns the Streams service bound to c.
func New(c *engine.Client) Service { return Service{c} }

// Get returns a stream after authorizing the client's principal to read it
// under the stream's audience. A foreign or unknown stream is not found.
func (s Service) Get(ctx context.Context, req model.Lookup[model.StreamRef]) (model.Stream, error) {
	if s.c.Engine().IsClosed() {
		return model.Stream{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Stream{}, err
	}
	if req.Ref.Scope() != s.c.Principal().Tenant {
		return model.Stream{}, engine.ErrNotFound()
	}
	var st model.Stream
	err := s.c.Engine().Store().View(ctx, req.Ref.Scope(), func(r store.Reader) error {
		var err error
		st, err = r.Stream(ctx, req.Ref)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Stream{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Stream{}, err
	}
	err = s.c.Authorize(ctx, policy.Authorization{
		Action: model.ActionStreamRead, Resource: req.Ref.Ref(),
		Audience: engine.ReadAudience(st.Audience), Revision: st.Revision,
	}, st.Ref.Ref())
	if err != nil {
		return model.Stream{}, err
	}
	return st.Clone(), nil
}

// submit hands s to the operation ledger, masking a not-found store answer.
func submit(ctx context.Context, c *engine.Client, s operations.Submission) (model.Acceptance, error) {
	a, err := operations.Submit(ctx, c, s)
	return a, route.NotFound(err)
}
