package slack

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

func serve(t *testing.T, x *Instance, sink *sinkRecorder, r request) *httpResult {
	t.Helper()
	rec := r.send(x.Handler(sink))
	return &httpResult{Code: rec.Code, Body: rec.Body.String(), Header: rec.Header()}
}

type httpResult struct {
	Code   int
	Body   string
	Header http.Header
}

func wantNone(t *testing.T, s *sinkRecorder) {
	t.Helper()
	if got := s.envelopes(); len(got) != 0 {
		t.Fatalf("sink accepted %+v, want nothing", got)
	}
}

func wantOne(t *testing.T, s *sinkRecorder) model.Envelope {
	t.Helper()
	got := s.envelopes()
	if len(got) != 1 {
		t.Fatalf("sink accepted %d envelopes %+v, want 1", len(got), got)
	}
	return got[0]
}

func payloadOf(t *testing.T, e model.Envelope) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload.Data, &m); err != nil {
		t.Fatalf("payload %q: %v", e.Payload.Data, err)
	}
	return m
}

func textOf(e model.Envelope) string {
	if len(e.Content.Parts) != 1 {
		return ""
	}
	return e.Content.Parts[0].Text
}

// url_verification: https://docs.slack.dev/reference/events/url_verification/
// asks for the challenge back, after the request's origin is verified.
func TestURLVerificationEchoesTheChallengeOnlyWhenSigned(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{}
	body := map[string]any{"type": "url_verification", "token": "deprecated", "challenge": "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P"}

	res := serve(t, x, s, jsonRequest(body))
	if res.Code != http.StatusOK || res.Body != "3eZbrw1aBm2rZgRNFdxV2595E9CY3gmdALWMmHkvFXO7tYXAYM8P" {
		t.Fatalf("answer = %d %q, want 200 and the challenge", res.Code, res.Body)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type %q, want text/plain", ct)
	}
	wantNone(t, s)

	forged := jsonRequest(body)
	forged.secret = "not-the-secret"
	if res := serve(t, x, s, forged); res.Code != http.StatusUnauthorized || strings.Contains(res.Body, "3eZb") {
		t.Fatalf("forged verification answered %d %q, want 401 without the challenge", res.Code, res.Body)
	}
	stale := jsonRequest(body)
	stale.at = testNow.Add(-ReplayWindow - time.Second)
	if res := serve(t, x, s, stale); res.Code != http.StatusUnauthorized {
		t.Fatalf("replayed verification answered %d, want 401", res.Code)
	}
	noChallenge := jsonRequest(map[string]any{"type": "url_verification"})
	if res := serve(t, x, s, noChallenge); res.Code != http.StatusBadRequest {
		t.Fatalf("verification without a challenge answered %d, want 400", res.Code)
	}
}

func TestHandlerRefusesBeforeReadingAnything(t *testing.T) {
	good := jsonRequest(callback("Ev1", messageEvent(testPerson, "hi", testTS, nil)))
	for name, tc := range map[string]struct {
		req  func(request) request
		mod  func(*credentials)
		want int
	}{
		"GET":                     {func(r request) request { r.method = http.MethodGet; return r }, nil, http.StatusMethodNotAllowed},
		"unsigned":                {func(r request) request { r.headers = map[string]string{HeaderSignature: ""}; return r }, nil, http.StatusUnauthorized},
		"no timestamp":            {func(r request) request { r.headers = map[string]string{HeaderTimestamp: ""}; return r }, nil, http.StatusUnauthorized},
		"signed with another key": {func(r request) request { r.secret = "other"; return r }, nil, http.StatusUnauthorized},
		"replayed":                {func(r request) request { r.at = testNow.Add(-time.Hour); return r }, nil, http.StatusUnauthorized},
		"no signing secret":       {nil, func(c *credentials) { c.secret = "" }, http.StatusServiceUnavailable},
		"credential port failing": {nil, func(c *credentials) { c.err = model.NewError(model.Unavailable, "down") }, http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			creds := newCredentials()
			if tc.mod != nil {
				tc.mod(creds)
			}
			x := open(t, testConfig(), func(d *adapterDeps) { d.Credentials = creds })
			s := &sinkRecorder{}
			r := good
			if tc.req != nil {
				r = tc.req(good)
			}
			if res := serve(t, x, s, r); res.Code != tc.want {
				t.Fatalf("answered %d, want %d", res.Code, tc.want)
			}
			wantNone(t, s)
		})
	}
}

