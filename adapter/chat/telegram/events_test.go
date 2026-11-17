package telegram

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
)

// serve posts update to a fresh instance's handler and returns the answer
// and what the sink accepted.
func serve(t *testing.T, cfg Config, update obj, sinks ...*sinkRecorder) (*http.Response, []model.Envelope) {
	t.Helper()
	sink := &sinkRecorder{}
	if len(sinks) > 0 {
		sink = sinks[0]
	}
	rec := jsonPost(update).send(open(t, cfg).Handler(sink))
	return rec.Result(), sink.envelopes()
}

func TestHandlerProtocol(t *testing.T) {
	ok := updateOf(10, "message", textMessage(privateChat(testPerson), person(testPerson), 1, "hi", nil))
	for _, tc := range []struct {
		name string
		p    post
		want int
	}{
		{"accepted", jsonPost(ok), http.StatusOK},
		{"get", func() post { p := jsonPost(ok); p.method = http.MethodGet; return p }(), http.StatusMethodNotAllowed},
		{"no secret token", func() post { p := jsonPost(ok); p.secret = "-"; return p }(), http.StatusUnauthorized},
		{"wrong secret token", func() post { p := jsonPost(ok); p.secret = "webhook-secret_0123456789-abcdeX"; return p }(), http.StatusUnauthorized},
		{"not json", func() post { p := jsonPost(ok); p.contentType = "text/plain"; return p }(), http.StatusBadRequest},
		{"no content type", func() post { p := jsonPost(ok); p.contentType = ""; return p }(), http.StatusBadRequest},
		{"charset parameter", func() post { p := jsonPost(ok); p.contentType = "application/json; charset=utf-8"; return p }(), http.StatusOK},
		{"broken json", func() post { p := jsonPost(ok); p.body = []byte(`{"update_id":`); return p }(), http.StatusBadRequest},
		{"no update id", jsonPost(obj{"message": textMessage(privateChat(testPerson), person(testPerson), 1, "hi", nil)}), http.StatusBadRequest},
		{"malformed message", jsonPost(updateOf(11, "message", obj{"text": "no ids"})), http.StatusBadRequest},
		{"too large", func() post {
			p := jsonPost(ok)
			p.mod = func(r *http.Request) { r.Body = http.MaxBytesReader(nil, r.Body, 8) }
			return p
		}(), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &sinkRecorder{}
			rec := tc.p.send(open(t, testConfig()).Handler(sink))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if tc.want != http.StatusOK && len(sink.envelopes()) != 0 {
				t.Errorf("a refused request reached the sink: %+v", sink.envelopes())
			}
			if body := strings.TrimSpace(rec.Body.String()); tc.want == http.StatusOK && body != "" {
				t.Errorf("body %q, want an empty 200", body)
			} else if tc.want != http.StatusOK && body != "" && body != http.StatusText(tc.want) {
				t.Errorf("body %q, want the status text or nothing", body)
			}
		})
	}
}

func TestHandlerRefusesWithoutVerifyingNothingElse(t *testing.T) {
	x := open(t, testConfig())
	sink := &sinkRecorder{}
	// Authentication comes first: a request that fails it never has its
	// body parsed, so a malformed one is 401, not 400.
	p := post{body: []byte("{"), contentType: "text/plain", secret: "nope", method: http.MethodPost}
	if rec := p.send(x.Handler(sink)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
}

func TestClosedAndUnconfiguredInstancesAnswer503(t *testing.T) {
	x := open(t, testConfig())
	if err := x.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rec := jsonPost(updateOf(1, "message", textMessage(privateChat(testPerson), person(testPerson), 1, "hi", nil))).send(x.Handler(&sinkRecorder{})); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("closed: status %d, want 503", rec.Code)
	}
	y := open(t, testConfig(), func(d *adapterDeps) { d.Credentials = nil })
	if rec := jsonPost(updateOf(1, "message", textMessage(privateChat(testPerson), person(testPerson), 1, "hi", nil))).send(y.Handler(&sinkRecorder{})); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no credentials port: status %d, want 503", rec.Code)
	}
	c := newCredentials()
	c.err = errors.New("vault down")
	z := open(t, testConfig(), func(d *adapterDeps) { d.Credentials = c })
	if rec := jsonPost(updateOf(1, "message", textMessage(privateChat(testPerson), person(testPerson), 1, "hi", nil))).send(z.Handler(&sinkRecorder{})); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("secret not resolvable: status %d, want 503", rec.Code)
	}
}

