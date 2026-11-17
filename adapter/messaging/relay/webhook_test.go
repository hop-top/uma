package relay_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

// sink records what the handler hands the engine.
type sink struct {
	mu  sync.Mutex
	got []model.Ingress
	err error
	// receipt is the answer of an accepted ingress.
	receipt model.IngressReceipt
}

func (s *sink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return model.IngressReceipt{}, s.err
	}
	s.got = append(s.got, in)
	return s.receipt, nil
}

func (s *sink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, errors.New("not used")
}

func (s *sink) ingresses() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.got...)
}

// post is one webhook call.
type call struct {
	method string
	body   string
	header http.Header
}

func serve(t *testing.T, inst *relay.Instance, s *sink, c call) *httptest.ResponseRecorder {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodPost
	}
	req := httptest.NewRequest(c.method, "/hooks/relay", strings.NewReader(c.body))
	for k, v := range c.header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	inst.Handler(s).ServeHTTP(rec, req)
	return rec
}

func bearer(secret string) http.Header { return http.Header{"Authorization": {"Bearer " + secret}} }

const hello = `{"from":"+15550100001","to":"+15550100002","body":"hello","message_id":"m-1"}`

// good is a call that authenticates under baseConfig (bearer).
func good(body string) call { return call{body: body, header: bearer(webhookSecret)} }

func newInstance(t *testing.T, cfg relay.Config, c *creds) *relay.Instance {
	t.Helper()
	if c == nil {
		c = newCreds()
	}
	return open(t, relay.NewFactory(relay.Options{}), cfg, adapter.Dependencies{Credentials: c})
}

func TestWebhookAcceptsAMessage(t *testing.T) {
	s := &sink{}
	rec := serve(t, newInstance(t, baseConfig(), nil), s, good(hello))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("answer %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var ans map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &ans); err != nil || ans["status"] != "accepted" {
		t.Fatalf("body %q: %v", rec.Body.String(), err)
	}
	got := s.ingresses()
	if len(got) != 1 {
		t.Fatalf("%d ingresses", len(got))
	}
	env := got[0].Envelope
	if env.Kind != model.EnvelopeMessage || env.Destination != "+15550100002" || env.MessageID != "m-1" {
		t.Errorf("envelope %+v", env)
	}
	if want := (model.ExternalRef{Channel: testChannel, Namespace: relay.NamespaceSMS, Kind: relay.RouteKind, ID: "+15550100001"}); env.Sender != want {
		t.Errorf("sender %+v, want %+v", env.Sender, want)
	}
	if env.Assurance != "verified" || env.SenderKind != "" || env.Audience.Visibility != model.Public || !env.OccurredAt.Equal(testNow) {
		t.Errorf("envelope %+v", env)
	}
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Kind != model.PartText || env.Content.Parts[0].Text != "hello" {
		t.Errorf("content %+v", env.Content)
	}
	if env.EventID == "" || got[0].Correlation.Validate() != nil {
		t.Errorf("event %q correlation %q", env.EventID, got[0].Correlation)
	}
}

func TestWebhookWhatsAppMediumUsesItsOwnNamespace(t *testing.T) {
	cfg := baseConfig()
	cfg.Medium = relay.MediumWhatsApp
	s := &sink{}
	if rec := serve(t, newInstance(t, cfg, nil), s, good(hello)); rec.Code != http.StatusOK {
		t.Fatalf("answer %d", rec.Code)
	}
	if ns := s.ingresses()[0].Envelope.Sender.Namespace; ns != relay.NamespaceWhatsApp {
		t.Errorf("namespace %q", ns)
	}
}

