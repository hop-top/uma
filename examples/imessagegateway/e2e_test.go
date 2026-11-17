package imessagegateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/gateway/imessage"
	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/testing/xrrtest"
)

// The end-to-end scenario: uma in process (memory store, relay adapter)
// with an imessage channel whose callback and lookup are the gateway, and
// the gateway's backend client an xrr client. The recorded tests run it
// against a real backend (recording) or its cassettes (replay); the
// stand-in tests run it against stand-ins of both backends.

// e2eWebhookSecret and e2eCallbackSecret are the channel's credentials in
// the test, shared by uma and the gateway. Fixture values.
const (
	e2eWebhookSecret  = "e2e-relay-webhook-secret-0123456789"  // #nosec G101
	e2eCallbackSecret = "e2e-relay-callback-secret-0123456789" // #nosec G101
)

// e2eText is what the test sends; e2eReply what the recipient answers.
const (
	e2eText  = "uma recorded test message: please reply recorded test reply"
	e2eReply = "recorded test reply"
)

// replyWait bounds the wait for the recipient's reply when recording.
const replyWait = 3 * time.Minute

// e2eSpec is one backend under the scenario.
type e2eSpec struct {
	name string
	// recipient is the test handle; address the "to" the backend reports
	// for inbound messages, which the inbox binds.
	recipient, address string
	backend            func(*http.Client) imessage.Backend
	// webhookToken is the path token of a backend that signs nothing.
	webhookToken string
	// signReplay signs a captured webhook for replay; nil for none.
	signReplay func(body []byte) http.Header
	// placeholders keep identifiers out of cassettes.
	placeholders []xrrtest.Option
	// fixtureValues replace recorded values (old, new) in a captured
	// webhook before it is written.
	fixtureValues [][2]string
}

// e2eRun is how the scenario runs.
type e2eRun struct {
	mode      xrrtest.Mode
	cassettes string
	fixture   string
	// listen is the gateway's fixed address when recording against a real
	// backend, which must reach its webhook; empty is any local port.
	listen string
	extra  []xrrtest.Option
	// reply makes the recipient answer, when recording: a person, or a
	// stand-in. webhookURL is the gateway's webhook.
	reply func(t *testing.T, webhookURL string)
}

// lateHandler serves a handler set after its server started.
type lateHandler struct{ h atomic.Pointer[http.Handler] }

func (l *lateHandler) set(h http.Handler) { l.h.Store(&h) }
func (l *lateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := l.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	http.Error(w, "not ready", http.StatusServiceUnavailable)
}

// countingTransport counts the backend calls.
type countingTransport struct {
	rt http.RoundTripper
	n  atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.rt.RoundTrip(r)
}