func TestClosedInstanceAnswers503(t *testing.T) {
	x := open(t, testConfig())
	_ = x.Close(t.Context())
	s := &sinkRecorder{}
	if res := serve(t, x, s, jsonRequest(callback("Ev1", messageEvent(testPerson, "hi", testTS, nil)))); res.Code != http.StatusServiceUnavailable {
		t.Fatalf("answered %d, want 503", res.Code)
	}
	wantNone(t, s)
}

func TestMalformedBodiesAre400(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{}
	for name, r := range map[string]request{
		"not JSON":        {contentType: "application/json", body: []byte("{"), at: testNow, secret: testSecret, method: http.MethodPost},
		"unknown content": {contentType: "text/csv", body: []byte("a,b"), at: testNow, secret: testSecret, method: http.MethodPost},
		"no event":        jsonRequest(map[string]any{"type": "event_callback", "team_id": testTeam, "event_id": "Ev1"}),
		"no event id":     jsonRequest(map[string]any{"type": "event_callback", "team_id": testTeam, "event": messageEvent(testPerson, "x", testTS, nil)}),
		"bad ts":          jsonRequest(callback("Ev1", messageEvent(testPerson, "x", "yesterday", nil))),
		"bad channel":     jsonRequest(callback("Ev1", messageEvent(testPerson, "x", testTS, map[string]any{"channel": "general"}))),
	} {
		t.Run(name, func(t *testing.T) {
			if res := serve(t, x, s, r); res.Code != http.StatusBadRequest {
				t.Fatalf("answered %d %q, want 400", res.Code, res.Body)
			}
			wantNone(t, s)
		})
	}
}

func TestMessageEventIsReportedAsAMessage(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{}
	if res := serve(t, x, s, jsonRequest(callback("Ev1", messageEvent(testPerson, "hello &amp; <@U0BOT>", testTS, nil)))); res.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", res.Code)
	}
	e := wantOne(t, s)
	id := testRoom + ":" + testTS
	if e.Kind != model.EnvelopeMessage || e.MessageID != id || e.Destination != testTeam || e.Assurance != "verified" || e.SenderKind != "" {
		t.Errorf("envelope = %+v, want a verified person's message %s for team %s", e, id, testTeam)
	}
	if e.Sender != x.userRef(testPerson) || e.Sender.Namespace != "slack.chat.user" {
		t.Errorf("sender = %+v, want the user in slack.chat.user", e.Sender)
	}
	if textOf(e) != "hello &amp; <@U0BOT>" {
		t.Errorf("text = %q, want Slack's own encoding, untouched", textOf(e))
	}
	// A top-level message is the root of a thread it may start: a per-thread
	// inbox keys on it, and replies carry it as thread_ts.
	if len(e.References) != 2 || e.References[0] != x.roomRef(testRoom) || e.References[1] != x.threadRef(testRoom, testTS) {
		t.Errorf("references = %+v, want the room and the thread the message roots", e.References)
	}
	if !e.OccurredAt.Equal(slackTime(testTS)) || e.Audience.Visibility != model.Public {
		t.Errorf("time %v audience %+v, want the ts and a public audience", e.OccurredAt, e.Audience)
	}
	p := payloadOf(t, e)
	if e.Payload.Name != PayloadSchema || p["mentioned"] != true || p["conversation"] != testRoom || p["ts"] != testTS || p["team_id"] != testTeam {
		t.Errorf("payload %s %+v, want the message facts with mentioned", e.Payload.Name, p)
	}
	if _, has := p["thread_ts"]; has || e.InReplyTo != "" {
		t.Errorf("payload %+v, reply parent %q for a top-level message", p, e.InReplyTo)
	}
	if err := e.Content.Validate(); err != nil {
		t.Errorf("content invalid: %v", err)
	}
}

func TestThreadReplyCarriesRoomAndThread(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{}
	reply := messageEvent(testPerson, "in thread", "1700000050.000200", map[string]any{"thread_ts": testTS})
	serve(t, x, s, jsonRequest(callback("Ev2", reply)))
	e := wantOne(t, s)
	want := []model.ExternalRef{x.roomRef(testRoom), x.threadRef(testRoom, testTS)}
	if len(e.References) != 2 || e.References[0] != want[0] || e.References[1] != want[1] {
		t.Fatalf("references = %+v, want room then thread %+v", e.References, want)
	}
	if payloadOf(t, e)["thread_ts"] != testTS {
		t.Errorf("payload does not carry thread_ts")
	}
	// Slack names no message a thread reply answers but the thread's root.
	if e.InReplyTo != testRoom+":"+testTS {
		t.Errorf("InReplyTo = %q, want the thread root", e.InReplyTo)
	}
}

