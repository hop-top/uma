package store

import (
	"time"

	"hop.top/uma/model"
)

// InteractionRecord is a stored interaction with what never leaves the
// engine: Ingress is the ingress record that created it, so redaction finds
// the raw copy; ResponseToken is the provider response credential and
// TokenExpiresAt its expiry, equal to Interaction.RespondUntil when a token
// is set. The record is the token's only location: the raw ingress copy
// never holds it, and Reader.Interaction and Reader.Interactions never
// return it.
type InteractionRecord struct {
	Interaction    model.Interaction
	Ingress        model.ID
	ResponseToken  string
	TokenExpiresAt time.Time
}

// Clone returns a deep copy.
func (r InteractionRecord) Clone() InteractionRecord {
	r.Interaction = r.Interaction.Clone()
	return r
}

// ResponseToken is the response credential of one interaction as
// Reader.InteractionToken returns it: Token is empty when the provider
// issued none or redaction or the expired-token sweep cleared it.
type ResponseToken struct {
	Token     string
	ExpiresAt time.Time
}

// ResponseClaim records one accepted response of Interaction by Operation
// under the interaction's new Revision: the single initial response when
// FollowUp is false, a follow-up otherwise. See
// Tx.RecordInteractionResponse.
type ResponseClaim struct {
	Interaction model.InteractionRef
	Operation   model.OperationRef
	FollowUp    bool
	Revision    model.Revision
}

// InteractionLink names the local message an interaction's control sits
// on, for an interaction recorded before any message of its inbox held
// the provider message it names (Interaction.MessageExternal), under the
// interaction's new Revision. See Tx.LinkInteraction.
type InteractionLink struct {
	Interaction model.InteractionRef
	Message     model.MessageRef
	Revision    model.Revision
}
