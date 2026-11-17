package telegram

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

func (r *rig) room(chat int64) model.ExternalRef  { return r.x.roomRef(chat) }
func (r *rig) user(id int64) model.ExternalRef    { return r.x.userRef(id) }
func (r *rig) topic(c, t int64) model.ExternalRef { return r.x.threadRef(c, t) }
func (r *rig) send(t *testing.T, route model.ExternalRef, c model.Content, mod ...func(*adapter.SendRequest)) (adapter.Outcome, error) {
	t.Helper()
	req := adapter.SendRequest{Effect: r.effect(route), Content: c}
	for _, m := range mod {
		if m != nil {
			m(&req)
		}
	}
	return r.x.Send(t.Context(), req)
}

func mustAccept(t *testing.T, out adapter.Outcome, err error) adapter.Outcome {
	t.Helper()
	if err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("outcome %+v, error %v; want provider_accepted", out, err)
	}
	return out
}

func wantRejected(t *testing.T, out adapter.Outcome, err error, code model.ErrorCode) {
	t.Helper()
	if err != nil || out.State != model.Rejected || out.Failure == nil || out.Failure.Code != code {
		t.Fatalf("outcome %+v, error %v; want a rejection %s", out, err, code)
	}
}

func wantErr(t *testing.T, err error, code model.ErrorCode) *model.Error {
	t.Helper()
	me, ok := err.(*model.Error)
	if !ok || me.Code != code {
		t.Fatalf("error %v, want %s", err, code)
	}
	return me
}

func TestSendToAChat(t *testing.T) {
	r := newRig(t)
	out := accept(t, r, r.room(testGroup), text("hello <b>&</b> world"))
	calls := r.callsOf("sendMessage")
	if len(calls) != 1 {
		t.Fatalf("%d sendMessage calls", len(calls))
	}
	a := calls[0].args()
	if a["chat_id"] != float64(testGroup) || a["text"] != "hello <b>&</b> world" {
		t.Errorf("args %v, want the chat ID as a number and the text as it is", a)
	}
	for _, k := range []string{"parse_mode", "message_thread_id", "reply_parameters", "reply_markup", "entities"} {
		if _, ok := a[k]; ok {
			t.Errorf("a plain text send carries %s", k)
		}
	}
	if calls[0].Token != testToken {
		t.Errorf("request carried token %q", calls[0].Token)
	}
	msgs := r.f.messages("acct1", testGroup)
	if len(msgs) != 1 || out.References[0] != model.ProviderMessageRef(testChannel, messageID(testGroup, msgs[0].id)) {
		t.Errorf("references %+v, messages %+v: want the provider message <chat>:<id>", out.References, msgs)
	}
	// Only the API purpose is resolved for a call.
	for _, p := range r.creds.purposes() {
		if p != CredentialPurposeAPI {
			t.Errorf("purpose %q resolved for a send", p)
		}
	}
}

func accept(t *testing.T, r *rig, route model.ExternalRef, c model.Content, mod ...func(*adapter.SendRequest)) adapter.Outcome {
	t.Helper()
	out, err := r.send(t, route, c, mod...)
	return mustAccept(t, out, err)
}

func TestSendToATopicAndToAUser(t *testing.T) {
	r := newRig(t)
	accept(t, r, r.topic(testGroup, 3), text("in a topic"))
	if a := r.callsOf("sendMessage")[0].args(); a["chat_id"] != float64(testGroup) || a["message_thread_id"] != float64(3) {
		t.Errorf("topic send args %v", a)
	}
	// A user's route is the private chat with them, whose chat ID is theirs.
	r.f.put("acct1", testPerson, testPerson, "/start", 0, 0)
	accept(t, r, r.user(testPerson), text("hello"))
	if a := r.callsOf("sendMessage")[1].args(); a["chat_id"] != float64(testPerson) {
		t.Errorf("user send args %v", a)
	}
}