func TestSinkFailuresAnswerRetryOrNot(t *testing.T) {
	u := updateOf(10, "message", textMessage(privateChat(testPerson), person(testPerson), 1, "hi", nil))
	for _, tc := range []struct {
		err  error
		want int
	}{
		{model.NewError(model.Unavailable, "store down"), http.StatusServiceUnavailable},
		{model.NewError(model.RateLimited, "slow down"), http.StatusServiceUnavailable},
		{model.NewError(model.Validation, "no such inbox"), http.StatusUnprocessableEntity},
		{model.NewError(model.Unauthorized, "denied"), http.StatusUnprocessableEntity},
	} {
		sink := &sinkRecorder{fail: func(model.Ingress) error { return tc.err }}
		if resp, _ := serve(t, testConfig(), u, sink); resp.StatusCode != tc.want {
			t.Errorf("sink error %v: status %d, want %d", tc.err, resp.StatusCode, tc.want)
		}
	}
}

func TestPrivateMessage(t *testing.T) {
	resp, got := serve(t, testConfig(), updateOf(10, "message", textMessage(privateChat(testPerson), person(testPerson), 7, "hello <b>there</b> & more", nil)))
	if resp.StatusCode != http.StatusOK || len(got) != 1 {
		t.Fatalf("status %d, %d envelopes, want 200 and one", resp.StatusCode, len(got))
	}
	e := got[0]
	if e.Kind != model.EnvelopeMessage || e.EventID != "message:424242:7" || e.MessageID != "424242:7" || e.Destination != "7000001" {
		t.Errorf("kind %q event %q message %q destination %q", e.Kind, e.EventID, e.MessageID, e.Destination)
	}
	if e.Sender != (model.ExternalRef{Channel: testChannel, Namespace: UserNamespace, Kind: RefKindUser, ID: "424242"}) || e.SenderKind != "" {
		t.Errorf("sender %+v kind %q, want the user, a person", e.Sender, e.SenderKind)
	}
	if e.Assurance != "verified" || e.Audience.Visibility != model.Public || !e.OccurredAt.Equal(time.Unix(testNow.Unix()+7, 0)) {
		t.Errorf("assurance %q audience %+v at %v", e.Assurance, e.Audience, e.OccurredAt)
	}
	// Text is Telegram's own: no entity is applied, nothing is escaped.
	if len(e.Content.Parts) != 1 || e.Content.Parts[0].Kind != model.PartText || e.Content.Parts[0].Text != "hello <b>there</b> & more" {
		t.Errorf("content %+v", e.Content)
	}
	if len(e.References) != 1 || e.References[0] != (model.ExternalRef{Channel: testChannel, Namespace: Namespace, Kind: model.RefKindRoom, ID: "424242"}) {
		t.Errorf("references %+v, want the private chat's room alone", e.References)
	}
	if e.Payload.Name != PayloadSchema || e.Payload.Version != PayloadVersion {
		t.Errorf("payload %+v", e.Payload)
	}
	for _, want := range []string{`"chat":424242`, `"chat_type":"private"`, `"message_id":7`, `"update_type":"message"`, `"bot_id":7000001`} {
		if !strings.Contains(string(e.Payload.Data), want) {
			t.Errorf("payload %s lacks %s", e.Payload.Data, want)
		}
	}
	if err := e.Content.Validate(); err != nil {
		t.Errorf("content invalid: %v", err)
	}
}

