package interactions

import (
	"context"

	"hop.top/uma/internal/services"
	"hop.top/uma/model"
)

// Get reads one interaction record the principal may read under its
// audience. It never returns the response token; a foreign or unknown
// interaction is not found.
func (s Service) Get(ctx context.Context, req model.Lookup[model.InteractionRef]) (model.Interaction, error) {
	return services.GetInteraction(ctx, s.c, req)
}

// List pages the interaction records the principal may read, each under its
// own audience, in recording order, optionally limited to Filter.Inboxes,
// Filter.Conversation and Filter.Identity (the actor). Records the principal
// may not read are skipped without trace.
func (s Service) List(ctx context.Context, req model.ListRequest) (model.Page[model.Interaction], error) {
	return services.ListInteractions(ctx, s.c, req)
}
