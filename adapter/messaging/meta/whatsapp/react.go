package whatsapp

import (
	"context"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// reactionBody is the Cloud API reaction object: the message reacted to
// and the emoji. An empty emoji removes the reaction; it is always sent.
type reactionBody struct {
	MessageID string `json:"message_id"`
	Emoji     string `json:"emoji"`
}

// React sends a reaction message to the effect's user route, reacting to
// the provider message the target names: a message the user sent or one
// the business sent. The emoji is sent as given, byte for byte. A removal
// sends an empty emoji whatever emoji the request names: a user holds one
// reaction per message (adapter.ReactionsSingle), so there is only one to
// remove.
//
// A reaction message is subject to the customer service window like a
// free-form message: one the instance knows to be outside it is rejected
// without a call, and otherwise Meta decides. The outcome is
// provider_accepted with the reaction message's wamid as reference, which
// the engine keeps on the reaction, never on the target. As for a send,
// the attempt rides in biz_opaque_callback_data, so a status webhook for
// the reaction message settles a lost answer; there is no reconciler.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	if err := x.ready(ctx); err != nil {
		return adapter.Outcome{}, err
	}
	route, err := x.userRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	t := r.Target
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || t.ID == "" {
		return adapter.Outcome{}, &model.Error{Code: model.Validation, SafeMessage: "target is not a whatsapp message of this channel", Retry: model.NoRetry}
	}
	rx := &reactionBody{MessageID: t.ID}
	if !r.Remove {
		rx.Emoji = r.Emoji
	}
	body := sendBody{
		MessagingProduct: "whatsapp", RecipientType: "individual", To: route.ID, Type: "reaction",
		Reaction: rx, CallbackData: CallbackData(r.Effect),
	}
	return x.post(ctx, route, body, true)
}

var _ adapter.Reactor = (*Instance)(nil)
