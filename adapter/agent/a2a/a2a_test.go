package a2a_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
)

func TestDescribe(t *testing.T) {
	f, err := a2a.New(a2a.Options{Authenticate: headerAuth})
	if err != nil {
		t.Fatal(err)
	}
	d := f.Describe()
	if d.Type != model.TypeA2aMessages || d.ContractMajor != adapter.ContractMajor {
		t.Fatalf("descriptor %+v", d)
	}
	if !slices.Equal(d.SenderKinds, []string{model.ContactKindAgent}) {
		t.Fatalf("sender kinds = %v, want [agent]", d.SenderKinds)
	}
	if !slices.Equal(d.IdentityNamespaces, []string{"a2a.principal"}) {
		t.Fatalf("namespaces = %v", d.IdentityNamespaces)
	}
	want := []model.Capability{model.CapabilityReceiveHTTP, model.CapabilitySend}
	if !slices.Equal(d.Capabilities, want) {
		t.Fatalf("capabilities = %v, want %v", d.Capabilities, want)
	}
	if d.Exchange.MessageIDs != adapter.MessageIDsProvider {
		t.Fatalf("message IDs = %q, want provider", d.Exchange.MessageIDs)
	}
	for _, e := range []string{model.EventMessageReceived, model.EventMessageCreated, model.EventTaskUpdated} {
		if !slices.Contains(d.Events, e) {
			t.Errorf("events %v lack %s", d.Events, e)
		}
	}
	if d.ConfigurationSchema.Name != "a2a.messages.config" || d.Maturity != "experimental" {
		t.Fatalf("schema %+v maturity %q", d.ConfigurationSchema, d.Maturity)
	}
	if err := model.ValidateTypeKey("a2a.messages"); err != nil {
		t.Fatalf("a2a.messages is not a valid type key: %v", err)
	}
}

func TestNewRequiresAuthenticator(t *testing.T) {
	if _, err := a2a.New(a2a.Options{}); !model.IsCode(err, model.Validation) {
		t.Fatalf("New without Authenticate = %v, want validation", err)
	}
	if _, err := a2a.New(a2a.Options{Authenticate: headerAuth, ErrorDomain: "not a domain"}); !model.IsCode(err, model.Validation) {
		t.Fatalf("New with a bad error domain = %v, want validation", err)
	}
}

