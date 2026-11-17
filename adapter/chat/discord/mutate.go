package discord

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// messageTarget checks that e names a message of this channel and splits
// its provider message ID. The route is the conversation's reply route; a
// mutation acts on the target alone, which may be any message of the
// channel: Discord answers for what the bot may change.
func (x *Instance) messageTarget(e adapter.Effect) (channel, msg string, err error) {
	if err := x.ready(); err != nil {
		return "", "", err
	}
	if _, err := x.parseRoute(e.Route); err != nil {
		return "", "", err
	}
	t := e.Target
	bad := final(model.Validation, "target is not a Discord message of this channel")
	if t == nil || t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || t.ParentID != "" {
		return "", "", bad
	}
	channel, msg, ok := parseMessageID(t.ID)
	if !ok {
		return "", "", bad
	}
	return channel, msg, nil
}

// Edit replaces the message the target names with the content, through
// Edit Message. Only the author edits content ("The original author can
// edit content, embeds, flags and components"), so another author's message
// is refused (50005) as a final rejection, never a new message. The content
// is the whole message: components it does not carry are removed.
func (x *Instance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	channel, msg, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if r.Audience.Visibility != model.Public {
		return rejected(model.Unsupported, "Discord messages are public to the channel"), nil
	}
	m, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	body := m.body()
	if m.components == nil {
		body["components"] = []any{}
	}
	if err := x.call(ctx, http.MethodPatch, "channels/"+channel+"/messages/"+msg, body, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// Delete deletes the message the target names. Another author's message in
// a guild needs MANAGE_MESSAGES; Discord's refusal is final.
func (x *Instance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	channel, msg, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if err := x.call(ctx, http.MethodDelete, "channels/"+channel+"/messages/"+msg, nil, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// React adds the bot's reaction to the message the target names, or removes
// it: Create Reaction and Delete Own Reaction, the emoji URL-encoded ("or
// the request will fail with 10014: Unknown Emoji"). A Unicode emoji
// Discord does not know is that refusal, rejected unsupported. A reactor
// holds one reaction per emoji, several emoji at once.
func (x *Instance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	channel, msg, err := x.messageTarget(r.Effect)
	if err != nil {
		return adapter.Outcome{}, err
	}
	if model.ValidateEmoji(r.Emoji) != nil {
		return rejected(model.Validation, "not an emoji"), nil
	}
	method := http.MethodPut
	if r.Remove {
		method = http.MethodDelete
	}
	if err := x.call(ctx, method, reactionPath(channel, msg, r.Emoji)+"/@me", nil, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

func reactionPath(channel, msg, emoji string) string {
	return "channels/" + channel + "/messages/" + msg + "/reactions/" + url.PathEscape(emoji)
}

// Signal shows the bot typing in the route's channel for Discord's ten
// seconds (Trigger Typing Indicator; "Generally bots should not use this
// route", except while a reply takes a few seconds). Kind "typing" only.
func (x *Instance) Signal(ctx context.Context, r adapter.SignalRequest) error {
	if err := x.ready(); err != nil {
		return err
	}
	if r.Kind != "typing" {
		return final(model.Unsupported, "Discord signals typing only")
	}
	if !r.ExpiresAt.IsZero() && !x.now().Before(r.ExpiresAt) {
		return final(model.DeadlineExpired, "the signal expired")
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return err
	}
	channel, out, err := x.channelOf(ctx, t)
	if err != nil {
		return err
	}
	if out.Failure != nil {
		return out.Failure
	}
	if err := x.call(ctx, http.MethodPost, "channels/"+channel+"/typing", nil, nil); err != nil {
		_, ferr := failure(ctx, err, readCall)
		return ferr
	}
	return nil
}

// TypingDuration is how long Discord shows a typing indicator.
const TypingDuration = 10 * time.Second

// ChangeMembership adds the participant to the thread the route names, or
// removes it: Add Thread Member and Remove Thread Member, 204 whether or
// not the user was a member. Only a thread has members the bot can change:
// a room or a direct message route is rejected unsupported. Removal needs
// MANAGE_THREADS (or a private thread the bot created) and an archived
// thread refuses both; Discord's refusal is final.
func (x *Instance) ChangeMembership(ctx context.Context, r adapter.MembershipRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	p := r.Participant
	if p.Channel != x.channel.Ref || p.Namespace != UserNamespace || !isSnowflake(p.ID) {
		return adapter.Outcome{}, final(model.Validation, "participant is not a Discord user of this channel")
	}
	if t.parent == "" {
		return rejected(model.Unsupported, "only a Discord thread has members the bot can change"), nil
	}
	method := http.MethodPut
	if r.Remove {
		method = http.MethodDelete
	}
	if err := x.call(ctx, method, "channels/"+t.channel+"/thread-members/"+p.ID, nil, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

var (
	_ adapter.Editor            = (*Instance)(nil)
	_ adapter.Deleter           = (*Instance)(nil)
	_ adapter.Reactor           = (*Instance)(nil)
	_ adapter.Signaler          = (*Instance)(nil)
	_ adapter.MembershipChanger = (*Instance)(nil)
)
