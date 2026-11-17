package gitlab_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	gitlab "hop.top/uma/adapter/ticket/gitlab"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for receive_http, send, reconcile,
// work_item_act, the message mutations and the reads the suite can express,
// against the stand-in GitLab REST shapes (fakegitlab_test.go).
//
// What the suite cannot express on GitLab, and the provider says so:
// deletion and reaction webhooks (GitLab sends neither), and channel-wide
// history (GitLab has no feed of notes across issues, so History reads one
// issue and the suite's channel-wide cases skip).
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			gl := newFakeGitLab(t, func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
			gl.token = conformance.CredentialSecret
			return gitlab.NewFactory(gitlab.Options{}), &provider{gl: gl}
		},
		Capabilities: gitlab.NewFactory(gitlab.Options{}).Describe().Capabilities,
		Selection:    model.Selection{Mode: model.ModePerWorkItem},
	})
}

var (
	_ conformance.WebhookProvider  = (*provider)(nil)
	_ conformance.FixtureShaper    = (*provider)(nil)
	_ conformance.HistoryProvider  = (*provider)(nil)
	_ conformance.WorkItemProvider = (*provider)(nil)
	_ conformance.HTTPProvider     = (*provider)(nil)
)

// provider is the conformance view of the fake GitLab: one account (API
// root) per channel, and one issue per peer.
type provider struct {
	gl *fakeGitLab

	mu       sync.Mutex
	prefixes map[model.ChannelRef]string
	peers    map[string]int64
	ids      map[string]int64
	queued   map[model.ChannelRef][]conformance.InboundEvent
	results  map[model.ChannelRef][]conformance.InboundResult
}

func (p *provider) prefix(ch model.ChannelRef) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefixes == nil {
		p.prefixes = map[model.ChannelRef]string{}
	}
	pre, ok := p.prefixes[ch]
	if !ok {
		pre = "acct" + strconv.Itoa(len(p.prefixes)+1)
		p.prefixes[ch] = pre
		p.gl.account(pre, botID)
	}
	return pre
}

func (p *provider) HTTPClient() ports.HTTPClient { return p.gl.client() }

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	return conformance.Account{Configuration: replyConfig(p.gl.apiURL(p.prefix(ch))).Extension(), CredentialRef: webhookSecretID}
}

// Route is the work item reference of the peer's issue. GitLab holds the
// issue from the start, opened a week before the suite's clock.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	p.mu.Lock()
	if p.peers == nil {
		p.peers = map[string]int64{}
	}
	n, ok := p.peers[peer]
	if !ok {
		n = int64(len(p.peers) + 1)
		p.peers[peer] = n
	}
	p.mu.Unlock()
	if pre := p.prefix(ch); !p.gl.hasIssue(pre, sandboxProject, n) {
		p.gl.setIssue(pre, sandboxProject, n, 5000001, "opened", time.Unix(1_700_000_000, 0).UTC().Add(-7*24*time.Hour))
	}
	return workItem(ch, n)
}

func workItem(ch model.ChannelRef, iid int64) model.ExternalRef {
	return model.ExternalRef{
		Channel: ch, Namespace: gitlab.Namespace, Kind: model.RefKindWorkItem,
		ID: strconv.FormatInt(iid, 10), ParentID: strconv.FormatInt(sandboxProject, 10),
	}
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []answer
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, statusAnswer(http.StatusBadRequest, `{"error":"body is invalid"}`, nil))
		case conformance.OutcomeUnauthorized:
			as = append(as, statusAnswer(http.StatusForbidden, `{"message":"403 Forbidden"}`, nil))
		case conformance.OutcomeTransient:
			as = append(as, unreachable)
		case conformance.OutcomeRateLimited:
			as = append(as, statusAnswer(http.StatusTooManyRequests, "Retry later", map[string]string{"Retry-After": strconv.Itoa(int(r.RetryAfter / time.Second))}))
		case conformance.OutcomeAnswerLost:
			as = append(as, answerLost)
		case conformance.OutcomeRequestLost:
			as = append(as, requestLost)
		default:
			return conformance.ErrNotExpressible
		}
	}
	p.gl.respond(p.prefix(ch), as...)
	return nil
}