// A thread parent reports thread_ts equal to its own ts: no thread yet.
func TestThreadParentRootsItsOwnThread(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{}
	serve(t, x, s, jsonRequest(callback("Ev1", messageEvent(testPerson, "parent", testTS, map[string]any{"thread_ts": testTS}))))
	if e := wantOne(t, s); len(e.References) != 2 || e.References[1] != x.threadRef(testRoom, testTS) {
		t.Fatalf("references = %+v, want the room and the thread the parent roots", e.References)
	} else if e.InReplyTo != "" {
		t.Fatalf("a thread root replies to %q, want nothing", e.InReplyTo)
	}
}

func TestOwnMessagesAreNeverReported(t *testing.T) {
	for name, ev := range map[string]map[string]any{
		"bot user":        messageEvent(testBotUser, "mine", testTS, map[string]any{"bot_id": testBotID}),
		"bot user only":   messageEvent(testBotUser, "mine", testTS, nil),
		"legacy bot id":   messageEvent("", "mine", testTS, map[string]any{"user": nil, "subtype": "bot_message", "bot_id": testBotID}),
		"own app mention": {"type": "app_mention", "channel": testRoom, "user": testBotUser, "text": "<@U0BOT> hi", "ts": testTS},
	} {
		t.Run(name, func(t *testing.T) {
			x, s := open(t, testConfig()), &sinkRecorder{}
			if res := serve(t, x, s, jsonRequest(callback("Ev1", ev))); res.Code != http.StatusOK {
				t.Fatalf("answered %d, want 200", res.Code)
			}
			wantNone(t, s)
		})
	}
}

func TestOtherBotsAreAutomatedBotSenders(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	// An integration's message names a bot and no user.
	ev := messageEvent("", "deploy finished", testTS, map[string]any{"user": nil, "subtype": "bot_message", "bot_id": "B0OTHER", "username": "ci"})
	serve(t, x, s, jsonRequest(callback("Ev1", ev)))
	e := wantOne(t, s)
	if e.Kind != model.EnvelopeAutomatedMessage || e.SenderKind != model.ContactKindBot || e.Sender != x.userRef("B0OTHER") {
		t.Fatalf("envelope = %+v, want an automated message from bot B0OTHER", e)
	}
}

// The same message reaches the app as a message event and an app_mention
// event, with two event IDs: both are the same event to core.
func TestMessageAndMentionOfOneMessageShareTheirIdentity(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	text := "<@U0BOT> status?"
	serve(t, x, s, jsonRequest(callback("Ev1", messageEvent(testPerson, text, testTS, nil))))
	serve(t, x, s, jsonRequest(callback("Ev2", map[string]any{"type": "app_mention", "channel": testRoom, "user": testPerson, "text": text, "ts": testTS, "event_ts": testTS})))
	got := s.envelopes()
	if len(got) != 2 {
		t.Fatalf("accepted %d, want both deliveries handed to the sink", len(got))
	}
	a, b := got[0], got[1]
	if a.EventID != b.EventID || a.MessageID != b.MessageID || a.Kind != b.Kind {
		t.Fatalf("event IDs %q/%q message IDs %q/%q: one message must read as one event", a.EventID, b.EventID, a.MessageID, b.MessageID)
	}
	if payloadOf(t, b)["mentioned"] != true || payloadOf(t, a)["mentioned"] != true {
		t.Errorf("both reports name the mention")
	}
}

func TestMentionIsTheBotUsersMention(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	// A bot ID is not a user ID; "<@B0BOT>" is nobody's mention.
	serve(t, x, s, jsonRequest(callback("Ev1", messageEvent(testPerson, "hey <@B0BOT> and <@U0OTHER>", testTS, nil))))
	if payloadOf(t, wantOne(t, s))["mentioned"] != false {
		t.Fatal("a mention of someone else counted as a mention of the bot")
	}
}

