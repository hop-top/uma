package ingress

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
)

// newID allocates an internal ID outside a unit of work. A generator
// failure is transient; a malformed value is permanent.
func newID(ctx context.Context, e *engine.Engine) (model.ID, error) {
	id, err := e.IDs().New(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", model.NewError(model.Transient, "ID allocation failed")
	}
	if err := id.Validate(); err != nil {
		return "", model.NewError(model.Permanent, "ID generator returned a malformed ID")
	}
	return id, nil
}