// Calls maps requests to capabilities: creating a note is send, editing or
// deleting one is edit and delete, updating an issue's state is
// work_item_act (an edit of its description is edit), and any read is
// reconcile.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	var out []conformance.ProviderCall
	for _, c := range p.gl.calls(p.prefix(ch)) {
		s := strings.Split(strings.TrimPrefix(c.Path, "/"), "/")
		if len(s) < 6 {
			continue
		}
		route := workItem(ch, mustInt(s[5]))
		pc := conformance.ProviderCall{Route: route, Applied: c.Applied}
		var req struct {
			Body        string  `json:"body"`
			StateEvent  string  `json:"state_event"`
			Description *string `json:"description"`
		}
		_ = json.Unmarshal([]byte(c.Body), &req)
		switch {
		case c.Method == http.MethodGet:
			pc.Capability = model.CapabilityReconcile
		case len(s) == 6 && req.StateEvent != "":
			pc.Capability = model.CapabilityWorkItemAct
			pc.Status, pc.Reason = model.WorkItemClosed, ""
			if req.StateEvent == "reopen" {
				pc.Status = model.WorkItemOpen
			}
		case len(s) == 6 && req.Description != nil:
			text, op := splitMarker(*req.Description)
			pc.Capability, pc.Operation, pc.Attempt = model.CapabilityEdit, model.OperationRef{Tenant: ch.Tenant, ID: op}, attemptOf(*req.Description)
			pc.Audience, pc.Content = model.Audience{Visibility: model.Public}, textOf(text)
			pc.Target = model.ProviderMessageRef(ch, "issue:"+strconv.FormatInt(3_000_000+mustInt(s[5]), 10))
			if c.Applied {
				pc.References = []model.ExternalRef{pc.Target}
			}
		case c.Method == http.MethodPost:
			text, op := splitMarker(req.Body)
			pc.Capability, pc.Operation = model.CapabilitySend, model.OperationRef{Tenant: ch.Tenant, ID: op}
			pc.Audience, pc.Content = model.Audience{Visibility: model.Public}, textOf(text)
			if c.Applied {
				pc.References = []model.ExternalRef{noteRefOn(ch, c.Note)}
			}
		case c.Method == http.MethodPut && len(s) == 8:
			text, op := splitMarker(req.Body)
			pc.Capability, pc.Operation, pc.Attempt = model.CapabilityEdit, model.OperationRef{Tenant: ch.Tenant, ID: op}, attemptOf(req.Body)
			pc.Audience, pc.Content, pc.Target = model.Audience{Visibility: model.Public}, textOf(text), noteRefOn(ch, mustInt(s[7]))
			if c.Applied {
				pc.References = []model.ExternalRef{pc.Target}
			}
		case c.Method == http.MethodDelete && len(s) == 8:
			pc.Capability, pc.Target = model.CapabilityDelete, noteRefOn(ch, mustInt(s[7]))
		default:
			continue
		}
		out = append(out, pc)
	}
	return out
}

func noteRefOn(ch model.ChannelRef, id int64) model.ExternalRef {
	return model.ProviderMessageRef(ch, "note:"+strconv.FormatInt(id, 10))
}

func mustInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// splitMarker returns a body's text and the operation its marker names.
func splitMarker(body string) (string, model.ID) {
	i := strings.LastIndex(body, "\n\n<!-- uma:")
	if i < 0 {
		return body, ""
	}
	op, _, _ := strings.Cut(strings.TrimPrefix(body[i:], "\n\n<!-- uma:"), ":")
	return body[:i], model.ID(op)
}

// attemptOf returns the attempt a body's marker names.
func attemptOf(body string) model.ID {
	i := strings.LastIndex(body, "\n\n<!-- uma:")
	if i < 0 {
		return ""
	}
	_, rest, _ := strings.Cut(strings.TrimPrefix(body[i:], "\n\n<!-- uma:"), ":")
	att, _, _ := strings.Cut(rest, " ")
	return model.ID(att)
}

// SeedWorkItem opens or closes the issue for peer. GitLab holds no other
// status.
func (p *provider) SeedWorkItem(ch model.ChannelRef, peer, status string) (model.ExternalRef, error) {
	route := p.Route(ch, peer)
	n, _ := strconv.ParseInt(route.ID, 10, 64)
	updated := time.Unix(1_700_000_000, 0).UTC().Add(-2 * time.Hour)
	switch status {
	case model.WorkItemOpen:
		p.gl.setIssue(p.prefix(ch), sandboxProject, n, 5000001, "opened", updated)
	case model.WorkItemClosed:
		p.gl.setIssue(p.prefix(ch), sandboxProject, n, 5000001, "closed", updated)
	default:
		return model.ExternalRef{}, conformance.ErrNotExpressible
	}
	return route, nil
}

// WorkItemStatus reads the issue's state as a normalized status.
func (p *provider) WorkItemStatus(ch model.ChannelRef, work model.ExternalRef) (string, error) {
	iid, _ := strconv.ParseInt(work.ID, 10, 64)
	iss, ok := p.gl.issue(p.prefix(ch), sandboxProject, iid)
	if !ok {
		return "", fmt.Errorf("no issue %d", iid)
	}
	if iss.State == "closed" {
		return model.WorkItemClosed, nil
	}
	return model.WorkItemOpen, nil
}