func TestMentionsOnlyDropsUnaddressedChannelMessages(t *testing.T) {
	cfg := testConfig()
	cfg.MentionsOnly = true
	x, s := open(t, cfg), &sinkRecorder{}
	serve(t, x, s, jsonRequest(callback("Ev1", messageEvent(testPerson, "unaddressed", testTS, nil))))
	serve(t, x, s, jsonRequest(callback("Ev2", messageEvent(testPerson, "<@U0BOT> addressed", "1700000001.000100", nil))))
	serve(t, x, s, jsonRequest(callback("Ev3", map[string]any{"type": "app_mention", "channel": testRoom, "user": testPerson, "text": "<@U0BOT> x", "ts": "1700000002.000100"})))
	serve(t, x, s, jsonRequest(callback("Ev4", messageEvent(testPerson, "private words", "1700000003.000100", map[string]any{"channel": "D0DM", "channel_type": "im"}))))
	var ids []string
	for _, e := range s.envelopes() {
		ids = append(ids, e.MessageID)
	}
	want := []string{testRoom + ":1700000001.000100", testRoom + ":1700000002.000100", "D0DM:1700000003.000100"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("reported %v, want %v: addressed messages and direct messages only", ids, want)
	}
}

func TestEventsOfOtherTeamsAndAppsAreAcknowledgedAndDropped(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	other := callback("Ev1", messageEvent(testPerson, "x", testTS, nil))
	other["team_id"] = "T0OTHER"
	if res := serve(t, x, s, jsonRequest(other)); res.Code != http.StatusOK {
		t.Fatalf("other team answered %d, want 200: not this channel's, not an error", res.Code)
	}
	app := callback("Ev2", messageEvent(testPerson, "x", testTS, nil))
	app["api_app_id"] = "A0OTHER"
	if res := serve(t, x, s, jsonRequest(app)); res.Code != http.StatusOK {
		t.Fatalf("other app answered %d, want 200", res.Code)
	}
	wantNone(t, s)
}

func TestEventsTheAdapterDoesNotConsumeAreAcknowledged(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	for name, body := range map[string]map[string]any{
		"channel_join":     callback("Ev1", messageEvent(testPerson, "joined", testTS, map[string]any{"subtype": "channel_join"})),
		"no text no files": callback("Ev2", messageEvent(testPerson, "", testTS, nil)),
		"rate limited":     {"type": "app_rate_limited", "team_id": testTeam, "api_app_id": testApp, "minute_rate_limited": testNow.Unix()},
		"unknown callback": callback("Ev3", map[string]any{"type": "team_join", "user": map[string]any{"id": testPerson}}),
		"unknown type":     {"type": "future_thing"},
	} {
		t.Run(name, func(t *testing.T) {
			if res := serve(t, x, s, jsonRequest(body)); res.Code != http.StatusOK {
				t.Fatalf("answered %d, want 200", res.Code)
			}
			wantNone(t, s)
		})
	}
}

func TestFileOnlyMessageCarriesItsFiles(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	ev := messageEvent(testPerson, "", testTS, map[string]any{"subtype": "file_share", "files": []any{
		map[string]any{"id": "F0FILE", "name": "report.pdf", "mimetype": "application/pdf", "size": 1234, "url_private": "https://files.slack.com/x"},
	}})
	serve(t, x, s, jsonRequest(callback("Ev1", ev)))
	e := wantOne(t, s)
	if len(e.Content.Parts) != 1 || e.Content.Parts[0].Kind != model.PartStructured || e.Content.Parts[0].Structured.Name != FilesSchema {
		t.Fatalf("content = %+v, want one %s part", e.Content, FilesSchema)
	}
	if strings.Contains(string(e.Content.Parts[0].Structured.Data), "files.slack.com") {
		t.Errorf("the private URL travels as data: %s", e.Content.Parts[0].Structured.Data)
	}
	if err := e.Content.Validate(); err != nil {
		t.Errorf("content invalid: %v", err)
	}
}

func TestRetriesAreHandledLikeDeliveries(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	r := jsonRequest(callback("Ev1", messageEvent(testPerson, "hi", testTS, nil)))
	r.headers = map[string]string{"X-Slack-Retry-Num": "1", "X-Slack-Retry-Reason": "http_timeout"}
	if res := serve(t, x, s, r); res.Code != http.StatusOK {
		t.Fatalf("retry answered %d, want 200", res.Code)
	}
	if got := s.envelopes(); len(got) != 1 || got[0].EventID != "message:"+testRoom+":"+testTS {
		t.Fatalf("envelopes = %+v, want the message once under its stable event ID", got)
	}
}

