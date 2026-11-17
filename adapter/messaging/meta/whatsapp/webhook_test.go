package whatsapp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/messaging/meta"
	"hop.top/uma/adapter/messaging/meta/whatsapp"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

var appSecret = []byte("app-secret")

// Documented webhook payloads (WhatsApp Cloud API messages webhook).

func notificationOf(pnid string, value string) []byte {
	return fmt.Appendf(nil, `{"object":"whatsapp_business_account","entry":[{"id":"102290129340398","changes":[{"value":{"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550783881","phone_number_id":%q},%s},"field":"messages"}]}]}`, pnid, value)
}

func textMessage(id, from, body string, ts int64) string {
	return fmt.Sprintf(`"contacts":[{"profile":{"name":"Sheena Nelson"},"wa_id":%q}],"messages":[{"from":%q,"id":%q,"timestamp":"%d","type":"text","text":{"body":%q}}]`, from, from, id, ts, body)
}

func statusValue(statuses ...string) string {
	return `"statuses":[` + strings.Join(statuses, ",") + `]`
}

const (
	sentStatus      = `{"id":"wamid.HBgLMTY1MDM4Nzk0MzkVAgASGBQzQUFERjg0NDEzNDdFODU3MUMxMAA=","status":"sent","timestamp":"1750030073","recipient_id":"16505551234","biz_opaque_callback_data":"uma1:00000000-0000-7000-8000-0000000000e1:00000000-0000-7000-8000-0000000000a1","conversation":{"id":"72b14d6bd5407799e66f64d1b338e567","expiration_timestamp":"1750116480","origin":{"type":"marketing"}},"pricing":{"billable":true,"pricing_model":"PMP","type":"regular","category":"marketing"}}`
	deliveredStatus = `{"id":"wamid.HBgLMTY1MDM4Nzk0MzkVAgASGBQzQUFERjg0NDEzNDdFODU3MUMxMAA=","status":"delivered","timestamp":"1750030080","recipient_id":"16505551234"}`
	readStatus      = `{"id":"wamid.HBgLMTY1MDM4Nzk0MzkVAgASGBQzQUFERjg0NDEzNDdFODU3MUMxMAA=","status":"read","timestamp":"1750030090","recipient_id":"16505551234"}`
	failedStatus    = `{"id":"wamid.HBgLMTY1MDM4Nzk0MzkVAgARGBI0QUQ2MjA4NEYyRkExNjMyREUA","status":"failed","timestamp":"1751142888","recipient_id":"16505551234","errors":[{"code":131049,"title":"This message was not delivered to maintain healthy ecosystem engagement.","message":"This message was not delivered to maintain healthy ecosystem engagement.","error_data":{"details":"In order to maintain a healthy ecosystem engagement, the message failed to be delivered."},"href":"/documentation/business-messaging/whatsapp/support/error-codes"}]}`
)

// sink records every ingress the handler accepts.
type sink struct {
	mu  sync.Mutex
	got []model.Ingress
	err error
}

func (s *sink) Accept(_ context.Context, in model.Ingress) (model.IngressReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return model.IngressReceipt{}, s.err
	}
	s.got = append(s.got, in)
	return model.IngressReceipt{ID: "r", Committed: true}, nil
}

func (s *sink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, model.NewError(model.Unsupported, "no interactions")
}

func (s *sink) ingress() []model.Ingress {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]model.Ingress(nil), s.got...)
}