func TestWebhookDeliveryIdentity(t *testing.T) {
	eventOf := func(body string) string {
		s := &sink{}
		serve(t, newInstance(t, baseConfig(), nil), s, good(body))
		return s.ingresses()[0].Envelope.EventID
	}
	a := eventOf(hello)
	if a != eventOf(hello) {
		t.Error("a redelivery changes its event id: the engine could not deduplicate it")
	}
	if a == eventOf(strings.Replace(hello, `"+15550100001"`, `"+15550100009"`, 1)) {
		t.Error("two senders using one message id share an event id: one would drop the other")
	}
	if a == eventOf(strings.Replace(hello, `"m-1"`, `"m-2"`, 1)) {
		t.Error("two message ids share an event id")
	}
}

func TestWebhookFieldNamesAndAliases(t *testing.T) {
	cfg := baseConfig()
	cfg.Inbound.Fields = relay.Fields{
		From: []string{"From", "from"}, To: []string{"To"}, Body: []string{"Body", "text"}, MessageID: []string{"MessageSid", "SmsSid"},
	}
	tests := []struct {
		name, body string
		text, id   string
	}{
		{"first alias", `{"From":"+1","To":"+2","Body":"hi","MessageSid":"SM1"}`, "hi", "SM1"},
		{"second alias", `{"from":"+1","To":"+2","text":"yo","SmsSid":"SM2"}`, "yo", "SM2"},
		{"first alias wins", `{"From":"+1","from":"+9","To":"+2","Body":"a","text":"b","MessageSid":"SM3","SmsSid":"SM4"}`, "a", "SM3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &sink{}
			rec := serve(t, newInstance(t, cfg, nil), s, good(tt.body))
			if rec.Code != http.StatusOK {
				t.Fatalf("answer %d", rec.Code)
			}
			env := s.ingresses()[0].Envelope
			if env.Content.Parts[0].Text != tt.text || env.MessageID != tt.id || env.Sender.ID != "+1" || env.Destination != "+2" {
				t.Errorf("envelope %+v", env)
			}
		})
	}
	// Names not configured are not read: the neutral keys mean nothing here.
	s := &sink{}
	if rec := serve(t, newInstance(t, cfg, nil), s, good(hello)); rec.Code != http.StatusBadRequest {
		t.Errorf("neutral keys under renamed config: %d", rec.Code)
	}
}

