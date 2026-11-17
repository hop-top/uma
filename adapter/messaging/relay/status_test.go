package relay_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"hop.top/uma/adapter/messaging/relay"
	"hop.top/uma/model"
)

func statusConfig() relay.Config {
	cfg := baseConfig()
	cfg.Inbound.Status.Enabled = true
	return cfg
}

// tagged is a status update naming the reply by operation and attempt.
func tagged(status string) string {
	return `{"status":"` + status + `","from":"+15550100002","operation_id":"` + string(testOp) +
		`","attempt_id":"` + string(testAttempt) + `","message_id":"gw-7"}`
}

// accept posts body to a status-enabled channel and returns the one
// envelope the sink saw.
func accept(t *testing.T, cfg relay.Config, body string) model.Envelope {
	t.Helper()
	s := &sink{}
	rec := serve(t, newInstance(t, cfg, nil), s, good(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("answer %d for %s", rec.Code, body)
	}
	got := s.ingresses()
	if len(got) != 1 {
		t.Fatalf("%d ingresses for %s", len(got), body)
	}
	return got[0].Envelope
}

func payloadOf(t *testing.T, env model.Envelope) relay.StatusPayload {
	t.Helper()
	if env.Payload.Name != relay.PayloadStatus || env.Payload.Version != 1 {
		t.Fatalf("payload %q v%d", env.Payload.Name, env.Payload.Version)
	}
	var p relay.StatusPayload
	if err := json.Unmarshal(env.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStatusUpdateBecomesAStatusEnvelope(t *testing.T) {
	env := accept(t, statusConfig(), tagged("delivered"))
	if env.Kind != model.EnvelopeStatus || env.Destination != "+15550100002" || env.MessageID != "gw-7" {
		t.Errorf("envelope %+v", env)
	}
	if env.Assurance != "verified" || env.Audience.Visibility != model.Public || !env.OccurredAt.Equal(testNow) {
		t.Errorf("envelope %+v", env)
	}
	if !env.Sender.IsZero() || len(env.Content.Parts) != 0 || env.EventID == "" {
		t.Errorf("a status carries no sender and no content: %+v", env)
	}
	// The envelope names the attempt the callback carried, so the engine
	// applies the report to it, and the relay's own id for the message.
	op := model.OperationRef{Tenant: testTenant, ID: testOp}
	want := []model.ExternalRef{model.ProviderMessageRef(testChannel, "gw-7"), model.EchoRef(testChannel, op, testAttempt)}
	if len(env.References) != 2 || env.References[0] != want[0] || env.References[1] != want[1] {
		t.Errorf("references %+v, want %+v", env.References, want)
	}
	if env.Delivery == nil || env.Delivery.State != model.Delivered || env.Delivery.Failure != nil {
		t.Errorf("delivery %+v", env.Delivery)
	}
	p := payloadOf(t, env)
	if p.Status != "delivered" || p.State != model.Delivered || p.Read || p.Operation != testOp || p.Attempt != testAttempt || p.MessageID != "gw-7" {
		t.Errorf("payload %+v", p)
	}
}

// The status values map to the engine's delivery states: it has no state
// for accepted apart from sent, nor for read apart from delivered, so the
// payload keeps the relay's own word. Matching ignores case.
func TestStatusValuesMapToDeliveryStates(t *testing.T) {
	tests := []struct {
		word  string
		state model.DeliveryState
		read  bool
	}{
		{"accepted", model.ProviderAccepted, false},
		{"sent", model.ProviderAccepted, false},
		{"delivered", model.Delivered, false},
		{"read", model.Delivered, true},
		{"failed", model.Failed, false},
		{"Delivered", model.Delivered, false},
		{"READ", model.Delivered, true},
	}
	for _, tt := range tests {
		t.Run(tt.word, func(t *testing.T) {
			env := accept(t, statusConfig(), tagged(tt.word))
			if env.Delivery.State != tt.state {
				t.Errorf("state %s, want %s", env.Delivery.State, tt.state)
			}
			p := payloadOf(t, env)
			if p.Status != strings.ToLower(tt.word) || p.State != tt.state || p.Read != tt.read {
				t.Errorf("payload %+v", p)
			}
			if (tt.state == model.Failed) != (env.Delivery.Failure != nil) {
				t.Errorf("failure %+v for %s", env.Delivery.Failure, tt.state)
			}
			// Only read carries a read time: the engine records it and keeps
			// the operation delivered.
			if r := env.Delivery.ReadAt; (r != nil) != tt.read || (r != nil && !r.Equal(testNow)) {
				t.Errorf("read time %v for %s, want %v when read is %v", r, tt.word, testNow, tt.read)
			}
		})
	}
}

// The answer says whether the engine applied the update: it is 200 either
// way, so a gateway does not retry what cannot succeed, and an update that
// reached no attempt says so. A message is answered as before.
func TestStatusAnswerSaysWhetherTheUpdateWasApplied(t *testing.T) {
	op := model.OperationRef{Tenant: testTenant, ID: testOp}
	for name, tt := range map[string]struct {
		body    string
		receipt model.IngressReceipt
		want    string
	}{
		"applied":   {tagged("delivered"), model.IngressReceipt{Committed: true, Operation: &op}, `{"status":"accepted"}`},
		"unmatched": {tagged("delivered"), model.IngressReceipt{Committed: true}, `{"status":"accepted","matched":false}`},
		"untagged": {
			`{"status":"delivered","from":"+15550100002","message_id":"gw-7"}`, model.IngressReceipt{Committed: true},
			`{"status":"accepted","matched":false}`,
		},
		"a duplicate keeps the answer": {tagged("delivered"), model.IngressReceipt{Committed: true, Duplicate: true, Operation: &op}, `{"status":"accepted"}`},
		"a message":                    {hello, model.IngressReceipt{Committed: true}, `{"status":"accepted"}`},
	} {
		t.Run(name, func(t *testing.T) {
			s := &sink{receipt: tt.receipt}
			rec := serve(t, newInstance(t, statusConfig(), nil), s, good(tt.body))
			if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != tt.want {
				t.Fatalf("answer %d %q, want 200 %s", rec.Code, rec.Body.String(), tt.want)
			}
		})
	}
}

// A failure is final, as a provider's report after the attempt reached it
// is (model.DeliveryReport): never retried. The relay's code, bounded to
// plain characters, is all of its own words the summary repeats.
func TestStatusFailureIsFinalAndSafe(t *testing.T) {
	failed := func(extra string) model.Envelope {
		return accept(t, statusConfig(), strings.Replace(tagged("failed"), `"message_id"`, extra+`"message_id"`, 1))
	}
	for name, tt := range map[string]struct {
		extra, summary, code string
	}{
		"no code":     {"", "relay reported the delivery failed", ""},
		"string code": {`"code":"30003",`, "relay reported the delivery failed (code 30003)", "30003"},
		"number code": {`"code":30003,`, "relay reported the delivery failed (code 30003)", "30003"},
		"null code":   {`"code":null,`, "relay reported the delivery failed", ""},
	} {
		t.Run(name, func(t *testing.T) {
			env := failed(tt.extra)
			f := env.Delivery.Failure
			if f == nil || f.Code != model.Permanent || f.Retry != model.NoRetry || f.SafeMessage != tt.summary {
				t.Fatalf("failure %+v", f)
			}
			if p := payloadOf(t, env); p.Code != tt.code || p.Failure == nil || *p.Failure != *f {
				t.Errorf("payload %+v", p)
			}
		})
	}
	// A code outside the plain set, or too long, is a malformed update.
	for name, code := range map[string]string{
		"space": `"a b"`, "control": `"a\u0000"`, "quote": `"a\"b"`, "object": `{"a":1}`, "bool": `true`,
		"long": `"` + strings.Repeat("c", 65) + `"`,
	} {
		t.Run(name, func(t *testing.T) {
			s := &sink{}
			body := strings.Replace(tagged("failed"), `"message_id"`, `"code":`+code+`,"message_id"`, 1)
			if rec := serve(t, newInstance(t, statusConfig(), nil), s, good(body)); rec.Code != http.StatusBadRequest || len(s.ingresses()) != 0 {
				t.Fatalf("answer %d, %d ingresses", rec.Code, len(s.ingresses()))
			}
		})
	}
	// The code means something on a failure only: on any other status it is
	// not read, so a relay that always sends one is not refused.
	if env := accept(t, statusConfig(), strings.Replace(tagged("delivered"), `"message_id"`, `"code":"a b","message_id"`, 1)); env.Delivery.Failure != nil {
		t.Errorf("failure on delivered: %+v", env.Delivery.Failure)
	}
}

// An update names a reply by operation and attempt, by the relay's message
// id, or both; the engine settles only what names an attempt.
func TestStatusNamesTheReply(t *testing.T) {
	cfg := statusConfig()
	op := model.OperationRef{Tenant: testTenant, ID: testOp}
	echo := model.EchoRef(testChannel, op, testAttempt)
	refs := func(env model.Envelope) (echoed, message bool) {
		for _, r := range env.References {
			echoed = echoed || r == echo
			message = message || r == model.ProviderMessageRef(testChannel, "gw-7")
		}
		return echoed, message
	}
	byID := accept(t, cfg, `{"status":"delivered","from":"+15550100002","message_id":"gw-7"}`)
	if e, m := refs(byID); e || !m || byID.MessageID != "gw-7" {
		t.Errorf("by message id: %+v", byID)
	}
	byTag := accept(t, cfg, `{"status":"delivered","from":"+15550100002","operation_id":"`+string(testOp)+`","attempt_id":"`+string(testAttempt)+`"}`)
	if e, m := refs(byTag); !e || m || byTag.MessageID != "" || byTag.EventID == "" {
		t.Errorf("by operation: %+v", byTag)
	}
	if e, m := refs(accept(t, cfg, tagged("delivered"))); !e || !m {
		t.Error("an update naming both carries both")
	}
}

func TestStatusEventIDIsBoundToTheUpdate(t *testing.T) {
	cfg := statusConfig()
	base := tagged("delivered")
	id := func(body string) string { return accept(t, cfg, body).EventID }
	want := id(base)
	// The same update, however often the relay sends it and whatever else
	// it says (a timestamp, a recipient), is one event.
	for name, body := range map[string]string{
		"again":       base,
		"extra keys":  strings.Replace(base, `"status"`, `"at":"2026-01-01T00:00:00Z","to":"+15550100001","status"`, 1),
		"key order":   `{"message_id":"gw-7","attempt_id":"` + string(testAttempt) + `","operation_id":"` + string(testOp) + `","from":"+15550100002","status":"delivered"}`,
		"status case": strings.Replace(base, "delivered", "DELIVERED", 1),
	} {
		if got := id(body); got != want {
			t.Errorf("%s: event id differs: a redelivery would not deduplicate", name)
		}
	}
	// Anything of the update's content that differs is another event: a
	// relay that reuses its ids cannot make one update pass for another.
	other := "00000000-0000-7000-8000-0000000000b2"
	for name, body := range map[string]string{
		"status":     strings.Replace(base, "delivered", "read", 1),
		"message id": strings.Replace(base, "gw-7", "gw-8", 1),
		"operation":  strings.Replace(base, string(testOp), other, 1),
		"attempt":    strings.Replace(base, string(testAttempt), other, 1),
		"line":       strings.Replace(base, "+15550100002", "+15550100003", 1),
	} {
		if got := id(body); got == want {
			t.Errorf("%s: shares an event id with another update: one would drop the other", name)
		}
	}
	failed := tagged("failed")
	withCode := func(c string) string {
		return strings.Replace(failed, `"message_id"`, `"code":"`+c+`","message_id"`, 1)
	}
	if id(failed) == id(withCode("30003")) || id(withCode("30003")) == id(withCode("30004")) {
		t.Error("failures with different codes share an event id")
	}
	// A status never collides with a message that happens to share ids.
	if msg := accept(t, cfg, hello).EventID; msg == want {
		t.Error("a message and a status share an event id")
	}
}

func TestStatusRefusesBadUpdates(t *testing.T) {
	op, at := string(testOp), string(testAttempt)
	line := `"from":"+15550100002"`
	tests := []struct{ name, body string }{
		{"not json", `status=delivered`},
		{"array", `[]`},
		{"status not a string", `{"status":1,` + line + `,"message_id":"m"}`},
		{"status null", `{"status":null,` + line + `,"message_id":"m"}`},
		{"unknown status", `{"status":"queued",` + line + `,"message_id":"m"}`},
		{"empty status", `{"status":"",` + line + `,"message_id":"m"}`},
		{"long status", `{"status":"` + strings.Repeat("d", 40) + `",` + line + `,"message_id":"m"}`},
		{"no line", `{"status":"sent","message_id":"m"}`},
		{"empty line", `{"status":"sent","from":"","message_id":"m"}`},
		{"line not a string", `{"status":"sent","from":2,"message_id":"m"}`},
		{"line too long", `{"status":"sent","from":"` + strings.Repeat("1", 200) + `","message_id":"m"}`},
		{"names no reply", `{"status":"sent",` + line + `}`},
		{"operation without attempt", `{"status":"sent",` + line + `,"message_id":"m","operation_id":"` + op + `"}`},
		{"attempt without operation", `{"status":"sent",` + line + `,"message_id":"m","attempt_id":"` + at + `"}`},
		{"operation not an id", `{"status":"sent",` + line + `,"message_id":"m","operation_id":"a b","attempt_id":"` + at + `"}`},
		{"attempt not an id", `{"status":"sent",` + line + `,"message_id":"m","operation_id":"` + op + `","attempt_id":""}`},
		{"operation not a string", `{"status":"sent",` + line + `,"message_id":"m","operation_id":5,"attempt_id":"` + at + `"}`},
		{"message id empty", `{"status":"sent",` + line + `,"message_id":""}`},
		{"message id not a string", `{"status":"sent",` + line + `,"message_id":7}`},
		{"message id too long", `{"status":"sent",` + line + `,"message_id":"` + strings.Repeat("m", 300) + `"}`},
		{"message id control", "{\"status\":\"sent\"," + line + ",\"message_id\":\"a\\u0000\"}"},
		{"trailing data", tagged("sent") + `{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &sink{}
			rec := serve(t, newInstance(t, statusConfig(), nil), s, good(tt.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("answer %d, want 400", rec.Code)
			}
			if len(s.ingresses()) != 0 {
				t.Fatal("the sink saw a refused update")
			}
		})
	}
}

// The route is off unless the channel turns it on: until then a request is a
// message whatever else it carries, and a sender that posts messages with a
// "status" key of its own is not misread.
func TestStatusRouteIsOffByDefault(t *testing.T) {
	s := &sink{}
	rec := serve(t, newInstance(t, baseConfig(), nil), s, good(tagged("delivered")))
	if rec.Code != http.StatusBadRequest || len(s.ingresses()) != 0 {
		t.Fatalf("a status update on a channel without the route: %d, %d ingresses", rec.Code, len(s.ingresses()))
	}
	withStatus := strings.Replace(hello, `"body"`, `"status":"sent","body"`, 1)
	if env := accept(t, baseConfig(), withStatus); env.Kind != model.EnvelopeMessage || env.Delivery != nil {
		t.Errorf("message carrying a status key: %+v", env)
	}
}

// A message on a channel with the route on is still a message: only the
// status key makes a request a status update.
func TestStatusRouteLeavesMessagesAlone(t *testing.T) {
	env := accept(t, statusConfig(), hello)
	if env.Kind != model.EnvelopeMessage || env.Delivery != nil || env.Sender.ID != "+15550100001" {
		t.Errorf("envelope %+v", env)
	}
	// Messages may share message_id and from with updates; nothing else of
	// either is read for the other.
	if env := accept(t, statusConfig(), `{"from":"+1","to":"+2","body":"hi","message_id":"m","operation_id":"x"}`); env.Kind != model.EnvelopeMessage {
		t.Errorf("envelope %+v", env)
	}
}

func TestStatusFieldNamesAndAliases(t *testing.T) {
	cfg := statusConfig()
	cfg.Inbound.Status.Fields = relay.StatusFields{
		Status: []string{"MessageStatus", "state"}, From: []string{"Line"}, OperationID: []string{"Op", "operation"},
		AttemptID: []string{"Attempt"}, MessageID: []string{"Sid"}, Code: []string{"ErrorCode"},
	}
	line := `"Line":"+15550100002"`
	env := accept(t, cfg, `{"MessageStatus":"failed",`+line+`,"Op":"`+string(testOp)+`","Attempt":"`+string(testAttempt)+`","Sid":"SM9","ErrorCode":30007}`)
	p := payloadOf(t, env)
	if env.Destination != "+15550100002" || env.MessageID != "SM9" || p.Operation != testOp || p.Attempt != testAttempt || p.Code != "30007" || p.Status != "failed" {
		t.Errorf("envelope %+v payload %+v", env, p)
	}
	// Second aliases work; the first present alias wins.
	env = accept(t, cfg, `{"state":"read","MessageStatus":"sent",`+line+`,"operation":"`+string(testOp)+`","Attempt":"`+string(testAttempt)+`"}`)
	if p := payloadOf(t, env); p.Status != "sent" || p.Operation != testOp {
		t.Errorf("payload %+v", p)
	}
	// Names not configured are not read: the neutral keys mean nothing here,
	// and the neutral status key makes no update.
	s := &sink{}
	if rec := serve(t, newInstance(t, cfg, nil), s, good(tagged("sent"))); rec.Code != http.StatusBadRequest {
		t.Errorf("neutral keys under renamed config: %d", rec.Code)
	}
}

// On iMessage the line is an address like any other: an Apple ID email
// address keeps one identity whatever case the relay reports it in.
func TestStatusLineFollowsTheMedium(t *testing.T) {
	cfg := statusConfig()
	cfg.Medium = relay.MediumIMessage
	env := accept(t, cfg, strings.Replace(tagged("sent"), "+15550100002", "Line@Example.com", 1))
	if env.Destination != "line@example.com" {
		t.Errorf("destination %q", env.Destination)
	}
	cfg.Medium = relay.MediumSMS
	if env := accept(t, cfg, strings.Replace(tagged("sent"), "+15550100002", "Line@Example.com", 1)); env.Destination != "Line@Example.com" {
		t.Errorf("destination %q", env.Destination)
	}
}

// An update is authenticated before a byte of it is parsed, as a message is.
func TestStatusUpdateNeedsAuthentication(t *testing.T) {
	inst := newInstance(t, statusConfig(), nil)
	for name, c := range map[string]call{
		"no credential":    {body: tagged("delivered")},
		"wrong credential": {body: tagged("delivered"), header: bearer("another-secret-0123456789abcdef")},
		"not json":         {body: "{", header: bearer("another-secret-0123456789abcdef")},
	} {
		s := &sink{}
		if rec := serve(t, inst, s, c); rec.Code != http.StatusUnauthorized || len(s.ingresses()) != 0 {
			t.Errorf("%s: answer %d, %d ingresses", name, rec.Code, len(s.ingresses()))
		}
	}
	if rec := serve(t, inst, &sink{}, call{method: http.MethodGet, header: bearer(webhookSecret)}); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestStatusUpdateIsBoundedByTheBodyLimit(t *testing.T) {
	cfg := statusConfig()
	cfg.Inbound.MaxBodyBytes = 256
	inst := newInstance(t, cfg, nil)
	s := &sink{}
	if rec := serve(t, inst, s, good(tagged("sent"))); rec.Code != http.StatusOK {
		t.Fatalf("under the limit: %d", rec.Code)
	}
	big := strings.Replace(tagged("sent"), `"gw-7"`, `"`+strings.Repeat("g", 300)+`"`, 1)
	s = &sink{}
	if rec := serve(t, inst, s, good(big)); rec.Code != http.StatusRequestEntityTooLarge || len(s.ingresses()) != 0 {
		t.Fatalf("over the limit: %d, %d ingresses", rec.Code, len(s.ingresses()))
	}
}

// Replay rejection covers updates as it covers messages: an accepted update
// sent again, byte for byte, is refused; one the engine did not take is
// released, so the relay's retry is a new attempt.
func TestStatusReplayIsRefused(t *testing.T) {
	cfg := replayConfig()
	cfg.Inbound.Status.Enabled = true
	inst := newInstance(t, cfg, nil)
	s := &sink{}
	if rec := serve(t, inst, s, replayCall(tagged("delivered"))); rec.Code != http.StatusOK {
		t.Fatalf("first: %d", rec.Code)
	}
	if rec := serve(t, inst, s, replayCall(tagged("delivered"))); rec.Code != http.StatusConflict {
		t.Fatalf("replay: %d", rec.Code)
	}
	if len(s.ingresses()) != 1 {
		t.Fatalf("%d ingresses", len(s.ingresses()))
	}
	for name, tt := range map[string]struct {
		s    *sink
		body string
		want int
	}{
		"retryable sink failure":  {&sink{err: model.NewError(model.Unavailable, "down")}, tagged("sent"), http.StatusServiceUnavailable},
		"permanent sink failure":  {&sink{err: errors.New("no")}, tagged("sent"), http.StatusUnprocessableEntity},
		"unusable update":         {&sink{}, `{"status":"sent"}`, http.StatusBadRequest},
		"unusable status keyword": {&sink{}, `{"status":"nope","from":"+1","message_id":"m"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			inst := newInstance(t, cfg, nil)
			for range 2 {
				if rec := serve(t, inst, tt.s, replayCall(tt.body)); rec.Code != tt.want {
					t.Fatalf("answer %d, want %d (a replay is 409)", rec.Code, tt.want)
				}
			}
		})
	}
}

// Status updates are not a capability of their own: receipt is the
// adapter telling a message's sender it was delivered or read, which the
// relay contract has no call for. The route is part of receive_http, on
// whatever the channel configures, and the descriptor and the reported
// capabilities say the same either way.
func TestStatusRouteDeclaresNoReceipt(t *testing.T) {
	for name, cfg := range map[string]relay.Config{"off": baseConfig(), "on": statusConfig()} {
		t.Run(name, func(t *testing.T) {
			inst := newInstance(t, cfg, nil)
			caps, err := inst.Capabilities(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range caps {
				if c.Name == model.CapabilityReceipt {
					t.Errorf("reports %s", c.Name)
				}
				if c.Name == model.CapabilityReceiveHTTP && !c.Available {
					t.Errorf("receive_http %+v", c)
				}
			}
		})
	}
	for _, c := range relay.NewFactory(relay.Options{}).Describe().Capabilities {
		if c == model.CapabilityReceipt {
			t.Error("declares receipt")
		}
	}
}