func post(h http.Handler, body []byte, signature string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
	if signature != "" {
		req.Header.Set(meta.SignatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func get(h http.Handler, q url.Values) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/webhooks/whatsapp?"+q.Encode(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func handler(t *testing.T, deps adapter.Dependencies) (http.Handler, *sink) {
	t.Helper()
	if deps.Credentials == nil {
		deps.Credentials = newCreds()
	}
	s := &sink{}
	return openInstance(t, whatsapp.NewFactory(whatsapp.Options{}), deps).Handler(s), s
}

func TestWebhookVerificationChallenge(t *testing.T) {
	h, _ := handler(t, adapter.Dependencies{})
	q := url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {"verify-token"}, "hub.challenge": {"1158201444"}}
	if code, body := get(h, q); code != http.StatusOK || body != "1158201444" {
		t.Fatalf("GET = %d %q, want 200 echoing the challenge", code, body)
	}
	q.Set("hub.verify_token", "guess")
	if code, body := get(h, q); code != http.StatusForbidden || strings.Contains(body, "1158201444") {
		t.Fatalf("wrong token GET = %d %q, want 403 without the challenge", code, body)
	}
	cr := newCreds()
	delete(cr.values, whatsapp.PurposeVerifyToken)
	h, _ = handler(t, adapter.Dependencies{Credentials: cr})
	q.Set("hub.verify_token", "verify-token")
	if code, _ := get(h, q); code != http.StatusServiceUnavailable {
		t.Fatalf("unresolvable token GET = %d, want 503", code)
	}
}

// The signature is checked over the raw body before anything is parsed
// or written: no sink call and no window state for an unverified request.
func TestWebhookUnverifiedWritesNothing(t *testing.T) {
	p := newProtocol()
	h, s := handler(t, adapter.Dependencies{Protocol: p})
	body := notificationOf(testPNID, textMessage("wamid.in-1", testUser, "hi", testNow.Unix()))
	for name, sig := range map[string]string{
		"missing":       "",
		"other secret":  meta.Sign([]byte("other"), body),
		"other body":    meta.Sign(appSecret, notificationOf(testPNID, textMessage("wamid.in-2", testUser, "hi", testNow.Unix()))),
		"no prefix":     strings.TrimPrefix(meta.Sign(appSecret, body), "sha256="),
		"sha1":          "sha1=" + strings.Repeat("0", 40),
		"empty mac":     "sha256=",
		"garbage value": "sha256=not-hex",
	} {
		if code := post(h, body, sig); code != http.StatusUnauthorized {
			t.Errorf("%s: POST = %d, want 401", name, code)
		}
	}
	if code := post(h, []byte("{not json"), meta.Sign([]byte("x"), []byte("{not json"))); code != http.StatusUnauthorized {
		t.Errorf("malformed unsigned body = %d, want 401 before parsing", code)
	}
	if got := s.ingress(); len(got) != 0 {
		t.Fatalf("sink got %d ingress from unverified requests", len(got))
	}
	if p.lastInbound(testUser) != 0 {
		t.Fatal("window recorded from an unverified request")
	}
	if code := post(h, []byte("{not json"), meta.Sign(appSecret, []byte("{not json"))); code != http.StatusBadRequest {
		t.Errorf("malformed signed body = %d, want 400", code)
	}
	cr := newCreds()
	delete(cr.values, whatsapp.PurposeAppSecret)
	h, s = handler(t, adapter.Dependencies{Credentials: cr})
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusServiceUnavailable || len(s.ingress()) != 0 {
		t.Fatalf("unresolvable secret = %d, want 503 and nothing written", code)
	}
}

func TestWebhookMapsInboundText(t *testing.T) {
	p := newProtocol()
	h, s := handler(t, adapter.Dependencies{Protocol: p})
	ts := testNow.Add(-time.Hour).Unix()
	body := notificationOf(testPNID, textMessage("wamid.in-1", testUser, "Does it come in another color?", ts))
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	got := s.ingress()
	if len(got) != 1 {
		t.Fatalf("ingress = %d", len(got))
	}
	env := got[0].Envelope
	if env.Kind != model.EnvelopeMessage || env.EventID != "wamid.in-1" || env.MessageID != "wamid.in-1" || env.Destination != testPNID {
		t.Errorf("envelope %+v", env)
	}
	if env.Sender != userRoute(testUser) || env.Audience.Visibility != model.Public || !env.OccurredAt.Equal(time.Unix(ts, 0)) {
		t.Errorf("sender %+v audience %+v at %v", env.Sender, env.Audience, env.OccurredAt)
	}
	if len(env.Content.Parts) != 1 || env.Content.Parts[0].Text != "Does it come in another color?" {
		t.Errorf("content %+v", env.Content)
	}
	if env.Payload.Name != whatsapp.InboundSchema || !strings.Contains(string(env.Payload.Data), "Sheena Nelson") {
		t.Errorf("payload %s %s", env.Payload.Name, env.Payload.Data)
	}
	if p.lastInbound(testUser) != ts {
		t.Fatalf("window last inbound = %d, want %d", p.lastInbound(testUser), ts)
	}
	older := notificationOf(testPNID, textMessage("wamid.in-0", testUser, "earlier", ts-600))
	post(h, older, meta.Sign(appSecret, older))
	if p.lastInbound(testUser) != ts {
		t.Fatal("an older message moved the window back")
	}
}

// Only changes for the channel's own verified phone number reach the sink.
func TestWebhookIgnoresOtherPhoneNumbers(t *testing.T) {
	h, s := handler(t, adapter.Dependencies{})
	body := notificationOf("999999999999999", textMessage("wamid.x", testUser, "hi", testNow.Unix()))
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK || len(s.ingress()) != 0 {
		t.Fatalf("POST = %d, ingress %d; want 200 and nothing", code, len(s.ingress()))
	}
	other := []byte(`{"object":"page","entry":[]}`)
	if code := post(h, other, meta.Sign(appSecret, other)); code != http.StatusOK || len(s.ingress()) != 0 {
		t.Fatalf("other object = %d", code)
	}
}

func TestWebhookMapsStatusesAsDeliveryEvidence(t *testing.T) {
	h, s := handler(t, adapter.Dependencies{})
	body := notificationOf(testPNID, statusValue(sentStatus, deliveredStatus, readStatus, failedStatus))
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	got := s.ingress()
	if len(got) != 4 {
		t.Fatalf("ingress = %d, want 4", len(got))
	}
	want := []struct {
		status string
		state  model.DeliveryState
		read   bool
	}{{"sent", model.ProviderAccepted, false}, {"delivered", model.Delivered, false}, {"read", model.Delivered, true}, {"failed", model.Failed, false}}
	ids := map[string]bool{}
	for i, in := range got {
		env := in.Envelope
		if env.Kind != model.EnvelopeStatus || !env.Sender.IsZero() || env.Destination != testPNID {
			t.Errorf("%d: envelope %+v", i, env)
		}
		if ids[env.EventID] {
			t.Errorf("%d: event ID %q repeats: statuses of one message must not deduplicate each other", i, env.EventID)
		}
		ids[env.EventID] = true
		if len(env.References) == 0 || env.References[0] != model.ProviderMessageRef(testChannel, env.MessageID) {
			t.Errorf("%d: references %+v", i, env.References)
		}
		var st whatsapp.Status
		if env.Payload.Name != whatsapp.StatusSchema || json.Unmarshal(env.Payload.Data, &st) != nil {
			t.Fatalf("%d: payload %s", i, env.Payload.Data)
		}
		if st.Status != want[i].status || st.State != want[i].state || st.Read != want[i].read || st.Recipient != testUser {
			t.Errorf("%d: status %+v, want %+v", i, st, want[i])
		}
	}
	var sent, failed whatsapp.Status
	_ = json.Unmarshal(got[0].Envelope.Payload.Data, &sent)
	_ = json.Unmarshal(got[3].Envelope.Payload.Data, &failed)
	if sent.Operation != "00000000-0000-7000-8000-0000000000e1" || sent.Attempt != "00000000-0000-7000-8000-0000000000a1" || sent.Category != "marketing" || !sent.Billable || sent.ConversationExpiresAt.Unix() != 1750116480 {
		t.Errorf("sent status %+v", sent)
	}
	if failed.Failure == nil || failed.Failure.Code != model.Permanent || failed.Failure.Retry != model.NoRetry || len(failed.ErrorCodes) != 1 || failed.ErrorCodes[0] != 131049 {
		t.Errorf("failed status %+v", failed)
	}
}

func TestWebhookMapsOtherMessageTypes(t *testing.T) {
	h, s := handler(t, adapter.Dependencies{})
	msgs := `"contacts":[{"profile":{"name":"Sheena Nelson"},"wa_id":"16505551234"}],"messages":[` +
		`{"from":"16505551234","id":"wamid.img","timestamp":"1749416383","type":"image","image":{"caption":"this one","mime_type":"image/jpeg","sha256":"abc=","id":"1003383421387256"}},` +
		`{"from":"16505551234","id":"wamid.react","timestamp":"1749416384","type":"reaction","reaction":{"message_id":"wamid.out-1","emoji":"\ud83d\udc4d"}},` +
		`{"from":"16505551234","id":"wamid.unreact","timestamp":"1749416385","type":"reaction","reaction":{"message_id":"wamid.out-1"}},` +
		`{"from":"16505551234","id":"wamid.untargeted","timestamp":"1749416385","type":"reaction","reaction":{"emoji":"\ud83d\udc4d"}},` +
		`{"from":"16505551234","id":"wamid.btn","timestamp":"1749416386","type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"yes-1","title":"Yes"}}},` +
		`{"from":"16505551234","id":"wamid.loc","timestamp":"1749416387","type":"location","location":{"latitude":1,"longitude":2}}]`
	body := notificationOf(testPNID, msgs)
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	got := s.ingress()
	if len(got) != 5 {
		t.Fatalf("ingress = %d, want 5 (a reaction without a target is dropped)", len(got))
	}
	img := got[0].Envelope.Content
	if len(img.Parts) != 2 || img.Parts[0].Text != "this one" || img.Parts[1].Structured == nil || img.Parts[1].Structured.Name != whatsapp.MediaSchema || !strings.Contains(string(img.Parts[1].Structured.Data), "1003383421387256") {
		t.Errorf("image content %+v", img)
	}
	react := got[1].Envelope
	if react.Kind != model.EnvelopeReactionAdded || react.MessageID != "wamid.out-1" || react.EventID != "wamid.react" || react.Content.Parts[0].Text != "👍" {
		t.Errorf("reaction %+v", react)
	}
	btn := got[3].Envelope.Content
	if btn.Parts[0].Text != "Yes" || !strings.Contains(string(btn.Parts[1].Structured.Data), "yes-1") {
		t.Errorf("button content %+v", btn)
	}
	loc := got[4].Envelope.Content
	if len(loc.Parts) != 1 || loc.Parts[0].Structured.Name != whatsapp.MessageSchema {
		t.Errorf("location content %+v", loc)
	}
	for _, in := range got {
		if in.Envelope.Kind == model.EnvelopeReactionRemoved {
			continue
		}
		if err := in.Envelope.Content.Validate(); err != nil {
			t.Errorf("%s: content invalid: %v", in.Envelope.EventID, err)
		}
	}
}

// WhatsApp does not say which emoji a removal removed: the reaction
// message has no emoji. It is emitted as a removal without content, for
// the engine to resolve against the sender's one reaction on the message.
func TestInboundReactionRemovalEmitted(t *testing.T) {
	h, s := handler(t, adapter.Dependencies{})
	msgs := `"contacts":[{"profile":{"name":"Sheena Nelson"},"wa_id":"16505551234"}],"messages":[` +
		`{"from":"16505551234","id":"wamid.unreact","timestamp":"1749416385","type":"reaction","reaction":{"message_id":"wamid.out-1"}},` +
		`{"from":"16505551234","id":"wamid.unreact-2","timestamp":"1749416386","type":"reaction","reaction":{"message_id":"wamid.out-1","emoji":""}}]`
	body := notificationOf(testPNID, msgs)
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	got := s.ingress()
	if len(got) != 2 {
		t.Fatalf("ingress = %d, want both removals", len(got))
	}
	for i, in := range got {
		e := in.Envelope
		if e.Kind != model.EnvelopeReactionRemoved || e.MessageID != "wamid.out-1" || e.EventID != []string{"wamid.unreact", "wamid.unreact-2"}[i] ||
			len(e.Content.Parts) != 0 || e.Sender != userRoute(testUser) || e.SystemActor || e.Audience.Visibility != model.Public || e.Assurance != "verified" {
			t.Errorf("removal %d = %+v", i, e)
		}
	}
}

func TestWebhookAnswersSinkFailures(t *testing.T) {
	body := notificationOf(testPNID, textMessage("wamid.in-1", testUser, "hi", testNow.Unix()))
	for code, want := range map[model.ErrorCode]int{
		model.Unavailable: http.StatusServiceUnavailable,
		model.Transient:   http.StatusServiceUnavailable,
		model.NotFound:    http.StatusUnprocessableEntity,
		model.Validation:  http.StatusUnprocessableEntity,
	} {
		h, s := handler(t, adapter.Dependencies{})
		s.err = model.NewError(code, "x")
		if got := post(h, body, meta.Sign(appSecret, body)); got != want {
			t.Errorf("sink %s: POST = %d, want %d", code, got, want)
		}
	}
	p := newProtocol()
	p.fail = model.NewError(model.Unavailable, "down")
	h, s := handler(t, adapter.Dependencies{Protocol: p})
	if got := post(h, body, meta.Sign(appSecret, body)); got != http.StatusServiceUnavailable || len(s.ingress()) != 0 {
		t.Errorf("window state down: POST = %d, ingress %d; want 503 before the sink", got, len(s.ingress()))
	}
}

func TestWebhookMethodsAndClose(t *testing.T) {
	inst := openInstance(t, whatsapp.NewFactory(whatsapp.Options{}), adapter.Dependencies{Credentials: newCreds()})
	h := inst.Handler(&sink{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT = %d", rec.Code)
	}
	_ = inst.Close(t.Context())
	body := notificationOf(testPNID, textMessage("wamid.in-1", testUser, "hi", testNow.Unix()))
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusServiceUnavailable {
		t.Fatalf("closed POST = %d", code)
	}
}

// End to end through Runtime.Webhook: a verified notification commits one
// message from the WhatsApp user, once across redelivery; statuses create
// no message.
func TestWebhookThroughEngine(t *testing.T) {
	clock := portstest.NewFixedClock(testNow)
	st := memory.New(memory.Options{Clock: clock})
	engine, err := uma.New(uma.Config{
		Store: st,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Adapters:    []adapter.Factory{whatsapp.NewFactory(whatsapp.Options{})},
		Credentials: newCreds(),
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(0x3a),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	inbox := model.InboxRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-00000000b0a1"}
	lock := true
	sel := model.Selection{Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1}
	if err := st.Transact(t.Context(), testTenant, func(tx store.Tx) error {
		if err := tx.PutChannel(t.Context(), channel(testPNID), ""); err != nil {
			return err
		}
		return tx.PutInbox(t.Context(), model.Inbox{Ref: inbox, Channel: testChannel, Name: "support", Destinations: []string{testPNID}, Selection: sel, Revision: "in1"}, "")
	}); err != nil {
		t.Fatal(err)
	}
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: testTenant, Actor: "webhook-service"}, Call: model.Call{Correlation: "wa"}}
	h, err := engine.Runtime().Webhook(t.Context(), call, testChannel)
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"hub.mode": {"subscribe"}, "hub.verify_token": {"verify-token"}, "hub.challenge": {"42"}}
	if code, b := get(h, q); code != http.StatusOK || b != "42" {
		t.Fatalf("verification through the engine = %d %q", code, b)
	}
	body := notificationOf(testPNID, textMessage("wamid.in-1", testUser, "hello", testNow.Unix()))
	for i := range 2 {
		if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
			t.Fatalf("delivery %d = %d", i+1, code)
		}
	}
	statuses := notificationOf(testPNID, statusValue(sentStatus, deliveredStatus))
	if code := post(h, statuses, meta.Sign(appSecret, statuses)); code != http.StatusOK {
		t.Fatalf("statuses = %d", code)
	}
	unsigned := notificationOf(testPNID, textMessage("wamid.in-2", testUser, "forged", testNow.Unix()))
	if code := post(h, unsigned, meta.Sign([]byte("forger"), unsigned)); code != http.StatusUnauthorized {
		t.Fatalf("forged = %d", code)
	}
	var msgs []model.Message
	if err := st.View(t.Context(), testTenant, func(r store.Reader) error {
		p, err := r.Messages(t.Context(), model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{inbox}}, Page: model.PageRequest{Limit: 50}})
		msgs = p.Items
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Author.External != userRoute(testUser) || m.Content.Parts[0].Text != "hello" || m.Source != model.TypeMetaWhatsapp {
		t.Fatalf("message %+v", m)
	}
}

