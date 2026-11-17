package imessage

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// blooioWebhookSecret is a fixture value, not a credential.
const blooioWebhookSecret = "whsec_test0123456789abcdef0123456789abcdef" // #nosec G101

// standInBlooio is a stand-in for the Blooio v4 API, modelled on
// https://docs.blooio.com/llms.txt (POST /messages, operationId
// sendMessage: from, to, text, Idempotency-Key header) and
// https://docs.blooio.com/guides/message-fields-v4 (the send answers the
// message object at the top level, id prefixed msg_). It answers what the
// test scripts; by default 202 with a message object naming the request's
// sender and recipient.
type standInBlooio struct {
	srv *httptest.Server

	mu     sync.Mutex
	reqs   []seenRequest
	status int
	body   string
	header http.Header
}

type seenRequest struct {
	Method, Path string
	Header       http.Header
	Body         map[string]any
}

func newStandInBlooio(t *testing.T) *standInBlooio {
	t.Helper()
	b := &standInBlooio{status: http.StatusAccepted}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		b.mu.Lock()
		b.reqs = append(b.reqs, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Clone(), m})
		status, body, header := b.status, b.body, b.header
		b.mu.Unlock()
		for k, v := range header {
			w.Header()[k] = v
		}
		if body == "" {
			echo, _ := json.Marshal(map[string]any{
				"id": "msg_0001", "chat_id": "chat_0001", "channel_type": "blooio", "direction": "outbound",
				"sender": m["from"], "recipient": m["to"], "text": m["text"], "status": "queued", "protocol": "pending",
			})
			body = string(echo)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *standInBlooio) backend(t *testing.T, now func() time.Time) *Blooio {
	t.Helper()
	be, err := NewBlooio(BlooioOptions{
		BaseURL: b.srv.URL + "/v4", APIKey: "bl_test_key", From: "+15550100002",
		WebhookSecret: []byte(blooioWebhookSecret), Client: b.srv.Client(), Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return be
}

func TestBlooioSendRequest(t *testing.T) {
	sb := newStandInBlooio(t)
	res := sb.backend(t, nil).Send(t.Context(), Outbound{Key: "op-1", To: "alice@example.com", Text: "hi"})
	if res.Outcome != Sent || res.MessageID != "msg_0001" {
		t.Fatalf("result %+v, want sent msg_0001", res)
	}
	req := sb.reqs[0]
	if req.Method != http.MethodPost || req.Path != "/v4/messages" {
		t.Errorf("request %s %s", req.Method, req.Path)
	}
	if req.Header.Get("Authorization") != "Bearer bl_test_key" || req.Header.Get("Idempotency-Key") != "op-1" {
		t.Errorf("headers %v", req.Header)
	}
	if req.Body["from"] != "+15550100002" || req.Body["to"] != "alice@example.com" || req.Body["text"] != "hi" || len(req.Body) != 3 {
		t.Errorf("body %v", req.Body)
	}
}

func TestBlooioSendAnswers(t *testing.T) {
	tests := []struct {
		status  int
		body    string
		outcome Outcome
		id      string
	}{
		{200, `{"id":"msg_0002"}`, Sent, "msg_0002"},
		{201, `{"id":"msg_0003"}`, Sent, "msg_0003"},
		{202, `{"message_id":"Qm8FhbH3P7R","status":"queued"}`, Sent, "Qm8FhbH3P7R"},
		{202, `not json`, Sent, ""},
		{207, `{}`, Unknown, ""},
		{400, `{"error":"invalid_request"}`, Rejected, ""},
		{403, `{}`, Rejected, ""},
		{404, `{}`, Rejected, ""},
		{422, `{}`, Rejected, ""},
		{401, `{}`, NotSent, ""},
		{503, `{}`, NotSent, ""},
		{408, `{}`, Unknown, ""},
		{409, `{}`, Unknown, ""},
		{500, `{}`, Unknown, ""},
		{502, `{}`, Unknown, ""},
	}
	for _, tt := range tests {
		t.Run(strconv.Itoa(tt.status), func(t *testing.T) {
			sb := newStandInBlooio(t)
			sb.status, sb.body = tt.status, tt.body
			res := sb.backend(t, nil).Send(t.Context(), Outbound{Key: "op-1", To: "+15550100001", Text: "hi"})
			if res.Outcome != tt.outcome || res.MessageID != tt.id {
				t.Fatalf("result %+v, want %v %q", res, tt.outcome, tt.id)
			}
		})
	}
}

func TestBlooioRateLimit(t *testing.T) {
	sb := newStandInBlooio(t)
	sb.status, sb.header = http.StatusTooManyRequests, http.Header{"Retry-After": {"12"}}
	res := sb.backend(t, nil).Send(t.Context(), Outbound{Key: "op-1", To: "+15550100001", Text: "hi"})
	if res.Outcome != NotSent || !res.RateLimited || res.RetryAfter != 12*time.Second {
		t.Fatalf("result %+v, want not sent, rate limited, 12s", res)
	}
}

func TestBlooioUnreachableIsNotSent(t *testing.T) {
	sb := newStandInBlooio(t)
	be := sb.backend(t, nil)
	sb.srv.Close()
	if res := be.Send(t.Context(), Outbound{Key: "op-1", To: "+15550100001", Text: "hi"}); res.Outcome != NotSent {
		t.Fatalf("result %+v, want not sent", res)
	}
}

// signBlooio signs body the way Blooio's guide does: t=<unix>,v1=<hex
// HMAC-SHA-256 of "<t>.<body>">. The algorithm is Sign's.
func signBlooio(secret string, at time.Time, body []byte) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	return "t=" + ts + ",v1=" + Sign([]byte(secret), ts, body)
}

func TestBlooioWebhookSignature(t *testing.T) {
	now := time.Unix(1_760_000_000, 0)
	be := newStandInBlooio(t).backend(t, func() time.Time { return now })
	body := []byte(`{"type":"message.received"}`)
	tests := []struct {
		name   string
		header []string
		ok     bool
	}{
		{"valid", []string{signBlooio(blooioWebhookSecret, now, body)}, true},
		{"within the window", []string{signBlooio(blooioWebhookSecret, now.Add(-4*time.Minute), body)}, true},
		{"second v1 matches", []string{"t=" + strconv.FormatInt(now.Unix(), 10) + ",v1=00ff,v1=" + Sign([]byte(blooioWebhookSecret), strconv.FormatInt(now.Unix(), 10), body)}, true},
		{"stale", []string{signBlooio(blooioWebhookSecret, now.Add(-6*time.Minute), body)}, false},
		{"future", []string{signBlooio(blooioWebhookSecret, now.Add(6*time.Minute), body)}, false},
		{"other secret", []string{signBlooio("whsec_other", now, body)}, false},
		{"no header", nil, false},
		{"two headers", []string{signBlooio(blooioWebhookSecret, now, body), signBlooio(blooioWebhookSecret, now, body)}, false},
		{"no timestamp", []string{"v1=" + Sign([]byte(blooioWebhookSecret), "", body)}, false},
		{"no signature", []string{"t=" + strconv.FormatInt(now.Unix(), 10)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for _, v := range tt.header {
				h.Add(BlooioSignatureHeader, v)
			}
			if err := be.VerifyWebhook(h, body); (err == nil) != tt.ok {
				t.Fatalf("VerifyWebhook = %v, want ok=%v", err, tt.ok)
			}
		})
	}
	// The signature covers the body.
	h := http.Header{BlooioSignatureHeader: {signBlooio(blooioWebhookSecret, now, body)}}
	if be.VerifyWebhook(h, []byte(`{"type":"message.sent"}`)) == nil {
		t.Fatal("a changed body verified")
	}
}

// blooioReceived is a message.received event as Blooio's v4 webhooks carry
// it (https://docs.blooio.com/guides/twilio-integration, "Receiving
// messages"; payload version 2026-10-01). Stand-in content.
const blooioReceived = `{"id":"evt_0001","type":"message.received","api_version":"2026-10-01","created_at":1760000000000,` +
	`"organization_id":"org_test","data":{"message_id":"msg_in_0001","chat_id":"chat_0001","channel_id":"ch_0001",` +
	`"channel_type":"blooio","channel_address":"+15550100002","kind":"received","direction":"inbound","status":"received",` +
	`"protocol":"imessage","message_type":"text","text":"recorded test reply","sender":"alice@example.com",` +
	`"recipient":"+15550100002","contact":{"contact_id":"ct_0001","identifier":"alice@example.com","name":null},` +
	`"group":null,"attachments":[{"url":"https://cdn.example.com/a.jpg","media_type":"image/jpeg","size":10}]}}`

func TestBlooioMessages(t *testing.T) {
	be := newStandInBlooio(t).backend(t, nil)
	got, err := be.Messages([]byte(blooioReceived))
	if err != nil || len(got) != 1 {
		t.Fatalf("Messages = %v, %v", got, err)
	}
	want := Inbound{From: "alice@example.com", To: "+15550100002", Body: "recorded test reply", MessageID: "msg_in_0001",
		Media: []Media{{URL: "https://cdn.example.com/a.jpg", ContentType: "image/jpeg"}}}
	if g := got[0]; g.From != want.From || g.To != want.To || g.Body != want.Body || g.MessageID != want.MessageID ||
		len(g.Media) != 1 || g.Media[0] != want.Media[0] {
		t.Fatalf("message %+v, want %+v", g, want)
	}
	drop := map[string]func(map[string]any){
		"sent event":       func(e map[string]any) { e["type"] = "message.sent" },
		"delivered event":  func(e map[string]any) { e["type"] = "message.delivered" },
		"own echo":         func(e map[string]any) { e["data"].(map[string]any)["direction"] = "outbound" },
		"group message":    func(e map[string]any) { e["data"].(map[string]any)["group"] = map[string]any{"group_id": "grp_1"} },
		"no sender":        func(e map[string]any) { delete(e["data"].(map[string]any), "sender") },
		"no message id":    func(e map[string]any) { delete(e["data"].(map[string]any), "message_id") },
		"nothing to carry": func(e map[string]any) { d := e["data"].(map[string]any); d["text"] = " "; d["attachments"] = []any{} },
	}
	for name, mutate := range drop {
		t.Run(name, func(t *testing.T) {
			var e map[string]any
			must(t, json.Unmarshal([]byte(blooioReceived), &e))
			mutate(e)
			body, _ := json.Marshal(e)
			if got, err := be.Messages(body); err != nil || len(got) != 0 {
				t.Fatalf("Messages = %v, %v; want nothing", got, err)
			}
		})
	}
	if _, err := be.Messages([]byte("not json")); err == nil {
		t.Error("a body that is no JSON parsed")
	}
}
