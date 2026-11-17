package slack

import (
	"context"
	"encoding/json"
	"errors"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// messageTarget checks that e names a message of this channel and splits
// its provider message ID. The route is the conversation's reply route; a
// mutation acts on the target alone, which may be any message of the
// channel: Slack answers for what the bot may change.
func (x *Instance) messageTarget(e adapter.Effect) (conversation, ts string, err error) {
	if err := x.ready(); err != nil {
		return "", "", err
	}
	if _, err := x.parseRoute(e.Route); err != nil {
		return "", "", err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a Slack message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || t.ParentID != "" {
		return "", "", bad
	}
	conversation, ts, ok := parseMessageID(t.ID)
	if !ok {
		return "", "", bad
	}
	return conversation, ts, nil
}

type updateArgs struct {
	Channel  string          `json:"channel"`
	TS       string          `json:"ts"`
	Text     string          `json:"text,omitempty"`
	Blocks   json.RawMessage `json:"blocks,omitempty"`
	Metadata *metadata       `json:"metadata,omitempty"`
}

// Edit replaces the message the target names with the content, through
// chat.update. The content is the whole message: text without blocks drops
// the message's old blocks ("the message's previous blocks will only be
// retained if the text argument is not provided",
// https://docs.slack.dev/reference/methods/chat.update/). Only a message
// the bot posted can be updated; Slack's refusal is a final rejection,
// never a new message. The method's reference tells a bot token to pass
// as_user when it edits its own message; that is a classic app's rule
// (workspace apps get as_user_not_supported), so the adapter does not send
// it.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	conv, ts, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Slack messages are public to the conversation"), nil
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	args := updateArgs{Channel: conv, TS: ts, Text: msg.text, Blocks: msg.blocks, Metadata: x.marker(r.Effect)}
	err = x.call(ctx, "chat.update", &args, nil)
	if args.Metadata != nil && x.metadataRefused(err) {
		args.Metadata = nil
		err = x.call(ctx, "chat.update", &args, nil)
	}
	if err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the message the target names, through chat.delete. A bot
// token deletes only messages its bot posted.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	conv, ts, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := x.call(ctx, "chat.delete", map[string]string{"channel": conv, "ts": ts}, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// React adds the bot's reaction to the message the target names, or
// removes it, through reactions.add and reactions.remove. Slack names a
// reaction, not an emoji: an emoji outside the adapter's table (see
// reactionNames) is a final rejection, unsupported, before any call.
// Adding what the bot already holds and removing what it does not are
// what was asked: Slack answers already_reacted and no_reaction, and the
// outcome is applied. A user may hold several reactions on a message.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	conv, ts, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	name, ok := reactionName(r.Emoji)
	if !ok {
		return rejected(model.Unsupported, "Slack has no reaction name for this emoji"), nil
	}
	method, already := "reactions.add", "already_reacted"
	if r.Remove {
		method, already = "reactions.remove", "no_reaction"
	}
	err = x.call(ctx, method, map[string]string{"channel": conv, "timestamp": ts, "name": name}, nil)
	if ae, ok := errors.AsType[*apiError](err); ok && ae.code == already {
		err = nil
	}
	if err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

var (
	_ adapter.Editor  = (*Instance)(nil)
	_ adapter.Deleter = (*Instance)(nil)
	_ adapter.Reactor = (*Instance)(nil)
)