// A message ID is unique per chat only, so the chat is part of its identity:
// the same message_id in two chats is two messages.
func TestMessageIDIncludesTheChat(t *testing.T) {
	_, a := serve(t, testConfig(), updateOf(1, "message", textMessage(groupChat(), person(testPerson), 5, "in the group", nil)))
	_, b := serve(t, testConfig(), updateOf(2, "message", textMessage(privateChat(testPerson), person(testPerson), 5, "in private", nil)))
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("%d and %d envelopes", len(a), len(b))
	}
	if a[0].MessageID != "-1001234567890:5" || b[0].MessageID != "424242:5" || a[0].EventID == b[0].EventID {
		t.Errorf("ids %q %q events %q %q", a[0].MessageID, b[0].MessageID, a[0].EventID, b[0].EventID)
	}
	if got := a[0].References[0]; got.ID != "-1001234567890" || got.Kind != model.RefKindRoom {
		t.Errorf("group room %+v", got)
	}
}

func TestRedeliveryKeepsTheEventID(t *testing.T) {
	u := updateOf(10, "message", textMessage(groupChat(), person(testPerson), 5, "hi", nil))
	sink := &sinkRecorder{}
	for range 2 {
		if resp, _ := serve(t, testConfig(), u, sink); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	got := sink.envelopes()
	if len(got) != 2 || got[0].EventID != got[1].EventID || got[0].MessageID != got[1].MessageID {
		t.Fatalf("a redelivered update must carry the same event and message ID: %+v", got)
	}
}

func TestTopicMessagesCarryTheirThread(t *testing.T) {
	_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), person(testPerson), 9, "in a topic", obj{"message_thread_id": 3, "is_topic_message": true})))
	if len(got) != 1 || len(got[0].References) != 2 {
		t.Fatalf("references %+v, want the chat and the topic", got)
	}
	want := model.ExternalRef{Channel: testChannel, Namespace: Namespace, Kind: model.RefKindThread, ID: "3", ParentID: "-1001234567890"}
	if got[0].References[1] != want {
		t.Errorf("thread %+v, want %+v", got[0].References[1], want)
	}
	if !strings.Contains(string(got[0].Payload.Data), `"thread":3`) {
		t.Errorf("payload %s", got[0].Payload.Data)
	}
}

func TestOwnMessagesAreNeverReported(t *testing.T) {
	bot := obj{"id": testBotID, "is_bot": true, "first_name": "uma", "username": testBotName}
	_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(privateChat(testPerson), bot, 9, "echo", nil)))
	if len(got) != 0 {
		t.Fatalf("the bot's own message was reported: %+v", got)
	}
	_, got = serve(t, testConfig(), updateOf(11, "edited_message", textMessage(privateChat(testPerson), bot, 9, "echo 2", obj{"edit_date": testNow.Unix() + 100})))
	if len(got) != 0 {
		t.Fatalf("an edit of the bot's own message was reported: %+v", got)
	}
}

func TestOtherBotsAreAutomatedMessages(t *testing.T) {
	other := obj{"id": 999, "is_bot": true, "first_name": "other", "username": "other_bot"}
	_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), other, 9, "beep", nil)))
	if len(got) != 1 || got[0].Kind != model.EnvelopeAutomatedMessage || got[0].SenderKind != model.ContactKindBot || got[0].Sender.ID != "999" {
		t.Fatalf("got %+v, want an automated message from a bot sender", got)
	}
}

// A message sent on behalf of a chat names the chat: an anonymous
// administrator speaks as the group, and a channel post is the channel's.
func TestMessagesOnBehalfOfAChat(t *testing.T) {
	fake := obj{"id": 1087968824, "is_bot": true, "first_name": "Group", "username": "GroupAnonymousBot"}
	_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), fake, 9, "as admin", obj{"sender_chat": groupChat()})))
	if len(got) != 1 || got[0].Sender != (model.ExternalRef{Channel: testChannel, Namespace: UserNamespace, Kind: RefKindChat, ID: "-1001234567890"}) || got[0].SenderKind != "" || got[0].Kind != model.EnvelopeMessage {
		t.Fatalf("anonymous admin: %+v", got)
	}
	channel := obj{"id": testChannelChat, "type": "channel", "title": "news"}
	post := obj{"message_id": 4, "sender_chat": channel, "chat": channel, "date": testNow.Unix(), "text": "announcement"}
	_, got = serve(t, testConfig(), updateOf(11, "channel_post", post))
	if len(got) != 1 || got[0].Sender.Kind != RefKindChat || got[0].Sender.ID != "-1009876543210" || got[0].MessageID != "-1009876543210:4" {
		t.Fatalf("channel post: %+v", got)
	}
	if !strings.Contains(string(got[0].Payload.Data), `"update_type":"channel_post"`) {
		t.Errorf("payload %s", got[0].Payload.Data)
	}
}

