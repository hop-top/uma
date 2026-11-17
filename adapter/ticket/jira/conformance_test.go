package jira_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for every capability it declares
// against the documented Jira Cloud REST and webhook shapes (stand-ins:
// see fakeJira and the payload below; a recording replaces them).
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			j := newFakeJira(t, func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
			return jira.NewFactory(), &provider{j: j}
		},
		Capabilities: jira.NewFactory().Describe().Capabilities,
		Selection:    model.Selection{Mode: model.ModePerWorkItem},
	})
}

const project = "SBX"

// provider is the conformance view of the fake: one Jira site (host) per
// channel, and one issue per peer.
type provider struct {
	j *fakeJira

	mu      sync.Mutex
	hosts   map[model.ChannelRef]string
	peers   map[string]int
	ids     map[string]int64
	bodies  map[string]string
	queued  map[model.ChannelRef][]conformance.InboundEvent
	results map[model.ChannelRef][]conformance.InboundResult
}

func (p *provider) host(ch model.ChannelRef) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.hosts == nil {
		p.hosts = map[model.ChannelRef]string{}
		p.ids, p.bodies = map[string]int64{}, map[string]string{}
		p.queued, p.results = map[model.ChannelRef][]conformance.InboundEvent{}, map[model.ChannelRef][]conformance.InboundResult{}
	}
	h, ok := p.hosts[ch]
	if !ok {
		h = "acct" + strconv.Itoa(len(p.hosts)+1) + ".example.atlassian.net"
		p.hosts[ch] = h
	}
	return h
}

func (p *provider) HTTPClient() ports.HTTPClient { return p.j.client() }

func (p *provider) acct(ch model.ChannelRef) *fakeAccount { return p.j.account(p.host(ch)) }

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := jira.Config{SiteURL: "https://" + p.host(ch), Replies: true, AccountID: ownAccount}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "jira-cred"}
}

// ShapeID gives fixture values Jira's forms, stable per value: a comment
// ID is numeric, a sender an account ID, a destination the project key.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	p.host(ch)
	p.mu.Lock()
	defer p.mu.Unlock()
	number := func(base int64) int64 {
		key := string(kind) + "/" + id
		if _, ok := p.ids[key]; !ok {
			p.ids[key] = base + int64(len(p.ids))
		}
		return p.ids[key]
	}
	switch kind {
	case conformance.FixtureMessage:
		return "comment:" + strconv.FormatInt(number(20_000), 10)
	case conformance.FixtureSender:
		return "712020:sender-" + strconv.FormatInt(number(1), 10)
	case conformance.FixtureDestination:
		return project
	}
	return id
}

// Route is the work item reference of the peer's issue; the account holds
// it from the start.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	p.host(ch)
	p.mu.Lock()
	n, ok := p.peers[peer]
	if p.peers == nil {
		p.peers = map[string]int{}
	}
	if !ok {
		n = len(p.peers) + 1
		p.peers[peer] = n
	}
	p.mu.Unlock()
	id := strconv.Itoa(10000 + n)
	a := p.acct(ch)
	if _, held := a.issues[id]; !held {
		p.j.putIssue(a, id, project+"-"+strconv.Itoa(n), "new")
	}
	return model.ExternalRef{Channel: ch, Namespace: jira.Namespace, Kind: model.RefKindWorkItem, ID: id}
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []answer
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, answer{})
		case conformance.OutcomeRejected:
			as = append(as, answer{status: http.StatusBadRequest})
		case conformance.OutcomeUnauthorized:
			as = append(as, answer{status: http.StatusForbidden})
		case conformance.OutcomeTransient:
			as = append(as, answer{dial: true})
		case conformance.OutcomeRateLimited:
			as = append(as, answer{status: http.StatusTooManyRequests, retry: strconv.Itoa(int(r.RetryAfter / time.Second))})
		case conformance.OutcomeAnswerLost:
			as = append(as, answer{status: http.StatusBadGateway, applied: true})
		case conformance.OutcomeRequestLost:
			as = append(as, answer{status: http.StatusGatewayTimeout})
		default:
			return conformance.ErrNotExpressible
		}
	}
	p.j.respond(p.acct(ch), as...)
	return nil
}

// Calls maps requests to capabilities: creating a comment is send, editing
// one edit, deleting one delete, listing comments or reading one reconcile.
// Property reads are part of a reconciliation, never a call of their own.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	var out []conformance.ProviderCall
	for _, c := range p.j.callsOf(p.acct(ch)) {
		seg := strings.Split(strings.TrimPrefix(c.Path, "/rest/api/3/"), "/")
		if len(seg) < 3 || seg[0] != "issue" || seg[2] != "comment" {
			continue
		}
		route := model.ExternalRef{Channel: ch, Namespace: jira.Namespace, Kind: model.RefKindWorkItem, ID: seg[1]}
		var req struct {
			Body       json.RawMessage `json:"body"`
			Properties []struct {
				Key   string `json:"key"`
				Value struct {
					Operation model.ID `json:"operation"`
					Attempt   model.ID `json:"attempt"`
				} `json:"value"`
			} `json:"properties"`
		}
		_ = json.Unmarshal([]byte(c.Body), &req)
		pc := conformance.ProviderCall{
			Route: route, Applied: c.Applied, Audience: model.Audience{Visibility: model.Public},
		}
		if len(req.Properties) > 0 {
			pc.Operation = model.OperationRef{Tenant: ch.Tenant, ID: req.Properties[0].Value.Operation}
			pc.Attempt = req.Properties[0].Value.Attempt
		}
		if len(req.Body) > 0 {
			pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: adfText(req.Body)}}}
		}
		target := model.ProviderMessageRef(ch, "comment:"+c.Comment)
		switch c.Method {
		case http.MethodPost:
			pc.Capability = model.CapabilitySend
			if c.Applied {
				pc.References = []model.ExternalRef{target}
			}
		case http.MethodPut:
			pc.Capability, pc.Target = model.CapabilityEdit, target
			if c.Applied {
				pc.References = []model.ExternalRef{target}
			}
		case http.MethodDelete:
			pc.Capability, pc.Target, pc.Content = model.CapabilityDelete, target, model.Content{}
			pc.Audience = model.Audience{}
		case http.MethodGet:
			pc = conformance.ProviderCall{Capability: model.CapabilityReconcile, Route: route}
		}
		out = append(out, pc)
	}
	return out
}