var (
	cbOperation = model.OperationRef{Tenant: testTenant, ID: "00000000-0000-7000-8000-0000000000e1"}
	cbAttempt   = model.ID("00000000-0000-7000-8000-0000000000a1")
)

func withCallback(status, data string) string {
	if data == "" {
		return status
	}
	return strings.Replace(status, `"status":`, `"biz_opaque_callback_data":`+strconvQuote(data)+`,"status":`, 1)
}

func strconvQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// A status echoing the callback data a send set is evidence for that
// attempt: the envelope names the attempt (EchoRef) and the wamid.
func TestStatusEvidenceNamesTheAttempt(t *testing.T) {
	data := whatsapp.CallbackData(adapter.Effect{Operation: cbOperation, Attempt: cbAttempt})
	for _, st := range []string{deliveredStatus, readStatus, failedStatus} {
		h, s := handler(t, adapter.Dependencies{})
		body := notificationOf(testPNID, statusValue(withCallback(st, data)))
		if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
			t.Fatalf("POST = %d", code)
		}
		env := s.ingress()[0].Envelope
		want := []model.ExternalRef{model.ProviderMessageRef(testChannel, env.MessageID), model.EchoRef(testChannel, cbOperation, cbAttempt)}
		if len(env.References) != 2 || env.References[0] != want[0] || env.References[1] != want[1] {
			t.Fatalf("references %+v, want %+v", env.References, want)
		}
		var p whatsapp.Status
		_ = json.Unmarshal(env.Payload.Data, &p)
		if p.Operation != cbOperation.ID || p.Attempt != cbAttempt {
			t.Fatalf("payload %+v, want the attempt", p)
		}
	}
}

