package github_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"time"

	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The provider calls the adapter's webhook as GitHub does: one signed
// issue_comment delivery per event, in the documented payload shape.
// Fixture values take GitHub's forms: a message is comment:<id>, a sender
// a numeric user ID, a destination the repository. Reaction events are not
// expressible: GitHub sends none, and a reaction summary travels with an
// edit delivery, the only payload carrying the comment's reaction rollup.

var (
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
)

// webhookState is what the provider remembers across deliveries.
type webhookState struct {
	ids     map[string]int64
	bodies  map[string]string
	queued  map[model.ChannelRef][]conformance.InboundEvent
	results map[model.ChannelRef][]conformance.InboundResult
}

func (p *provider) state() *webhookState {
	if p.hook == nil {
		p.hook = &webhookState{
			ids: map[string]int64{}, bodies: map[string]string{},
			queued: map[model.ChannelRef][]conformance.InboundEvent{}, results: map[model.ChannelRef][]conformance.InboundResult{},
		}
	}
	return p.hook
}

// ShapeID gives fixture values GitHub's forms, stable per value.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state()
	number := func(base int64) int64 {
		key := string(kind) + "/" + id
		if _, ok := st.ids[key]; !ok {
			st.ids[key] = base + int64(len(st.ids))
		}
		return st.ids[key]
	}
	switch kind {
	case conformance.FixtureMessage:
		return "comment:" + strconv.FormatInt(number(2_000_000_000), 10)
	case conformance.FixtureSender:
		return strconv.FormatInt(number(6_000_000), 10)
	case conformance.FixtureDestination:
		return sandboxRepo
	}
	return id
}

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	switch ev.Kind {
	case conformance.InboundMessage, conformance.InboundEdited, conformance.InboundDeleted, conformance.InboundReactionSummary:
	default:
		return conformance.ErrNotExpressible
	}
	if (ev.Visibility != "" && ev.Visibility != model.Public) || len(ev.Claims) > 0 {
		return conformance.ErrNotExpressible
	}
	// GitHub holds a comment from the moment it is delivered; an edit the
	// delivery reports, but not an echo (uma's own edit is already there).
	if id, ok := strings.CutPrefix(ev.MessageID, "comment:"); ok && ev.Echo == nil && (ev.Kind == conformance.InboundMessage || ev.Kind == conformance.InboundEdited) {
		cid, _ := strconv.ParseInt(id, 10, 64)
		author, _ := strconv.ParseInt(ev.Sender, 10, 64)
		n := p.Route(ch, ev.Sender).ID
		at := time.Unix(1_700_000_000, 0).UTC()
		p.gh.putComment(p.prefix(ch), fakeComment{
			ID: cid, Issue: sandboxRepo + "#" + n, Body: ev.Text, UserID: author, Login: "user-" + ev.Sender,
			CreatedAt: at, UpdatedAt: at.Add(ev.OccurredAfter),
		})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.state()
	st.queued[ch] = append(st.queued[ch], ev)
	return nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.state().results[ch]...)
}

func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.state().queued[ch]
	p.state().queued[ch] = nil
	p.mu.Unlock()
	for _, ev := range evs {
		body := p.payload(ch, ev)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(github.HeaderEvent, "issue_comment")
		req.Header.Set(github.HeaderDelivery, ev.EventID)
		req.Header.Set(github.HeaderSignature, sign(conformance.CredentialSecret, body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		p.state().results[ch] = append(p.state().results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

// payload renders ev as the issue_comment delivery of its action.
func (p *provider) payload(ch model.ChannelRef, ev conformance.InboundEvent) []byte {
	route := p.Route(ch, ev.Sender)
	n, _ := strconv.Atoi(route.ID)
	id, _ := strconv.ParseInt(strings.TrimPrefix(ev.MessageID, "comment:"), 10, 64)
	author, _ := strconv.ParseInt(ev.Sender, 10, 64)
	created := time.Unix(1_700_000_000, 0).UTC()
	updated := created.Add(ev.OccurredAfter)
	user := func(id int64) map[string]any {
		return map[string]any{"login": "user-" + strconv.FormatInt(id, 10), "id": id, "type": "User", "site_admin": false}
	}
	key := ev.MessageID
	p.mu.Lock()
	text := ev.Text
	if text == "" {
		text = p.state().bodies[key]
	}
	p.state().bodies[key] = text
	p.mu.Unlock()

	comment := map[string]any{
		"id": id, "node_id": "IC_" + strconv.FormatInt(id, 10), "user": user(author),
		"url":        "https://api.github.com/repos/" + sandboxRepo + "/issues/comments/" + strconv.FormatInt(id, 10),
		"html_url":   "https://github.com/" + sandboxRepo + "/issues/" + route.ID + "#issuecomment-" + strconv.FormatInt(id, 10),
		"issue_url":  "https://api.github.com/repos/" + sandboxRepo + "/issues/" + route.ID,
		"created_at": created.Format(time.RFC3339), "updated_at": created.Format(time.RFC3339),
		"author_association": "NONE", "body": text,
	}
	sender := author
	action := "created"
	ev2 := map[string]any{}
	switch ev.Kind {
	case conformance.InboundEdited, conformance.InboundReactionSummary:
		action = "edited"
		comment["updated_at"] = updated.Format(time.RFC3339)
		ev2["changes"] = map[string]any{"body": map[string]any{"from": "earlier"}}
		if ev.Echo != nil {
			// The edit uma made, round-tripped: the account's own edit, with
			// the marker of its attempt in the body.
			comment["body"] = text + "\n\n<!-- uma:" + string(ev.Echo.Operation.ID) + ":" + string(ev.Echo.Attempt) + " -->"
			sender = botID
		}
		if ev.Kind == conformance.InboundReactionSummary {
			comment["reactions"] = rollup(ev.Counts)
		}
	case conformance.InboundDeleted:
		action = "deleted"
	}
	ev2["action"] = action
	ev2["comment"] = comment
	ev2["sender"] = user(sender)
	ev2["repository"] = map[string]any{"id": 700000001, "name": "uma-sandbox", "full_name": sandboxRepo, "private": true, "owner": map[string]any{"login": "example", "id": 9000001, "type": "Organization"}}
	ev2["issue"] = map[string]any{
		"id": 3_000_000_000 + n, "number": n, "title": "Export fails", "state": "open", "user": user(author),
		"html_url":   "https://github.com/" + sandboxRepo + "/issues/" + route.ID,
		"created_at": created.Format(time.RFC3339), "updated_at": created.Format(time.RFC3339), "labels": []any{}, "assignees": []any{},
	}
	b, _ := json.Marshal(ev2)
	return b
}

// rollup is the reactions object of a comment with counts.
func rollup(counts []model.ReactionCount) map[string]any {
	out := map[string]any{"total_count": 0}
	total := 0
	for _, c := range counts {
		for _, rc := range reactionContents {
			if rc.emoji == c.Emoji {
				out[rc.content] = c.Count
				total += c.Count
			}
		}
	}
	out["total_count"] = total
	return out
}
