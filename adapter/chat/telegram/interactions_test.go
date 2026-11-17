package telegram

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// callback is a callback_query update: a button of an inline keyboard of
// message pressed by from (https://core.telegram.org/bots/api#callbackquery).
func callback(updateID int64, queryID string, from obj, message obj, data string) obj {
	q := obj{"id": queryID, "from": from, "chat_instance": "ci-1"}
	if message != nil {
		q["message"] = message
	}
	if data != "" {
		q["data"] = data
	}
	return updateOf(updateID, "callback_query", q)
}

func botMessage(chat obj, id int64, extra obj) obj {
	m := textMessage(chat, obj{"id": testBotID, "is_bot": true, "first_name": "uma", "username": testBotName}, id, "Approve?", obj{"reply_markup": obj{"inline_keyboard": []any{}}})
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func interactionOf(t *testing.T, got []model.Envelope) model.InteractionInput {
	t.Helper()
	if len(got) != 1 || got[0].Kind != model.EnvelopeInteraction || got[0].Interaction == nil {
		t.Fatalf("envelopes %+v, want one interaction", got)
	}
	return *got[0].Interaction
}

func TestCallbackQueryIsAnAction(t *testing.T) {
	sink := &sinkRecorder{}
	rec := jsonPost(callback(40, "cbq-1", person(testPerson), botMessage(groupChat(), 77, obj{"message_thread_id": 4}), "approve:42")).send(open(t, testConfig()).Handler(sink))
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
	}
	got := sink.envelopes()
	in := interactionOf(t, got)
	if in.Kind != model.InteractionAction || in.Action != "approve:42" || in.Arguments != "" || in.ID != "callback:40" {
		t.Errorf("interaction %+v, want the action approve:42 with ID callback:40", in)
	}
	if in.Actor != (model.ExternalRef{Channel: testChannel, Namespace: UserNamespace, Kind: RefKindUser, ID: "424242"}) {
		t.Errorf("actor %+v", in.Actor)
	}
	if in.Bot == nil || in.Bot.ID != "7000001" {
		t.Errorf("bot %+v", in.Bot)
	}
	if in.Message == nil || *in.Message != model.ProviderMessageRef(testChannel, "-1001234567890:77") {
		t.Errorf("message %+v, want the message the button sits on", in.Message)
	}
	if len(in.Context) != 2 || in.Context[0].Kind != model.RefKindRoom || in.Context[1].Kind != model.RefKindThread || in.Context[1].ID != "4" {
		t.Errorf("context %+v, want the chat and its topic", in.Context)
	}
	// The query ID is the response credential, good for AnswerWindow from
	// when the adapter read the update.
	if in.ResponseToken != "cbq-1" || !in.ResponseExpiresAt.Equal(testNow.Add(AnswerWindow)) || !in.Deadline.Equal(testNow.Add(AckDeadline)) {
		t.Errorf("token %q expires %v deadline %v", in.ResponseToken, in.ResponseExpiresAt, in.Deadline)
	}
	e := got[0]
	if e.EventID != in.ID || e.Sender != in.Actor || e.Destination != "7000001" || !e.OccurredAt.Equal(testNow) || len(e.References) != 2 || e.Assurance != "verified" {
		t.Errorf("envelope %+v", e)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("interaction invalid: %v", err)
	}
}

// The actor is the user Telegram says pressed the button (the update's
// "from"), never a field the data claims.
func TestCallbackActorIsTheSignedFrom(t *testing.T) {
	_, got := serve(t, testConfig(), callback(41, "cbq-2", person(555), botMessage(privateChat(555), 5, nil), `{"user":"424242","actor":"someone-else"}`))
	in := interactionOf(t, got)
	if in.Actor.ID != "555" || in.Action != `{"user":"424242","actor":"someone-else"}` {
		t.Errorf("actor %+v action %q", in.Actor, in.Action)
	}
}

func TestCallbackRedeliveryKeepsTheID(t *testing.T) {
	sink := &sinkRecorder{}
	for range 2 {
		serve(t, testConfig(), callback(42, "cbq-3", person(testPerson), botMessage(groupChat(), 5, nil), "x"), sink)
	}
	got := sink.envelopes()
	if len(got) != 2 || got[0].EventID != got[1].EventID || got[0].Interaction.ID != got[1].Interaction.ID {
		t.Fatalf("%+v", got)
	}
}