func TestValidateConfiguration(t *testing.T) {
	f, _ := a2a.New(a2a.Options{Authenticate: headerAuth})
	bad := map[string]func(*a2a.Config){
		"no destination": func(c *a2a.Config) { c.Destination = "" },
		"relative url":   func(c *a2a.Config) { c.PublicURL = "/a2a" },
		"no skills":      func(c *a2a.Config) { c.Skills = nil },
		"skill no tags":  func(c *a2a.Config) { c.Skills[0].Tags = nil },
		"bad scheme": func(c *a2a.Config) {
			c.SecuritySchemes = map[string]json.RawMessage{"x": json.RawMessage(`{"nope":{}}`)}
		},
		"two kinds in a scheme": func(c *a2a.Config) {
			c.SecuritySchemes = map[string]json.RawMessage{"x": json.RawMessage(`{"httpAuthSecurityScheme":{},"mtlsSecurityScheme":{}}`)}
		},
		"undeclared requirement": func(c *a2a.Config) { c.SecurityRequirements = []map[string][]string{{"ghost": nil}} },
		"zero wait":              func(c *a2a.Config) { c.BlockingWait = "0s" },
		"wait not a duration":    func(c *a2a.Config) { c.BlockingWait = "soon" },
		"poll below range":       func(c *a2a.Config) { c.PollInterval = "1ms" },
		"poll above wait":        func(c *a2a.Config) { c.BlockingWait = "1s"; c.PollInterval = "2s" },
	}
	if issues := f.Validate(a2a.Configuration(testConfig())); len(issues) != 0 {
		t.Fatalf("valid configuration refused: %+v", issues)
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			if issues := f.Validate(a2a.Configuration(testConfig(mutate))); len(issues) == 0 {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	if issues := f.Validate(model.Extension{Name: "other.config", Version: 1}); len(issues) != 1 || issues[0].Code != string(model.SchemaMismatch) {
		t.Fatalf("foreign schema = %+v", issues)
	}
	if issues := f.Validate(model.Extension{Name: a2a.ConfigSchema, Version: 1, Data: json.RawMessage(`{"unknown":1}`)}); len(issues) == 0 {
		t.Fatal("unknown field accepted")
	}
}

// A failing or absent host authentication is HTTP 401 naming the declared
// scheme, before the body is read; a retryable failure is 503.
func TestAuthenticateRequired(t *testing.T) {
	r := newRig(t)
	rp := r.post([]string{"A2A-Version", "1.0"}, []byte("not even json"))
	if rp.Status != http.StatusUnauthorized {
		t.Fatalf("no credentials: HTTP %d, want 401 before the body is read", rp.Status)
	}
	if got := rp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Fatalf("WWW-Authenticate = %q, want the declared Bearer scheme", got)
	}
	if rp := r.rpc("agent-1", "GetTask", map[string]any{"id": "x"}, "X-Unavailable", "1"); rp.Status != http.StatusServiceUnavailable {
		t.Fatalf("retryable failure: HTTP %d, want 503", rp.Status)
	}
	// An authenticated request reaches the method.
	r.rpc("agent-1", "GetTask", map[string]any{"id": "x"}).fail(t, -32001)
}

func TestAuthenticateGetsChannel(t *testing.T) {
	var got model.ChannelRef
	r := newRig(t, rigOptions{options: a2a.Options{Authenticate: func(_ *http.Request, ch model.ChannelRef) (a2a.Principal, error) {
		got = ch
		return a2a.Principal{ID: "agent-1"}, nil
	}}})
	r.rpc("agent-1", "GetTask", map[string]any{"id": "x"})
	if got != chanA {
		t.Fatalf("authenticator saw channel %v, want %v", got, chanA)
	}
}

// An authenticator returning a principal the engine cannot name refuses
// the request: it never reaches the sink.
func TestInvalidPrincipalRefused(t *testing.T) {
	r := newRig(t, rigOptions{options: a2a.Options{Authenticate: func(*http.Request, model.ChannelRef) (a2a.Principal, error) {
		return a2a.Principal{ID: ""}, nil
	}}})
	if rp := r.rpc("x", "GetTask", map[string]any{"id": "x"}); rp.Status != http.StatusUnauthorized {
		t.Fatalf("empty principal: HTTP %d, want 401", rp.Status)
	}
}

func TestVersionHeaderRequired(t *testing.T) {
	r := newRig(t)
	for name, h := range map[string][]string{
		"missing":     {"A2A-Version", ""},
		"0.3":         {"A2A-Version", "0.3"},
		"1.1":         {"A2A-Version", "1.1"},
		"with patch":  {"A2A-Version", "1.0.1"},
		"other value": {"A2A-Version", "latest"},
	} {
		t.Run(name, func(t *testing.T) {
			e := r.rpc("agent-1", "GetTask", map[string]any{"id": "x"}, h...).fail(t, -32009)
			if info := e.info(t); info.Reason != "VERSION_NOT_SUPPORTED" || info.Domain != "a2a-protocol.org" {
				t.Fatalf("detail = %+v", info)
			}
		})
	}
	r.rpc("agent-1", "GetTask", map[string]any{"id": "x"}).fail(t, -32001)
	// The version may also travel as a request parameter.
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, r.srv.URL+"/a2a/?A2A-Version=1.0", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Agent", "agent-1")
	r.do(req).fail(t, -32001)
}

func TestJSONRPCEnvelope(t *testing.T) {
	r := newRig(t)
	post := func(body string) reply {
		return r.post([]string{"X-Agent", "agent-1", "A2A-Version", "1.0"}, []byte(body))
	}
	post(`{"jsonrpc":"2.0","id":1,"method":`).fail(t, -32700)
	post(`[{"jsonrpc":"2.0","id":1,"method":"GetTask"}]`).fail(t, -32600)
	post(`{"jsonrpc":"1.0","id":1,"method":"GetTask"}`).fail(t, -32600)
	post(`{"jsonrpc":"2.0","id":1}`).fail(t, -32600)
	post(`{"jsonrpc":"2.0","method":"GetTask"}`).fail(t, -32600)
	post(`{"jsonrpc":"2.0","id":{"a":1},"method":"GetTask"}`).fail(t, -32600)
	post(`{"jsonrpc":"2.0","id":1,"method":"Nope"}`).fail(t, -32601)
	for _, id := range []string{`7`, `"abc"`, `null`} {
		rp := post(`{"jsonrpc":"2.0","id":` + id + `,"method":"GetTask","params":{"id":"x"}}`)
		rp.fail(t, -32001)
		if string(rp.ID) != id {
			t.Errorf("response id = %s, want %s", rp.ID, id)
		}
	}
	post(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":[1]}`).fail(t, -32602)
	post(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x","historyLength":-1}}`).fail(t, -32602)
	// Method and content-type guards.
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, r.srv.URL+"/a2a/", nil)
	if rp := r.do(req); rp.Status != http.StatusMethodNotAllowed {
		t.Fatalf("PUT: HTTP %d, want 405", rp.Status)
	}
	rp := r.post([]string{"X-Agent", "agent-1", "Content-Type", "text/plain"}, []byte("{}"))
	if rp.Status != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: HTTP %d, want 415", rp.Status)
	}
}

func TestStreamingUnsupported(t *testing.T) {
	r := newRig(t)
	for _, m := range []string{"SendStreamingMessage", "SubscribeToTask", "GetExtendedAgentCard"} {
		e := r.rpc("agent-1", m, map[string]any{}).fail(t, -32004)
		if info := e.info(t); info.Domain != "a2a-protocol.org" {
			t.Errorf("%s: detail %+v", m, info)
		}
	}
}

func TestPushUnsupported(t *testing.T) {
	r := newRig(t)
	for _, m := range []string{
		"CreateTaskPushNotificationConfig", "GetTaskPushNotificationConfig",
		"ListTaskPushNotificationConfigs", "DeleteTaskPushNotificationConfig",
	} {
		e := r.rpc("agent-1", m, map[string]any{"taskId": "t"}).fail(t, -32003)
		if info := e.info(t); info.Reason != "PUSH_NOTIFICATION_NOT_SUPPORTED" {
			t.Errorf("%s: detail %+v", m, info)
		}
	}
}
