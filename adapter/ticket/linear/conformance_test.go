package linear_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	linear "hop.top/uma/adapter/ticket/linear"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for receive_http, send, reconcile and
// the message mutations against the stand-in Linear endpoint (fake_test.go)
// and stand-in webhook deliveries (webhook_test.go); recorded exchanges
// replace both, see docs/recording/linear.md.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			lin := newFakeLinear(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
			return linear.NewFactory(linear.Options{}), &provider{lin: lin}
		},
		Capabilities: linear.NewFactory(linear.Options{}).Describe().Capabilities,
		Selection:    model.Selection{Mode: model.ModePerWorkItem},
	})
}

// provider is the conformance view of the stand-in: one account (API root)
// per channel, one issue per peer.
type provider struct {
	lin *fakeLinear

	mu       sync.Mutex
	prefixes map[model.ChannelRef]string
	peers    map[string]int
	ids      map[string]int
	queued   map[model.ChannelRef][]conformance.InboundEvent
	results  map[model.ChannelRef][]conformance.InboundResult
	bodies   map[string]string
}

var (
	_ conformance.HTTPProvider    = (*provider)(nil)
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
)

func uuidN(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

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
		p.lin.account(pre, botID)
	}
	return pre
}

func (p *provider) HTTPClient() ports.HTTPClient { return p.lin }

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	return conformance.Account{Configuration: replyConfig(p.lin.apiURL(p.prefix(ch))).Extension(), CredentialRef: webhookSecretID}
}

// Route is the work item reference of the peer's issue. Linear holds the
// issue from the start.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	p.mu.Lock()
	if p.peers == nil {
		p.peers = map[string]int{}
	}
	n, ok := p.peers[peer]
	if !ok {
		n = len(p.peers) + 1
		p.peers[peer] = n
	}
	p.mu.Unlock()
	id := uuidN(1000 + n)
	if pre := p.prefix(ch); !p.lin.hasIssue(pre, id) {
		p.lin.putIssue(pre, fakeIssue{ID: id, Identifier: "ENG-" + strconv.Itoa(n), Title: "Export fails", Updated: time.Unix(1_700_000_000, 0).UTC().Add(-7 * 24 * time.Hour)})
	}
	return issueRoute2(ch, id)
}

func issueRoute2(ch model.ChannelRef, id string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: linear.Namespace, Kind: model.RefKindWorkItem, ID: id}
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []answer
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, statusAnswer(http.StatusBadRequest, gqlErr("Argument Validation Error", "invalid input"), nil))
		case conformance.OutcomeUnauthorized:
			as = append(as, statusAnswer(http.StatusForbidden, gqlErr("Forbidden", "forbidden"), nil))
		case conformance.OutcomeTransient:
			as = append(as, unreachable)
		case conformance.OutcomeRateLimited:
			as = append(as, statusAnswer(http.StatusTooManyRequests, `{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`,
				map[string]string{"Retry-After": strconv.Itoa(int(r.RetryAfter / time.Second))}))
		case conformance.OutcomeAnswerLost:
			as = append(as, answerLost)
		case conformance.OutcomeRequestLost:
			as = append(as, requestLost)
		default:
			return conformance.ErrNotExpressible
		}
	}
	p.lin.respond(p.prefix(ch), as...)
	return nil
}

// Calls maps operations to capabilities: commentCreate is send,
// commentUpdate edit, commentDelete delete, and a read is reconcile.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	pre := p.prefix(ch)
	var out []conformance.ProviderCall
	for _, c := range p.lin.callsOf(pre) {
		in, _ := c.Vars["input"].(map[string]any)
		text, _ := in["body"].(string)
		body, op, attempt := splitMarker(text)
		pc := conformance.ProviderCall{Applied: c.Applied, Operation: model.OperationRef{Tenant: ch.Tenant, ID: op}}
		if cm, ok := p.lin.comment(pre, c.Comment); ok {
			pc.Route = issueRoute2(ch, cm.IssueID)
		}
		switch c.Op {
		case "commentCreate":
			issue, _ := in["issueId"].(string)
			pc.Capability, pc.Route, pc.Audience = model.CapabilitySend, issueRoute2(ch, issue), model.Audience{Visibility: model.Public}
			pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}}
			if c.Applied {
				pc.References = []model.ExternalRef{commentRef(ch, c.Comment)}
			}
		case "commentUpdate":
			pc.Capability, pc.Attempt, pc.Target, pc.Audience = model.CapabilityEdit, attempt, commentRef(ch, c.Comment), model.Audience{Visibility: model.Public}
			pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: body}}}
			if c.Applied {
				pc.References = []model.ExternalRef{pc.Target}
			}
		case "commentDelete":
			pc.Capability, pc.Target = model.CapabilityDelete, commentRef(ch, c.Comment)
		default:
			pc = conformance.ProviderCall{Capability: model.CapabilityReconcile}
		}
		out = append(out, pc)
	}
	return out
}

