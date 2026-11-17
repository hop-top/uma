package github_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// X-GitHub-Delivery is not covered by X-Hub-Signature-256, which signs the
// raw body only: anyone holding a captured signed body can send it under a
// header of their choosing. So the header never decides that a delivery was
// already seen; the signed bytes do. These tests pin that through the real
// engine.

// ingress returns every ingress the sink accepted.
func (s *recordingSink) ingress() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.got...)
}

// eventKinds lists the kinds of every committed event, in commit order.
func (r *engineRig) eventKinds() []string {
	r.t.Helper()
	var out []string
	r.view(func(rd store.Reader) error {
		p, err := rd.Events(r.ctx, model.Filter{}, "", 1000)
		for _, e := range p.Items {
			out = append(out, e.Kind)
		}
		return err
	})
	return out
}

// reactionCount is the count-only total of emoji on the comment message.
func (r *engineRig) reactionCount(emoji string) int {
	r.t.Helper()
	for _, m := range r.messages() {
		if strings.HasPrefix(m.Content.Parts[0].Text, "Still") || strings.HasPrefix(m.Content.Parts[0].Text, "Fixed") {
			for _, x := range m.Reactions {
				if x.Emoji == emoji {
					return x.Count
				}
			}
		}
	}
	return 0
}

// withReactions is golden fixture name with the comment's reaction rollup.
func withReactions(t *testing.T, name string, plusOne int) []byte {
	t.Helper()
	return edited(t, name, func(ev map[string]any) {
		ev["comment"].(map[string]any)["reactions"] = map[string]any{"total_count": plusOne, "+1": plusOne}
	})
}

// A captured signed body replayed under a new X-GitHub-Delivery is the same
// event: the comment's reaction counts, replaced by every snapshot, must not
// go back to what an older body said.
func TestEngineReplayUnderNewDeliveryIDIsADuplicate(t *testing.T) {
	r := newEngineRig(t)
	created := withReactions(t, "issue_comment-created", 2)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.deliver("issue_comment", "d-created", created)
	r.deliver("issue_comment", "d-edited", withReactions(t, "issue_comment-edited", 3))
	if n := r.reactionCount("👍"); n != 3 {
		t.Fatalf("reactions = %d, want 3 from the later body", n)
	}
	events := r.eventKinds()

	for _, id := range []string{"replayed-1", "replayed-2", "00000000-0000-0000-0000-000000000000"} {
		r.deliver("issue_comment", id, created)
	}
	if n := r.reactionCount("👍"); n != 3 {
		t.Fatalf("reactions after replays = %d, want 3: a replay of the first body is a duplicate", n)
	}
	if got := r.eventKinds(); len(got) != len(events) {
		t.Fatalf("events after replays = %v, want %v", got, events)
	}
}

// GitHub's redelivery reuses the delivery GUID and resends the body: the
// duplicate it always was.
func TestEngineRedeliveryIsADuplicate(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.deliver("issue_comment", "d-created", withReactions(t, "issue_comment-created", 2))
	r.deliver("issue_comment", "d-edited", withReactions(t, "issue_comment-edited", 3))
	events := r.eventKinds()
	for range 3 {
		r.deliver("issue_comment", "d-edited", withReactions(t, "issue_comment-edited", 3))
		r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	}
	if got := r.eventKinds(); len(got) != len(events) || r.reactionCount("👍") != 3 || len(r.messages()) != 2 {
		t.Fatalf("after redeliveries: events %v, want %v; reactions %d; messages %d", got, events, r.reactionCount("👍"), len(r.messages()))
	}
}

// Two different events never collapse, whatever their delivery header says:
// a second change within one second of the first arrives under the same
// X-GitHub-Delivery here, and is still applied (the engine refetches it).
func TestEngineDifferentEventsUnderOneDeliveryIDAreBoth(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	r.deliver("issues", "shared", golden(t, "issues-labeled"))
	retitle := func(ev map[string]any) { ev["issue"].(map[string]any)["title"] = "Export fails on every file" }
	r.deliver("issues", "shared", edited(t, "issues-labeled", retitle))
	var cur map[string]any
	if err := json.Unmarshal(edited(t, "issues-labeled", retitle), &cur); err != nil {
		t.Fatal(err)
	}
	r.gh.mu.Lock()
	r.gh.accounts["a"].issues[sandboxRepo+"#7"] = cur["issue"].(map[string]any)
	r.gh.mu.Unlock()
	r.start()
	r.stop()
	if w := r.workItems()[0]; w.Subject != "Export fails on every file" {
		t.Fatalf("subject = %q, want the second change applied: a shared delivery ID does not make two events one", w.Subject)
	}
}