func TestMentionsOnly(t *testing.T) {
	cfg := testConfig()
	cfg.MentionsOnly = true
	botMsg := obj{"message_id": 2, "from": obj{"id": testBotID, "is_bot": true, "first_name": "uma"}, "chat": groupChat(), "date": testNow.Unix(), "text": "earlier"}
	for _, tc := range []struct {
		name string
		chat obj
		text string
		ex   obj
		want bool
	}{
		{"private is always reported", privateChat(testPerson), "plain", nil, true},
		{"group without a mention", groupChat(), "plain", nil, false},
		{"mention", groupChat(), "hi @uma_test_bot there", obj{"entities": []obj{{"type": "mention", "offset": 3, "length": 13}}}, true},
		{"mention in another case", groupChat(), "hi @UMA_TEST_BOT", obj{"entities": []obj{{"type": "mention", "offset": 3, "length": 13}}}, true},
		// "😀" is two UTF-16 code units: the offset counts them.
		{"mention after an emoji", groupChat(), "😀 @uma_test_bot", obj{"entities": []obj{{"type": "mention", "offset": 3, "length": 13}}}, true},
		{"mention of another bot", groupChat(), "hi @other_bot", obj{"entities": []obj{{"type": "mention", "offset": 3, "length": 10}}}, false},
		{"mention without an entity", groupChat(), "hi @uma_test_bot", nil, false},
		{"mention as a prefix of a longer name", groupChat(), "hi @uma_test_bot_2", obj{"entities": []obj{{"type": "mention", "offset": 3, "length": 15}}}, false},
		{"text mention", groupChat(), "hi you", obj{"entities": []obj{{"type": "text_mention", "offset": 3, "length": 3, "user": obj{"id": testBotID, "is_bot": true, "first_name": "uma"}}}}, true},
		{"text mention of someone else", groupChat(), "hi you", obj{"entities": []obj{{"type": "text_mention", "offset": 3, "length": 3, "user": person(5)}}}, false},
		{"reply to the bot", groupChat(), "sure", obj{"reply_to_message": botMsg}, true},
		{"reply to a person", groupChat(), "sure", obj{"reply_to_message": textMessage(groupChat(), person(5), 1, "x", nil)}, false},
		{"mention in a caption", groupChat(), "", obj{"caption": "look @uma_test_bot", "caption_entities": []obj{{"type": "mention", "offset": 5, "length": 13}}, "photo": []obj{{"file_id": "f", "file_unique_id": "u", "width": 10, "height": 10}}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serve(t, cfg, updateOf(10, "message", textMessage(tc.chat, person(testPerson), 9, tc.text, tc.ex)))
			if (len(got) == 1) != tc.want {
				t.Fatalf("reported %d envelopes, want reported=%v", len(got), tc.want)
			}
			if tc.want && tc.chat["type"] != "private" && !strings.Contains(string(got[0].Payload.Data), `"addressed":true`) {
				t.Errorf("payload %s: the message addresses the bot", got[0].Payload.Data)
			}
		})
	}
	// Off by default: the same group message is reported.
	if _, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), person(testPerson), 9, "plain", nil))); len(got) != 1 {
		t.Errorf("without mentions_only a group message was dropped")
	}
}