func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// splitMarker returns a body's text and the operation and attempt its
// marker names.
func splitMarker(body string) (string, model.ID, model.ID) {
	i := strings.LastIndex(body, "\n\n<!-- uma:")
	if i < 0 {
		return body, "", ""
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(body[i:], "\n\n<!-- uma:"), " -->")
	op, att, _ := strings.Cut(inner, ":")
	return body[:i], model.ID(op), model.ID(att)
}

// ShapeID gives fixture values Linear's forms (UUIDs), stable per value.
func (p *provider) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	if kind == conformance.FixtureDestination {
		return "ENG"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids == nil {
		p.ids = map[string]int{}
	}
	key := string(kind) + "/" + id
	if _, ok := p.ids[key]; !ok {
		p.ids[key] = len(p.ids) + 1
	}
	switch kind {
	case conformance.FixtureMessage:
		return "comment:" + uuidN(2000+p.ids[key])
	case conformance.FixtureSender:
		return uuidN(6000 + p.ids[key])
	}
	return id
}

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	switch ev.Kind {
	case conformance.InboundMessage, conformance.InboundEdited, conformance.InboundDeleted:
	default:
		return conformance.ErrNotExpressible
	}
	if (ev.Visibility != "" && ev.Visibility != model.Public) || len(ev.Claims) > 0 {
		return conformance.ErrNotExpressible
	}
	// Linear holds a comment from the moment it is delivered; an edit the
	// delivery reports, but not an echo (uma's own edit is already there).
	if id, ok := strings.CutPrefix(ev.MessageID, "comment:"); ok && ev.Echo == nil && (ev.Kind == conformance.InboundMessage || ev.Kind == conformance.InboundEdited) {
		at := time.Unix(1_700_000_000, 0).UTC()
		p.lin.putComment(p.prefix(ch), fakeComment{ID: id, IssueID: p.Route(ch, ev.Sender).ID, Body: ev.Text, UserID: ev.Sender, Created: at})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queued == nil {
		p.queued, p.results, p.bodies = map[model.ChannelRef][]conformance.InboundEvent{}, map[model.ChannelRef][]conformance.InboundResult{}, map[string]string{}
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
		body := p.payload(ch, ev)
		req := httptest.NewRequest(http.MethodPost, "/webhooks/linear", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		req.Header.Set(linear.HeaderEvent, "Comment")
		req.Header.Set(linear.HeaderDelivery, ev.EventID)
		req.Header.Set(linear.HeaderSignature, sign(conformance.CredentialSecret, body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

// payload renders ev as the Comment delivery of its action. The delivery
// is stamped at the suite's clock.
func (p *provider) payload(ch model.ChannelRef, ev conformance.InboundEvent) []byte {
	route := p.Route(ch, ev.Sender)
	id := strings.TrimPrefix(ev.MessageID, "comment:")
	now := time.Unix(1_700_000_000, 0).UTC()
	p.mu.Lock()
	text := ev.Text
	if text == "" {
		text = p.bodies[ev.MessageID]
	}
	p.bodies[ev.MessageID] = text
	p.mu.Unlock()
	author, actor, action := ev.Sender, ev.Sender, "create"
	data := obj{
		"id": id, "body": text, "createdAt": now.Format(time.RFC3339Nano), "updatedAt": now.Format(time.RFC3339Nano),
		"issueId": route.ID, "userId": author, "user": obj{"id": author, "name": "user-" + author},
		"issue": obj{"id": route.ID, "identifier": "ENG-1", "title": "Export fails", "url": "https://linear.app/example/issue/ENG-1", "team": obj{"key": "ENG"}},
	}
	extra := obj{}
	switch ev.Kind {
	case conformance.InboundEdited:
		action = "update"
		upd := now.Add(ev.OccurredAfter).Format(time.RFC3339Nano)
		data["updatedAt"], data["editedAt"] = upd, upd
		extra["updatedFrom"] = obj{"body": "earlier"}
		if ev.Echo != nil {
			// The edit uma made, round-tripped: the account's own edit, with
			// the marker of its attempt in the body.
			data["body"] = marked(text, ev.Echo.Operation, ev.Echo.Attempt)
			data["userId"], data["user"], actor = botID, obj{"id": botID}, botID
		}
	case conformance.InboundDeleted:
		action = "remove"
	}
	extra["actor"] = obj{"id": actor, "type": "user"}
	return payload("Comment", action, data, now, extra)
}
