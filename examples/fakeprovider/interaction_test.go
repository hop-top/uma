package fakeprovider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Interactions through the webhook variant: a signed request reports what
// an actor did, the handler acknowledges it only once the sink holds it
// durably, and a redelivery is acknowledged without a second record.

const (
	interactionToken = "tok-fake-secret-0001"
	actionID         = "approve"
)

var interactionAt = time.Unix(1_700_000_000, 0).UTC()

// interactionBody is a signed-able webhook body reporting an action by u1
// on a message of the support destination.
func interactionBody(t *testing.T, id string, deadline time.Time, mod ...func(*fakeprovider.WebhookEvent)) []byte {
	t.Helper()
	ev := fakeprovider.WebhookEvent{
		EventID: id, Destination: "support", Sender: "u1", OccurredAt: interactionAt,
		Interaction: &fakeprovider.WebhookInteraction{
			Kind: model.InteractionAction, Action: actionID, Token: interactionToken,
			TokenExpiresAt: interactionAt.Add(15 * time.Minute), Deadline: deadline,
		},
	}
	for _, m := range mod {
		m(&ev)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (h *inboundHost) post(hd http.Handler, body []byte) int {
	h.t.Helper()
	code, _ := inPost(hd, body, fakeprovider.Sign(inSecret, body))
	return code
}

// interactions returns the stored interactions of the support inbox.
func (h *inboundHost) interactions() []model.Interaction {
	h.t.Helper()
	var out []model.Interaction
	err := h.store.View(h.ctx, inTenant, func(r store.Reader) error {
		p, err := r.Interactions(h.ctx, model.ListRequest{
			Filter: model.Filter{Inboxes: []model.InboxRef{inSupport}}, Page: model.PageRequest{Limit: 100},
		})
		out = p.Items
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func (h *inboundHost) eventCount(kind string) int {
	h.t.Helper()
	n := 0
	err := h.store.View(h.ctx, inTenant, func(r store.Reader) error {
		p, err := r.Events(h.ctx, model.Filter{Kinds: []string{kind}}, "", 100)
		n = len(p.Items)
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// The acknowledgement waits for durable acceptance: a store that cannot
// promise durability gets a 503 and no acknowledgement, so the provider
// redelivers; over a durable one the same request is a 200, recorded as
// the acknowledgement it was.
func TestWebhookInteractionAckAfterDurable(t *testing.T) {
	deadline := interactionAt.Add(3 * time.Second)
	body := interactionBody(t, "ix-1", deadline)

	h, f := func() (*inboundHost, *fakeprovider.Factory) {
		f := fakeprovider.NewWebhook()
		return newInboundHost(t, f, f.Config()), f
	}()
	if code := h.post(h.mount(), body); code != http.StatusServiceUnavailable {
		t.Fatalf("interaction over a non-durable store = %d, want 503", code)
	}
	if acks := f.Script(inChannel).Acks(); len(acks) != 0 {
		t.Fatalf("acknowledgements = %+v, want none before durable acceptance", acks)
	}

	f = fakeprovider.NewWebhook()
	h = newInboundHostOn(t, f, f.Config(), true)
	if code := h.post(h.mount(), body); code != http.StatusOK {
		t.Fatalf("interaction over a durable store = %d, want 200", code)
	}
	acks := f.Script(inChannel).Acks()
	if len(acks) != 1 || acks[0].EventID != "ix-1" || acks[0].Late || !acks[0].Deadline.Equal(deadline) {
		t.Fatalf("acknowledgements = %+v, want one in time for ix-1", acks)
	}
	ins := h.interactions()
	if len(ins) != 1 || ins[0].Kind != model.InteractionAction || ins[0].Action != actionID || ins[0].Actor.External.ID != "u1" {
		t.Fatalf("interactions = %+v, want the one action by u1", ins)
	}
	if ms := h.messages(inSupport); len(ms) != 0 {
		t.Fatalf("messages = %+v: an interaction is no message", ms)
	}
	if strings.Contains(fmt.Sprintf("%+v", ins), interactionToken) {
		t.Fatalf("interaction record holds the response token: %+v", ins)
	}
}

// A redelivered interaction is acknowledged again and recorded once, one
// interaction.received event.
func TestWebhookInteractionRedelivery(t *testing.T) {
	f := fakeprovider.NewWebhook()
	h := newInboundHostOn(t, f, f.Config(), true)
	hd := h.mount()
	body := interactionBody(t, "ix-1", interactionAt.Add(3*time.Second))
	for i := range 3 {
		if code := h.post(hd, body); code != http.StatusOK {
			t.Fatalf("delivery %d = %d, want 200", i+1, code)
		}
	}
	if n := len(h.interactions()); n != 1 {
		t.Fatalf("interactions = %d, want 1 after redelivery", n)
	}
	if n := h.eventCount(model.EventInteractionReceived); n != 1 {
		t.Fatalf("interaction.received events = %d, want 1", n)
	}
	if n := h.eventCount(model.EventMessageReceived); n != 0 {
		t.Fatalf("message.received events = %d, want none for an interaction", n)
	}
	if n := len(f.Script(inChannel).Acks()); n != 3 {
		t.Fatalf("acknowledgements = %d, want one per delivery", n)
	}
	// The token sits beside the record and nowhere else.
	err := h.store.View(h.ctx, inTenant, func(r store.Reader) error {
		tok, err := r.InteractionToken(h.ctx, h.interactions()[0].Ref)
		if err == nil && tok.Token != interactionToken {
			t.Errorf("stored token = %q, want the one the provider sent", tok.Token)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A scripted deadline against the engine's clock makes a late
// acknowledgement observable: the interaction is still recorded, since the
// provider can only be told late, never un-told.
func TestWebhookInteractionLateAckIsObservable(t *testing.T) {
	f := fakeprovider.NewWebhook()
	h := newInboundHostOn(t, f, f.Config(), true)
	hd := h.mount()
	now := h.clock.Now()
	if code := h.post(hd, interactionBody(t, "ix-fresh", now.Add(3*time.Second))); code != http.StatusOK {
		t.Fatalf("in-time interaction = %d", code)
	}
	h.clock.Advance(5 * time.Second)
	if code := h.post(hd, interactionBody(t, "ix-late", now.Add(3*time.Second))); code != http.StatusOK {
		t.Fatalf("late interaction = %d, want it still accepted", code)
	}
	acks := f.Script(inChannel).Acks()
	if len(acks) != 2 || acks[0].Late || !acks[1].Late || acks[1].EventID != "ix-late" {
		t.Fatalf("acknowledgements = %+v, want the second one late", acks)
	}
	if n := len(h.interactions()); n != 2 {
		t.Fatalf("interactions = %d, want both recorded", n)
	}
}

// What the sink refuses is not acknowledged: nothing is recorded and the
// provider is told the request cannot succeed.
func TestWebhookInteractionRefusals(t *testing.T) {
	f := fakeprovider.NewWebhook()
	h := newInboundHostOn(t, f, f.Config(), true)
	hd := h.mount()
	deadline := interactionAt.Add(3 * time.Second)
	for name, mod := range map[string]func(*fakeprovider.WebhookEvent){
		"options are evaluated, not accepted": func(e *fakeprovider.WebhookEvent) { e.Interaction.Kind = model.InteractionOptions },
		"an action names its action":          func(e *fakeprovider.WebhookEvent) { e.Interaction.Action = "" },
		"a response token needs its expiry": func(e *fakeprovider.WebhookEvent) {
			e.Interaction.TokenExpiresAt = time.Time{}
		},
		"a deadline is required": func(e *fakeprovider.WebhookEvent) { e.Interaction.Deadline = time.Time{} },
		"an unknown kind":        func(e *fakeprovider.WebhookEvent) { e.Interaction.Kind = "gesture" },
	} {
		if code := h.post(hd, interactionBody(t, "ix-bad", deadline, mod)); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d, want 422", name, code)
		}
	}
	if acks := f.Script(inChannel).Acks(); len(acks) != 0 {
		t.Errorf("acknowledgements = %+v, want none", acks)
	}
	if n := len(h.interactions()); n != 0 {
		t.Errorf("interactions = %d, want none", n)
	}
	// Unsigned: never reaches the sink.
	body := interactionBody(t, "ix-1", deadline)
	req := httptest.NewRequest(http.MethodPost, "/hooks/fake", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned interaction = %d, want 401", rec.Code)
	}
}

// A receive variant delivers an interaction envelope queued with
// Script.Interact like any ingress: recorded as an interaction, the actor
// the sender, and acknowledged out of the queue.
func TestScriptInteractDeliversAnInteraction(t *testing.T) {
	h, f := newReceiveHost(t, 0, nil)
	script := f.Script(inChannel)
	script.Interact("support", model.Audience{Visibility: model.Public}, model.InteractionInput{
		ID: "ix-1", Kind: model.InteractionCommand, Command: "triage", Arguments: "now",
		Actor:         model.ExternalRef{Channel: inChannel, Namespace: fakeprovider.Namespace, Kind: "user", ID: "u1"},
		ResponseToken: interactionToken, ResponseExpiresAt: interactionAt.Add(time.Hour), Deadline: interactionAt.Add(3 * time.Second),
	})
	ctx, cancel := context.WithCancel(h.ctx)
	done := h.receive(ctx)
	inEventually(t, "the interaction to be recorded", func() bool { return len(h.interactions()) == 1 && script.Pending() == 0 })
	cancel()
	if err := <-done; err != nil {
		t.Logf("Run: %v", err)
	}
	in := h.interactions()[0]
	if in.Kind != model.InteractionCommand || in.Command != "triage" || in.Arguments != "now" || in.Actor.External.ID != "u1" {
		t.Fatalf("interaction = %+v, want the triage command by u1", in)
	}
	if ds := script.Deliveries(); len(ds) != 1 || !ds[0].Acked {
		t.Fatalf("deliveries = %+v, want one acknowledged", ds)
	}
	if ms := h.messages(inSupport); len(ms) != 0 {
		t.Fatalf("messages = %+v: an interaction is no message", ms)
	}
}
