package discord

import (
	"context"
	"net/http"
	"net/url"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Respond answers an interaction through its webhook: the application ID
// and the interaction's token, which the engine hands over as the response
// token ("Interaction tokens are valid for 15 minutes"). The bot token is
// never sent there.
//
// The adapter acknowledged the interaction when it recorded it, with a
// deferral (see deferType), so the answer depends on it:
//
//   - a command, or a modal not opened from a message, was deferred with
//     DEFERRED_CHANNEL_MESSAGE_WITH_SOURCE: everyone sees it loading. Its
//     initial response in mode message edits that loading message (Edit
//     Original Interaction Response); mode ephemeral is rejected
//     unsupported, as the deferral fixed the response's visibility ("The
//     ephemeral flag will be ignored, and the value you provided in the
//     initial defer response will be preserved"); mode update has no
//     message to update and is rejected unsupported;
//   - a component, or a modal opened from one, was deferred with
//     DEFERRED_UPDATE_MESSAGE: mode update edits the message the control
//     sits on (the effect's target, which must be it); modes message and
//     ephemeral post a followup message, the latter with the EPHEMERAL flag.
//
// A follow-up posts a followup message, ephemeral in mode ephemeral. A
// followup sent while a deferred command still shows loading "will function
// as Edit Original Interaction Response" and drop the flag, so the adapter
// reads the original response first and rejects the follow-up with
// model.Conflict while it is loading: answer the interaction first.
//
// The window is Discord's: a token past it is rejected with
// deadline_expired before any call. A lost answer is not reconciled.
func (x *Instance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	ic, out, err := x.interactionTarget(r.Effect)
	if err != nil || out.State != "" {
		return out, err
	}
	if want := audienceOf(r.Mode); r.Audience.Visibility != want {
		return rejected(model.Unsupported, "Discord shows a "+r.Mode+" response to a "+string(want)+" audience only"), nil
	}
	deferred := deferType(ic.Kind, ic.Message != nil)
	switch r.Mode {
	case model.ResponseModeUpdate:
		switch {
		case r.Target == nil:
			return adapter.Outcome{}, final(model.Validation, "an update names the message it replaces")
		case deferred != callbackDeferredUpdateMessage:
			return rejected(model.Unsupported, "this interaction has no message to update"), nil
		case ic.Message == nil || *r.Target != *ic.Message:
			return rejected(model.Validation, "an update replaces the message the control sits on"), nil
		}
		return x.hook(ctx, http.MethodPatch, ic.Token, "/messages/@original", r.Content, 0, false)
	case model.ResponseModeMessage, model.ResponseModeEphemeral:
	default:
		return rejected(model.Unsupported, "Discord has no such response mode"), nil
	}
	flags := 0
	if r.Mode == model.ResponseModeEphemeral {
		flags = flagEphemeral
	}
	if !r.FollowUp && deferred == callbackDeferredMessage {
		if flags != 0 {
			return rejected(model.Unsupported, "the interaction was acknowledged publicly; an ephemeral answer is a follow-up after the response"), nil
		}
		return x.hook(ctx, http.MethodPatch, ic.Token, "/messages/@original", r.Content, 0, true)
	}
	return x.followUp(ctx, ic, deferred, r.Content, flags)
}

// SendEphemeral posts a message only the interaction's user sees: a
// followup message with the EPHEMERAL flag ("only the user can see"). Discord
// has ephemeral messages for interactions only: a request naming no
// interaction is rejected unsupported. Core holds the recipient to the
// interaction's actor, the one user Discord shows it to.
func (x *Instance) SendEphemeral(ctx context.Context, r adapter.EphemeralRequest) (adapter.Outcome, error) {
	if r.Interaction == nil {
		if err := x.ready(); err != nil {
			return adapter.Outcome{}, err
		}
		return rejected(model.Unsupported, "Discord shows an ephemeral message only in answer to an interaction"), nil
	}
	ic, out, err := x.interactionTarget(r.Effect)
	if err != nil || out.State != "" {
		return out, err
	}
	if !r.ExpiresAt.IsZero() && !x.now().Before(r.ExpiresAt) {
		return rejected(model.DeadlineExpired, "the ephemeral message expired"), nil
	}
	return x.followUp(ctx, ic, deferType(ic.Kind, ic.Message != nil), r.Content, flagEphemeral)
}

// interactionTarget checks the interaction an effect answers: one with a
// usable token, inside its window.
func (x *Instance) interactionTarget(e adapter.Effect) (*adapter.InteractionContext, adapter.Outcome, error) {
	switch {
	case x.isClosed():
		return nil, adapter.Outcome{}, model.NewError(model.Unavailable, "instance closed")
	case x.http == nil:
		return nil, adapter.Outcome{}, model.NewError(model.Unavailable, "no HTTP client injected")
	case e.Interaction == nil:
		return nil, adapter.Outcome{}, final(model.Validation, "a response names the interaction it answers")
	}
	ic := e.Interaction
	switch {
	case ic.Token == "":
		return nil, rejected(model.Unsupported, "Discord gave this interaction no token"), nil
	case !validToken(ic.Token):
		return nil, rejected(model.Validation, "the response token is not an interaction token"), nil
	case !ic.ExpiresAt.IsZero() && !x.now().Before(ic.ExpiresAt):
		return nil, rejected(model.DeadlineExpired, "the interaction's response window has passed"), nil
	}
	return ic, adapter.Outcome{}, nil
}

func audienceOf(mode string) model.Visibility {
	if mode == model.ResponseModeEphemeral {
		return model.Restricted
	}
	return model.Public
}

// followUp posts a followup message. After a public deferral it first reads
// the original response: one still loading would be edited instead of a
// new message being posted.
func (x *Instance) followUp(ctx context.Context, ic *adapter.InteractionContext, deferred int, c model.Content, flags int) (adapter.Outcome, error) {
	if deferred == callbackDeferredMessage {
		var orig message
		err := x.do(ctx, request{method: http.MethodGet, path: x.hookPath(ic.Token) + "/messages/@original", bare: true}, &orig)
		if err != nil {
			return failure(ctx, err, openCall)
		}
		if orig.Flags&flagLoading != 0 {
			return rejected(model.Conflict, "the interaction still shows loading; answer it before a follow-up"), nil
		}
	}
	return x.hook(ctx, http.MethodPost, ic.Token, "", c, flags, flags == 0)
}

func (x *Instance) hookPath(token string) string {
	return "webhooks/" + x.cfg.ApplicationID + "/" + token
}

// hook calls the interaction's webhook: suffix "" posts a followup (wait
// true, so the message comes back), "/messages/@original" edits the
// original response. named reports the message in the outcome.
func (x *Instance) hook(ctx context.Context, method, token, suffix string, c model.Content, flags int, named bool) (adapter.Outcome, error) {
	m, rerr := render(c)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	body := m.body()
	if flags != 0 {
		body["flags"] = flags
	}
	req := request{method: method, path: x.hookPath(token) + suffix, body: body, bare: true}
	if suffix == "" {
		req.query = url.Values{"wait": {"true"}}
	}
	var got message
	if err := x.do(ctx, req, &got); err != nil {
		return failure(ctx, err, writeCall)
	}
	out := adapter.Outcome{State: model.ProviderAccepted}
	if named && isSnowflake(got.ID) && isSnowflake(got.ChannelID) {
		out.References = []model.ExternalRef{model.ProviderMessageRef(x.channel.Ref, messageID(got.ChannelID, got.ID))}
	}
	return out, nil
}

var (
	_ adapter.InteractionResponder = (*Instance)(nil)
	_ adapter.EphemeralSender      = (*Instance)(nil)
)
