package discord

import (
	"encoding/json"
	"net/http"
	"testing"

	"hop.top/uma/model"
)

// msgEvent is a MESSAGE_CREATE payload by author in channel, in a guild
// unless guild is empty.
func msgEvent(id, channel, guild, author, content string, extra map[string]any) map[string]any {
	m := map[string]any{
		"id": id, "channel_id": channel, "type": 0, "content": content, "timestamp": testNow.Format("2006-01-02T15:04:05.000000+00:00"),
		"author": map[string]any{"id": author, "username": "someone"}, "mentions": []any{}, "attachments": []any{},
	}
	if guild != "" {
		m["guild_id"] = guild
	}
	for k, v := range extra {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

// handleJob runs one dispatch through the receiver's handling.
func (r *rig) handleJob(t *testing.T, sink *sinkRecorder, typ string, d any, seq int64) error {
	t.Helper()
	raw, _ := json.Marshal(d)
	return r.x.handle(t.Context(), sink, laneJob{seq: seq, session: "sess-1", typ: typ, data: raw, cursor: "c"})
}

func mention(id string) map[string]any {
	return map[string]any{"mentions": []any{map[string]any{"id": id}}}
}

func TestMessageMapping(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "seed", "")
	r.f.thread("acct1", testRoom, testThread)
	no := false
	all := newRig(t, func(c *Config) { c.MentionsOnly = &no })
	all.f.put("acct1", testRoom, testPerson, "seed", "")
	cases := []struct {
		name   string
		r      *rig
		ev     map[string]any
		kind   string
		sender string
		refs   int
	}{
		{"guild mention", r, msgEvent("1200000000000000001", testRoom, testGuild, testPerson, "<@"+testBotUser+"> help", mention(testBotUser)), model.EnvelopeMessage, testPerson, 1},
		{"deprecated mention form", r, msgEvent("1200000000000000002", testRoom, testGuild, testPerson, "<@!"+testBotUser+"> help", nil), model.EnvelopeMessage, testPerson, 1},
		{"guild without mention, mentions only", r, msgEvent("1200000000000000003", testRoom, testGuild, testPerson, "chatter", nil), "", "", 0},
		{"guild without mention, all messages", all, msgEvent("1200000000000000004", testRoom, testGuild, testPerson, "chatter", nil), model.EnvelopeMessage, testPerson, 1},
		{"content withheld", all, msgEvent("1200000000000000005", testRoom, testGuild, testPerson, "", nil), "", "", 0},
		{"direct message", r, msgEvent("1200000000000000006", "1100000000000000099", "", testPerson, "hi", nil), model.EnvelopeMessage, testPerson, 1},
		{"thread", r, msgEvent("1200000000000000007", testThread, testGuild, testPerson, "in thread", mention(testBotUser)), model.EnvelopeMessage, testPerson, 2},
		{"own echo", r, msgEvent("1200000000000000008", testRoom, testGuild, testBotUser, "<@"+testBotUser+">", mention(testBotUser)), "", "", 0},
		{"own interaction webhook", r, msgEvent("1200000000000000009", testRoom, testGuild, "1100000000000000077", "x", map[string]any{"webhook_id": testApp, "mentions": []any{map[string]any{"id": testBotUser}}}), "", "", 0},
		{"other bot", r, msgEvent("1200000000000000010", testRoom, testGuild, testOther, "beep", map[string]any{"author": map[string]any{"id": testOther, "bot": true}, "mentions": []any{map[string]any{"id": testBotUser}}}), model.EnvelopeAutomatedMessage, testOther, 1},
		{"webhook message", r, msgEvent("1200000000000000011", testRoom, testGuild, testOther, "hook", map[string]any{"webhook_id": "1100000000000000055", "mentions": []any{map[string]any{"id": testBotUser}}}), model.EnvelopeAutomatedMessage, testOther, 1},
		{"system message", r, msgEvent("1200000000000000012", testRoom, testGuild, testPerson, "joined", map[string]any{"type": 7, "mentions": []any{map[string]any{"id": testBotUser}}}), "", "", 0},
		{"reply", r, msgEvent("1200000000000000013", testRoom, testGuild, testPerson, "re", map[string]any{"type": 19, "mentions": []any{map[string]any{"id": testBotUser}}, "message_reference": map[string]any{"message_id": "1200000000000000001"}}), model.EnvelopeMessage, testPerson, 1},
		{"attachments only", r, msgEvent("1200000000000000014", "1100000000000000099", "", testPerson, "", map[string]any{"attachments": []any{map[string]any{"id": "1300000000000000001", "filename": "a.png", "content_type": "image/png", "size": 10}}}), model.EnvelopeMessage, testPerson, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &sinkRecorder{}
			if err := c.r.handleJob(t, sink, "MESSAGE_CREATE", c.ev, 5); err != nil {
				t.Fatalf("handle: %v", err)
			}
			envs := sink.envelopes()
			if c.kind == "" {
				if len(envs) != 0 {
					t.Fatalf("reported %+v, want nothing", envs)
				}
				return
			}
			if len(envs) != 1 {
				t.Fatalf("envelopes = %+v", envs)
			}
			e := envs[0]
			id := messageID(c.ev["channel_id"].(string), c.ev["id"].(string))
			if e.Kind != c.kind || e.Sender.ID != c.sender || e.Sender.Namespace != UserNamespace || e.MessageID != id || e.EventID != "message:"+id || e.Destination != testApp || len(e.References) != c.refs {
				t.Fatalf("envelope = %+v", e)
			}
			if (c.kind == model.EnvelopeAutomatedMessage) != (e.SenderKind == model.ContactKindBot) {
				t.Fatalf("sender kind %q for %s", e.SenderKind, c.kind)
			}
			if c.refs == 2 && (e.References[0] != r.x.roomRef(testRoom) || e.References[1] != r.x.threadRef(testRoom, testThread)) {
				t.Fatalf("thread references = %+v", e.References)
			}
			if sink.ingresses()[0].ReceiverCursor != "c" {
				t.Fatal("ingress without the receiver cursor")
			}
		})
	}
	// Only a reply answers the message it references.
	for name, c := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"reply":          {map[string]any{"type": 19, "message_reference": map[string]any{"message_id": "1200000000000000001"}}, testRoom + ":1200000000000000001"},
		"reply, channel": {map[string]any{"type": 19, "message_reference": map[string]any{"message_id": "1200000000000000001", "channel_id": testThread}}, testThread + ":1200000000000000001"},
		"forward":        {map[string]any{"type": 0, "message_reference": map[string]any{"message_id": "1200000000000000001"}}, ""},
		"no reference":   {map[string]any{"type": 19}, ""},
	} {
		c.extra["mentions"] = []any{map[string]any{"id": testBotUser}}
		sink := &sinkRecorder{}
		if err := r.handleJob(t, sink, "MESSAGE_CREATE", msgEvent("1200000000000000030", testRoom, testGuild, testPerson, "re", c.extra), 7); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if envs := sink.envelopes(); len(envs) != 1 || envs[0].InReplyTo != c.want {
			t.Fatalf("%s: envelopes %+v, want InReplyTo %q", name, envs, c.want)
		}
	}
	// The thread's parent was read once and remembered.
	sink := &sinkRecorder{}
	_ = r.handleJob(t, sink, "MESSAGE_CREATE", msgEvent("1200000000000000020", testThread, testGuild, testPerson, "again", mention(testBotUser)), 6)
	if n := len(r.callsOf(http.MethodGet, "channels/"+testThread)); n != 1 {
		t.Fatalf("Get Channel of the thread %d times, want once", n)
	}
}

