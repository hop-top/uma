package github_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// commentEvent is an issue_comment delivery of the golden created payload
// with the action, comment (id, author, body) and sender changed. reactions
// is the comment's reaction rollup; nil leaves it out.
func commentEvent(t *testing.T, action string, commentID, author, sender int64, body string, reactions map[string]any) []byte {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal(golden(t, "issue_comment-edited"), &ev); err != nil {
		t.Fatal(err)
	}
	ev["action"] = action
	c := ev["comment"].(map[string]any)
	c["id"], c["body"] = commentID, body
	c["user"] = map[string]any{"login": "someone", "id": author, "type": "User"}
	if reactions != nil {
		c["reactions"] = reactions
	}
	ev["sender"] = map[string]any{"login": "editor", "id": sender, "type": "User"}
	b, _ := json.Marshal(ev)
	return b
}

// An edit uma made reaches the webhook with the marker of its attempt in
// the body: the delivery names that attempt, so the engine settles the
// edit from it, and the content has no marker.
func TestOwnEditWebhookIsEcho(t *testing.T) {
	r := newSendRig(t)
	c := r.own(tryOne)
	if out, err := r.edit(tryTwo, target(c.ID), "Corrected answer."); err != nil || out.State != model.ProviderAccepted {
		t.Fatalf("Edit = %+v, %v", out, err)
	}
	stored := r.gh.stored("a", issueKey)[0]
	h := newHookRig(t, replyConfig(""))
	if code, text := h.serve(hookRequest("issue_comment", "d-edit-echo", commentEvent(t, "edited", stored.ID, botID, botID, stored.Body, nil))); code != http.StatusOK {
		t.Fatalf("delivery = %d %q", code, text)
	}
	envs := h.sink.envelopes()
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeMessageEdited {
		t.Fatalf("envelopes = %+v, want the edit", envs)
	}
	e := envs[0]
	if e.MessageID != "comment:"+strconv.FormatInt(stored.ID, 10) || e.Sender != userRef(strconv.FormatInt(botID, 10)) || e.Content.Parts[0].Text != "Corrected answer." {
		t.Fatalf("envelope = %+v", e)
	}
	echo := model.EchoRef(chanA, opA, tryTwo)
	var got []model.ExternalRef
	for _, ref := range e.References {
		if ref.Namespace == model.EchoNamespace {
			got = append(got, ref)
		}
	}
	if len(got) != 1 || got[0] != echo {
		t.Fatalf("echo references = %+v, want the edit attempt %+v", got, echo)
	}
}

// Another user's edit, or a copied marker in a comment the account did not
// write, names no attempt.
func TestEditWebhookNamesNoAttemptOfOthers(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	body := "text\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryTwo) + " -->"
	h.serve(hookRequest("issue_comment", "d-forged", commentEvent(t, "edited", 2000000011, 5000001, 5000001, body, nil)))
	h.serve(hookRequest("issue_comment", "d-moderated", commentEvent(t, "edited", 2000000012, botID, 5000002, body, nil)))
	for _, e := range h.sink.envelopes() {
		for _, ref := range e.References {
			if ref.Namespace == model.EchoNamespace {
				t.Fatalf("envelope %s names attempt %+v; only the account's own edit echoes", e.EventID, ref)
			}
		}
	}
}

// A maintainer editing a contributor's comment is the editor: the
// delivery's sender is the user who edited, not the comment's author.
func TestOthersEditWebhookNamesEditor(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	h.serve(hookRequest("issue_comment", "d-others", commentEvent(t, "edited", 2000000013, 5000001, 5000002, "Moderated.", nil)))
	envs := h.sink.envelopes()
	if len(envs) != 1 || envs[0].Kind != model.EnvelopeMessageEdited {
		t.Fatalf("envelopes = %+v", envs)
	}
	if envs[0].Sender != userRef("5000002") {
		t.Fatalf("sender = %+v, want the editing user 5000002, not the author", envs[0].Sender)
	}
	for _, ref := range envs[0].References {
		if ref.Namespace == model.EchoNamespace {
			t.Fatalf("references %+v: another user's edit is no echo", envs[0].References)
		}
	}
}

func TestEditedDescriptionStripsMarkerAndEchoes(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	var ev map[string]any
	if err := json.Unmarshal(golden(t, "issues-edited"), &ev); err != nil {
		t.Fatal(err)
	}
	iss := ev["issue"].(map[string]any)
	iss["body"] = "New description.\n\n<!-- uma:" + string(opA.ID) + ":" + string(tryTwo) + " -->"
	iss["user"] = map[string]any{"login": "uma-sandbox[bot]", "id": botID, "type": "Bot"}
	ev["sender"] = map[string]any{"login": "uma-sandbox[bot]", "id": botID, "type": "Bot"}
	b, _ := json.Marshal(ev)
	if code, text := h.serve(hookRequest("issues", "d-desc", b)); code != http.StatusOK {
		t.Fatalf("delivery = %d %q", code, text)
	}
	envs := h.sink.envelopes()
	if len(envs) != 2 || envs[1].Kind != model.EnvelopeMessageEdited || envs[1].Content.Parts[0].Text != "New description." {
		t.Fatalf("envelopes = %+v, want the edit with no marker in its content", envs)
	}
	found := false
	for _, ref := range envs[1].References {
		found = found || ref == model.EchoRef(chanA, opA, tryTwo)
	}
	if !found {
		t.Fatalf("references = %+v, want the edit attempt's echo", envs[1].References)
	}
}