func TestMediaIsDescribedNeverFetched(t *testing.T) {
	photo := obj{"photo": []obj{
		{"file_id": "small", "file_unique_id": "us", "width": 90, "height": 60, "file_size": 1000},
		{"file_id": "big", "file_unique_id": "ub", "width": 1280, "height": 853, "file_size": 90000},
	}, "caption": "look at this", "text": nil}
	_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(privateChat(testPerson), person(testPerson), 9, "", photo)))
	if len(got) != 1 || len(got[0].Content.Parts) != 2 {
		t.Fatalf("got %+v, want the caption and a media part", got)
	}
	c := got[0].Content
	if c.Parts[0].Kind != model.PartText || c.Parts[0].Text != "look at this" {
		t.Errorf("first part %+v, want the caption", c.Parts[0])
	}
	m := c.Parts[1]
	if m.Kind != model.PartStructured || m.Structured.Name != MediaSchema || m.Structured.Version != MediaVersion {
		t.Fatalf("second part %+v", m)
	}
	for _, want := range []string{`"kind":"photo"`, `"file_id":"big"`, `"file_unique_id":"ub"`, `"width":1280`, `"size":90000`} {
		if !strings.Contains(string(m.Structured.Data), want) {
			t.Errorf("media %s lacks %s", m.Structured.Data, want)
		}
	}
	if strings.Contains(string(m.Structured.Data), "small") {
		t.Errorf("media %s describes a smaller size too", m.Structured.Data)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("content invalid: %v", err)
	}

	doc := obj{"document": obj{"file_id": "d1", "file_unique_id": "ud", "file_name": "report.pdf", "mime_type": "application/pdf", "file_size": 123}, "text": nil}
	_, got = serve(t, testConfig(), updateOf(11, "message", textMessage(privateChat(testPerson), person(testPerson), 10, "", doc)))
	if len(got) != 1 || len(got[0].Content.Parts) != 1 || got[0].Content.Parts[0].Kind != model.PartStructured {
		t.Fatalf("a document alone: %+v", got)
	}
	if d := string(got[0].Content.Parts[0].Structured.Data); !strings.Contains(d, `"name":"report.pdf"`) || !strings.Contains(d, `"mime_type":"application/pdf"`) {
		t.Errorf("document %s", d)
	}

	loc := obj{"location": obj{"latitude": 1.5, "longitude": 2.5}, "text": nil}
	_, got = serve(t, testConfig(), updateOf(12, "message", textMessage(privateChat(testPerson), person(testPerson), 11, "", loc)))
	if len(got) != 1 || !strings.Contains(string(got[0].Content.Parts[0].Structured.Data), `"kind":"location"`) || strings.Contains(string(got[0].Content.Parts[0].Structured.Data), "1.5") {
		t.Fatalf("a location is named by kind alone: %+v", got)
	}
}

func TestMessagesThatSayNothingAreDropped(t *testing.T) {
	for name, extra := range map[string]obj{
		"new members":       {"new_chat_members": []obj{person(5)}, "text": nil},
		"pinned":            {"pinned_message": obj{"message_id": 1}, "text": nil},
		"blank text":        {"text": "  \n "},
		"ephemeral":         {"message_id": 0, "ephemeral_message_id": 3},
		"business":          {"business_connection_id": "bc1"},
		"ephemeral with id": {"ephemeral_message_id": 3},
		// "In specific instances ... the server might automatically schedule
		// a message instead of sending it immediately. In such cases, this
		// field will be 0 and the relevant message will be unusable".
		"scheduled": {"message_id": 0},
	} {
		t.Run(name, func(t *testing.T) {
			resp, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), person(testPerson), 9, "x", extra)))
			if resp.StatusCode != http.StatusOK || len(got) != 0 {
				t.Fatalf("status %d, %d envelopes, want an acknowledged nothing", resp.StatusCode, len(got))
			}
		})
	}
}

func TestUpdatesTheAdapterDoesNotConsumeAreAcknowledged(t *testing.T) {
	for _, field := range []string{"inline_query", "poll", "my_chat_member", "chat_member", "business_message", "guest_message", "message_reaction_count", "pre_checkout_query", "managed_bot"} {
		resp, got := serve(t, testConfig(), obj{"update_id": 10, field: obj{"id": "x"}})
		if resp.StatusCode != http.StatusOK || len(got) != 0 {
			t.Errorf("%s: status %d, %d envelopes", field, resp.StatusCode, len(got))
		}
	}
}

