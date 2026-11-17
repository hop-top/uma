package relay_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for receive_http, send, reconcile and
// start_conversation against a scripted gateway that serves the optional
// lookup: a lost answer is looked up and never resent, and a request the
// gateway never took is sent again only after the lookup said not_found.
// A channel without a lookup is covered by the engine tests. The suite runs
// once per medium: the wire contract is one, the identity namespace differs,
// and iMessage peers are email addresses where the others are numbers.
func TestAdapterConformance(t *testing.T) {
	for _, m := range []medium{
		{relay.MediumSMS, relay.NamespaceSMS},
		{relay.MediumWhatsApp, relay.NamespaceWhatsApp},
		{relay.MediumIMessage, relay.NamespaceIMessage},
	} {
		t.Run(m.name, func(t *testing.T) {
			conformance.RunAdapterSuite(t, conformance.AdapterHarness{
				New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
					p := newScriptedGateway(t, m)
					f := relay.NewFactory(relay.Options{CallbackHosts: []string{p.host()}, AllowHTTP: true})
					return f, p
				},
				Capabilities: []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityStartConversation},
			})
		})
	}
}

// medium is the channel medium a scripted gateway serves and the identity
// namespace its peers are reported in.
type medium struct{ name, namespace string }

const gatewayNumber = "+15005550006"

// scriptedGateway is the operator's gateway as the suite scripts it. Each
// channel is one account, told apart by its callback and lookup paths.
type scriptedGateway struct {
	srv    *httptest.Server
	medium medium

	mu     sync.Mutex
	byChan map[model.ChannelRef]*gatewayAccount
	byPath map[string]*gatewayAccount
	seq    int
}

type gatewayAccount struct {
	ch        model.ChannelRef
	namespace string
	script    []conformance.Response
	calls     []conformance.ProviderCall
	queued    []conformance.InboundEvent
	inbound   []conformance.InboundResult
	// sent is the gateway's id of each operation it applied, by
	// Idempotency-Key: what its lookup answers from.
	sent map[string]string
}

func newScriptedGateway(t *testing.T, m medium) *scriptedGateway {
	t.Helper()
	g := &scriptedGateway{medium: m, byChan: map[model.ChannelRef]*gatewayAccount{}, byPath: map[string]*gatewayAccount{}}
	g.srv = httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *scriptedGateway) host() string { return strings.TrimPrefix(g.srv.URL, "http://") }

func (g *scriptedGateway) account(ch model.ChannelRef) *gatewayAccount {
	g.mu.Lock()
	defer g.mu.Unlock()
	a, ok := g.byChan[ch]
	if !ok {
		a = &gatewayAccount{ch: ch, namespace: g.medium.namespace, sent: map[string]string{}}
		g.byChan[ch] = a
		g.byPath["/out/"+string(ch.ID)] = a
		g.byPath["/lookup/"+string(ch.ID)] = a
	}
	return a
}

