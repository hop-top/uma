package telegram

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/testing/xrrtest"
)

// cassettes holds recorded Bot API exchanges. See TestRecordedBot to record
// them.
const cassettes = "testdata/cassettes/telegram"

type botToken string

func (t botToken) Resolve(context.Context, ports.CredentialRequest) (ports.Credential, error) {
	return ports.Secret{Bytes: []byte(t)}, nil
}

// TestRecordedBot replays recorded exchanges with a Telegram bot, in one
// throwaway group: messages sent as text, as HTML and MarkdownV2 with user
// text escaped, with a keyboard and as a reply, edited, reacted to, and
// deleted, and the refusals of an edit and a deletion of a message that is
// gone, and of markup Telegram cannot parse. It skips until cassettes exist.
// To record, against a bot of your own and a group it is a member of
// (docs/recording/telegram.md):
//
//	TELEGRAM_BOT_TOKEN=123456:ABC... TELEGRAM_BOT_USERNAME=my_test_bot TELEGRAM_CHAT_ID=-100... \
//	  XRR_MODE=record go test -count=1 -run TestRecordedBot
//
// The token, which Telegram takes in the request path, the bot's ID and
// username and the chat ID are recorded as placeholders; review the diff of
// testdata/cassettes before committing.
//
// Updates are not recorded: Telegram sends them, and a callback query needs
// a person to press a button.
func TestRecordedBot(t *testing.T) {
	mode := os.Getenv("XRR_MODE")
	if _, err := os.Stat(cassettes); err != nil && mode != string(xrrtest.ModeRecord) {
		t.Skip("no recorded Telegram cassettes; record with XRR_MODE=record and a development bot")
	}
	env := func(k, replay string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		if mode == string(xrrtest.ModeRecord) {
			t.Fatalf("recording needs %s", k)
		}
		return replay
	}
	token := env("TELEGRAM_BOT_TOKEN", "7000001:REPLAY-token")
	username := env("TELEGRAM_BOT_USERNAME", "uma_replay_bot")
	chatText := env("TELEGRAM_CHAT_ID", "-1001234567890")
	chat, err := strconv.ParseInt(chatText, 10, 64)
	if err != nil {
		t.Fatalf("TELEGRAM_CHAT_ID %q: %v", chatText, err)
	}
	botID, err := strconv.ParseInt(strings.SplitN(token, ":", 2)[0], 10, 64)
	if err != nil {
		t.Fatalf("TELEGRAM_BOT_TOKEN has no bot ID before its colon: %v", err)
	}
	// The token comes first: the bot's ID is its prefix.
	client := xrrtest.NewClient(t, cassettes,
		xrrtest.WithPlaceholder(token, "TELEGRAM_BOT_TOKEN"),
		xrrtest.WithPlaceholder(chatText, "TELEGRAM_CHAT_ID"),
		xrrtest.WithPlaceholder(strconv.FormatInt(botID, 10), "TELEGRAM_BOT_ID"),
		xrrtest.WithPlaceholder(username, "TELEGRAM_BOT_USERNAME"),
	)
	cfg := Config{BotID: botID, BotUsername: username}
	inst, err := NewFactory(Options{}).Open(t.Context(), model.Channel{
		Ref: testChannel, Type: Type, Configuration: cfg.Extension(), CredentialRef: "telegram",
	}, adapter.Dependencies{Credentials: botToken(token), HTTP: client, Clock: &fixedClock{testNow}})
	if err != nil {
		t.Fatal(err)
	}
	botScenario(t, inst.(*Instance), chat)
}

// botScenario is what TestRecordedBot records: every call of it must hold
// against Telegram and against the scripted bot alike.
func botScenario(t *testing.T, x *Instance, chat int64) {
	t.Helper()
	ctx := t.Context()
	room := x.roomRef(chat)
	eff := adapter.Effect{
		Operation: model.OperationRef{Tenant: testTenant, ID: testOp}, Attempt: testAttempt,
		Route: room, Audience: model.Audience{Visibility: model.Public},
	}
	send := func(c model.Content, mod ...func(*adapter.SendRequest)) adapter.Outcome {
		t.Helper()
		req := adapter.SendRequest{Effect: eff, Content: c}
		for _, m := range mod {
			m(&req)
		}
		out, err := x.Send(ctx, req)
		if err != nil || out.State != model.ProviderAccepted || len(out.References) != 1 {
			t.Fatalf("Send = %+v, %v", out, err)
		}
		return out
	}
	// Text with every character that means something in a parse mode, sent
	// under none.
	const hostile = "uma <b>recorded</b> & *more* _text_ [x](http://y) `z` ~s~ ||p|| > q #1 +2 -3 =4 {5} .6 !7 \\8"
	plain := send(text(hostile))
	sent := plain.References[0]
	eff.Target = &sent

	// The same text composed into markup, escaped for each mode.
	send(content(markup(ParseModeHTML, "<b>html</b>"), textPart(hostile)))
	send(content(markup(ParseModeMarkdownV2, "*markdownv2*"), textPart(hostile)))
	send(content(textPart("pick one"), keyboard(`{"inline_keyboard":[[{"text":"Approve","callback_data":"approve:1"}]]}`)))
	send(text("a reply"), func(q *adapter.SendRequest) { q.Target = &sent })

	edited := text("uma recorded edit")
	if out, err := x.Edit(ctx, adapter.EditRequest{Effect: eff, Content: edited}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	// To what the message already says: Telegram answers "not modified".
	if out, err := x.Edit(ctx, adapter.EditRequest{Effect: eff, Content: edited}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit to the same text = %+v, %v", out, err)
	}
	for _, step := range []struct {
		emoji  string
		remove bool
	}{{"👍", false}, {"🔥", false}, {"🔥", true}} {
		if out, err := x.React(ctx, adapter.ReactRequest{Effect: eff, Emoji: step.emoji, Remove: step.remove}); err != nil || out.State != model.ProviderAccepted {
			t.Fatalf("React %+v = %+v, %v", step, out, err)
		}
	}
	if out, err := x.Delete(ctx, adapter.DeleteRequest{Effect: eff}); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Delete = %+v, %v", out, err)
	}
	// The message is gone: Telegram refuses, and the refusal is final.
	if out, err := x.Delete(ctx, adapter.DeleteRequest{Effect: eff}); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("Delete of a deleted message = %+v, %v; want a rejection, not_found", out, err)
	}
	if out, err := x.Edit(ctx, adapter.EditRequest{Effect: eff, Content: text("too late")}); err != nil || out.State != model.Rejected || out.Failure.Code != model.NotFound {
		t.Fatalf("Edit of a deleted message = %+v, %v; want a rejection, not_found", out, err)
	}
	// Markup Telegram cannot parse.
	out, err := x.Send(ctx, adapter.SendRequest{Effect: adapter.Effect{Operation: eff.Operation, Attempt: eff.Attempt, Route: room, Audience: eff.Audience}, Content: content(markup(ParseModeHTML, "<blink>x</blink>"))})
	if err != nil || out.State != model.Rejected || out.Failure.Code != model.Validation {
		t.Fatalf("Send of unparsable markup = %+v, %v; want a rejection, validation", out, err)
	}
}

// The scenario holds against the scripted bot too, so a recording that fails
// is Telegram's answer, not a slip of the scenario.
func TestRecordedScenarioAgainstTheScriptedBot(t *testing.T) {
	r := newRig(t)
	botScenario(t, r.x, testGroup)
}