func summaryOf(t *testing.T, e model.Envelope) model.ReactionSummary {
	t.Helper()
	if e.Kind != model.EnvelopeReactionAdded || !e.SystemActor || !e.Sender.IsZero() || len(e.Content.Parts) != 1 || e.Content.Parts[0].Structured == nil {
		t.Fatalf("envelope = %+v, want a system actor's reaction summary", e)
	}
	s, err := model.DecodeReactionSummary(*e.Content.Parts[0].Structured)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// GitHub sends no reaction webhooks: counts arrive only in the rollup of a
// comment payload, as a count-only snapshot.
func TestReactionCountsBecomeSummary(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	rollup := map[string]any{"url": "https://api.github.com/repos/example/uma-sandbox/issues/comments/2000000014/reactions", "total_count": 4, "+1": 2, "-1": 0, "laugh": 0, "confused": 0, "heart": 1, "hooray": 0, "rocket": 1, "eyes": 0}
	h.serve(hookRequest("issue_comment", "d-r1", commentEvent(t, "edited", 2000000014, 5000001, 5000001, "text", rollup)))
	envs := h.sink.envelopes()
	if len(envs) != 2 || envs[0].Kind != model.EnvelopeMessageEdited {
		t.Fatalf("envelopes = %+v, want the edit then the summary", envs)
	}
	e := envs[1]
	s := summaryOf(t, e)
	want := []model.ReactionCount{{Emoji: "👍", Count: 2}, {Emoji: "❤️", Count: 1}, {Emoji: "🚀", Count: 1}}
	if len(s.Counts) != len(want) || !s.Complete {
		t.Fatalf("summary = %+v, want %v complete", s, want)
	}
	for i := range want {
		if s.Counts[i] != want[i] {
			t.Fatalf("summary = %+v, want %v", s, want)
		}
	}
	if e.MessageID != "comment:2000000014" || e.EventID == envs[0].EventID || e.References[0] != wiRef("7", sandboxRepo) || e.SourceRevision != envs[0].SourceRevision {
		t.Fatalf("summary envelope = %+v", e)
	}

	// A rollup of zeros still reports: it clears counts that moved to zero.
	zero := map[string]any{"total_count": 0, "+1": 0, "heart": 0}
	h.serve(hookRequest("issue_comment", "d-r2", commentEvent(t, "edited", 2000000014, 5000001, 5000001, "text", zero)))
	if last := h.sink.envelopes(); len(summaryOf(t, last[len(last)-1]).Counts) != 0 {
		t.Fatalf("zero rollup = %+v, want an empty snapshot", last[len(last)-1])
	}

	// A new comment with no reactions reports none; a payload without a
	// rollup neither.
	before := len(h.sink.envelopes())
	h.serve(hookRequest("issue_comment", "d-r3", commentEvent(t, "created", 2000000015, 5000001, 5000001, "text", zero)))
	h.serve(hookRequest("issue_comment", "d-r4", commentEvent(t, "edited", 2000000014, 5000001, 5000001, "text", nil)))
	for _, e := range h.sink.envelopes()[before:] {
		if e.SystemActor {
			t.Fatalf("envelope %+v: no reactions to report", e)
		}
	}
	h.serve(hookRequest("issue_comment", "d-r5", commentEvent(t, "created", 2000000016, 5000001, 5000001, "text", rollup)))
	last := h.sink.envelopes()
	if n := len(summaryOf(t, last[len(last)-1]).Counts); n != 3 {
		t.Fatalf("created with reactions = %d counts, want 3", n)
	}
}

// A summary for a comment the sink does not know is dropped quietly: it
// has no content to record the message from.
func TestReactionSummaryOfUnknownMessageIsDropped(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	h.sink.refuse = func(in model.Ingress) error {
		if in.Envelope.SystemActor {
			return model.NewError(model.NotFound, "target message not found")
		}
		return nil
	}
	rollup := map[string]any{"total_count": 1, "+1": 1}
	if code, _ := h.serve(hookRequest("issue_comment", "d-r1", commentEvent(t, "edited", 2000000014, 5000001, 5000001, "text", rollup))); code != http.StatusOK {
		t.Fatalf("delivery = %d, want 200", code)
	}
}

func TestDescriptorHasReactionSummaryEvent(t *testing.T) {
	// Counts arrive as message updates; the adapter declares the event.
	d := github.NewFactory(github.Options{}).Describe()
	found := false
	for _, e := range d.Events {
		found = found || e == model.EventMessageUpdated
	}
	if !found {
		t.Fatalf("events = %v, want message.updated", d.Events)
	}
}