// callOf is the suite's view of one callback request. Callers hold g.mu.
func (a *gatewayAccount) callOf(body map[string]any) conformance.ProviderCall {
	to, _ := body["to"].(string)
	text, _ := body["body"].(string)
	op, _ := body["operation_id"].(string)
	at, _ := body["attempt_id"].(string)
	return conformance.ProviderCall{
		Capability: model.CapabilitySend,
		Operation:  model.OperationRef{Tenant: a.ch.Tenant, ID: model.ID(op)},
		Attempt:    model.ID(at),
		Route:      model.ExternalRef{Channel: a.ch, Namespace: a.namespace, Kind: relay.RouteKind, ID: to},
		Audience:   model.Audience{Visibility: model.Public},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

func (g *scriptedGateway) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	g.mu.Lock()
	a := g.byPath[r.URL.Path]
	if a == nil || r.Header.Get("Authorization") != "Bearer "+conformance.CredentialSecret {
		g.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet {
		g.lookup(w, r, a)
		return
	}
	resp := conformance.Response{Outcome: conformance.OutcomeAccepted}
	if len(a.script) > 0 {
		resp, a.script = a.script[0], a.script[1:]
	}
	call := a.callOf(body)
	applied := resp.Outcome == conformance.OutcomeAccepted || resp.Outcome == conformance.OutcomeAnswerLost
	var id string
	if applied {
		g.seq++
		id = fmt.Sprintf("gw-%04d", g.seq)
		call.Applied = true
		call.References = []model.ExternalRef{model.ProviderMessageRef(a.ch, id)}
		a.sent[r.Header.Get("Idempotency-Key")] = id
	}
	a.calls = append(a.calls, call)
	g.mu.Unlock()

	switch resp.Outcome {
	case conformance.OutcomeAccepted:
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"message_id":%q}`, id)
	case conformance.OutcomeRejected:
		w.WriteHeader(http.StatusUnprocessableEntity)
	case conformance.OutcomeRateLimited:
		if resp.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(resp.RetryAfter.Seconds())))
		}
		w.WriteHeader(http.StatusTooManyRequests)
	case conformance.OutcomeAnswerLost, conformance.OutcomeRequestLost:
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
	}
}

// lookup answers what the gateway holds for the operation the query names,
// recording the call as the suite's reconcile. Callers hold g.mu; it
// releases it.
func (g *scriptedGateway) lookup(w http.ResponseWriter, r *http.Request, a *gatewayAccount) {
	op := r.URL.Query().Get(relay.LookupQuery)
	a.calls = append(a.calls, conformance.ProviderCall{
		Capability: model.CapabilityReconcile, Operation: model.OperationRef{Tenant: a.ch.Tenant, ID: model.ID(op)},
	})
	id, sent := a.sent[op]
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if !sent {
		_, _ = io.WriteString(w, `{"status":"not_found"}`)
		return
	}
	_, _ = fmt.Fprintf(w, `{"status":"sent","message_id":%q}`, id)
}

// HTTPClient is the client the suite's engine injects: one round trip per
// call, and a dial that fails before the request leaves when the script says
// the call never reaches the gateway.
func (g *scriptedGateway) HTTPClient() ports.HTTPClient {
	return rtClient{unreachable{g: g, next: http.DefaultTransport}}
}

type unreachable struct {
	g    *scriptedGateway
	next http.RoundTripper
}

func (u unreachable) RoundTrip(r *http.Request) (*http.Response, error) {
	u.g.mu.Lock()
	a := u.g.byPath[r.URL.Path]
	if a == nil || r.Method != http.MethodPost || len(a.script) == 0 || a.script[0].Outcome != conformance.OutcomeTransient {
		u.g.mu.Unlock()
		return u.next.RoundTrip(r)
	}
	a.script = a.script[1:]
	call := conformance.ProviderCall{Capability: model.CapabilitySend}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		call = a.callOf(body)
	}
	a.calls = append(a.calls, call)
	u.g.mu.Unlock()
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

func (g *scriptedGateway) Account(ch model.ChannelRef) conformance.Account {
	g.account(ch)
	cfg := relay.Config{
		Medium:   g.medium.name,
		Auth:     relay.Auth{Scheme: "bearer"},
		Callback: &relay.Callback{URL: g.srv.URL + "/out/" + string(ch.ID), LookupURL: g.srv.URL + "/lookup/" + string(ch.ID)},
	}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "relay-account"}
}

func (g *scriptedGateway) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: g.medium.namespace, Kind: relay.RouteKind, ID: peer}
}

// ShapeID gives fixture values a gateway's forms: senders phone numbers (on
// iMessage email addresses), the suite's destination the channel's number,
// message and event IDs opaque.
func (g *scriptedGateway) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		if g.medium.name == relay.MediumIMessage {
			return appleID(id)
		}
		return phone(id)
	case conformance.FixtureDestination:
		if id == "support" {
			return gatewayNumber
		}
		return phone("destination " + id)
	case conformance.FixtureMessage, conformance.FixtureEvent:
		sum := sha256.Sum256([]byte(id))
		return "gw" + hex.EncodeToString(sum[:8])
	}
	return id
}

func phone(peer string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(peer))
	return fmt.Sprintf("+1555%07d", h.Sum32()%10_000_000)
}

// appleID is an email handle, lowercase as the adapter reports it.
func appleID(peer string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(peer))
	return fmt.Sprintf("peer%08x@example.com", h.Sum32())
}

// Deliver queues ev for Push: a relay reports new public messages only.
func (g *scriptedGateway) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Kind != conformance.InboundMessage || (ev.Visibility != "" && ev.Visibility != model.Public) {
		return conformance.ErrNotExpressible
	}
	a := g.account(ch)
	g.mu.Lock()
	defer g.mu.Unlock()
	a.queued = append(a.queued, ev)
	return nil
}

// Push posts each queued event the way the gateway does, authenticated with
// the channel's webhook secret.
func (g *scriptedGateway) Push(ch model.ChannelRef, h http.Handler) error {
	a := g.account(ch)
	g.mu.Lock()
	evs := a.queued
	a.queued = nil
	g.mu.Unlock()
	for _, ev := range evs {
		body, _ := json.Marshal(map[string]string{"from": ev.Sender, "to": ev.Destination, "body": ev.Text, "message_id": ev.MessageID})
		req := httptest.NewRequest(http.MethodPost, "/hooks/relay", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+conformance.CredentialSecret)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		g.mu.Lock()
		a.inbound = append(a.inbound, conformance.InboundResult{EventID: ev.EventID, Acked: rec.Code == http.StatusOK})
		g.mu.Unlock()
	}
	return nil
}

func (g *scriptedGateway) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	a := g.account(ch)
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]conformance.InboundResult(nil), a.inbound...)
}

func (g *scriptedGateway) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	a := g.account(ch)
	g.mu.Lock()
	defer g.mu.Unlock()
	a.script = append(a.script, rs...)
	return nil
}

func (g *scriptedGateway) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	a := g.account(ch)
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]conformance.ProviderCall(nil), a.calls...)
}

// Revoke is not expressible: the adapter learns of a withdrawn permission
// only from a refused call.
func (g *scriptedGateway) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}
