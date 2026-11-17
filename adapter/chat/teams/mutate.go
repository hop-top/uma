package teams

import (
	"context"
	"net/http"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// messageTarget checks that e names a message of this channel and splits
// its provider message ID. The route is the conversation's reply route; a
// mutation acts on the target alone, which may be any message of the
// channel: Teams answers for what the bot may change.
func (x *Instance) messageTarget(e adapter.Effect) (conversation, activity string, err error) {
	if err := x.ready(); err != nil {
		return "", "", err
	}
	if _, err := x.parseRoute(e.Route); err != nil {
		return "", "", err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a Teams message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || t.ParentID != "" {
		return "", "", bad
	}
	conversation, activity, ok := parseMessageID(t.ID)
	if !ok {
		return "", "", bad
	}
	return conversation, activity, nil
}

// Edit replaces the message the target names with the content, through
// the Connector's update activity (PUT
// /v3/conversations/{conversationId}/activities/{activityId}). The content
// is the whole message. Teams lets a bot update only its own messages ("An
// agent can't update or delete messages sent by the user",
// https://learn.microsoft.com/en-us/microsoftteams/platform/bots/build-conversational-capability);
// its refusal is a final rejection, never a new message.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	conv, id, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Teams messages are public to the conversation"), nil
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	msg.ID = id
	if err := x.call(ctx, http.MethodPut, conversationPath(conv, id), msg, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the message the target names, through the Connector's
// delete activity. A bot deletes only its own messages.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	conv, id, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := x.call(ctx, http.MethodDelete, conversationPath(conv, id), nil, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// React adds the bot's reaction to the message the target names, or
// removes it: PUT or DELETE
// /v3/conversations/{conversationId}/activities/{activityId}/reactions/{type},
// the call the Teams SDK makes for addReaction and deleteReaction
// (https://learn.microsoft.com/en-us/microsoftteams/platform/teams-sdk/in-depth-guides/message-reactions).
// Teams names a reaction by type: an emoji outside the six documented
// types (see reactionTypes) is a final rejection, unsupported, before any
// call. Adding a reaction the bot holds is a no-op at Teams; the outcome
// is applied either way.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	conv, id, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	kind, ok := reactionType(r.Emoji)
	if !ok {
		return rejected(model.Unsupported, "Teams has no reaction type for this emoji"), nil
	}
	method := http.MethodPut
	if r.Remove {
		method = http.MethodDelete
	}
	if err := x.call(ctx, method, conversationPath(conv, id)+"/reactions/"+url.PathEscape(kind), nil, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

var (
	_ adapter.Editor  = (*Instance)(nil)
	_ adapter.Deleter = (*Instance)(nil)
	_ adapter.Reactor = (*Instance)(nil)
)
