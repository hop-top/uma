package a2a_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const tenant model.TenantID = "a2a-tenant"

var (
	chanA = model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000a1"}
	inbox = model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000c1"}
)

const destination = "agents"

// testConfig is a valid channel configuration; override mutates it.
func testConfig(override ...func(*a2a.Config)) a2a.Config {
	c := a2a.Config{
		Destination: destination, PublicURL: "https://agent.example.com/a2a",
		Name: "Support desk", Description: "Answers support requests", Version: "1.0.0",
		Skills: []a2a.Skill{{ID: "support", Name: "Support", Description: "Handles requests", Tags: []string{"support"}}},
		SecuritySchemes: map[string]json.RawMessage{
			"bearer": json.RawMessage(`{"httpAuthSecurityScheme":{"scheme":"Bearer"}}`),
		},
		SecurityRequirements: []map[string][]string{{"bearer": {}}},
		BlockingWait:         "2s", PollInterval: "50ms",
	}
	for _, f := range override {
		f(&c)
	}
	return c
}

// headerAuth is the host authenticator of the tests: the agent is what the
// X-Agent header says; X-Refuse refuses, and an absent header does too.
func headerAuth(r *http.Request, _ model.ChannelRef) (a2a.Principal, error) {
	switch id := r.Header.Get("X-Agent"); {
	case r.Header.Get("X-Unavailable") != "":
		return a2a.Principal{}, model.NewError(model.Unavailable, "idp down")
	case id == "":
		return a2a.Principal{}, model.NewError(model.Unauthorized, "no credentials")
	default:
		return a2a.Principal{ID: id}, nil
	}
}

var allowAll = policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
	return policy.Decision{Allow: true}, nil
})

// rig is one a2a.messages channel served over httptest through an engine.
type rig struct {
	t      *testing.T
	f      *a2a.Factory
	engine uma.Engine
	st     store.Store
	srv    *httptest.Server
	client uma.Client
	clock  *portstest.FixedClock
	ops    atomic.Int64
}

type rigOptions struct {
	options a2a.Options
	config  []func(*a2a.Config)
	// factory wraps the factory the engine registers.
	factory    func(adapter.Factory) adapter.Factory
	authorizer policy.Authorizer
	// wrap decorates the mounted handler.
	wrap func(http.Handler) http.Handler
}

func newRig(t *testing.T, opt ...rigOptions) *rig {
	t.Helper()
	var o rigOptions
	if len(opt) > 0 {
		o = opt[0]
	}
	if o.options.Authenticate == nil {
		o.options.Authenticate = headerAuth
	}
	f, err := a2a.New(o.options)
	if err != nil {
		t.Fatal(err)
	}
	var reg adapter.Factory = f
	if o.factory != nil {
		reg = o.factory(f)
	}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
	st := memory.New(memory.Options{Clock: clock})
	var authz policy.Authorizer = allowAll
	if o.authorizer != nil {
		authz = o.authorizer
	}
	engine, err := uma.New(uma.Config{
		Store: st, Authorizer: authz, Adapters: []adapter.Factory{reg}, Clock: clock,
		IDs: portstest.NewSequenceIDs(0xa2a), Blobs: &portstest.MemoryBlobs{},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	err = st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := tx.PutChannel(ctx, model.Channel{
			Ref: chanA, Type: a2a.Type, Configuration: a2a.Configuration(testConfig(o.config...)), Revision: "r1",
		}, ""); err != nil {
			return err
		}
		return tx.PutInbox(ctx, model.Inbox{
			Ref: inbox, Channel: chanA, Name: "agents", Destinations: []string{destination}, Revision: "r1",
			Selection: model.Selection{Mode: model.ModePerAgentContext},
		}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	hd, err := engine.Runtime().Webhook(ctx, uma.RuntimeCall{
		Principal: model.Principal{Tenant: tenant, Actor: "a2a-service"}, Call: model.Call{Correlation: "a2a"},
	}, chanA)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	mux := http.NewServeMux()
	if o.wrap != nil {
		hd = o.wrap(hd)
	}
	mux.Handle("/a2a/", hd)
	srv := httptest.NewServer(mux)
	client, err := engine.For(model.Principal{Tenant: tenant, Actor: "host"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.Close()
		_ = engine.Close(context.Background())
	})
	return &rig{t: t, f: f, engine: engine, st: st, srv: srv, client: client, clock: clock}
}

// reply is a decoded JSON-RPC response.
type reply struct {
	Status int
	Header http.Header
	ID     json.RawMessage
	Result json.RawMessage
	Error  *rpcErr
	Body   []byte
}

type rpcErr struct {
	Code    int               `json:"code"`
	Message string            `json:"message"`
	Data    []json.RawMessage `json:"data"`
}

// info returns the ErrorInfo of the error, if it carries one.
func (e *rpcErr) info(t *testing.T) (info errInfo) {
	t.Helper()
	for _, d := range e.Data {
		var probe struct {
			Type string `json:"@type"`
		}
		if json.Unmarshal(d, &probe) == nil && strings.HasSuffix(probe.Type, "google.rpc.ErrorInfo") {
			if err := json.Unmarshal(d, &info); err != nil {
				t.Fatal(err)
			}
			return info
		}
	}
	t.Fatalf("error %d has no ErrorInfo in %s", e.Code, e.Data)
	return info
}

type errInfo struct {
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata"`
}

// rpc posts a JSON-RPC request as agent, with the A2A-Version header.
func (r *rig) rpc(agent, method string, params any, header ...string) reply {
	r.t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		r.t.Fatal(err)
	}
	return r.post(append([]string{"X-Agent", agent, "A2A-Version", "1.0"}, header...), body)
}

// post sends body to the endpoint with header pairs; an empty value leaves
// the header out.
func (r *rig) post(header []string, body []byte) reply {
	r.t.Helper()
	req, err := http.NewRequestWithContext(r.t.Context(), http.MethodPost, r.srv.URL+"/a2a/", bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		if header[i+1] == "" {
			req.Header.Del(header[i])
			continue
		}
		req.Header.Set(header[i], header[i+1])
	}
	return r.do(req)
}

func (r *rig) do(req *http.Request) reply {
	r.t.Helper()
	res, err := r.srv.Client().Do(req) // #nosec G704 -- request to the test server
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		r.t.Fatal(err)
	}
	out := reply{Status: res.StatusCode, Header: res.Header, Body: b}
	var env struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *rpcErr         `json:"error"`
	}
	if res.StatusCode == http.StatusOK && json.Unmarshal(b, &env) == nil {
		out.ID, out.Result, out.Error = env.ID, env.Result, env.Error
	}
	return out
}