// A missing or foreign callback value never names an attempt.
func TestStatusEvidenceNeverGuessesAnAttempt(t *testing.T) {
	for name, data := range map[string]string{
		"missing":       "",
		"campaign tag":  "campaign-2026-q4",
		"bare uuid":     string(cbAttempt),
		"prefix only":   "uma1:",
		"no attempt":    "uma1:" + string(cbOperation.ID) + ":",
		"not ids":       "uma1:op-1:attempt-1",
		"extra field":   "uma1:" + string(cbOperation.ID) + ":" + string(cbAttempt) + ":x",
		"other version": "uma2:" + string(cbOperation.ID) + ":" + string(cbAttempt),
	} {
		t.Run(name, func(t *testing.T) {
			h, s := handler(t, adapter.Dependencies{})
			body := notificationOf(testPNID, statusValue(withCallback(deliveredStatus, data)))
			if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
				t.Fatalf("POST = %d", code)
			}
			env := s.ingress()[0].Envelope
			if len(env.References) != 1 || env.References[0] != model.ProviderMessageRef(testChannel, env.MessageID) {
				t.Fatalf("references %+v, want the wamid only", env.References)
			}
			var p whatsapp.Status
			_ = json.Unmarshal(env.Payload.Data, &p)
			if p.Operation != "" || p.Attempt != "" {
				t.Fatalf("payload names %q/%q from a foreign value", p.Operation, p.Attempt)
			}
		})
	}
}