func TestCallbackThatCannotBeScopedIsAcknowledgedAndDropped(t *testing.T) {
	for name, u := range map[string]obj{
		// A message sent through inline mode has no chat.
		"inline message": updateOf(43, "callback_query", obj{"id": "q", "from": person(testPerson), "inline_message_id": "abc", "chat_instance": "c", "data": "x"}),
		"no message":     callback(44, "q", person(testPerson), nil, "x"),
		// A game button has a game_short_name and no data.
		"game":    updateOf(45, "callback_query", obj{"id": "q", "from": person(testPerson), "message": botMessage(groupChat(), 5, nil), "game_short_name": "snake", "chat_instance": "c"}),
		"no data": callback(46, "q", person(testPerson), botMessage(groupChat(), 5, nil), ""),
	} {
		t.Run(name, func(t *testing.T) {
			resp, got := serve(t, testConfig(), u)
			if resp.StatusCode != http.StatusOK || len(got) != 0 {
				t.Fatalf("status %d, %d envelopes, want an acknowledged nothing", resp.StatusCode, len(got))
			}
		})
	}
	// A message the bot can no longer read is still named by chat and ID.
	inaccessible := obj{"message_id": 9, "chat": groupChat(), "date": 0}
	_, got := serve(t, testConfig(), callback(47, "q", person(testPerson), inaccessible, "x"))
	if in := interactionOf(t, got); in.Message == nil || in.Message.ID != "-1001234567890:9" {
		t.Errorf("interaction %+v", in)
	}
}

// An interaction is recorded before it is acknowledged: a sink that fails
// gets no 200.
func TestInteractionIsAcknowledgedOnlyAfterTheSinkAccepted(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{model.NewError(model.Unavailable, "down"), http.StatusServiceUnavailable},
		{model.NewError(model.Validation, "no inbox"), http.StatusUnprocessableEntity},
	} {
		sink := &sinkRecorder{fail: func(model.Ingress) error { return tc.err }}
		resp, _ := serve(t, testConfig(), callback(48, "q", person(testPerson), botMessage(groupChat(), 5, nil), "x"), sink)
		if resp.StatusCode != tc.want {
			t.Errorf("sink error %v: status %d, want %d", tc.err, resp.StatusCode, tc.want)
		}
	}
}

