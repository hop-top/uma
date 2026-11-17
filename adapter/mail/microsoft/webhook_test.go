package microsoft

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"hop.top/uma/model"
)

func notify(t *testing.T, e *env, sink *testSink, ns ...gNotification) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(gNotificationCollection{Value: ns})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "https://hooks.example.test/graph", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.inst.Handler(sink).ServeHTTP(rec, req)
	return rec
}

func clientStateOf(t *testing.T, e *env) string {
	t.Helper()
	cs, err := e.inst.clientState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func created(cs, id string) gNotification {
	n := gNotification{
		SubscriptionID: "sub-1", ClientState: cs, ChangeType: "created", TenantID: "tenant-1",
		Resource: "users/someone-else/messages/" + id,
	}
	n.ResourceData = &struct {
		ID string `json:"id"`
	}{ID: id}
	return n
}

func TestWebhookEchoesValidationToken(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: "https://hooks.example.test/graph"})
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://hooks.example.test/graph?validationToken=Validation%3A+Testing+client+%3Cb%3E", strings.NewReader(""))
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	rec := httptest.NewRecorder()
	e.inst.Handler(&testSink{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("validation answer %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if got := rec.Body.String(); got != "Validation: Testing client <b>" {
		t.Fatalf("token = %q, want the URL-decoded token as plain text", got)
	}
}

func TestWebhookRejectsClientStateMismatch(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: "https://hooks.example.test/graph"})
	e.inbound("m1", "peer@example.test", "hi")
	sink := &testSink{}
	cs := clientStateOf(t, e)
	for _, bad := range []string{"", "secretClientValue", cs[:len(cs)-1], cs + "x"} {
		rec := notify(t, e, sink, created(bad, "m1"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("clientState %q answered %d, want 403", bad, rec.Code)
		}
	}
	if len(sink.got) != 0 || e.g.count("GET /v1.0/users/") != 0 {
		t.Fatalf("a notification with a foreign clientState was processed: sink %v, requests %q", sink.ids(), e.g.requests())
	}
}

func TestWebhookFetchesNotifiedMessageFromOwnMailbox(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: "https://hooks.example.test/graph"})
	e.inbound("m1", "peer@example.test", "hi")
	sink := &testSink{}
	cs := clientStateOf(t, e)
	rec := notify(t, e, sink, created("forged", "m1"), created(cs, "m1"))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("answer %d, want 202", rec.Code)
	}
	if ids := sink.ids(); len(ids) != 1 || ids[0] != "m1" {
		t.Fatalf("accepted %v, want m1 once (the forged duplicate dropped)", ids)
	}
	for _, r := range e.g.requests() {
		if strings.Contains(r, "someone-else") {
			t.Fatalf("fetched the notified resource path %q instead of the channel mailbox", r)
		}
	}
}

func TestWebhookRetryableSinkFailureAsksForRedelivery(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: "https://hooks.example.test/graph"})
	e.inbound("m1", "peer@example.test", "hi")
	sink := &testSink{refuse: func(model.Ingress) error { return model.NewError(model.Unavailable, "store down") }}
	if rec := notify(t, e, sink, created(clientStateOf(t, e), "m1")); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("answer %d, want 503 so Graph redelivers", rec.Code)
	}
	sink.refuse = func(model.Ingress) error { return model.NewError(model.Validation, "unroutable") }
	if rec := notify(t, e, sink, created(clientStateOf(t, e), "m1")); rec.Code != http.StatusAccepted {
		t.Fatalf("answer %d to a permanent refusal, want 202", rec.Code)
	}
}

func TestWebhookLifecycleEventsReachTheReceiver(t *testing.T) {
	e := newEnv(t, Config{NotificationURL: "https://hooks.example.test/graph"})
	cs := clientStateOf(t, e)
	for _, c := range []struct {
		event string
		work  int
	}{
		{eventReauthorizationRequired, lifecycleRenew},
		{eventSubscriptionRemoved, lifecycleRecreate},
		{eventMissed, 0},
	} {
		rec := notify(t, e, &testSink{}, gNotification{SubscriptionID: "sub-1", ClientState: cs, LifecycleEvent: c.event})
		if rec.Code != http.StatusAccepted {
			t.Fatalf("%s answered %d", c.event, rec.Code)
		}
		select {
		case <-e.inst.state.wake:
		default:
			t.Fatalf("%s did not wake the receiver", c.event)
		}
		if got := e.inst.state.takeLifecycle(); got != c.work {
			t.Fatalf("%s left work %b, want %b", c.event, got, c.work)
		}
	}
}

func TestClientStateIsPerChannelAndHidesTheSecret(t *testing.T) {
	e := newEnv(t, Config{})
	cs := clientStateOf(t, e)
	if len(cs) > 128 || strings.Contains(cs, string(e.creds.secret)) {
		t.Fatalf("client state %q", cs)
	}
	other := *e.inst
	other.ch.Ref.ID = "00000000-0000-7000-8000-0000000000c2"
	cs2, err := other.clientState(t.Context())
	if err != nil || cs2 == cs {
		t.Fatalf("two channels share a client state: %q %q %v", cs, cs2, err)
	}
}
