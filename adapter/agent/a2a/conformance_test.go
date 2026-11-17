package a2a_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// The adapter passes the shared adapter suite for every capability it
// declares: inbound traffic through the receiver mounted by
// Runtime.Webhook, sends through the engine.
func TestAdapterConformance(t *testing.T) {
	f, _ := a2a.New(a2a.Options{Authenticate: headerAuth})
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			f, err := a2a.New(a2a.Options{Authenticate: headerAuth})
			if err != nil {
				t.Fatal(err)
			}
			p := &provider{}
			return &spyFactory{Factory: f, p: p}, p
		},
		Capabilities: f.Describe().Capabilities,
		Selection:    model.Selection{Mode: model.ModePerAgentContext},
	})
}

// spyFactory records every send the engine makes through the adapter: the
// adapter has no transport, so what it reports delivered is the provider's
// whole view.
type spyFactory struct {
	*a2a.Factory
	p *provider
}

func (s *spyFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := s.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return &spyInstance{Instance: inst, ch: ch.Ref, p: s.p}, nil
}

type spyInstance struct {
	adapter.Instance
	ch model.ChannelRef
	p  *provider
}

func (s *spyInstance) Handler(sink adapter.Sink) http.Handler {
	return s.Instance.(adapter.HTTPReceiver).Handler(sink)
}

func (s *spyInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	out, err := s.Instance.(adapter.Sender).Send(ctx, r)
	s.p.record(s.ch, conformance.ProviderCall{
		Capability: model.CapabilitySend, Operation: r.Operation, Route: r.Route,
		Applied: err == nil && out.State == model.ProviderAccepted, References: out.References,
		Audience: r.Audience, Content: r.Content,
	})
	return out, err
}

// provider plays the remote agents: each queued event is one SendMessage
// from the event's sender, into the context that sender's earlier events
// of the same room opened.
type provider struct {
	mu       sync.Mutex
	queued   map[model.ChannelRef][]conformance.InboundEvent
	results  map[model.ChannelRef][]conformance.InboundResult
	calls    map[model.ChannelRef][]conformance.ProviderCall
	contexts map[string]string
}

func (p *provider) record(ch model.ChannelRef, c conformance.ProviderCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[model.ChannelRef][]conformance.ProviderCall{}
	}
	p.calls[ch] = append(p.calls[ch], c)
}

func (p *provider) Account(model.ChannelRef) conformance.Account {
	return conformance.Account{
		Configuration: a2a.Configuration(testConfig(func(c *a2a.Config) { c.Destination = "support" })),
		CredentialRef: "a2a",
	}
}

// Route is the agent context peer.
func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: a2a.NamespaceContext, Kind: model.RefKindContext, ID: peer}
}

// ShapeEvent gives the event the message ID the receiver derives from the
// agent and the client's message ID.
func (p *provider) ShapeEvent(ch model.ChannelRef, ev conformance.InboundEvent) conformance.InboundEvent {
	sender := model.ExternalRef{Channel: ch, Namespace: a2a.NamespacePrincipal, Kind: a2a.KindPrincipal, ID: ev.Sender}
	ev.MessageID = a2a.InboundMessageID(sender, ev.EventID)
	return ev
}

func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
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

func (p *provider) Push(ch model.ChannelRef, h http.Handler) error {
	p.mu.Lock()
	evs := p.queued[ch]
	delete(p.queued, ch)
	p.mu.Unlock()
	for _, ev := range evs {
		key := string(ch.ID) + "\x00" + ev.Sender + "\x00" + ev.Room
		p.mu.Lock()
		ctxID := p.contexts[key]
		p.mu.Unlock()
		msg := map[string]any{"messageId": ev.EventID, "role": "ROLE_USER", "parts": []map[string]any{{"text": ev.Text}}}
		if ctxID != "" {
			msg["contextId"] = ctxID
		}
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "SendMessage",
			"params": map[string]any{"message": msg, "configuration": map[string]any{"returnImmediately": true}},
		})
		req := httptest.NewRequest(http.MethodPost, "/a2a/", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("A2A-Version", "1.0")
		req.Header.Set("X-Agent", ev.Sender)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out struct {
			Result struct {
				Task struct {
					ContextID string `json:"contextId"`
				} `json:"task"`
			} `json:"result"`
			Error *rpcErr `json:"error"`
		}
		acked := rec.Code == http.StatusOK && json.Unmarshal(rec.Body.Bytes(), &out) == nil && out.Error == nil
		p.mu.Lock()
		if acked && out.Result.Task.ContextID != "" {
			if p.contexts == nil {
				p.contexts = map[string]string{}
			}
			p.contexts[key] = out.Result.Task.ContextID
		}
		if p.results == nil {
			p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev.EventID, Acked: acked})
		p.mu.Unlock()
	}
	return nil
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results[ch])
}

// Respond accepts what the adapter always does; it has no provider to
// refuse, lose or throttle a call.
func (p *provider) Respond(_ model.ChannelRef, rs ...conformance.Response) error {
	for _, r := range rs {
		if r.Outcome != conformance.OutcomeAccepted {
			return conformance.ErrNotExpressible
		}
	}
	return nil
}

func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls[ch])
}

func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// Credentialless: sends need no credential, so there is none to revoke.
func (p *provider) Credentialless() bool { return true }