// A bot cannot message a user who never started it, or one who blocked it;
// Telegram refuses, and nothing is retried.
func TestSendToAUserWhoNeverStartedTheBot(t *testing.T) {
	r := newRig(t)
	out, err := r.send(t, r.user(777), text("hello"))
	wantRejected(t, out, err, model.Unauthorized)
	if !strings.Contains(out.Failure.SafeMessage, "bot can't initiate conversation with a user") {
		t.Errorf("message %q: want Telegram's reason", out.Failure.SafeMessage)
	}
	r.f.put("acct1", 888, 888, "/start", 0, 0)
	r.acct.blocked[888] = true
	out, err = r.send(t, r.user(888), text("hello"))
	wantRejected(t, out, err, model.Unauthorized)
	if !strings.Contains(out.Failure.SafeMessage, "bot was blocked by the user") {
		t.Errorf("message %q", out.Failure.SafeMessage)
	}
}

func TestReplyAnswersTheMessage(t *testing.T) {
	r := newRig(t)
	target := model.ProviderMessageRef(testChannel, messageID(testGroup, 42))
	accept(t, r, r.room(testGroup), text("yes"), func(q *adapter.SendRequest) { q.Target = &target })
	rp, _ := r.callsOf("sendMessage")[0].args()["reply_parameters"].(map[string]any)
	if rp["message_id"] != float64(42) || rp["allow_sending_without_reply"] != true {
		t.Errorf("reply_parameters %v, want message 42, sent even if it is gone", rp)
	}
	// A message of another chat is no thread of this one.
	other := model.ProviderMessageRef(testChannel, messageID(-100777, 42))
	accept(t, r, r.room(testGroup), text("elsewhere"), func(q *adapter.SendRequest) { q.Target = &other })
	if _, ok := r.callsOf("sendMessage")[1].args()["reply_parameters"]; ok {
		t.Error("a reply to a message of another chat carried reply_parameters")
	}
	// A reply to a message of a forum topic goes into the topic.
	data, _ := json.Marshal(map[string]any{"thread": 5})
	inTopic := model.ProviderMessageRef(testChannel, messageID(testGroup, 50))
	accept(t, r, r.room(testGroup), text("in the topic"), func(q *adapter.SendRequest) {
		q.Target = &inTopic
		q.Thread = &adapter.Thread{Metadata: []model.Extension{{Name: PayloadSchema, Version: PayloadVersion, Data: data}}}
	})
	if a := r.callsOf("sendMessage")[2].args(); a["message_thread_id"] != float64(5) {
		t.Errorf("args %v, want the topic of the message answered", a)
	}
}

// A route that names a topic keeps it: the topic of the message answered
// only fills in a route with none.
func TestARoutesTopicWinsOverTheMessagesTopic(t *testing.T) {
	r := newRig(t)
	data, _ := json.Marshal(map[string]any{"thread": 5})
	target := model.ProviderMessageRef(testChannel, messageID(testGroup, 50))
	accept(t, r, r.topic(testGroup, 3), text("x"), func(q *adapter.SendRequest) {
		q.Target = &target
		q.Thread = &adapter.Thread{Metadata: []model.Extension{{Name: PayloadSchema, Version: PayloadVersion, Data: data}}}
	})
	if a := r.callsOf("sendMessage")[0].args(); a["message_thread_id"] != float64(3) {
		t.Errorf("args %v, want the route's topic 3", a)
	}
}

func TestSendRefusesWhatItCannotCarry(t *testing.T) {
	r := newRig(t)
	for name, tc := range map[string]struct {
		c    model.Content
		mod  func(*adapter.SendRequest)
		code model.ErrorCode
	}{
		"attachment":   {model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "x"}, {Kind: model.PartAttachment}}}, nil, model.Unsupported},
		"other schema": {model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: "acme.thing", Version: 1, Data: []byte(`{}`)}}}}, nil, model.Unsupported},
		"no content":   {model.Content{}, nil, model.Validation},
		"blank text":   {text(""), nil, model.Validation},
		"too long":     {text(strings.Repeat("é", MaxTextRunes+1)), nil, model.Validation},
		"restricted":   {text("x"), func(q *adapter.SendRequest) { q.Audience = model.Audience{Visibility: model.Restricted} }, model.Unsupported},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := r.send(t, r.room(testGroup), tc.c, tc.mod)
			wantRejected(t, out, err, tc.code)
			if n := len(r.callsOf("sendMessage")); n != 0 {
				t.Fatalf("%d calls reached Telegram, want none", n)
			}
		})
	}
	// The limit is in characters, not bytes: 4096 two-byte letters fit.
	accept(t, r, r.room(testGroup), text(strings.Repeat("é", MaxTextRunes)))
}