// A message carrying a context ID replies to that message; one without, or
// a reaction naming its target, reports no reply parent.
func TestWebhookReportsReplyParent(t *testing.T) {
	h, s := handler(t, adapter.Dependencies{})
	msgs := `"contacts":[{"profile":{"name":"Sheena Nelson"},"wa_id":"16505551234"}],"messages":[` +
		`{"from":"16505551234","id":"wamid.reply","timestamp":"1749416385","type":"text","text":{"body":"approve"},"context":{"from":"15550001111","id":"wamid.out-1"}},` +
		`{"from":"16505551234","id":"wamid.fwd","timestamp":"1749416386","type":"text","text":{"body":"fwd"},"context":{"forwarded":true}},` +
		`{"from":"16505551234","id":"wamid.plain","timestamp":"1749416387","type":"text","text":{"body":"hi"}}]`
	body := notificationOf(testPNID, msgs)
	if code := post(h, body, meta.Sign(appSecret, body)); code != http.StatusOK {
		t.Fatalf("POST = %d", code)
	}
	got := s.ingress()
	if len(got) != 3 {
		t.Fatalf("ingress = %d, want 3", len(got))
	}
	for i, want := range []string{"wamid.out-1", "", ""} {
		if got[i].Envelope.InReplyTo != want {
			t.Errorf("message %d InReplyTo = %q, want %q", i, got[i].Envelope.InReplyTo, want)
		}
	}
}