// ok requires a JSON-RPC success and returns its result.
func (rp reply) ok(t *testing.T) json.RawMessage {
	t.Helper()
	if rp.Status != http.StatusOK || rp.Error != nil || len(rp.Result) == 0 {
		t.Fatalf("want a result, got HTTP %d: %s", rp.Status, rp.Body)
	}
	return rp.Result
}

// fail requires a JSON-RPC error of code.
func (rp reply) fail(t *testing.T, code int) *rpcErr {
	t.Helper()
	if rp.Status != http.StatusOK || rp.Error == nil || rp.Error.Code != code {
		t.Fatalf("want error %d, got HTTP %d: %s", code, rp.Status, rp.Body)
	}
	return rp.Error
}

// Decoded results.
type task struct {
	ID        string `json:"id"`
	ContextID string `json:"contextId"`
	Status    struct {
		State     string   `json:"state"`
		Message   *message `json:"message"`
		Timestamp string   `json:"timestamp"`
	} `json:"status"`
	Artifacts []struct {
		ArtifactID string `json:"artifactId"`
		Name       string `json:"name"`
		Parts      []part `json:"parts"`
	} `json:"artifacts"`
	History []message `json:"history"`
}

type message struct {
	MessageID string `json:"messageId"`
	ContextID string `json:"contextId"`
	TaskID    string `json:"taskId"`
	Role      string `json:"role"`
	Parts     []part `json:"parts"`
}

type part struct {
	Text      *string         `json:"text"`
	Raw       *string         `json:"raw"`
	URL       *string         `json:"url"`
	Data      json.RawMessage `json:"data"`
	Filename  string          `json:"filename"`
	MediaType string          `json:"mediaType"`
}

type sendResult struct {
	Task    *task    `json:"task"`
	Message *message `json:"message"`
}

type listResult struct {
	Tasks         []task  `json:"tasks"`
	NextPageToken *string `json:"nextPageToken"`
	PageSize      int     `json:"pageSize"`
}

func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

// send posts SendMessage with one text part.
func (r *rig) send(agent, messageID, text string, extra map[string]any, header ...string) reply {
	r.t.Helper()
	msg := map[string]any{
		"messageId": messageID, "role": "ROLE_USER", "parts": []map[string]any{{"text": text}},
	}
	params := map[string]any{"message": msg}
	for k, v := range extra {
		switch k {
		case "configuration":
			params[k] = v
		default:
			msg[k] = v
		}
	}
	return r.rpc(agent, "SendMessage", params, header...)
}

func immediately() map[string]any {
	return map[string]any{"configuration": map[string]any{"returnImmediately": true}}
}

// textOf returns the text of the first part.
func textOf(parts []part) string {
	if len(parts) == 0 || parts[0].Text == nil {
		return ""
	}
	return *parts[0].Text
}

func text(s string) model.Content {
	return model.Content{Parts: []model.Part{{Kind: "text", Text: s}}}
}

func (r *rig) op() model.Command {
	return model.Command{Call: model.Call{Correlation: "host"}, Operation: model.OperationID("op-" + itoa(r.ops.Add(1)))}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// denials is an authorizer that allows everything but the actions denied.
type denials struct {
	mu     sync.Mutex
	denied map[string]bool
}

func (d *denials) deny(actions ...string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.denied = map[string]bool{}
	for _, a := range actions {
		d.denied[a] = true
	}
}

func (d *denials) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return policy.Decision{Allow: !d.denied[a.Action]}, nil
}

// server is a handler served over httptest for tests that bypass the
// engine.
type server struct {
	*rig
}

func newServer(t *testing.T, h http.Handler) server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/a2a/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return server{&rig{t: t, srv: srv}}
}