func serve(t *testing.T, h http.Handler, listen string) string {
	t.Helper()
	if listen == "" {
		srv := httptest.NewServer(h)
		t.Cleanup(srv.Close)
		return srv.URL
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		t.Fatalf("listen on %s: %v", listen, err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// runE2E runs the scenario: a first message, a repeated callback, the
// lookup, and the recipient's reply landing in the same conversation.
func runE2E(t *testing.T, spec e2eSpec, run e2eRun) {
	ctx := t.Context()
	opts := append([]xrrtest.Option{xrrtest.WithMode(run.mode)}, spec.placeholders...)
	xc := xrrtest.NewClient(t, run.cassettes, append(opts, run.extra...)...)
	counter := &countingTransport{rt: xc.Transport}
	be := spec.backend(&http.Client{Transport: counter, Timeout: 2 * time.Minute})

	gwLate, umaLate := &lateHandler{}, &lateHandler{}
	gwURL := serve(t, gwLate, run.listen)
	umaURL := serve(t, umaLate, "")

	engine, err := newEngine(gatewaySecrets{webhook: []byte(e2eWebhookSecret), callback: []byte(e2eCallbackSecret)},
		strings.TrimPrefix(gwURL, "http://"), portstest.NewSequenceIDs(0x1e55))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(t.Context()) })
	host := model.Principal{Tenant: "imessage-e2e", Actor: "host"}
	channel, inbox, webhook, err := configureIMessage(ctx, engine, host, gwURL, spec.address)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/uma/imessage", webhook)
	umaLate.set(mux)

	keys, err := imessage.OpenKeyStore(t.TempDir(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keys.Close() })
	captured := make(chan []byte, 1)
	gw, err := imessage.New(imessage.Config{
		Backend: be, Keys: keys, CallbackSecret: []byte(e2eCallbackSecret),
		WebhookURL: umaURL + "/uma/imessage", WebhookSecret: []byte(e2eWebhookSecret), WebhookToken: spec.webhookToken,
		Forwarded: func(body []byte, from string) {
			if strings.EqualFold(from, spec.recipient) {
				select {
				case captured <- bytes.Clone(body):
				default:
				}
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gwLate.set(gw.Handler())
	webhookURL := gwURL + imessage.PathWebhook
	if spec.webhookToken != "" {
		webhookURL += "/" + spec.webhookToken
	}

	client, err := engine.For(host)
	if err != nil {
		t.Fatal(err)
	}
	call := model.Call{Correlation: "imessage-e2e"}
	acc, err := client.Conversations().Start(ctx, model.StartConversation{
		Command:   model.Command{Call: call, Operation: "imessage-e2e-start"},
		Inbox:     inbox,
		Recipient: model.ExternalRef{Channel: channel, Namespace: relay.NamespaceIMessage, Kind: relay.RouteKind, ID: spec.recipient},
		Content:   model.Content{Parts: []model.Part{{Kind: model.PartText, Text: e2eText}}},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	conv := acc.Operation.ResultMessage.Conversation
	step(t, engine, host)
	op, err := client.Operations().Get(ctx, model.Lookup[model.OperationRef]{Call: call, Ref: acc.Operation.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if op.State != model.ProviderAccepted || len(op.ProviderReferences) != 1 || op.ProviderReferences[0].ID == "" {
		t.Fatalf("operation %s %+v (failure %v), want provider_accepted with the backend's id", op.State, op.ProviderReferences, op.Failure)
	}
	backendID := op.ProviderReferences[0].ID
	if n := counter.n.Load(); n != 1 {
		t.Fatalf("%d backend calls, want 1", n)
	}

	// The relay sends the reply again with the same key: the gateway
	// answers the same id and sends nothing.
	key := string(op.Ref.ID)
	dup, _ := json.Marshal(map[string]string{
		"medium": imessage.Medium, "to": spec.recipient, "body": e2eText, "operation_id": key, "attempt_id": "repeat",
	})
	status, answer := gatewayCall(t, http.MethodPost, gwURL+imessage.PathReply, key, dup)
	if status != http.StatusOK || answer["message_id"] != backendID {
		t.Fatalf("repeated callback answered %d %v, want 200 with %s", status, answer, backendID)
	}
	status, answer = gatewayCall(t, http.MethodGet, gwURL+imessage.PathLookup+"?operation_id="+key, "", nil)
	if status != http.StatusOK || answer["status"] != "sent" || answer["message_id"] != backendID {
		t.Fatalf("lookup answered %d %v, want sent %s", status, answer, backendID)
	}
	if n := counter.n.Load(); n != 1 {
		t.Fatalf("%d backend calls after the repeat and the lookup, want 1", n)
	}

	// The recipient answers.
	if run.mode == xrrtest.ModeRecord {
		run.reply(t, webhookURL)
		select {
		case raw := <-captured:
			writeFixture(t, run.fixture, scrubFixture(raw, spec.fixtureValues))
		case <-time.After(replyWait):
			t.Fatalf("no reply from %s within %s", spec.recipient, replyWait)
		}
	} else {
		raw, err := os.ReadFile(run.fixture)
		if err != nil {
			t.Fatalf("cassettes without the reply webhook %s: record again (%v)", run.fixture, err)
		}
		postWebhook(t, webhookURL, raw, spec.signReplay)
	}
	step(t, engine, host)
	assertReply(t, client, conv, spec.recipient)
}

// step runs claim rounds until none claims anything.
func step(t *testing.T, engine uma.Engine, host model.Principal) {
	t.Helper()
	call := uma.RuntimeCall{Principal: host, Call: model.Call{Correlation: "imessage-e2e-worker"}}
	for range 20 {
		n, err := engine.Runtime().Step(t.Context(), call, uma.WorkerOptions{Owner: "imessage-e2e", Limit: 10})
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("work never ran out")
}

// gatewayCall calls the gateway as the relay does.
func gatewayCall(t *testing.T, method, url, key string, body []byte) (int, map[string]string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e2eCallbackSecret)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var m map[string]string
	data, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(data, &m)
	return resp.StatusCode, m
}

func postWebhook(t *testing.T, url string, body []byte, sign func([]byte) http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if sign != nil {
		for k, v := range sign(body) {
			req.Header[k] = v
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gateway webhook answered %d %s", resp.StatusCode, data)
	}
}

// assertReply finds the recipient's reply in the conversation the first
// message started: a message.received from their relay.imessage identity.
func assertReply(t *testing.T, client uma.Client, conv model.ConversationRef, recipient string) {
	t.Helper()
	call := model.Call{Correlation: "imessage-e2e-read"}
	page, err := client.Messages().List(t.Context(), model.ListRequest{Call: call, Filter: model.Filter{Conversation: &conv}})
	if err != nil {
		t.Fatal(err)
	}
	var found *model.Message
	for i, m := range page.Items {
		ext := m.Author.External
		if ext.Namespace == relay.NamespaceIMessage && ext.ID == recipient && len(m.Content.Parts) > 0 &&
			strings.EqualFold(strings.TrimSpace(m.Content.Parts[0].Text), e2eReply) {
			found = &page.Items[i]
		}
	}
	if found == nil {
		t.Fatalf("no reply %q from %s in the conversation; messages %+v", e2eReply, recipient, page.Items)
	}
	events, err := client.Events().List(t.Context(), model.ListEvents{
		Call: call, Start: model.StartBeginning,
		Filter: model.Filter{Conversation: &conv, Kinds: []string{model.EventMessageReceived}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("%d message.received events in the conversation, want 1", len(events.Items))
	}
}

// fixtureFields are fields a webhook may carry that name a person or the
// account: their string values are replaced.
var fixtureFields = map[string]string{
	"name": "", "displayName": "", "first_name": "", "last_name": "", "organization_id": "org_test",
}

// scrubFixture replaces the recorded identifiers with their replay values,
// in any letter case (a backend may report an Apple ID as it was typed),
// and blanks names, then indents the JSON for review.
func scrubFixture(raw []byte, values [][2]string) []byte {
	s := string(raw)
	for _, v := range values {
		if v[0] != "" && v[0] != v[1] {
			s = regexp.MustCompile("(?i)"+regexp.QuoteMeta(v[0])).ReplaceAllLiteralString(s, v[1])
		}
	}
	var doc any
	if json.Unmarshal([]byte(s), &doc) != nil {
		return []byte(s)
	}
	blank(doc)
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return []byte(s)
	}
	return append(out, '\n')
}

func blank(v any) {
	switch v := v.(type) {
	case map[string]any:
		for k, x := range v {
			if repl, ok := fixtureFields[k]; ok {
				if _, isString := x.(string); isString {
					v[k] = repl
					continue
				}
			}
			blank(x)
		}
	case []any:
		for _, x := range v {
			blank(x)
		}
	}
}

var fixtureMu sync.Mutex

func writeFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	fixtureMu.Lock()
	defer fixtureMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