func TestWebhookRefusesBadMessages(t *testing.T) {
	tests := []struct{ name, body string }{
		{"not json", `from=+1&to=+2&body=hi`},
		{"empty body", ``},
		{"array", `[]`},
		{"null", `null`},
		{"string", `"x"`},
		{"missing from", `{"to":"+2","body":"hi","message_id":"m"}`},
		{"missing to", `{"from":"+1","body":"hi","message_id":"m"}`},
		{"empty from", `{"from":"","to":"+2","body":"hi","message_id":"m"}`},
		{"from not a string", `{"from":15550100001,"to":"+2","body":"hi","message_id":"m"}`},
		{"from with control character", "{\"from\":\"+1\\u0000\",\"to\":\"+2\",\"body\":\"hi\",\"message_id\":\"m\"}"},
		{"from too long", `{"from":"` + strings.Repeat("1", 200) + `","to":"+2","body":"hi","message_id":"m"}`},
		{"no body and no media", `{"from":"+1","to":"+2","message_id":"m"}`},
		{"empty body and no media", `{"from":"+1","to":"+2","body":"","message_id":"m"}`},
		{"blank body and no media", `{"from":"+1","to":"+2","body":" \n\t","message_id":"m"}`},
		{"body not a string", `{"from":"+1","to":"+2","body":["hi"],"message_id":"m"}`},
		{"missing message id", `{"from":"+1","to":"+2","body":"hi"}`},
		{"message id not a string", `{"from":"+1","to":"+2","body":"hi","message_id":7}`},
		{"message id too long", `{"from":"+1","to":"+2","body":"hi","message_id":"` + strings.Repeat("m", 300) + `"}`},
		{"trailing data", hello + `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &sink{}
			rec := serve(t, newInstance(t, baseConfig(), nil), s, good(tt.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("answer %d, want 400", rec.Code)
			}
			if len(s.ingresses()) != 0 {
				t.Fatal("the sink saw a refused message")
			}
		})
	}
}

func TestWebhookGeneratesMissingIDsOnlyWhenAsked(t *testing.T) {
	cfg := baseConfig()
	cfg.Inbound.GenerateMissingID = true
	body := `{"from":"+1","to":"+2","body":"hi"}`
	s := &sink{}
	inst := newInstance(t, cfg, nil)
	for range 2 {
		if rec := serve(t, inst, s, good(body)); rec.Code != http.StatusOK {
			t.Fatalf("answer %d", rec.Code)
		}
	}
	got := s.ingresses()
	if got[0].Envelope.MessageID == "" || got[0].Envelope.MessageID == got[1].Envelope.MessageID ||
		got[0].Envelope.EventID == got[1].Envelope.EventID {
		t.Errorf("generated ids must differ: %+v %+v", got[0].Envelope, got[1].Envelope)
	}
	// A message that does carry an id keeps it.
	s = &sink{}
	serve(t, inst, s, good(hello))
	if s.ingresses()[0].Envelope.MessageID != "m-1" {
		t.Errorf("id %q", s.ingresses()[0].Envelope.MessageID)
	}
}

func TestWebhookMedia(t *testing.T) {
	ok := func(media string) model.Envelope {
		t.Helper()
		s := &sink{}
		rec := serve(t, newInstance(t, baseConfig(), nil), s, good(`{"from":"+1","to":"+2","message_id":"m","media":`+media+`}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("answer %d for %s", rec.Code, media)
		}
		return s.ingresses()[0].Envelope
	}
	// media alone is a message.
	env := ok(`[{"url":"https://cdn.example.com/a.jpg","content_type":"image/jpeg"},"https://cdn.example.com/b"]`)
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Kind != model.PartStructured {
		t.Fatalf("parts %+v", env.Content.Parts)
	}
	p := env.Content.Parts[0].Structured
	if p.Name != relay.PayloadMedia || p.Version != 1 {
		t.Fatalf("part %+v", p)
	}
	var m struct {
		Items []struct {
			URL         string `json:"url"`
			ContentType string `json:"content_type"`
		} `json:"items"`
	}
	if err := json.Unmarshal(p.Data, &m); err != nil || len(m.Items) != 2 ||
		m.Items[0].URL != "https://cdn.example.com/a.jpg" || m.Items[0].ContentType != "image/jpeg" ||
		m.Items[1].URL != "https://cdn.example.com/b" || m.Items[1].ContentType != "" {
		t.Fatalf("media %s: %+v %v", p.Data, m, err)
	}
	// text and media are two parts, text first.
	s := &sink{}
	serve(t, newInstance(t, baseConfig(), nil), s, good(`{"from":"+1","to":"+2","body":"look","message_id":"m","media":["https://cdn.example.com/a"]}`))
	parts := s.ingresses()[0].Envelope.Content.Parts
	if len(parts) != 2 || parts[0].Kind != model.PartText || parts[1].Kind != model.PartStructured {
		t.Errorf("parts %+v", parts)
	}
}

