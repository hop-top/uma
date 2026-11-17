package teams

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// AckDeadline is how long Teams waits for the answer to an invoke: "Your
// app has five seconds to respond to the invoke"
// (https://learn.microsoft.com/en-us/microsoftteams/platform/messaging-extensions/how-to/action-commands/respond-to-task-module-submit).
const AckDeadline = 5 * time.Second

// ActionExecuteInvoke is the invoke name of an Adaptive Card
// Action.Execute
// (https://learn.microsoft.com/en-us/adaptive-cards/authoring-cards/universal-action-model).
const ActionExecuteInvoke = "adaptiveCard/action"

// invokeValue is the value of an adaptiveCard/action invoke.
type invokeValue struct {
	Action struct {
		Type string          `json:"type"`
		ID   string          `json:"id"`
		Verb string          `json:"verb"`
		Data json.RawMessage `json:"data"`
	} `json:"action"`
	Trigger string `json:"trigger"`
}

// invokeAck is the answer to an Action.Execute the sink recorded: status
// 200 with a message for the client to show, empty here, which leaves the
// card as it is. The host answers later, by a reply or an edit of the
// card's message; Teams takes no other answer to the invoke itself.
var invokeAck = []byte(`{"statusCode":200,"type":"application/vnd.microsoft.activity.message","value":""}`)

// invoke answers an invoke activity. A user's Action.Execute
// (adaptiveCard/action, trigger manual) is recorded as an action
// interaction, then acknowledged. A refresh the client triggers on its own
// (trigger automatic) asks for a card within five seconds, which nothing in
// uma can give: it is acknowledged the same way and recorded as nothing,
// and the card stays as it is. Any other invoke, such as a dialog's
// task/fetch and task/submit, is answered 501 Not Implemented, as the Bot
// Framework SDK answers an invoke it has no handler for.
func (x *Instance) invoke(ctx context.Context, sink adapter.Sink, act activity) verdict {
	if act.Name != ActionExecuteInvoke {
		return statusAnswer(http.StatusNotImplemented)
	}
	ack := verdict{status: http.StatusOK, body: invokeAck}
	var v invokeValue
	if json.Unmarshal(act.Value, &v) != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	if v.Trigger == "automatic" || x.own(act, act.From.ID) {
		return ack
	}
	d, err := x.actionDelivery(act, v)
	if err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	return x.deliver(ctx, sink, []delivery{d}, ack)
}

// actionDelivery is the interaction envelope of a user's Action.Execute:
// the action is its verb (its id when it has no verb), the arguments its
// data (a string as it is, an object as JSON), and the message the card's, which replyToId names. Teams
// gives no response credential: the interaction has no token.
func (x *Instance) actionDelivery(act activity, v invokeValue) (delivery, error) {
	if !validConversation(act.Conversation.ID) || !validID(act.ID) || !validID(act.From.ID) {
		return delivery{}, malformed("invoke conversation, id or from")
	}
	action := v.Action.Verb
	if action == "" {
		action = v.Action.ID
	}
	if action == "" {
		return delivery{}, malformed("Action.Execute without verb or id")
	}
	at := x.now().UTC()
	in := model.InteractionInput{
		ID: "action:" + messageID(act.Conversation.ID, act.ID), Kind: model.InteractionAction, Action: action,
		Actor: x.userRef(act.From.ID), Deadline: at.Add(AckDeadline), Bot: ptr(x.userRef(x.cfg.botAccount())),
	}
	var str string
	switch {
	case json.Unmarshal(v.Action.Data, &str) == nil:
		// "data: string, object": a string is the arguments as they are.
		in.Arguments = str
	case len(v.Action.Data) > 0 && string(v.Action.Data) != "null":
		in.Arguments = string(v.Action.Data)
	}
	if validID(act.ReplyToID) {
		ref := model.ProviderMessageRef(x.channel.Ref, messageID(act.Conversation.ID, act.ReplyToID))
		in.Message = &ref
	}
	occurred := act.at()
	if occurred.IsZero() {
		occurred = at
	}
	env := model.Envelope{
		EventID: in.ID, Destination: x.cfg.TenantID, Kind: model.EnvelopeInteraction, Sender: in.Actor,
		SenderKind: senderKind(act.From), Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: occurred, Interaction: &in,
		References: x.cardReferences(act),
	}
	in.Context = append(in.Context, env.References...)
	return delivery{ingress: x.ingress(env)}, nil
}

// cardReferences scope an interaction as the card's message is scoped: its
// conversation and, in a channel, the thread the card is in or roots.
func (x *Instance) cardReferences(act activity) []model.ExternalRef {
	refs := []model.ExternalRef{x.roomRef(act.Conversation.ID)}
	if !act.channel() {
		return refs
	}
	root := threadRoot(act.Conversation.ID)
	if root == "" {
		root = act.ReplyToID
	}
	if validID(root) {
		refs = append(refs, x.threadRef(act.Conversation.ID, root))
	}
	return refs
}
