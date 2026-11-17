package teams

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"hop.top/uma/model"
)

func only(t *testing.T, r *rig) model.Envelope {
	t.Helper()
	envs := r.sink.envelopes()
	if len(envs) != 1 {
		t.Fatalf("envelopes = %d, want 1: %+v", len(envs), envs)
	}
	return envs[0]
}

func TestPersonalMessageMapped(t *testing.T) {
	r := newRig(t)
	if rec := r.post(t, r.message("1001", "hello <b>there</b>")); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	env := only(t, r)
	id := testPersonal + "|1001"
	switch {
	case env.Kind != model.EnvelopeMessage, env.EventID != "message:"+id, env.MessageID != id:
		t.Fatalf("envelope %+v", env)
	case env.Destination != testTenantID, env.Sender != r.x.userRef(testUser), env.SenderKind != "", env.Assurance != "verified":
		t.Fatalf("envelope routing %+v", env)
	case len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != "hello <b>there</b>":
		t.Fatalf("content %+v", env.Content)
	case len(env.References) != 1 || env.References[0] != r.x.roomRef(testPersonal):
		t.Fatalf("references %+v, want the personal chat only", env.References)
	case !env.OccurredAt.Equal(testNow):
		t.Fatalf("occurred %v", env.OccurredAt)
	}
	var p map[string]any
	_ = json.Unmarshal(env.Payload.Data, &p)
	if env.Payload.Name != PayloadSchema || p["conversation_type"] != "personal" || p["aad_object_id"] != "aad-alice" || p["mentioned"] != false {
		t.Fatalf("payload %s %s", env.Payload.Name, env.Payload.Data)
	}
}