func TestWebhookRefusesBadMedia(t *testing.T) {
	many := strings.TrimSuffix(strings.Repeat(`"https://cdn.example.com/a",`, 11), ",")
	for name, media := range map[string]string{ // #nosec G101 -- refused-media fixtures
		"not an array":           `"https://cdn.example.com/a"`,
		"file scheme":            `["file:///etc/passwd"]`,
		"ftp scheme":             `["ftp://cdn.example.com/a"]`,
		"scheme-less":            `["//cdn.example.com/a"]`,
		"content type too long":  `[{"url":"https://cdn.example.com/a","content_type":"` + strings.Repeat("a", 300) + `"}]`,
		"javascript scheme":      `["javascript:alert(1)"]`,
		"no host":                `["https:///a"]`,
		"credentials in the url": `["https://u:p@cdn.example.com/a"]`,
		"number":                 `[7]`,
		"object without url":     `[{"content_type":"image/png"}]`,
		"content type not text":  `[{"url":"https://cdn.example.com/a","content_type":3}]`,
		"too many":               `[` + many + `]`,
		"url too long":           `["https://cdn.example.com/` + strings.Repeat("a", 3000) + `"]`,
	} {
		t.Run(name, func(t *testing.T) {
			s := &sink{}
			rec := serve(t, newInstance(t, baseConfig(), nil), s, good(`{"from":"+1","to":"+2","body":"hi","message_id":"m","media":`+media+`}`))
			if rec.Code != http.StatusBadRequest || len(s.ingresses()) != 0 {
				t.Fatalf("answer %d, %d ingresses", rec.Code, len(s.ingresses()))
			}
		})
	}
}

