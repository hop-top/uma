package a2a_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/agent/a2a"
	"hop.top/uma/model"
)

// A message the host's policy does not admit is a JSON-RPC error that
// reveals nothing, and writes nothing.
func TestDeniedIngressIsErrorWithoutWrites(t *testing.T) {
	d := &denials{}
	r := newRig(t, rigOptions{authorizer: d})
	d.deny(model.ActionChannelReceive)
	e := r.send("agent-1", "m-1", "hello", immediately()).fail(t, -32603)
	if info := e.info(t); info.Reason != "UNAUTHORIZED" || info.Domain != "uma.hop.top" {
		t.Fatalf("detail = %+v", info)
	}
	if convs, _ := r.engineConversations(); len(convs) != 0 || r.taskCount() != 0 {
		t.Fatalf("a denied message left %d conversations and %d tasks", len(convs), r.taskCount())
	}
}

// Reads the service principal may not make are indistinguishable from an
// absent task.
func TestDeniedTaskReadIsNotFound(t *testing.T) {
	d := &denials{}
	r := newRig(t, rigOptions{authorizer: d})
	a := decode[sendResult](t, r.send("agent-1", "m-1", "hello", immediately()).ok(t)).Task
	d.deny(model.ActionTaskRead)
	r.rpc("agent-1", "GetTask", map[string]any{"id": a.ID}).fail(t, -32001)
	d.deny()
	r.rpc("agent-1", "GetTask", map[string]any{"id": a.ID}).ok(t)
}

// The caller's own deadline bounds the wait below blocking_wait.
func TestCallerDeadlineBoundsWait(t *testing.T) {
	r := newRig(t, rigOptions{
		config: []func(*a2a.Config){func(c *a2a.Config) { c.BlockingWait = "30s"; c.PollInterval = "50ms" }},
		wrap: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx, cancel := context.WithTimeout(req.Context(), 300*time.Millisecond)
				defer cancel()
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		},
	})
	start := time.Now()
	e := r.send("agent-1", "m-1", "hello", nil).fail(t, -32603)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %v, the caller's deadline was 300ms", d)
	}
	if e.info(t).Reason != "DEADLINE_EXCEEDED" {
		t.Fatalf("detail = %+v", e.info(t))
	}
}

// A sink that is not the engine's (no task or attachment interface) gets
// no service but the card.
func TestHandlerNeedsTaskAndAttachmentSink(t *testing.T) {
	f, _ := a2a.New(a2a.Options{Authenticate: headerAuth})
	inst, err := f.Open(t.Context(), model.Channel{Ref: chanA, Type: a2a.Type, Configuration: a2a.Configuration(testConfig())}, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	h := inst.(adapter.HTTPReceiver).Handler(bareSink{})
	srv := newServer(t, h)
	rp := srv.post([]string{"X-Agent", "a", "A2A-Version", "1.0"}, []byte(`{"jsonrpc":"2.0","id":1,"method":"GetTask","params":{"id":"x"}}`))
	if rp.Status != http.StatusServiceUnavailable {
		t.Fatalf("POST: HTTP %d, want 503", rp.Status)
	}
	if rp := srv.get("/a2a/agent-card.json"); rp.Status != http.StatusOK {
		t.Fatalf("card: HTTP %d", rp.Status)
	}
}

type bareSink struct{}

func (bareSink) Accept(context.Context, model.Ingress) (model.IngressReceipt, error) {
	return model.IngressReceipt{}, nil
}

func (bareSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, nil
}

func TestClosedInstanceAnswers503(t *testing.T) {
	f, _ := a2a.New(a2a.Options{Authenticate: headerAuth})
	inst, err := f.Open(t.Context(), model.Channel{Ref: chanA, Type: a2a.Type, Configuration: a2a.Configuration(testConfig())}, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	h := inst.(adapter.HTTPReceiver).Handler(bareSink{})
	if err := inst.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	srv := newServer(t, h)
	if rp := srv.get("/a2a/agent-card.json"); rp.Status != http.StatusServiceUnavailable {
		t.Fatalf("closed: HTTP %d, want 503", rp.Status)
	}
	if _, err := inst.Capabilities(t.Context()); !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Capabilities after Close = %v, want unavailable", err)
	}
}

func TestOpenValidates(t *testing.T) {
	f, _ := a2a.New(a2a.Options{Authenticate: headerAuth})
	for name, ch := range map[string]model.Channel{
		"other type": {Ref: chanA, Type: "native.webwidget", Configuration: a2a.Configuration(testConfig())},
		"bad config": {Ref: chanA, Type: a2a.Type, Configuration: model.Extension{Name: a2a.ConfigSchema, Version: 1}},
	} {
		if _, err := f.Open(t.Context(), ch, adapter.Dependencies{}); !model.IsCode(err, model.Validation) {
			t.Errorf("%s: Open = %v, want validation", name, err)
		}
	}
	inst, err := f.Open(t.Context(), model.Channel{Ref: chanA, Type: a2a.Type, Configuration: a2a.Configuration(testConfig())}, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := inst.Capabilities(t.Context())
	if err != nil || len(st) != 2 || !st[0].Available || !st[1].Available {
		t.Fatalf("capabilities = %+v, %v", st, err)
	}
}