func TestSendJoinsTextPartsWithABlankLine(t *testing.T) {
	r := newRig(t)
	c := model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "one"}, {Kind: model.PartText, Text: "two"}}}
	accept(t, r, r.room(testGroup), c)
	if a := r.callsOf("sendMessage")[0].args(); a["text"] != "one\n\ntwo" {
		t.Errorf("text %q", a["text"])
	}
}

func TestSendRejectsABadRoute(t *testing.T) {
	r := newRig(t)
	other := r.room(testGroup)
	other.Channel.ID = "00000ad0-0000-7000-8000-0000000000ff"
	for name, route := range map[string]model.ExternalRef{
		"another channel":      other,
		"not a number":         {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "general"},
		"zero":                 {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "0"},
		"padded":               {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "007"},
		"a username":           {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "@channel"},
		"a room with a parent": {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "5", ParentID: "6"},
		"a negative user":      {Channel: testChannel, Namespace: UserNamespace, Kind: RefKindUser, ID: "-5"},
		"a chat identity":      {Channel: testChannel, Namespace: UserNamespace, Kind: RefKindChat, ID: "-5"},
		"topic zero":           {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindThread, ID: "0", ParentID: "-5"},
		"topic of nothing":     {Channel: testChannel, Namespace: Namespace, Kind: model.RefKindThread, ID: "3"},
		"another namespace":    {Channel: testChannel, Namespace: "slack.chat", Kind: model.RefKindRoom, ID: "5"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.send(t, route, text("x"))
			_ = wantErr(t, err, model.Validation)
			if n := len(r.f.calls("acct1")); n != 0 {
				t.Fatalf("%d calls reached Telegram", n)
			}
		})
	}
}

// Only plain text is measured here: under a parse mode Telegram counts the
// characters after parsing, so its refusal is the judge.
func TestMarkupLengthIsTelegramsToJudge(t *testing.T) {
	r := newRig(t)
	long := strings.Repeat("a", MaxTextRunes+10)
	out, err := r.send(t, r.room(testGroup), content(markup(ParseModeHTML, long)))
	wantRejected(t, out, err, model.Validation)
	if n := len(r.callsOf("sendMessage")); n != 1 {
		t.Fatalf("%d calls, want the markup sent for Telegram to judge", n)
	}
}

// A 403 description is shown only when it is plain words.
func TestForbiddenDescriptionIsEchoedOnlyWhenPlain(t *testing.T) {
	for desc, echoed := range map[string]bool{
		"Forbidden: bot was blocked by the user":                 true,
		"Forbidden: bot can't initiate conversation with a user": true,
		"Forbidden: <b>x</b> 12345":                              false,
		"Forbidden: " + strings.Repeat("a", 200):                 false,
		"Something else entirely":                                false,
	} {
		r := newRig(t)
		r.f.respond("acct1", failWith(403, desc))
		out, err := r.send(t, r.room(testGroup), text("x"))
		wantRejected(t, out, err, model.Unauthorized)
		if got := strings.Contains(out.Failure.SafeMessage, strings.TrimPrefix(desc, "Forbidden: ")); got != echoed {
			t.Errorf("%q: message %q, echoed %v, want %v", desc, out.Failure.SafeMessage, got, echoed)
		}
	}
}