// adfText joins the text of a document's paragraphs.
func adfText(doc json.RawMessage) string {
	var d struct {
		Content []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"content"`
	}
	_ = json.Unmarshal(doc, &d)
	var paras []string
	for _, p := range d.Content {
		var s string
		for _, t := range p.Content {
			s += t.Text
		}
		paras = append(paras, s)
	}
	return strings.Join(paras, "\n\n")
}

// Revoke: a credential's permissions are only visible in Jira's answers.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

var (
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
	_ conformance.HTTPProvider    = (*provider)(nil)
)

// Deliver queues the event as a comment webhook. Jira holds a comment from
// the moment it is delivered, except an echo (uma's own change is already
// there).
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	switch ev.Kind {
	case conformance.InboundMessage, conformance.InboundEdited, conformance.InboundDeleted:
	default:
		return conformance.ErrNotExpressible
	}
	if (ev.Visibility != "" && ev.Visibility != model.Public) || len(ev.Claims) > 0 {
		return conformance.ErrNotExpressible
	}
	route := p.Route(ch, ev.Sender)
	if id, ok := strings.CutPrefix(ev.MessageID, "comment:"); ok && ev.Echo == nil && (ev.Kind == conformance.InboundMessage || ev.Kind == conformance.InboundEdited) {
		at := time.Unix(1_700_000_000, 0).UTC()
		p.j.putComment(p.acct(ch), &fakeComment{
			ID: id, Issue: route.ID, Author: ev.Sender, Body: adfOf(ev.Text), Created: at, Updated: at.Add(ev.OccurredAfter),
			Props: map[string]json.RawMessage{},
		})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queued[ch] = append(p.queued[ch], ev)
	return nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.host(ch)
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.host(ch)
	p.mu.Lock()
	evs := p.queued[ch]
	p.queued[ch] = nil
	p.mu.Unlock()
	for _, ev := range evs {
		body := p.payload(ch, ev)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/jira", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(jira.HeaderIdentifier, ev.EventID)
		req.Header.Set(jira.HeaderSignature, sign(conformance.CredentialSecret, body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

// sign is the X-Hub-Signature of body: "sha256=" and the hex HMAC.
func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// payload renders ev as the comment webhook of its action, in the shape
// the webhooks documentation gives: timestamp, webhookEvent, the issue and
// the comment as the REST API returns them.
func (p *provider) payload(ch model.ChannelRef, ev conformance.InboundEvent) []byte {
	route := p.Route(ch, ev.Sender)
	cid := strings.TrimPrefix(ev.MessageID, "comment:")
	created := time.Unix(1_700_000_000, 0).UTC()
	updated := created.Add(ev.OccurredAfter)
	key := ev.MessageID
	p.mu.Lock()
	text := ev.Text
	if text == "" {
		text = p.bodies[key]
	}
	p.bodies[key] = text
	p.mu.Unlock()

	comment := map[string]any{
		"self": "https://" + p.host(ch) + "/rest/api/3/issue/" + route.ID + "/comment/" + cid, "id": cid,
		"author": userJSON(ev.Sender), "updateAuthor": userJSON(ev.Sender), "body": adfOf(text),
		"created": jiraTime(created), "updated": jiraTime(created), "jsdPublic": true,
	}
	event, actor := "comment_created", ev.Sender
	switch ev.Kind {
	case conformance.InboundEdited:
		event = "comment_updated"
		comment["updated"] = jiraTime(updated)
		if ev.Echo != nil {
			// The edit uma made, round-tripped: the account's own edit,
			// with the property of its attempt.
			actor = ownAccount
			comment["author"], comment["updateAuthor"] = userJSON(ownAccount), userJSON(ownAccount)
			comment["properties"] = []any{map[string]any{"key": jira.EchoPropertyKey, "value": map[string]any{
				"operation": ev.Echo.Operation.ID, "attempt": ev.Echo.Attempt,
			}}}
		}
	case conformance.InboundDeleted:
		event = "comment_deleted"
	}
	issue := map[string]any{"id": route.ID, "key": project + "-" + strconv.Itoa(len(route.ID)), "self": "https://" + p.host(ch) + "/rest/api/3/issue/" + route.ID, "fields": map[string]any{
		"summary": "Export fails", "labels": []any{}, "project": map[string]any{"key": project},
		"status":   map[string]any{"name": "To Do", "statusCategory": map[string]any{"key": "new"}},
		"reporter": userJSON(ev.Sender), "creator": userJSON(ev.Sender),
		"created": jiraTime(created), "updated": jiraTime(created),
	}}
	b, _ := json.Marshal(map[string]any{
		"timestamp": updated.UnixMilli(), "webhookEvent": event, "user": userJSON(actor), "issue": issue, "comment": comment,
	})
	return b
}
