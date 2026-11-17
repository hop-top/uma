package enginetest

import (
	"context"
	"sync/atomic"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// InteractionSpy wraps a store and counts the two interaction writes and
// reads a test must prove were or were not made: token reads and response
// claims.
type InteractionSpy struct {
	store.Store
	tokens, claims atomic.Int64
}

// TokenReads is the number of Reader.InteractionToken calls so far.
func (s *InteractionSpy) TokenReads() int { return int(s.tokens.Load()) }

// Claims is the number of Tx.RecordInteractionResponse calls so far.
func (s *InteractionSpy) Claims() int { return int(s.claims.Load()) }

func (s *InteractionSpy) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(spyReader{r, s}) })
}

func (s *InteractionSpy) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, t, func(tx store.Tx) error { return fn(spyTx{tx, s}) })
}

type spyReader struct {
	store.Reader
	s *InteractionSpy
}

func (r spyReader) InteractionToken(ctx context.Context, ref model.InteractionRef) (store.ResponseToken, error) {
	r.s.tokens.Add(1)
	return r.Reader.InteractionToken(ctx, ref)
}

type spyTx struct {
	store.Tx
	s *InteractionSpy
}

func (t spyTx) InteractionToken(ctx context.Context, ref model.InteractionRef) (store.ResponseToken, error) {
	t.s.tokens.Add(1)
	return t.Tx.InteractionToken(ctx, ref)
}

func (t spyTx) RecordInteractionResponse(ctx context.Context, c store.ResponseClaim) (model.Interaction, error) {
	t.s.claims.Add(1)
	return t.Tx.RecordInteractionResponse(ctx, c)
}

// InteractionToken is the response token SeedInteraction stores.
const InteractionToken = "tok-secret-0001"

// InteractionSeed describes one stored-interaction fixture: an action by
// Actor in conversation Conv of Channel, recorded at Now under Audience.
// RespondUntil is its respond window (zero: none); Tokenless leaves the
// response token out, as a provider without one does.
type InteractionSeed struct {
	Conv         model.ConversationRef
	ID           model.ID
	Actor        model.Identity
	Channel      model.Channel
	Audience     model.Audience
	Now          time.Time
	RespondUntil time.Time
	Tokenless    bool
}

// Record returns the fixture as a store record. Its window and token expiry
// are RespondUntil, its token InteractionToken.
func (s InteractionSeed) Record() store.InteractionRecord {
	msg := model.ExternalRef{Channel: s.Channel.Ref, Namespace: s.Actor.Namespace, Kind: model.RefKindMessage, ID: "pm-1"}
	rec := store.InteractionRecord{
		Interaction: model.Interaction{
			Ref:  model.InteractionRef{Conversation: s.Conv, ID: s.ID},
			Kind: model.InteractionAction, Action: "approve",
			Actor: model.Author{
				Identity: s.Actor.Ref, Kind: s.Actor.Kind,
				External: model.ExternalRef{Channel: s.Channel.Ref, Namespace: s.Actor.Namespace, Kind: string(model.KindIdentity), ID: s.Actor.SourceID},
			},
			MessageExternal: &msg,
			External:        []model.ExternalRef{model.ProviderInteractionRef(s.Channel.Ref, "pi-"+string(s.ID))},
			Audience:        s.Audience, OccurredAt: s.Now, RecordedAt: s.Now,
			Deadline: s.Now.Add(3 * time.Second), RespondUntil: s.RespondUntil,
			Revision: "ix1", Source: s.Channel.Type,
		},
		Ingress: "00000009-0000-7000-8000-000000000001",
	}
	if !s.Tokenless {
		rec.ResponseToken, rec.TokenExpiresAt = InteractionToken, s.RespondUntil
	}
	return rec
}
