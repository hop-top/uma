package runtime_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

const hookType model.TypeKey = "com.example.ingress.hook"

// hookAdapter is an HTTP receiver whose handler the test supplies. It
// counts opens and closes.
type hookAdapter struct {
	handler func(adapter.Sink) http.Handler
	openErr error
	bare    bool // open an instance without the HTTPReceiver interface
	opens   atomic.Int32
	closes  atomic.Int32
}

func (a *hookAdapter) Describe() adapter.Descriptor {
	return adapter.Descriptor{
		Type: hookType, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(hookType) + ".ingress-test", Version: 1},
		Capabilities:        []model.Capability{model.CapabilityReceiveHTTP},
		Events:              []string{model.EventMessageReceived},
		IdentityNamespaces:  []string{ingressNS},
	}
}
func (*hookAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *hookAdapter) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	if a.openErr != nil {
		return nil, a.openErr
	}
	a.opens.Add(1)
	if a.bare {
		return &hookBare{a: a}, nil
	}
	return &hookInstance{hookBare{a: a}}, nil
}

type hookBare struct{ a *hookAdapter }

func (*hookBare) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceiveHTTP, Available: true}}, nil
}
func (i *hookBare) Close(context.Context) error { i.a.closes.Add(1); return nil }

type hookInstance struct{ hookBare }

func (i *hookInstance) Handler(s adapter.Sink) http.Handler { return i.a.handler(s) }

func newHookHarness(t *testing.T, a *hookAdapter) *harness {
	t.Helper()
	return newHarness(t, hookType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a) })
}

func mount(h *harness) (http.Handler, error) {
	return h.RT.Webhook(h.Ctx, runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "mount"}}, h.Channel.Ref)
}

// A sloppy adapter ignores the read error and answers 202; the configured
// limit still holds: a declared oversized body never reaches the adapter
// and a streamed one is answered 413.
func TestWebhookBodyLimitHoldsAgainstASloppyAdapter(t *testing.T) {
	var calls atomic.Int32
	a := &hookAdapter{handler: func(adapter.Sink) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("adapter detail"))
		})
	}}
	h := newHarness(t, hookType, func(c *engine.Config) { c.Adapters = append(c.Adapters, a); c.WebhookBodyLimit = 64 })
	hd, err := mount(h)
	if err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), 65)

	rec := httptest.NewRecorder()
	hd.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(big)))
	if rec.Code != http.StatusRequestEntityTooLarge || calls.Load() != 0 {
		t.Fatalf("declared oversized: status %d, adapter calls %d; want 413 before the adapter", rec.Code, calls.Load())
	}

	req := httptest.NewRequest(http.MethodPost, "/", io.MultiReader(bytes.NewReader(big)))
	req.ContentLength = -1
	rec = httptest.NewRecorder()
	hd.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || strings.Contains(rec.Body.String(), "adapter detail") {
		t.Fatalf("streamed oversized: status %d body %q; want a bare 413", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	hd.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(big[:64])))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("body at the limit: status %d, want the adapter's 202", rec.Code)
	}
}

// A replaced binding finishes its in-flight request and only then closes
// its instance; new requests to it get 503.
func TestWebhookRetiredBindingClosesAfterInflightRequests(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	a := &hookAdapter{handler: func(adapter.Sink) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/slow" {
				once.Do(func() { close(entered) })
				<-release
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}}
	h := newHookHarness(t, a)
	old, err := mount(h)
	if err != nil {
		t.Fatal(err)
	}
	slow := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		old.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/slow", nil))
		slow <- rec.Code
	}()
	<-entered
	reconfigure(h)
	cur, err := mount(h)
	if err != nil || cur == old {
		t.Fatalf("remount = %v, %v; want a new binding", cur, err)
	}
	if n := a.closes.Load(); n != 0 {
		t.Fatalf("closes = %d while a request is in flight, want 0", n)
	}
	rec := httptest.NewRecorder()
	old.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("retired binding = %d, want 503", rec.Code)
	}
	close(release)
	if code := <-slow; code != http.StatusNoContent {
		t.Fatalf("in-flight request = %d", code)
	}
	if n := a.closes.Load(); n != 1 {
		t.Fatalf("closes = %d after the in-flight request ended, want 1", n)
	}
	if err := h.E.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if o, c := a.opens.Load(), a.closes.Load(); o != 2 || c != 2 {
		t.Fatalf("opens %d closes %d after Close, want every instance closed", o, c)
	}
}

func reconfigure(h *harness) {
	h.T.Helper()
	next := h.Channel
	next.Revision = h.Channel.Revision + "+"
	h.Tx(func(tx store.Tx) error { return tx.PutChannel(h.Ctx, next, h.Channel.Revision) })
	h.Channel = next
}

func TestWebhookOpenFailuresStaySafe(t *testing.T) {
	a := &hookAdapter{openErr: errors.New("dial 10.0.0.7: provider internals")}
	h := newHookHarness(t, a)
	_, err := mount(h)
	requireCode(t, err, model.Unavailable, "adapter instance unavailable")
	if strings.Contains(err.Error(), "internals") {
		t.Fatalf("err = %v leaks the provider error", err)
	}

	bare := &hookAdapter{bare: true}
	h = newHookHarness(t, bare)
	_, err = mount(h)
	requireCode(t, err, model.Permanent, "does not match its descriptor")
	if bare.closes.Load() != 1 {
		t.Fatal("mismatched instance not closed")
	}
}

// uploadInstance takes uploads on /uploads.
type uploadInstance struct{ hookInstance }

func (*uploadInstance) Upload(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/uploads") }

type uploadAdapter struct{ *hookAdapter }

func (a uploadAdapter) Open(ctx context.Context, ch model.Channel, d adapter.Dependencies) (adapter.Instance, error) {
	a.opens.Add(1)
	return &uploadInstance{hookInstance{hookBare{a: a.hookAdapter}}}, nil
}

// A request the adapter reports as an attachment upload is bounded by the
// attachment size limit instead of the webhook body limit; every other
// request keeps the webhook limit.
func TestWebhookUploadsTakeTheAttachmentLimit(t *testing.T) {
	a := uploadAdapter{&hookAdapter{handler: func(adapter.Sink) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.ReadAll(r.Body); err != nil {
				return
			}
			w.WriteHeader(http.StatusAccepted)
		})
	}}}
	h := newHarness(t, hookType, func(c *engine.Config) {
		c.Adapters = append(c.Adapters, a)
		c.WebhookBodyLimit, c.AttachmentSizeLimit = 64, 256
	})
	hd, err := mount(h)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path string
		size int
		want int
	}{
		{"/w/uploads", 256, http.StatusAccepted},
		{"/w/uploads", 257, http.StatusRequestEntityTooLarge},
		{"/w/messages", 65, http.StatusRequestEntityTooLarge},
	} {
		rec := httptest.NewRecorder()
		hd.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, c.path, bytes.NewReader(bytes.Repeat([]byte("x"), c.size))))
		if rec.Code != c.want {
			t.Fatalf("%s with %d bytes: status %d, want %d", c.path, c.size, rec.Code, c.want)
		}
	}
}