func TestEditedMessage(t *testing.T) {
	m := textMessage(groupChat(), person(testPerson), 9, "second thoughts", obj{"edit_date": testNow.Unix() + 60})
	sink := &sinkRecorder{}
	if resp, _ := serve(t, testConfig(), updateOf(20, "edited_message", m), sink); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(sink.got) != 1 {
		t.Fatalf("%d ingress", len(sink.got))
	}
	e := sink.got[0].Envelope
	if e.Kind != model.EnvelopeMessageEdited || e.EventID != "update:20" || e.MessageID != "-1001234567890:9" || e.Content.Parts[0].Text != "second thoughts" {
		t.Errorf("edit %+v", e)
	}
	if e.SourceRevision != "1700000060" || !e.OccurredAt.Equal(time.Unix(testNow.Unix()+60, 0)) || e.Sender.ID != "424242" || len(e.References) != 0 {
		t.Errorf("revision %q at %v sender %+v references %+v", e.SourceRevision, e.OccurredAt, e.Sender, e.References)
	}
	// An edit of a message uma never saw records the message instead.
	unknown := &sinkRecorder{fail: func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited {
			return model.NewError(model.NotFound, "no such message")
		}
		return nil
	}}
	if resp, _ := serve(t, testConfig(), updateOf(20, "edited_message", m), unknown); resp.StatusCode != http.StatusOK {
		t.Fatalf("fallback: status %d", resp.StatusCode)
	}
	if len(unknown.got) != 1 || unknown.got[0].Envelope.Kind != model.EnvelopeMessage || unknown.got[0].Envelope.EventID != "message:-1001234567890:9" || len(unknown.got[0].Envelope.References) == 0 {
		t.Fatalf("fallback ingress %+v, want the message itself", unknown.got)
	}
	// An edit with no edit_date cannot be ordered.
	if _, got := serve(t, testConfig(), updateOf(21, "edited_message", textMessage(groupChat(), person(testPerson), 9, "x", nil))); len(got) != 0 {
		t.Errorf("an edit without edit_date was reported: %+v", got)
	}
	// Edited channel posts are edits too.
	channel := obj{"id": testChannelChat, "type": "channel"}
	cp := obj{"message_id": 4, "sender_chat": channel, "chat": channel, "date": testNow.Unix(), "edit_date": testNow.Unix() + 5, "text": "fixed"}
	if _, got := serve(t, testConfig(), updateOf(22, "edited_channel_post", cp)); len(got) != 1 || got[0].Kind != model.EnvelopeMessageEdited {
		t.Errorf("edited channel post: %+v", got)
	}
}