func TestSendOutcomes(t *testing.T) {
	for name, tc := range map[string]struct {
		script scripted
		check  func(t *testing.T, out adapter.Outcome, err error)
	}{
		"markup refused": {failWith(400, "Bad Request: can't parse entities: Can't find end of the entity starting at byte offset 7"),
			func(t *testing.T, out adapter.Outcome, err error) {
				wantRejected(t, out, err, model.Validation)
				// The description may quote the message: it is not echoed.
				if strings.Contains(out.Failure.SafeMessage, "byte offset") {
					t.Errorf("message %q echoes Telegram's description", out.Failure.SafeMessage)
				}
			}},
		"too long":         {failWith(400, "Bad Request: message is too long"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Validation) }},
		"chat gone":        {failWith(400, "Bad Request: chat not found"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.NotFound) }},
		"kicked":           {failWith(403, "Forbidden: bot was kicked from the supergroup chat"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Unauthorized) }},
		"no right to post": {failWith(403, "Forbidden: not enough rights to send text messages to the chat"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Unauthorized) }},
		"unknown 4xx":      {failWith(418, "I'm a teapot"), func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.Permanent) }},
		"flood control": {rateLimitedFor(7 * time.Second), func(t *testing.T, o adapter.Outcome, e error) {
			me := wantErr(t, e, model.RateLimited)
			if me.RetryAfter != 7*time.Second || me.Retry != model.RetrySameOperation {
				t.Errorf("retry %v after %v, want the same operation after Telegram's 7s", me.Retry, me.RetryAfter)
			}
		}},
		"unreachable":  {unreachable, func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.Transient) }},
		"answer lost":  {answerLost, func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.OutcomeUnknown) }},
		"request lost": {requestLost, func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.OutcomeUnknown) }},
		"server error": {failWith(502, "Bad Gateway"), func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.OutcomeUnknown) }},
		"no message in the answer": {scripted{status: 200, body: `{"ok":true,"result":{"message_id":0,"chat":{"id":-1001234567890,"type":"supergroup"}}}`},
			func(t *testing.T, o adapter.Outcome, e error) { _ = wantErr(t, e, model.OutcomeUnknown) }},
		"migrated": {scripted{status: 400, body: `{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1009999}}`},
			func(t *testing.T, o adapter.Outcome, e error) { wantRejected(t, o, e, model.NotFound) }},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t)
			r.f.respond("acct1", tc.script)
			out, err := r.send(t, r.room(testGroup), text("x"))
			tc.check(t, out, err)
		})
	}
}

// The bot token is in the request path, so an error built from the request
// would name it. None does.
func TestNoErrorNamesTheBotToken(t *testing.T) {
	for _, s := range []scripted{unreachable, answerLost, requestLost, failWith(403, "Forbidden: bot was blocked by the user"), failWith(502, "Bad Gateway"), rateLimitedFor(time.Second)} {
		r := newRig(t)
		r.f.respond("acct1", s)
		out, err := r.send(t, r.room(testGroup), text("x"))
		if strings.Contains(joined(out, err), "AAtestToken") || strings.Contains(joined(out, err), "7000001") {
			t.Errorf("outcome %+v error %v names the token", out, err)
		}
	}
}

func joined(out adapter.Outcome, err error) string {
	s := ""
	if err != nil {
		s += err.Error() + " "
		if me, ok := err.(*model.Error); ok {
			s += me.SafeMessage
		}
	}
	if out.Failure != nil {
		s += out.Failure.SafeMessage + " " + out.Failure.Error()
	}
	return s
}

// A token Telegram refuses is resolved again once, fresh.
func TestRefusedTokenIsResolvedAgain(t *testing.T) {
	r := newRig(t)
	r.creds.token = "wrong"
	var fresh []bool
	r.x.creds = credentialFunc(func(ctx contextT, req credReq) (credT, error) {
		fresh = append(fresh, req.Fresh)
		if req.Fresh {
			return secretOf(testToken), nil
		}
		return secretOf("wrong"), nil
	})
	accept(t, r, r.room(testGroup), text("x"))
	if len(fresh) != 2 || fresh[0] || !fresh[1] {
		t.Errorf("resolutions %v, want a first and then a fresh one", fresh)
	}
	r.x.creds = credentialFunc(func(ctx contextT, req credReq) (credT, error) { return secretOf("always wrong"), nil })
	_, err := r.send(t, r.room(testGroup), text("x"))
	_ = wantErr(t, err, model.Unauthorized)
}

func TestUnreadyInstance(t *testing.T) {
	r := newRig(t)
	_ = r.x.Close(t.Context())
	_, err := r.send(t, r.room(testGroup), text("x"))
	_ = wantErr(t, err, model.Unavailable)
	y := open(t, testConfig())
	_, err = y.Send(t.Context(), adapter.SendRequest{Effect: adapter.Effect{Route: y.roomRef(testGroup)}, Content: text("x")})
	_ = wantErr(t, err, model.Unavailable)
}

var _ = http.StatusOK
