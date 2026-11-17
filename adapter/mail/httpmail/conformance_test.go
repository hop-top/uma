package httpmail_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/httpmail"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// TestAdapterConformance runs the shared adapter suite for receive_http: the
// provider is the relay posting each event to the handler Runtime.Webhook
// mounted, authenticated with the account's secret. The adapter has no
// outbound capability, so the suite judges its inbound and bypass cases and
// that every other capability is refused as unsupported.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			return httpmail.NewFactory(httpmail.Options{}), &provider{}
		},
		Capabilities: []model.Capability{model.CapabilityReceiveHTTP},
	})
}

// provider is the relay of every account.
type provider struct {
	mu      sync.Mutex
	queued  map[model.ChannelRef][]conformance.InboundEvent
	results map[model.ChannelRef][]conformance.InboundResult
}

func domain(ch model.ChannelRef) string {
	return "acct-" + strings.ToLower(string(ch.ID[len(ch.ID)-4:])) + ".test"
}

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := httpmail.Config{
		Recipients: []httpmail.Recipient{
			{Address: "support@" + domain(ch), Destination: "support@" + domain(ch)},
			{Address: "only-b@" + domain(ch), Destination: "only-b@" + domain(ch)},
		},
		Auth: httpmail.Auth{Scheme: "bearer"},
	}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "relay-cred"}
}

func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: httpmail.Namespace, Kind: httpmail.RouteKind, ID: peer}
}

// ShapeID gives senders and destinations mailbox addresses (destinations on
// the account's domain) and message IDs the msg-id form.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return id + "@peers.test"
	case conformance.FixtureDestination:
		return id + "@" + domain(ch)
	case conformance.FixtureMessage:
		return "<" + id + "@peers.test>"
	}
	return id
}

// Deliver queues ev as a message the relay will post. Mail has no private
// audience; claims ride in a member of the payload, which never identifies
// anyone.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Kind != "" && ev.Kind != conformance.InboundMessage {
		return conformance.ErrNotExpressible
	}
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.queued == nil {
		p.queued = map[model.ChannelRef][]conformance.InboundEvent{}
	}
	p.queued[ch] = append(p.queued[ch], ev)
	return nil
}

// Push posts the queued events to h, each as the relay does: one JSON object
// under the account's bearer token. The relay counts a 200 as delivered.
func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.queued[ch]
	delete(p.queued, ch)
	p.mu.Unlock()
	for _, ev := range evs {
		body, err := json.Marshal(map[string]any{
			"from": ev.Sender, "to": ev.Destination, "subject": "hello", "body": ev.Text,
			"message_id": ev.MessageID, "claims": ev.Claims,
		})
		if err != nil {
			return err
		}
		req := httptest.NewRequest(http.MethodPost, "/hooks/mail", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+conformance.CredentialSecret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		p.mu.Lock()
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		p.mu.Unlock()
	}
	return nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results[ch])
}

// Respond, Calls and Revoke: the adapter calls nothing, so there is no
// provider answer to script, no call to see and no permission to withdraw.
func (p *provider) Respond(model.ChannelRef, ...conformance.Response) error {
	return conformance.ErrNotExpressible
}

func (p *provider) Calls(model.ChannelRef) []conformance.ProviderCall { return nil }

func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}
