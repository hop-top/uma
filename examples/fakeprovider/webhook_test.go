package fakeprovider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func newWebhookHost(t *testing.T) (*inboundHost, *fakeprovider.Factory) {
	f := fakeprovider.NewWebhook()
	return newInboundHost(t, f, f.Config()), f
}

func (h *inboundHost) mount() http.Handler {
	h.t.Helper()
	hd, err := h.engine.Runtime().Webhook(h.ctx, inCall(), inChannel)
	if err != nil {
		h.t.Fatalf("Webhook: %v", err)
	}
	return hd
}

func inEvent(id, dest string) []byte {
	b, _ := json.Marshal(fakeprovider.WebhookEvent{
		EventID: id, MessageID: "m-" + id, Destination: dest, Sender: "u1", Text: "hello " + id,
		OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	return b
}

// post serves one request and returns the status and body.
func inPost(hd http.Handler, body []byte, signature string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/fake", bytes.NewReader(body))
	if signature != "" {
		req.Header.Set(fakeprovider.SignatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestWebhookVerifiedEventCommitsOnceAcrossRedelivery(t *testing.T) {
	h, _ := newWebhookHost(t)
	hd := h.mount()
	body := inEvent("e1", "support")
	for i := range 2 {
		if code, _ := inPost(hd, body, fakeprovider.Sign(inSecret, body)); code != http.StatusOK {
			t.Fatalf("delivery %d = %d, want 200", i+1, code)
		}
	}
	if ms := h.messages(inSupport); len(ms) != 1 {
		t.Fatalf("support messages = %d, want 1 after a redelivered event", len(ms))
	}
	sales := inEvent("e2", "sales")
	if code, _ := inPost(hd, sales, fakeprovider.Sign(inSecret, sales)); code != http.StatusOK {
		t.Fatalf("sales delivery = %d", code)
	}
	if ms := h.messages(inSales); len(ms) != 1 {
		t.Fatalf("sales messages = %d, want 1: one channel routes to several inboxes", len(ms))
	}
}

func TestWebhookUnverifiedRequestWritesNothing(t *testing.T) {
	h, _ := newWebhookHost(t)
	hd := h.mount()
	body := inEvent("e1", "support")
	for name, sig := range map[string]string{
		"missing":    "",
		"wrong key":  fakeprovider.Sign([]byte("other"), body),
		"other body": fakeprovider.Sign(inSecret, inEvent("e2", "support")),
	} {
		code, text := inPost(hd, body, sig)
		if code != http.StatusUnauthorized {
			t.Errorf("%s signature = %d, want 401", name, code)
		}
		if strings.TrimSpace(text) != http.StatusText(http.StatusUnauthorized) {
			t.Errorf("%s signature body = %q, want the status text only", name, text)
		}
	}
	if ms := h.messages(inSupport); len(ms) != 0 {
		t.Fatalf("messages = %d after unverified requests, want 0", len(ms))
	}
	err := h.store.View(h.ctx, inTenant, func(r store.Reader) error {
		p, err := r.Contacts(h.ctx, model.ListRequest{Page: model.PageRequest{Limit: 10}})
		if err == nil && len(p.Items) != 0 {
			t.Errorf("contacts = %d after unverified requests, want 0", len(p.Items))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWebhookOversizedBodyIsRefused(t *testing.T) {
	h, _ := newWebhookHost(t)
	hd := h.mount()
	// A valid signed event padded past the limit: whitespace keeps it
	// parseable, so only the size limit can refuse it.
	body := append(inEvent("big", "support"), bytes.Repeat([]byte(" "), 2<<20)...)
	sig := fakeprovider.Sign(inSecret, body)
	if code, _ := inPost(hd, body, sig); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("declared oversized body = %d, want 413", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/webhooks/fake", io.MultiReader(bytes.NewReader(body)))
	req.ContentLength = -1
	req.Header.Set(fakeprovider.SignatureHeader, sig)
	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("streamed oversized body = %d, want 413", rec.Code)
	}
	if ms := h.messages(inSupport); len(ms) != 0 {
		t.Fatalf("messages = %d after oversized bodies, want 0", len(ms))
	}
}

func TestWebhookRepeatMountSharesTheBinding(t *testing.T) {
	h, _ := newWebhookHost(t)
	first, second := h.mount(), h.mount()
	if first != second {
		t.Fatal("a repeat mount opened a second binding")
	}
	other := inCall()
	other.Principal.Actor = "another-service"
	_, err := h.engine.Runtime().Webhook(h.ctx, other, inChannel)
	inCode(t, err, model.Conflict)
}

func TestWebhookRemountAfterReconfigurationRetiresTheOldBinding(t *testing.T) {
	h, _ := newWebhookHost(t)
	old := h.mount()
	h.tx(func(tx store.Tx) error {
		next := h.channel
		next.Revision = "ch2"
		return tx.PutChannel(h.ctx, next, h.channel.Revision)
	})
	cur := h.mount()
	if cur == old {
		t.Fatal("remount after reconfiguration shared the stale binding")
	}
	body := inEvent("e1", "support")
	if code, _ := inPost(old, body, fakeprovider.Sign(inSecret, body)); code != http.StatusServiceUnavailable {
		t.Fatalf("retired handler = %d, want 503", code)
	}
	if code, _ := inPost(cur, body, fakeprovider.Sign(inSecret, body)); code != http.StatusOK {
		t.Fatalf("current handler = %d, want 200", code)
	}
}

func TestWebhookRefusesChannelsWithoutHTTPReceive(t *testing.T) {
	f := fakeprovider.NewIngressOnly()
	h := newInboundHost(t, f, f.Config())
	_, err := h.engine.Runtime().Webhook(h.ctx, inCall(), inChannel)
	inCode(t, err, model.Unsupported)

	foreign := inCall()
	foreign.Principal.Tenant = "other-tenant"
	_, err = h.engine.Runtime().Webhook(h.ctx, foreign, inChannel)
	inCode(t, err, model.NotFound)
}

func TestWebhookRevokedPermissionIsUnauthorized(t *testing.T) {
	h, f := newWebhookHost(t)
	f.Script(inChannel).Revoke(model.CapabilityReceiveHTTP)
	_, err := h.engine.Runtime().Webhook(h.ctx, inCall(), inChannel)
	inCode(t, err, model.Unauthorized)
}

func TestWebhookAfterCloseAnswersUnavailable(t *testing.T) {
	h, _ := newWebhookHost(t)
	hd := h.mount()
	if err := h.engine.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := inEvent("e1", "support")
	if code, _ := inPost(hd, body, fakeprovider.Sign(inSecret, body)); code != http.StatusServiceUnavailable {
		t.Fatalf("handler after Close = %d, want 503", code)
	}
	_, err := h.engine.Runtime().Webhook(h.ctx, inCall(), inChannel)
	inCode(t, err, model.Unavailable)
}