func TestWebhookIgnoresUnknownFields(t *testing.T) {
	s := &sink{}
	rec := serve(t, newInstance(t, baseConfig(), nil), s, good(`{"from":"+1","to":"+2","body":"hi","message_id":"m","provider":"acme","extra":{"a":1}}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d", rec.Code)
	}
}

func TestWebhookMethod(t *testing.T) {
	c := newCreds()
	s := &sink{}
	rec := serve(t, newInstance(t, baseConfig(), c), s, call{method: http.MethodGet, header: bearer(webhookSecret)})
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("answer %d allow %q", rec.Code, rec.Header().Get("Allow"))
	}
	if len(c.requests()) != 0 {
		t.Error("a secret was resolved for a request that is not a delivery")
	}
}

// Nothing unauthenticated reaches the parser or the sink.
func TestWebhookAuthenticatesBeforeParsing(t *testing.T) {
	garbage := `{not json`
	tests := []struct {
		name   string
		header http.Header
		want   int
	}{
		{"no credential", nil, http.StatusUnauthorized},
		{"wrong credential", bearer("not-the-secret-0123456789abcdef"), http.StatusUnauthorized},
		{"wrong scheme word", http.Header{"Authorization": {"Basic " + webhookSecret}}, http.StatusUnauthorized},
		{"right credential", bearer(webhookSecret), http.StatusBadRequest}, // parsed only now
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &sink{}
			rec := serve(t, newInstance(t, baseConfig(), nil), s, call{body: garbage, header: tt.header})
			if rec.Code != tt.want || len(s.ingresses()) != 0 {
				t.Fatalf("answer %d, want %d (%d ingresses)", rec.Code, tt.want, len(s.ingresses()))
			}
			if tt.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("no bearer challenge")
			}
			if tt.want == http.StatusUnauthorized && strings.Contains(rec.Body.String(), "json") {
				t.Errorf("body %q reveals parsing", rec.Body.String())
			}
		})
	}
}

func TestWebhookBodyBound(t *testing.T) {
	big := `{"from":"+1","to":"+2","message_id":"m","body":"` + strings.Repeat("x", relay.DefaultMaxBodyBytes) + `"}`
	c := newCreds()
	s := &sink{}
	rec := serve(t, newInstance(t, baseConfig(), c), s, good(big))
	if rec.Code != http.StatusRequestEntityTooLarge || len(s.ingresses()) != 0 {
		t.Fatalf("answer %d", rec.Code)
	}
	if len(c.requests()) != 0 {
		t.Error("a secret was resolved for an oversize body")
	}
	// The channel's own limit applies.
	cfg := baseConfig()
	cfg.Inbound.MaxBodyBytes = 64
	if rec := serve(t, newInstance(t, cfg, nil), &sink{}, good(hello+strings.Repeat(" ", 64))); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("channel limit: %d", rec.Code)
	}
	if rec := serve(t, newInstance(t, cfg, nil), &sink{}, good(`{"from":"+1","to":"+2","body":"x","message_id":"m"}`)); rec.Code != http.StatusOK {
		t.Errorf("a body inside the limit: %d", rec.Code)
	}
}

func TestWebhookCredentialFailureIsRetryable(t *testing.T) {
	c := newCreds()
	c.fail = errors.New("vault down")
	s := &sink{}
	rec := serve(t, newInstance(t, baseConfig(), c), s, good(hello))
	if rec.Code != http.StatusServiceUnavailable || len(s.ingresses()) != 0 {
		t.Fatalf("answer %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "vault") {
		t.Error("the credential port's error reaches the sender")
	}
	// No credential port at all never passes a request.
	inst := open(t, relay.NewFactory(relay.Options{}), baseConfig(), adapter.Dependencies{})
	if rec := serve(t, inst, &sink{}, good(hello)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no credentials port: %d", rec.Code)
	}
}

func TestWebhookResolvesTheChannelCredentialForItsPurpose(t *testing.T) {
	c := newCreds()
	serve(t, newInstance(t, baseConfig(), c), &sink{}, good(hello))
	reqs := c.requests()
	if len(reqs) != 1 || reqs[0].Purpose != relay.PurposeWebhook || reqs[0].Channel != testChannel || reqs[0].Reference != "relay-account" {
		t.Fatalf("requests %+v", reqs)
	}
}

func TestWebhookSecretRotation(t *testing.T) {
	c := newCreds()
	c.set("relay-old|"+relay.PurposeWebhook, secretOf("old-secret-0123456789abcdef0123"))
	cfg := baseConfig()
	cfg.Auth.PreviousCredentialRef = "relay-old"
	for name, secret := range map[string]string{"current": webhookSecret, "previous": "old-secret-0123456789abcdef0123"} {
		t.Run(name, func(t *testing.T) {
			if rec := serve(t, newInstance(t, cfg, c), &sink{}, good2(secret)); rec.Code != http.StatusOK {
				t.Fatalf("answer %d", rec.Code)
			}
		})
	}
	if rec := serve(t, newInstance(t, cfg, c), &sink{}, good2("neither-secret-0123456789abcdef")); rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key: %d", rec.Code)
	}
}

func TestWebhookSinkFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"transient", model.NewError(model.Transient, "busy"), http.StatusServiceUnavailable},
		{"unavailable", model.NewError(model.Unavailable, "closed"), http.StatusServiceUnavailable},
		{"validation", model.NewError(model.Validation, "bad"), http.StatusUnprocessableEntity},
		{"unauthorized", model.NewError(model.Unauthorized, "no"), http.StatusUnprocessableEntity},
		{"plain error", errors.New("boom"), http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serve(t, newInstance(t, baseConfig(), nil), &sink{err: tt.err}, good(hello))
			if rec.Code != tt.want {
				t.Fatalf("answer %d, want %d", rec.Code, tt.want)
			}
			if strings.Contains(rec.Body.String(), "busy") || strings.Contains(rec.Body.String(), "boom") {
				t.Errorf("sink error text reaches the sender: %q", rec.Body.String())
			}
		})
	}
}

func TestWebhookDuplicateIsAccepted(t *testing.T) {
	s := &sink{receipt: model.IngressReceipt{Duplicate: true, Committed: true}}
	if rec := serve(t, newInstance(t, baseConfig(), nil), s, good(hello)); rec.Code != http.StatusOK {
		t.Fatalf("answer %d", rec.Code)
	}
}

func TestWebhookClosedInstance(t *testing.T) {
	inst := newInstance(t, baseConfig(), nil)
	_ = inst.Close(t.Context())
	s := &sink{}
	if rec := serve(t, inst, s, good(hello)); rec.Code != http.StatusServiceUnavailable || len(s.ingresses()) != 0 {
		t.Fatalf("answer %d", rec.Code)
	}
}
