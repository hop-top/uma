package telegram

import (
	"context"
	"strings"
	"unicode/utf8"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// MaxToastRunes is how much text a callback answer may show: "Text of the
// notification. If not specified, nothing will be shown to the user, 0-200
// characters" (https://core.telegram.org/bots/api#answercallbackquery).
const MaxToastRunes = 200

// Respond answers an interaction. Telegram has no response URL: a response
// is an ordinary call, and the callback query a button press opened is
// answered once, whatever else is done, to clear the client's progress bar.
//
//   - message: a new message to the route, in its topic when it is one, and,
//     for a callback query, the query is then answered with nothing to show;
//     public audience;
//   - ephemeral: the answer of the callback query itself, a notification of at
//     most MaxToastRunes characters at the top of the chat screen that only the
//     person who pressed the button sees; a restricted audience. Markup and
//     keyboards have no place in one;
//   - update: editMessageText on the message the control sits on, which must
//     be the effect's target, then the query is answered; public audience.
//
// A command has no callback query and so no token: only a message answers it.
// A response is rejected before any call when it is a follow-up (a query takes
// one answer; further messages are ordinary replies), the audience does not
// fit the mode, or the window has passed (deadline_expired); Telegram's refusal
// of a query that has become too old is the same. The answer that clears the
// progress bar is made after the message or edit, and is not part of its
// outcome: if it fails, the response still happened. A lost answer is not
// reconciled: Telegram has no lookup to settle it.
func (x *Instance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	if err := x.ready(); err != nil {
		return adapter.Outcome{}, err
	}
	if r.Interaction == nil {
		return adapter.Outcome{}, final(model.Validation, "a response names the interaction it answers")
	}
	ic := r.Interaction
	switch r.Mode {
	case model.ResponseModeMessage, model.ResponseModeEphemeral, model.ResponseModeUpdate:
	default:
		return rejected(model.Unsupported, "Telegram has no such response mode"), nil
	}
	want := model.Public
	if r.Mode == model.ResponseModeEphemeral {
		want = model.Restricted
	}
	switch {
	case r.FollowUp:
		return rejected(model.Unsupported, "Telegram takes one answer to a callback query; send further messages as replies"), nil
	case r.Audience.Visibility != want:
		return rejected(model.Unsupported, "Telegram shows a "+r.Mode+" response to a "+string(want)+" audience only"), nil
	case ic.Token == "" && r.Mode != model.ResponseModeMessage:
		return rejected(model.Unsupported, "Telegram gave this interaction no callback query to answer"), nil
	case ic.Token != "" && !ic.ExpiresAt.IsZero() && !x.now().Before(ic.ExpiresAt):
		return rejected(model.DeadlineExpired, "the interaction's response window has passed"), nil
	}
	switch r.Mode {
	case model.ResponseModeEphemeral:
		return x.answerToast(ctx, ic.Token, r.Content)
	case model.ResponseModeUpdate:
		return x.respondUpdate(ctx, r)
	}
	t, err := x.parseRoute(r.Route)
	if err != nil {
		return adapter.Outcome{}, err
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	out, err := x.postMessage(ctx, &sendArgs{ChatID: t.chat, ThreadID: t.thread, Text: msg.text, ParseMode: msg.parseMode, ReplyMarkup: msg.keyboard})
	x.acknowledge(ctx, ic.Token, out, err)
	return out, err
}

func (x *Instance) respondUpdate(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	ic := r.Interaction
	switch {
	case r.Target == nil:
		return adapter.Outcome{}, final(model.Validation, "an update names the message it replaces")
	case ic.Message == nil || *r.Target != *ic.Message:
		return rejected(model.Validation, "an update replaces the message the control sits on"), nil
	}
	chatID, id, ok := parseMessageID(r.Target.ID)
	if !ok {
		return adapter.Outcome{}, final(model.Validation, "target is not a Telegram message of this channel")
	}
	msg, rerr := render(r.Content)
	if rerr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: rerr}, nil
	}
	out, err := x.editMessage(ctx, chatID, id, msg)
	x.acknowledge(ctx, ic.Token, out, err)
	return out, err
}

// acknowledge answers the callback query with nothing to show, after a
// response that was applied: the client then stops waiting. Its failure
// changes nothing, so it is not reported.
func (x *Instance) acknowledge(ctx context.Context, token string, out adapter.Outcome, err error) {
	if token == "" || err != nil || out.State != model.ProviderAccepted {
		return
	}
	_ = x.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": token}, nil)
}

// answerToast answers the callback query with a notification.
func (x *Instance) answerToast(ctx context.Context, token string, c model.Content) (adapter.Outcome, error) {
	var texts []string
	for _, p := range c.Parts {
		if p.Kind != model.PartText {
			return rejected(model.Unsupported, "a callback answer shows text only"), nil
		}
		texts = append(texts, p.Text)
	}
	text := strings.Join(texts, "\n\n")
	switch {
	case strings.TrimSpace(text) == "":
		return rejected(model.Validation, "content has no text"), nil
	case utf8.RuneCountInString(text) > MaxToastRunes:
		return rejected(model.Validation, "a callback answer shows at most 200 characters"), nil
	}
	if err := x.call(ctx, "answerCallbackQuery", map[string]string{"callback_query_id": token, "text": text}, nil); err != nil {
		return failure(ctx, err, writeCall)
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

var _ adapter.InteractionResponder = (*Instance)(nil)
