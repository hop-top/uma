// Package examples contains compile-only adopter examples; do not treat them as a running uma host.
package examples

import (
	"context"
	"net/http"
	"regexp"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// MinimalBot answers every inbound message through the bot companion, which
// reads the message, skips the bot's own echoes, replies under operation IDs
// derived from the event and acknowledges each batch after its replies.
// Host supplies an engine, authenticated bot principal, and previously opened subscription.
func MinimalBot(ctx context.Context, engine uma.Engine, principal model.Principal,
	sub model.SubscriptionRef) error {
	client, err := engine.For(principal)
	if err != nil {
		return err
	}
	receipt := func(ctx context.Context, t *bot.Turn) error {
		_, err := t.Reply(ctx, model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "Received."}}})
		return err
	}
	b, err := bot.New(bot.Config{
		Client: client, Self: principal.Actor, Subscription: sub, Wait: 10 * time.Second,
		// The empty expression matches every message.
		Patterns: []bot.Pattern{{Name: "receipt", Match: regexp.MustCompile(""), Handle: receipt}},
	})
	if err != nil {
		return err
	}
	return b.Run(ctx)
}

// DurableInbox composes an injected durable backend; this artifact ships no actual backend.
// The returned handler serves the channel's webhook; the host owns workers and port binding.
func DurableInbox(ctx context.Context, config uma.Config, service model.Principal,
	channel model.ChannelRef) (uma.Engine, http.Handler, error) {
	config.RequireDurable = true
	engine, err := uma.New(config)
	if err != nil {
		return nil, nil, err
	}
	handler, err := engine.Runtime().Webhook(ctx, uma.RuntimeCall{
		Principal: service, Call: model.Call{Correlation: "mount-webhook"},
	}, channel)
	if err != nil {
		_ = engine.Close(ctx)
		return nil, nil, err
	}
	return engine, handler, nil
}

func ListInbox(ctx context.Context, engine uma.Engine, viewer model.Principal,
	inbox model.InboxRef, cursor model.Cursor) (model.Page[model.Conversation], error) {
	client, err := engine.For(viewer)
	if err != nil {
		return model.Page[model.Conversation]{}, err
	}
	return client.Conversations().List(ctx, model.ListRequest{
		Call:   model.Call{Correlation: "inbox-page"},
		Filter: model.Filter{Inboxes: []model.InboxRef{inbox}},
		Page:   model.PageRequest{Limit: 50, After: cursor},
	})
}