// The event ID is a digest of the signed body; the delivery GUID is
// metadata only (the correlation).
func TestWebhookEventIDIsTheBodyDigest(t *testing.T) {
	h := newHookRig(t, replyConfig(""))
	body := golden(t, "issues-opened")
	want := bodyDigest(body)
	for _, id := range []string{"72d3162e-cc78-11e3-81ab-4c9367dc0958", "another", "72d3162e-cc78-11e3-81ab-4c9367dc0958"} {
		if code, text := h.serve(hookRequest("issues", id, body)); code != http.StatusOK {
			t.Fatalf("delivery %s = %d %q", id, code, text)
		}
	}
	ins := h.sink.ingress()
	if len(ins) != 3 {
		t.Fatalf("ingress = %d, want 3", len(ins))
	}
	for i, in := range ins {
		if in.Envelope.EventID != want {
			t.Errorf("delivery %d event ID %q, want %q", i, in.Envelope.EventID, want)
		}
	}
	if c := ins[0].Correlation; c != "github-72d3162e-cc78-11e3-81ab-4c9367dc0958" || ins[1].Correlation != "github-another" {
		t.Errorf("correlations %q, %q, want the delivery GUID as metadata", c, ins[1].Correlation)
	}

	// Another body is another event, under the same GUID or another.
	other := h.post("issues", "issues-closed", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	if len(other) == 0 || other[0].EventID == want || !strings.HasPrefix(other[0].EventID, "sha256:") {
		t.Fatalf("a different body kept event ID %v, want its own digest", other)
	}
	// The reaction snapshot of a body is its own event of the same body.
	h.serve(hookRequest("issue_comment", "d-r", withReactions(t, "issue_comment-edited", 1)))
	var ids []string
	for _, e := range h.sink.envelopes() {
		if e.Kind == model.EnvelopeReactionAdded {
			ids = append(ids, e.EventID)
		}
	}
	if len(ids) != 1 || !strings.HasPrefix(ids[0], "sha256:") || !strings.HasSuffix(ids[0], "/reactions") {
		t.Fatalf("reaction snapshot event IDs = %v, want the body digest plus /reactions", ids)
	}
}

// X-GitHub-Event is unsigned too: it must not turn a signed body into an
// event it is not. An issue_comment body sent as an issues event would
// otherwise read as the issue's own action, issue_comment.deleted as the
// deletion of the issue.
func TestWebhookEventHeaderMustMatchTheBody(t *testing.T) {
	r := newEngineRig(t)
	r.deliver("issues", "d-opened", golden(t, "issues-opened"))
	for _, tc := range []struct{ event, fixture string }{
		{"issues", "issue_comment-deleted"},
		{"issues", "issue_comment-edited"},
		{"issue_comment", "issues-deleted"},
		{"issue_comment", "issues-closed"},
	} {
		rec := httptest.NewRecorder()
		r.hook.ServeHTTP(rec, hookRequest(tc.event, "forged", golden(t, tc.fixture)))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s body as %s = %d, want 400", tc.fixture, tc.event, rec.Code)
		}
	}
	if w := r.workItems()[0]; w.Binding != model.WorkItemBindingActive || w.RawStatus != "open" {
		t.Fatalf("work item = %+v after forged events, want it untouched", w)
	}
	if n := len(r.messages()); n != 1 {
		t.Fatalf("messages = %d, want the description only", n)
	}
	// Events the adapter does not consume stay acknowledged and dropped.
	rec := httptest.NewRecorder()
	r.hook.ServeHTTP(rec, hookRequest("ping", "d-ping", golden(t, "ping")))
	if rec.Code != http.StatusOK {
		t.Fatalf("ping = %d, want 200", rec.Code)
	}
}