// Revoke: a token's scopes are only visible in GitLab's answers, so a
// revocation cannot be known before a call.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// HoldHistory: the suite reads a channel's window across issues, which
// GitLab has no feed for; one issue's history is covered by the adapter's
// own tests.
func (p *provider) HoldHistory(model.ChannelRef, conformance.InboundEvent, time.Time) error {
	return conformance.ErrNotExpressible
}

// ShapeID gives fixture values GitLab's forms, stable per value.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids == nil {
		p.ids = map[string]int64{}
	}
	number := func(base int64) int64 {
		key := string(kind) + "/" + id
		if _, ok := p.ids[key]; !ok {
			p.ids[key] = base + int64(len(p.ids))
		}
		return p.ids[key]
	}
	switch kind {
	case conformance.FixtureMessage:
		return "note:" + strconv.FormatInt(number(2_000_000_000), 10)
	case conformance.FixtureSender:
		return strconv.FormatInt(number(6_000_000), 10)
	case conformance.FixtureDestination:
		return sandboxPath
	}
	return id
}

// Deliver queues an inbound event. GitLab sends no deletion or reaction
// webhook, and an internal or claimed message is not what its note events
// carry.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	switch ev.Kind {
	case conformance.InboundMessage, conformance.InboundEdited:
	default:
		return conformance.ErrNotExpressible
	}
	if (ev.Visibility != "" && ev.Visibility != model.Public) || len(ev.Claims) > 0 {
		return conformance.ErrNotExpressible
	}
	// GitLab holds a note from the moment it is delivered; an edit the
	// delivery reports, but not an echo (uma's own edit is already there).
	if id, ok := strings.CutPrefix(ev.MessageID, "note:"); ok && ev.Echo == nil {
		nid, _ := strconv.ParseInt(id, 10, 64)
		author, _ := strconv.ParseInt(ev.Sender, 10, 64)
		route := p.Route(ch, ev.Sender)
		at := time.Unix(1_700_000_000, 0).UTC()
		pre := p.prefix(ch)
		if _, held := p.gl.note(pre, nid); held {
			p.gl.setNoteBody(pre, nid, ev.Text, at.Add(ev.OccurredAfter))
		} else {
			p.gl.putNote(pre, fakeNote{
				ID: nid, Issue: issueKey(sandboxProject, mustInt(route.ID)), Body: ev.Text, Author: author,
				Username: "user-" + ev.Sender, Created: at, Updated: at.Add(ev.OccurredAfter),
			})
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queued == nil {
		p.queued, p.results = map[model.ChannelRef][]conformance.InboundEvent{}, map[model.ChannelRef][]conformance.InboundResult{}
	}
	p.queued[ch] = append(p.queued[ch], ev)
	return nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.queued[ch]
	p.queued[ch] = nil
	p.mu.Unlock()
	for _, ev := range evs {
		body, _ := json.Marshal(p.payload(ch, ev))
		req := httptest.NewRequest(http.MethodPost, "/webhooks/gitlab", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(gitlab.HeaderEvent, "Note Hook")
		req.Header.Set(gitlab.HeaderIdempotency, strings.NewReplacer("_", "-").Replace(ev.EventID))
		req.Header.Set(gitlab.HeaderToken, conformance.CredentialSecret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

// payload renders ev as the note event of its action.
func (p *provider) payload(ch model.ChannelRef, ev conformance.InboundEvent) map[string]any {
	route := p.Route(ch, ev.Sender)
	iid, _ := strconv.ParseInt(route.ID, 10, 64)
	id, _ := strconv.ParseInt(strings.TrimPrefix(ev.MessageID, "note:"), 10, 64)
	author, _ := strconv.ParseInt(ev.Sender, 10, 64)
	created := time.Unix(1_700_000_000, 0).UTC()
	updated := created.Add(ev.OccurredAfter)
	text, actor, username, action := ev.Text, author, "user-"+ev.Sender, "create"
	if ev.Kind == conformance.InboundEdited {
		action = "update"
		if ev.Echo != nil {
			// The edit uma made, round-tripped: the account's own edit,
			// with the marker of its attempt in the body.
			text += "\n\n<!-- uma:" + string(ev.Echo.Operation.ID) + ":" + string(ev.Echo.Attempt) + " -->"
			actor, username = botID, "project_700000001_bot_fixture"
		}
	}
	return map[string]any{
		"object_kind": "note", "event_type": "note",
		"user":    map[string]any{"id": actor, "name": username, "username": username},
		"project": map[string]any{"id": sandboxProject, "path_with_namespace": sandboxPath},
		"object_attributes": map[string]any{
			"id": id, "note": text, "noteable_type": "Issue", "author_id": author, "internal": false, "system": false, "action": action,
			"created_at": ts(created), "updated_at": ts(updated),
		},
		"issue": map[string]any{
			"id": 3_000_000 + iid, "iid": iid, "title": "Export fails", "state": "opened", "author_id": author,
			"created_at": ts(created), "updated_at": ts(created),
		},
	}
}
