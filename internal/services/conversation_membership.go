package services

import (
	"context"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// AddParticipant queues adding an identity to a conversation at the
// provider as an outbound operation; operations.ChangeMembership holds its
// rules.
func (s Conversations) AddParticipant(ctx context.Context, req model.ParticipantChange) (model.Acceptance, error) {
	return operations.ChangeMembership(ctx, s.c, req, false)
}

// RemoveParticipant queues removing a participant from a conversation at
// the provider, as AddParticipant.
func (s Conversations) RemoveParticipant(ctx context.Context, req model.ParticipantChange) (model.Acceptance, error) {
	return operations.ChangeMembership(ctx, s.c, req, true)
}