func TestEditedMessageIsAnEdit(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	ev := map[string]any{
		"type": "message", "subtype": "message_changed", "channel": testRoom, "channel_type": "channel", "hidden": true,
		"ts": "1700000100.000300", "event_ts": "1700000100.000300",
		"message": map[string]any{
			"type": "message", "user": testPerson, "text": "edited text", "ts": testTS,
			"edited": map[string]any{"user": testPerson, "ts": "1700000100.000000"},
		},
		"previous_message": map[string]any{"type": "message", "user": testPerson, "text": "old text", "ts": testTS},
	}
	serve(t, x, s, jsonRequest(callback("Ev9", ev)))
	e := wantOne(t, s)
	if e.Kind != model.EnvelopeMessageEdited || e.MessageID != testRoom+":"+testTS || textOf(e) != "edited text" {
		t.Fatalf("envelope = %+v, want an edit of %s to %q", e, testRoom+":"+testTS, "edited text")
	}
	if e.SourceRevision != "1700000100.000000" || !e.OccurredAt.Equal(slackTime("1700000100.000000")) || e.Sender != x.userRef(testPerson) {
		t.Errorf("revision %q at %v by %+v, want the edit's ts and its author", e.SourceRevision, e.OccurredAt, e.Sender)
	}
	if e.EventID != "Ev9" {
		t.Errorf("event ID %q, want Slack's", e.EventID)
	}
}

// message_changed also reports unfurls and thread bookkeeping, which carry
// no "edited" property and are no edit.
func TestMessageChangedWithoutEditedIsNotAnEdit(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	ev := map[string]any{
		"type": "message", "subtype": "message_changed", "channel": testRoom, "ts": "1700000100.000300",
		"message": map[string]any{"type": "message", "user": testPerson, "text": "see <https://example.com>", "ts": testTS, "attachments": []any{}},
	}
	serve(t, x, s, jsonRequest(callback("Ev9", ev)))
	wantNone(t, s)
}

func TestEditOfAnUnknownMessageFallsBackToTheMessage(t *testing.T) {
	x := open(t, testConfig())
	s := &sinkRecorder{fail: func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited {
			return model.NewError(model.NotFound, "no such message")
		}
		return nil
	}}
	ev := map[string]any{
		"type": "message", "subtype": "message_changed", "channel": testRoom, "ts": "1700000100.000300",
		"message": map[string]any{"type": "message", "user": testPerson, "text": "edited", "ts": testTS, "edited": map[string]any{"user": testPerson, "ts": "1700000100.000000"}},
	}
	if res := serve(t, x, s, jsonRequest(callback("Ev9", ev))); res.Code != http.StatusOK {
		t.Fatalf("answered %d, want 200", res.Code)
	}
	e := wantOne(t, s)
	if e.Kind != model.EnvelopeMessage || e.MessageID != testRoom+":"+testTS || textOf(e) != "edited" {
		t.Fatalf("envelope = %+v, want the message recorded as first observed", e)
	}
}

func TestDeletedMessageIsADeletionByNoOne(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	ev := map[string]any{
		"type": "message", "subtype": "message_deleted", "hidden": true, "channel": testRoom, "channel_type": "channel",
		"ts": "1700000200.000400", "deleted_ts": testTS, "event_ts": "1700000200.000400",
		"previous_message": map[string]any{"type": "message", "user": testPerson, "text": "gone", "ts": testTS},
	}
	serve(t, x, s, jsonRequest(callback("Ev8", ev)))
	e := wantOne(t, s)
	if e.Kind != model.EnvelopeMessageDeleted || e.MessageID != testRoom+":"+testTS || !e.SystemActor || !e.Sender.IsZero() {
		t.Fatalf("envelope = %+v, want a deletion of %s by a system actor: Slack names no deleter", e, testRoom+":"+testTS)
	}
	// The same message deleted twice, or one the sink never held, is no failure.
	sink := &sinkRecorder{fail: func(model.Ingress) error { return model.NewError(model.NotFound, "unknown") }}
	if res := serve(t, x, sink, jsonRequest(callback("Ev8", ev))); res.Code != http.StatusOK {
		t.Fatalf("deletion of an unknown message answered %d, want 200", res.Code)
	}
}