func TestReactions(t *testing.T) {
	react := func(id int64, old, new []obj, mod func(obj)) obj {
		r := obj{"chat": groupChat(), "message_id": 9, "user": person(testPerson), "date": testNow.Unix() + 30, "old_reaction": old, "new_reaction": new}
		if mod != nil {
			mod(r)
		}
		return updateOf(id, "message_reaction", r)
	}
	emoji := func(e string) obj { return obj{"type": "emoji", "emoji": e} }

	_, got := serve(t, testConfig(), react(30, nil, []obj{emoji("👍")}, nil))
	if len(got) != 1 || got[0].Kind != model.EnvelopeReactionAdded || got[0].Content.Parts[0].Text != "👍" || got[0].MessageID != "-1001234567890:9" || got[0].Sender.ID != "424242" {
		t.Fatalf("added: %+v", got)
	}
	_, got = serve(t, testConfig(), react(31, []obj{emoji("👍")}, nil, nil))
	if len(got) != 1 || got[0].Kind != model.EnvelopeReactionRemoved || got[0].Content.Parts[0].Text != "👍" {
		t.Fatalf("removed: %+v", got)
	}
	// A change of reaction removes the old emoji and adds the new, in that
	// order, as two events of distinct IDs; one kept is no event.
	_, got = serve(t, testConfig(), react(32, []obj{emoji("👍"), emoji("🔥")}, []obj{emoji("🔥"), emoji("🎉")}, nil))
	if len(got) != 2 || got[0].Kind != model.EnvelopeReactionRemoved || got[0].Content.Parts[0].Text != "👍" ||
		got[1].Kind != model.EnvelopeReactionAdded || got[1].Content.Parts[0].Text != "🎉" || got[0].EventID == got[1].EventID {
		t.Fatalf("changed: %+v", got)
	}
	// A redelivery repeats the same IDs.
	_, again := serve(t, testConfig(), react(32, []obj{emoji("👍"), emoji("🔥")}, []obj{emoji("🔥"), emoji("🎉")}, nil))
	if len(again) != 2 || again[0].EventID != got[0].EventID || again[1].EventID != got[1].EventID {
		t.Errorf("event IDs differ on redelivery: %+v vs %+v", again, got)
	}
	// Custom emoji and paid reactions have no emoji to report.
	_, got = serve(t, testConfig(), react(33, nil, []obj{{"type": "custom_emoji", "custom_emoji_id": "5"}, {"type": "paid"}}, nil))
	if len(got) != 0 {
		t.Errorf("custom reactions reported: %+v", got)
	}
	// The bot's own reaction is never reported.
	_, got = serve(t, testConfig(), react(34, nil, []obj{emoji("👍")}, func(r obj) { r["user"] = obj{"id": testBotID, "is_bot": true} }))
	if len(got) != 0 {
		t.Errorf("own reaction reported: %+v", got)
	}
	// An anonymous reactor is a chat.
	_, got = serve(t, testConfig(), react(35, nil, []obj{emoji("👍")}, func(r obj) { delete(r, "user"); r["actor_chat"] = groupChat() }))
	if len(got) != 1 || got[0].Sender.Kind != RefKindChat {
		t.Errorf("anonymous reactor: %+v", got)
	}
	// A reaction on a message uma does not hold is dropped, not an error.
	unknown := &sinkRecorder{fail: func(model.Ingress) error { return model.NewError(model.NotFound, "no such message") }}
	if resp, _ := serve(t, testConfig(), react(36, nil, []obj{emoji("👍")}, nil), unknown); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200 for a reaction on an unknown message", resp.StatusCode)
	}
}

func TestEnvelopesValidate(t *testing.T) {
	// Every envelope the adapter makes is one core accepts as shaped: IDs
	// of a group chat are negative numbers.
	_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), person(testPerson), 9, "x", obj{"message_thread_id": 2})))
	if len(got) != 1 {
		t.Fatal("no envelope")
	}
	for _, r := range got[0].References {
		if err := r.Validate(); err != nil {
			t.Errorf("reference %+v: %v", r, err)
		}
	}
	if err := got[0].Sender.Validate(); err != nil {
		t.Errorf("sender: %v", err)
	}
}

// A message replying to one reports it; in a forum topic, Telegram's
// reply_to_message of a plain topic post is the topic's own root, which is
// thread membership and no reply.
func TestReplyParentReported(t *testing.T) {
	parent := textMessage(groupChat(), person(5), 7, "announce", nil)
	for _, tc := range []struct {
		name string
		ex   obj
		want string
	}{
		{"reply", obj{"reply_to_message": parent}, strconv.FormatInt(testGroup, 10) + ":7"},
		{"no reply", nil, ""},
		{"topic root", obj{"message_thread_id": 7, "is_topic_message": true, "reply_to_message": parent}, ""},
		{"reply inside a topic", obj{"message_thread_id": 3, "is_topic_message": true, "reply_to_message": parent}, strconv.FormatInt(testGroup, 10) + ":7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serve(t, testConfig(), updateOf(10, "message", textMessage(groupChat(), person(testPerson), 9, "approve", tc.ex)))
			if len(got) != 1 || got[0].InReplyTo != tc.want {
				t.Fatalf("got %+v, want InReplyTo %q", got, tc.want)
			}
		})
	}
}
