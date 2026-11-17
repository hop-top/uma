package engine

import (
	"context"

	"hop.top/uma/ports"
)

// Observe hands ob to the host's Observer, if one is configured. The call is
// synchronous: ports.Observer requires implementations not to block.
func (e *Engine) Observe(ctx context.Context, ob ports.Observation) {
	if e.deps.observer != nil {
		e.deps.observer.Observe(ctx, ob)
	}
}

// Observing reports whether an Observer is configured, so callers skip
// building observations nobody receives.
func (e *Engine) Observing() bool { return e.deps.observer != nil }