func TestReactionsAreReactionsOnMessages(t *testing.T) {
	x, s := open(t, testConfig()), &sinkRecorder{}
	react := func(typ, name string, mod ...func(map[string]any)) map[string]any {
		e := map[string]any{
			"type": typ, "user": testPerson, "reaction": name, "item_user": testBotUser, "event_ts": "1700000300.000500",
			"item": map[string]any{"type": "message", "channel": testRoom, "ts": testTS},
		}
		for _, m := range mod {
			m(e)
		}
		return e
	}
	serve(t, x, s, jsonRequest(callback("Ev1", react("reaction_added", "thumbsup"))))
	serve(t, x, s, jsonRequest(callback("Ev2", react("reaction_removed", "thumbsup"))))
	got := s.envelopes()
	if len(got) != 2 || got[0].Kind != model.EnvelopeReactionAdded || got[1].Kind != model.EnvelopeReactionRemoved {
		t.Fatalf("envelopes = %+v, want an added then a removed reaction", got)
	}
	for _, e := range got {
		if e.MessageID != testRoom+":"+testTS || textOf(e) != "👍" || e.Sender != x.userRef(testPerson) || e.Destination != testTeam {
			t.Errorf("envelope %+v, want 👍 by the person on %s", e, testRoom+":"+testTS)
		}
		if e.Content.Validate() != nil {
			t.Errorf("content invalid")
		}
	}
	if got[0].EventID != "Ev1" || got[1].EventID != "Ev2" {
		t.Errorf("event IDs %q %q, want Slack's", got[0].EventID, got[1].EventID)
	}

	// What uma cannot express as an emoji, or cannot place, is dropped.
	s2 := &sinkRecorder{}
	for _, ev := range []map[string]any{
		react("reaction_added", "partyparrot"),     // a custom emoji: no Unicode form
		react("reaction_added", "+1::skin-tone-3"), // a skin tone variant
		react("reaction_added", "thumbsup", func(m map[string]any) { m["item"] = map[string]any{"type": "file", "file": "F1"} }),
		react("reaction_added", "thumbsup", func(m map[string]any) { m["user"] = testBotUser }), // the bot's own
	} {
		if res := serve(t, x, s2, jsonRequest(callback("EvX", ev))); res.Code != http.StatusOK {
			t.Fatalf("answered %d, want 200", res.Code)
		}
	}
	wantNone(t, s2)
}

func TestSinkFailuresMapToRetryAdvice(t *testing.T) {
	x := open(t, testConfig())
	req := jsonRequest(callback("Ev1", messageEvent(testPerson, "hi", testTS, nil)))
	for name, tc := range map[string]struct {
		err     error
		code    int
		noRetry bool
	}{
		"retryable": {model.NewError(model.Unavailable, "busy"), http.StatusServiceUnavailable, false},
		"permanent": {model.NewError(model.Validation, "no"), http.StatusUnprocessableEntity, true},
		"disabled":  {model.NewError(model.Disabled, "inbox disabled"), http.StatusUnprocessableEntity, true},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sinkRecorder{fail: func(model.Ingress) error { return tc.err }}
			res := serve(t, x, s, req)
			if res.Code != tc.code {
				t.Fatalf("answered %d, want %d", res.Code, tc.code)
			}
			if got := res.Header.Get("X-Slack-No-Retry") == "1"; got != tc.noRetry {
				t.Fatalf("x-slack-no-retry = %v, want %v: a permanent refusal asks Slack to stop retrying", got, tc.noRetry)
			}
		})
	}
}

func TestEditKeepsTheAuthorsKindOnlyForTheAuthor(t *testing.T) {
	x := open(t, testConfig())
	edit := func(editor string) model.Envelope {
		s := &sinkRecorder{}
		ev := map[string]any{
			"type": "message", "subtype": "message_changed", "channel": testRoom, "ts": "1700000100.000300",
			"message": map[string]any{
				"type": "message", "user": "B0OTHER", "bot_id": "B0OTHER", "text": "edited", "ts": testTS,
				"edited": map[string]any{"user": editor, "ts": "1700000100.000000"},
			},
		}
		serve(t, x, s, jsonRequest(callback("Ev9", ev)))
		return wantOne(t, s)
	}
	if e := edit("B0OTHER"); e.SenderKind != model.ContactKindBot {
		t.Errorf("a bot's own edit has kind %q, want bot", e.SenderKind)
	}
	if e := edit(testPerson); e.SenderKind != "" || e.Sender != x.userRef(testPerson) {
		t.Errorf("a person's edit of a bot's message = %+v, want the person, kind person", e)
	}
}