func commandMessage(chat obj, from obj, id int64, text string, extra obj) obj {
	cmdLen := len([]rune(strings.Fields(text)[0]))
	m := textMessage(chat, from, id, text, obj{"entities": []obj{{"type": "bot_command", "offset": 0, "length": cmdLen}}})
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestBotCommandsAreInteractionsNotMessages(t *testing.T) {
	for name, tc := range map[string]struct {
		chat    obj
		text    string
		command string
		args    string
	}{
		"start with a payload":  {privateChat(testPerson), "/start promo-42", "start", "promo-42"},
		"no arguments":          {privateChat(testPerson), "/help", "help", ""},
		"several words":         {privateChat(testPerson), "/echo  hello   world ", "echo", "hello   world"},
		"in a group":            {groupChat(), "/status@uma_test_bot now", "status", "now"},
		"suffix in capitals":    {groupChat(), "/status@UMA_TEST_BOT", "status", ""},
		"unsuffixed in a group": {groupChat(), "/status", "status", ""},
		"underscores":           {privateChat(testPerson), "/set_name Pat", "set_name", "Pat"},
	} {
		t.Run(name, func(t *testing.T) {
			_, got := serve(t, testConfig(), updateOf(50, "message", commandMessage(tc.chat, person(testPerson), 12, tc.text, nil)))
			in := interactionOf(t, got)
			if in.Kind != model.InteractionCommand || in.Command != tc.command || in.Arguments != tc.args {
				t.Errorf("interaction %+v, want command %q arguments %q", in, tc.command, tc.args)
			}
			// A command carries no credential: Telegram gives none.
			if in.ResponseToken != "" || !in.ResponseExpiresAt.IsZero() || in.Message != nil {
				t.Errorf("token %q expires %v message %v, want none", in.ResponseToken, in.ResponseExpiresAt, in.Message)
			}
			if want := "command:" + idString(tc.chat["id"].(int64)) + ":12"; in.ID != want || got[0].EventID != want {
				t.Errorf("interaction ID %q event %q, want %q", in.ID, got[0].EventID, want)
			}
			if in.Actor.ID != "424242" || len(in.Context) == 0 || in.Context[0].Kind != model.RefKindRoom {
				t.Errorf("actor %+v context %+v", in.Actor, in.Context)
			}
			if err := in.Validate(); err != nil {
				t.Errorf("invalid: %v", err)
			}
		})
	}
}

func TestCommandIDNamesTheMessage(t *testing.T) {
	_, a := serve(t, testConfig(), updateOf(50, "message", commandMessage(privateChat(testPerson), person(testPerson), 12, "/start", nil)))
	_, b := serve(t, testConfig(), updateOf(51, "message", commandMessage(privateChat(testPerson), person(testPerson), 12, "/start", nil)))
	_, c := serve(t, testConfig(), updateOf(52, "message", commandMessage(privateChat(testPerson), person(testPerson), 13, "/start", nil)))
	ia, ib, ic := interactionOf(t, a), interactionOf(t, b), interactionOf(t, c)
	if ia.ID != "command:424242:12" || ia.ID != ib.ID || ia.ID == ic.ID {
		t.Errorf("IDs %q %q %q: one message is one command, whichever update delivers it", ia.ID, ib.ID, ic.ID)
	}
}

func TestACommandForAnotherBotIsNotOurs(t *testing.T) {
	cfg := testConfig()
	for _, mentionsOnly := range []bool{false, true} {
		cfg.MentionsOnly = mentionsOnly
		resp, got := serve(t, cfg, updateOf(53, "message", commandMessage(groupChat(), person(testPerson), 12, "/status@other_bot", nil)))
		if resp.StatusCode != http.StatusOK || len(got) != 0 {
			t.Errorf("mentions_only %v: status %d, %d envelopes: a command for another bot is not reported", mentionsOnly, resp.StatusCode, len(got))
		}
	}
}

// Telegram delivers an unaddressed command to a bot in privacy mode only
// when it means that bot, so mentions_only does not drop one; a plain
// message that does not address the bot it still drops.
func TestMentionsOnlyKeepsCommands(t *testing.T) {
	cfg := testConfig()
	cfg.MentionsOnly = true
	_, got := serve(t, cfg, updateOf(54, "message", commandMessage(groupChat(), person(testPerson), 12, "/status", nil)))
	if in := interactionOf(t, got); in.Command != "status" {
		t.Errorf("interaction %+v", in)
	}
	if _, got = serve(t, cfg, updateOf(55, "message", textMessage(groupChat(), person(testPerson), 13, "status", nil))); len(got) != 0 {
		t.Errorf("a plain message was reported: %+v", got)
	}
}

func TestOnlyALeadingCommandIsACommand(t *testing.T) {
	m := textMessage(privateChat(testPerson), person(testPerson), 12, "please /start now", obj{"entities": []obj{{"type": "bot_command", "offset": 7, "length": 6}}})
	_, got := serve(t, testConfig(), updateOf(56, "message", m))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessage {
		t.Fatalf("got %+v, want an ordinary message", got)
	}
	// A command typed without its entity (a client that did not make one) is text.
	_, got = serve(t, testConfig(), updateOf(57, "message", textMessage(privateChat(testPerson), person(testPerson), 13, "/start", nil)))
	if len(got) != 1 || got[0].Kind != model.EnvelopeMessage {
		t.Fatalf("got %+v, want an ordinary message", got)
	}
}

func TestCommandInATopicAndFromAChat(t *testing.T) {
	_, got := serve(t, testConfig(), updateOf(58, "message", commandMessage(groupChat(), person(testPerson), 12, "/status", obj{"message_thread_id": 6})))
	if in := interactionOf(t, got); len(in.Context) != 2 || in.Context[1].Kind != model.RefKindThread || in.Context[1].ID != "6" {
		t.Errorf("context %+v, want the topic", in.Context)
	}
	// An anonymous administrator's command is the chat's.
	fake := obj{"id": 1087968824, "is_bot": true, "first_name": "Group", "username": "GroupAnonymousBot"}
	_, got = serve(t, testConfig(), updateOf(59, "message", commandMessage(groupChat(), fake, 14, "/status", obj{"sender_chat": groupChat()})))
	if in := interactionOf(t, got); in.Actor.Kind != RefKindChat || in.Actor.ID != "-1001234567890" {
		t.Errorf("actor %+v", in.Actor)
	}
}

func TestEditedCommandsAreNotRecordedAsMessages(t *testing.T) {
	m := commandMessage(privateChat(testPerson), person(testPerson), 12, "/start again", obj{"edit_date": testNow.Unix() + 9})
	resp, got := serve(t, testConfig(), updateOf(60, "edited_message", m))
	if resp.StatusCode != http.StatusOK || len(got) != 0 {
		t.Errorf("status %d, %d envelopes: an edited command is neither a message nor a second command", resp.StatusCode, len(got))
	}
}

var _ = time.Second