func TestEditDeleteReaction(t *testing.T) {
	r := newRig(t)
	r.f.put("acct1", testRoom, testPerson, "seed", "")
	sink := &sinkRecorder{}
	edited := "2026-10-13T12:01:00.000000+00:00"
	ev := msgEvent("1200000000000000001", testRoom, testGuild, testPerson, "<@"+testBotUser+"> fixed", map[string]any{"edited_timestamp": edited, "mentions": []any{map[string]any{"id": testBotUser}}})
	if err := r.handleJob(t, sink, "MESSAGE_UPDATE", ev, 7); err != nil {
		t.Fatal(err)
	}
	envs := sink.envelopes()
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeMessageEdited || envs[0].SourceRevision != edited || envs[0].Sender.ID != testPerson || envs[0].Content.Parts[0].Text != "<@"+testBotUser+"> fixed" {
		t.Fatalf("edit = %+v", envs)
	}
	// An edit of a message the sink does not hold records the message.
	sink = &sinkRecorder{fail: func(in model.Ingress) error {
		if in.Envelope.Kind == model.EnvelopeMessageEdited {
			return model.NewError(model.NotFound, "unknown message")
		}
		return nil
	}}
	_ = r.handleJob(t, sink, "MESSAGE_UPDATE", ev, 8)
	if envs := sink.envelopes(); len(envs) != 1 || envs[0].Kind != model.EnvelopeMessage {
		t.Fatalf("fallback = %+v", envs)
	}
	// An update without edited_timestamp (an embed unfurled) is dropped.
	sink = &sinkRecorder{}
	_ = r.handleJob(t, sink, "MESSAGE_UPDATE", msgEvent("1200000000000000001", testRoom, testGuild, testPerson, "<@"+testBotUser+">", mention(testBotUser)), 9)
	if len(sink.envelopes()) != 0 {
		t.Fatal("unfurl reported as an edit")
	}

	_ = r.handleJob(t, sink, "MESSAGE_DELETE", map[string]any{"id": "1200000000000000001", "channel_id": testRoom, "guild_id": testGuild}, 10)
	if envs := sink.envelopes(); len(envs) != 1 || envs[0].Kind != model.EnvelopeMessageDeleted || !envs[0].SystemActor || envs[0].MessageID != messageID(testRoom, "1200000000000000001") {
		t.Fatalf("delete = %+v", envs)
	}

	reaction := func(user, name string, id any) map[string]any {
		return map[string]any{"user_id": user, "channel_id": testRoom, "message_id": "1200000000000000001", "guild_id": testGuild, "emoji": map[string]any{"id": id, "name": name}}
	}
	sink = &sinkRecorder{}
	_ = r.handleJob(t, sink, "MESSAGE_REACTION_ADD", reaction(testPerson, "👍", nil), 11)
	_ = r.handleJob(t, sink, "MESSAGE_REACTION_REMOVE", reaction(testPerson, "👍", nil), 12)
	_ = r.handleJob(t, sink, "MESSAGE_REACTION_ADD", reaction(testPerson, "👍", nil), 13)
	_ = r.handleJob(t, sink, "MESSAGE_REACTION_ADD", reaction(testPerson, "party", "1400000000000000001"), 14)
	_ = r.handleJob(t, sink, "MESSAGE_REACTION_ADD", reaction(testBotUser, "👍", nil), 15)
	envs = sink.envelopes()
	if len(envs) != 3 || envs[0].Kind != model.EnvelopeReactionAdded || envs[1].Kind != model.EnvelopeReactionRemoved || envs[2].Kind != model.EnvelopeReactionAdded {
		t.Fatalf("reactions = %+v", envs)
	}
	if envs[0].EventID == envs[2].EventID || envs[0].Content.Parts[0].Text != "👍" {
		t.Fatalf("two adds share event ID %q, or wrong emoji", envs[0].EventID)
	}
	// A reaction to a message the sink does not hold is dropped.
	sink = &sinkRecorder{fail: func(model.Ingress) error { return model.NewError(model.NotFound, "unknown") }}
	if err := r.handleJob(t, sink, "MESSAGE_REACTION_ADD", reaction(testPerson, "👍", nil), 16); err != nil {
		t.Fatalf("unknown target = %v, want dropped", err)
	}
}

// A sink refusal comes back to the receiver, which decides what it ends.
func TestHandleReturnsRefusal(t *testing.T) {
	r := newRig(t)
	sink := &sinkRecorder{fail: func(model.Ingress) error { return model.NewError(model.Transient, "busy") }}
	err := r.handleJob(t, sink, "MESSAGE_CREATE", msgEvent("1200000000000000001", "1100000000000000099", "", testPerson, "hi", nil), 1)
	if !model.IsCode(err, model.Transient) {
		t.Fatalf("handle = %v, want the refusal", err)
	}
	// A thread lookup Discord refuses is the dispatch's retryable failure.
	r.f.respond("acct1")
	r.f.mu.Lock()
	r.acct.channels = map[string]*fakeChannel{}
	r.f.mu.Unlock()
	err = r.handleJob(t, &sinkRecorder{}, "MESSAGE_CREATE", msgEvent("1200000000000000002", testThread, testGuild, testPerson, "x", mention(testBotUser)), 2)
	if !model.IsCode(err, model.NotFound) {
		t.Fatalf("handle of an unreadable channel = %v, want not found", err)
	}
}