func TestChannelMessagesScopedToTheirThread(t *testing.T) {
	r := newRig(t)
	root := r.activityOf("message", "500", testUser, testTeamChannel+";messageid=500", "channel")
	root["text"] = "<at>uma</at> help"
	root["entities"] = []any{map[string]any{"type": "mention", "mentioned": map[string]any{"id": testBot, "name": "uma"}, "text": "<at>uma</at>"}}
	reply := r.activityOf("message", "501", testOther, testTeamChannel+";messageid=500", "channel")
	reply["text"] = "me too"
	bare := r.activityOf("message", "600", testOther, testTeamChannel, "channel")
	bare["text"] = "new post"
	for _, a := range []map[string]any{root, reply, bare} {
		if rec := r.post(t, a); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	envs := r.sink.envelopes()
	if len(envs) != 3 {
		t.Fatalf("envelopes %d", len(envs))
	}
	room := r.x.roomRef(testTeamChannel)
	for i, want := range []struct{ id, root string }{{"500", "500"}, {"501", "500"}, {"600", "600"}} {
		env := envs[i]
		if env.MessageID != testTeamChannel+"|"+want.id {
			t.Fatalf("message %d id %s: the base conversation names it", i, env.MessageID)
		}
		if len(env.References) != 2 || env.References[0] != room || env.References[1] != r.x.threadRef(testTeamChannel, want.root) {
			t.Fatalf("message %d references %+v, want the channel and thread %s", i, env.References, want.root)
		}
	}
	var p map[string]any
	_ = json.Unmarshal(envs[0].Payload.Data, &p)
	if p["mentioned"] != true {
		t.Fatalf("payload %s: the mention entity names the bot", envs[0].Payload.Data)
	}
}

func TestBotsOwnActivitiesNeverReported(t *testing.T) {
	r := newRig(t)
	own := r.activityOf("message", "1001", testBot, testPersonal, "personal")
	own["text"] = "echo"
	asRecipient := r.activityOf("message", "1002", "28:other-registration", testPersonal, "personal")
	asRecipient["recipient"] = map[string]any{"id": "28:other-registration"}
	asRecipient["text"] = "echo"
	edit := r.activityOf("messageUpdate", "1001", testBot, testPersonal, "personal")
	edit["text"], edit["channelData"] = "edited", map[string]any{"eventType": "editMessage", "tenant": map[string]any{"id": testTenantID}}
	del := r.activityOf("messageDelete", "1001", testBot, testPersonal, "personal")
	del["channelData"] = map[string]any{"eventType": "softDeleteMessage", "tenant": map[string]any{"id": testTenantID}}
	react := r.activityOf("messageReaction", "f:1", testBot, testPersonal, "personal")
	react["replyToId"], react["reactionsAdded"] = "1000", []any{map[string]any{"type": "like"}}
	// The bot's account, whatever the activity names as its recipient.
	noRecipient := r.activityOf("message", "1003", testBot, testPersonal, "personal")
	noRecipient["recipient"] = map[string]any{}
	noRecipient["text"] = "echo"
	for _, a := range []map[string]any{own, asRecipient, noRecipient, edit, del, react} {
		if rec := r.post(t, a); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if envs := r.sink.envelopes(); len(envs) != 0 {
		t.Fatalf("the bot's own activities were reported: %+v", envs)
	}
}

func TestOtherBotsAreAutomatedBotSenders(t *testing.T) {
	r := newRig(t)
	a := r.activityOf("message", "1001", "28:another-bot", testGroupChat, "groupChat")
	a["text"] = "beep"
	r.post(t, a)
	env := only(t, r)
	if env.Kind != model.EnvelopeAutomatedMessage || env.SenderKind != model.ContactKindBot {
		t.Fatalf("envelope %s/%s, want an automated message from a bot", env.Kind, env.SenderKind)
	}
}

func TestOtherTenantsAndChannelsDropped(t *testing.T) {
	r := newRig(t)
	other := r.message("1001", "hi")
	other["channelData"] = map[string]any{"tenant": map[string]any{"id": "00000000-0000-4000-8000-0000000000ee"}}
	other["conversation"] = map[string]any{"id": testPersonal, "conversationType": "personal", "tenantId": "00000000-0000-4000-8000-0000000000ee"}
	webchat := r.message("1002", "hi")
	webchat["channelId"] = "webchat"
	k, _ := signingKeys()
	r.f.addKey("kid-webchat", k, true, "webchat")
	webchatKey := bearer(r.f.token(testAppID, r.f.serviceURL(testPrefix), func(_ jwt.MapClaims, kid *string) { *kid = "kid-webchat" }))
	if rec := r.post(t, other); rec.Code != http.StatusOK {
		t.Fatalf("other tenant: status %d, want 200: acknowledged, not this channel's", rec.Code)
	}
	if rec := r.post(t, webchat, webchatKey); rec.Code != http.StatusOK {
		t.Fatalf("other channel: status %d, want 200: acknowledged, not this channel's", rec.Code)
	}
	if envs := r.sink.envelopes(); len(envs) != 0 {
		t.Fatalf("envelopes %+v, want none", envs)
	}
}

func TestAttachmentOnlyMessageDescribed(t *testing.T) {
	r := newRig(t)
	a := r.message("1001", "")
	a["attachments"] = []any{
		map[string]any{"contentType": "text/html", "content": "<p></p>"},
		map[string]any{"contentType": "application/vnd.microsoft.teams.file.download.info", "name": "report.pdf", "contentUrl": "https://example.invalid/secret"},
	}
	r.post(t, a)
	env := only(t, r)
	p := env.Content.Parts
	if len(p) != 1 || p[0].Kind != model.PartStructured || p[0].Structured.Name != AttachmentsSchema {
		t.Fatalf("content %+v", env.Content)
	}
	if s := string(p[0].Structured.Data); !strings.Contains(s, "report.pdf") || strings.Contains(s, "secret") || strings.Contains(s, "text/html") {
		t.Fatalf("attachments %s: names and types only", s)
	}
	// A legacy Action.Submit: data and no text, nothing to say.
	submit := r.message("1002", "")
	submit["value"] = map[string]any{"choice": "yes"}
	r.post(t, submit)
	if n := len(r.sink.envelopes()); n != 1 {
		t.Fatalf("envelopes = %d: a value-only message is not reported", n)
	}
}

func TestEditDeleteAndReactionsMapped(t *testing.T) {
	r := newRig(t)
	edit := r.activityOf("messageUpdate", "1001", testUser, testPersonal, "personal")
	edit["text"] = "fixed"
	edit["channelData"] = map[string]any{"eventType": "editMessage", "tenant": map[string]any{"id": testTenantID}}
	undelete := r.activityOf("messageUpdate", "1001", testUser, testPersonal, "personal")
	undelete["text"] = "back"
	undelete["channelData"] = map[string]any{"eventType": "undeleteMessage", "tenant": map[string]any{"id": testTenantID}}
	del := r.activityOf("messageDelete", "1001", testUser, testPersonal, "personal")
	del["channelData"] = map[string]any{"eventType": "softDeleteMessage", "tenant": map[string]any{"id": testTenantID}}
	react := r.activityOf("messageReaction", "f:9f78d1f3", testUser, testPersonal, "personal")
	react["replyToId"] = "1001"
	react["reactionsAdded"] = []any{map[string]any{"type": "like"}, map[string]any{"type": "1f440_eyes"}, map[string]any{"type": "unheard-of"}}
	react["reactionsRemoved"] = []any{map[string]any{"type": "heart"}}
	otherDelete := r.activityOf("messageDelete", "1001", testUser, testPersonal, "personal")
	otherDelete["channelData"] = map[string]any{"eventType": "hardDeleteMessage", "tenant": map[string]any{"id": testTenantID}}
	for _, a := range []map[string]any{edit, undelete, otherDelete, del, react} {
		if rec := r.post(t, a); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	envs := r.sink.envelopes()
	id := testPersonal + "|1001"
	if len(envs) != 5 {
		t.Fatalf("envelopes = %d, want edit, delete and three reactions: %+v", len(envs), envs)
	}
	if e := envs[0]; e.Kind != model.EnvelopeMessageEdited || e.MessageID != id || e.Content.Parts[0].Text != "fixed" || e.SourceRevision == "" || e.Sender != r.x.userRef(testUser) {
		t.Fatalf("edit %+v", e)
	}
	if e := envs[1]; e.Kind != model.EnvelopeMessageDeleted || e.MessageID != id || e.Sender != r.x.userRef(testUser) {
		t.Fatalf("delete %+v", e)
	}
	for i, want := range []struct{ kind, emoji string }{
		{model.EnvelopeReactionAdded, "👍"}, {model.EnvelopeReactionAdded, "👀"}, {model.EnvelopeReactionRemoved, "❤️"},
	} {
		e := envs[2+i]
		if e.Kind != want.kind || e.MessageID != id || e.Content.Parts[0].Text != want.emoji || e.Sender != r.x.userRef(testUser) {
			t.Fatalf("reaction %d %+v, want %s %s", i, e, want.kind, want.emoji)
		}
	}
	if envs[2].EventID == envs[3].EventID {
		t.Fatal("two reactions of one activity share an event ID")
	}
}

func TestEditsWithDifferentTextHaveDifferentEventIDs(t *testing.T) {
	r := newRig(t)
	for _, text := range []string{"one", "two", "one"} {
		e := r.activityOf("messageUpdate", "1001", testUser, testPersonal, "personal")
		e["text"] = text
		e["channelData"] = map[string]any{"eventType": "editMessage", "tenant": map[string]any{"id": testTenantID}}
		r.post(t, e)
	}
	envs := r.sink.envelopes()
	if envs[0].EventID == envs[1].EventID || envs[0].EventID != envs[2].EventID {
		t.Fatalf("event IDs %s %s %s: the same edit is one event, another text another", envs[0].EventID, envs[1].EventID, envs[2].EventID)
	}
}

func TestEditOfAnUnknownMessageRecordsIt(t *testing.T) {
	r := newRig(t)
	r.sink.fail = func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited {
			return model.NewError(model.NotFound, "no such message")
		}
		return nil
	}
	e := r.activityOf("messageUpdate", "1001", testUser, testPersonal, "personal")
	e["text"] = "fixed"
	e["channelData"] = map[string]any{"eventType": "editMessage", "tenant": map[string]any{"id": testTenantID}}
	if rec := r.post(t, e); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if env := only(t, r); env.Kind != model.EnvelopeMessage || env.Content.Parts[0].Text != "fixed" {
		t.Fatalf("fallback %+v, want the message as first seen", env)
	}
	// A reaction to a message the sink does not hold is dropped.
	r.sink.fail = func(model.Ingress) error { return model.NewError(model.NotFound, "no such message") }
	react := r.activityOf("messageReaction", "f:1", testUser, testPersonal, "personal")
	react["replyToId"], react["reactionsAdded"] = "999", []any{map[string]any{"type": "like"}}
	if rec := r.post(t, react); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 for a reaction to an unknown message", rec.Code)
	}
}

func TestSinkRefusalsAnswered(t *testing.T) {
	for _, c := range []struct {
		code model.ErrorCode
		want int
	}{{model.Unavailable, http.StatusServiceUnavailable}, {model.Validation, http.StatusUnprocessableEntity}} {
		r := newRig(t)
		r.sink.fail = func(model.Ingress) error { return model.NewError(c.code, "refused") }
		if rec := r.post(t, r.message("1001", "hi")); rec.Code != c.want {
			t.Fatalf("%s: status %d, want %d", c.code, rec.Code, c.want)
		}
	}
}

func TestHandlerRequestShape(t *testing.T) {
	r := newRig(t)
	get := httptest.NewRequest(http.MethodGet, "/api/messages", nil)
	rec := httptest.NewRecorder()
	r.x.Handler(r.sink).ServeHTTP(rec, get)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}
	bad := httptest.NewRequest(http.MethodPost, "/api/messages", strings.NewReader("{"))
	bad.Header.Set("Authorization", "Bearer x")
	rec = httptest.NewRecorder()
	r.x.Handler(r.sink).ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed: %d", rec.Code)
	}
	malformedIDs := r.message("bad id", "hi")
	if rec := r.post(t, malformedIDs); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed activity ID: %d", rec.Code)
	}
	_ = r.x.Close(t.Context())
	if rec := r.post(t, r.message("1001", "hi")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed: %d", rec.Code)
	}
	if n := len(r.sink.envelopes()); n != 0 {
		t.Fatalf("envelopes %d", n)
	}
}

func TestUnconsumedActivitiesAcknowledged(t *testing.T) {
	r := newRig(t)
	for _, kind := range []string{"conversationUpdate", "installationUpdate", "typing", "event"} {
		if rec := r.post(t, r.activityOf(kind, "1", testUser, testPersonal, "personal")); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", kind, rec.Code)
		}
	}
	if n := len(r.sink.envelopes()); n != 0 {
		t.Fatalf("envelopes %d", n)
	}
}

// A message naming the activity it replies to reports it, in the message
// ID form of its conversation; one naming none, or an invalid one, reports
// no parent.
func TestMessageReplyParentReported(t *testing.T) {
	r := newRig(t)
	reply := r.message("1002", "approve")
	reply["replyToId"] = "1001"
	if rec := r.post(t, reply); rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if env := only(t, r); env.InReplyTo != testPersonal+"|1001" {
		t.Fatalf("InReplyTo = %q, want the replied-to activity", env.InReplyTo)
	}
	bad := r.message("1003", "x")
	bad["replyToId"] = "a b"
	r.post(t, bad)
	plain := r.message("1004", "y")
	r.post(t, plain)
	for _, env := range r.sink.envelopes()[1:] {
		if env.InReplyTo != "" {
			t.Fatalf("%s InReplyTo = %q, want none", env.MessageID, env.InReplyTo)
		}
	}
}
