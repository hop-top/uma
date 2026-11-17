package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
)

// Webhook opens the channel's adapter instance once and returns an
// http.Handler serving the adapter's HTTP receiver with the engine-bound
// sink of call's service principal. The channel must belong to the
// principal's tenant and its adapter must declare and currently offer
// receive_http.
//
// The principal needs model.ActionChannelReceive on the channel to mount,
// and the sink asks again for every ingress, so a live binding stops
// accepting once the grant is revoked; the adapter answers the denial.
//
// The adapter verifies provider signatures or transport authentication,
// and answers the provider; the sink is the only write path, so a request
// the adapter does not verify writes nothing. The engine bounds the body
// to Config.WebhookBodyLimit, or Config.AttachmentSizeLimit for a request
// the adapter reports as an upload (adapter.UploadReceiver), answers 503 once the binding is retired or the engine closes, and keeps
// provider internals out of every response it writes itself.
//
// Mounting the same channel again under the same principal and channel
// revision shares the open binding. A mount after the channel's
// configuration changed replaces it: the earlier handler answers 503 from
// then on and its instance closes when its last request ends. A mount under
// a different service principal while a binding is live is model.Conflict.
// Engine.Close closes every binding.
func (rt *Runtime) Webhook(ctx context.Context, call Call, ref model.ChannelRef) (http.Handler, error) {
	e := rt.e
	if e.IsClosed() {
		return nil, engine.ErrClosed()
	}
	if err := call.Principal.Validate(); err != nil {
		return nil, err
	}
	done, err := e.Begin()
	if err != nil {
		return nil, err
	}
	defer done()
	snk, err := ingress.Bind(ctx, e, call.Principal, ref)
	if err != nil {
		return nil, err
	}
	if b, err := rt.hooks.lookup(snk); b != nil || err != nil {
		return handlerOf(b, err)
	}
	inst, err := openInbound(ctx, e, snk, model.CapabilityReceiveHTTP)
	if err != nil {
		return nil, err
	}
	// checkInstance guarantees the interface for a declared receive_http.
	h := inst.(adapter.HTTPReceiver).Handler(snk)
	if h == nil {
		_ = inst.Close(context.WithoutCancel(ctx))
		return nil, &model.Error{Code: model.Permanent, SafeMessage: "adapter returned no webhook handler", Retry: model.NoRetry}
	}
	return handlerOf(rt.hooks.install(&webhookBinding{e: e, sink: snk, inst: inst, handler: h}))
}

// handlerOf keeps a nil binding a nil handler.
func handlerOf(b *webhookBinding, err error) (http.Handler, error) {
	if err != nil || b == nil {
		return nil, err
	}
	return b, nil
}

// webhooks holds the engine's live webhook bindings, one per channel. It is
// safe for concurrent use; the zero value is ready.
type webhooks struct {
	mu     sync.Mutex
	closed bool
	bound  map[model.ChannelRef]*webhookBinding
}

// lookup returns the live binding the sink's mount shares, nil when none
// matches, or a conflict for another principal's binding.
func (w *webhooks) lookup(s *ingress.Sink) (*webhookBinding, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, engine.ErrClosed()
	}
	return w.shared(s)
}

func (w *webhooks) shared(s *ingress.Sink) (*webhookBinding, error) {
	b, ok := w.bound[s.Channel().Ref]
	switch {
	case !ok:
		return nil, nil
	case b.sink.Principal() != s.Principal():
		return nil, &model.Error{Code: model.Conflict, SafeMessage: "webhook already mounted for the channel under another service principal", Retry: model.NoRetry}
	case b.sink.Channel().Revision != s.Channel().Revision:
		return nil, nil
	}
	return b, nil
}

// install records b as its channel's binding, unless a concurrent mount
// installed a binding b would share, which is returned instead and b
// closed. A binding for an older channel revision is retired.
func (w *webhooks) install(b *webhookBinding) (*webhookBinding, error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		b.retire()
		return nil, engine.ErrClosed()
	}
	existing, err := w.shared(b.sink)
	if existing != nil || err != nil {
		w.mu.Unlock()
		b.retire()
		return existing, err
	}
	old := w.bound[b.sink.Channel().Ref]
	if w.bound == nil {
		w.bound = map[model.ChannelRef]*webhookBinding{}
	}
	w.bound[b.sink.Channel().Ref] = b
	w.mu.Unlock()
	if old != nil {
		old.retire()
	}
	return b, nil
}

// closeAll retires every binding and refuses new ones; Engine.Close calls
// it.
func (w *webhooks) closeAll() {
	w.mu.Lock()
	w.closed = true
	bound := w.bound
	w.bound = nil
	w.mu.Unlock()
	for _, b := range bound {
		b.retire()
	}
}

// webhookBinding is one open instance serving one channel's webhook. Its
// instance closes once the binding is retired and no request is in flight.
type webhookBinding struct {
	e       *engine.Engine
	sink    *ingress.Sink
	inst    adapter.Instance
	handler http.Handler

	mu       sync.Mutex
	inflight int
	retired  bool
	closed   bool
}

func (b *webhookBinding) enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.retired {
		return false
	}
	b.inflight++
	return true
}

func (b *webhookBinding) leave() {
	b.mu.Lock()
	b.inflight--
	b.mu.Unlock()
	b.closeIfIdle()
}

func (b *webhookBinding) retire() {
	b.mu.Lock()
	b.retired = true
	b.mu.Unlock()
	b.closeIfIdle()
}

func (b *webhookBinding) closeIfIdle() {
	b.mu.Lock()
	if !b.retired || b.inflight > 0 || b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	_ = b.inst.Close(context.Background())
}

// ServeHTTP admits the request as engine work, bounds its body and hands it
// to the adapter's handler.
func (b *webhookBinding) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.enter() {
		plainStatus(w, http.StatusServiceUnavailable)
		return
	}
	defer b.leave()
	done, err := b.e.Begin()
	if err != nil {
		plainStatus(w, http.StatusServiceUnavailable)
		return
	}
	defer done()
	limit := b.e.WebhookBodyLimit()
	if u, ok := b.inst.(adapter.UploadReceiver); ok && u.Upload(r) {
		limit = b.e.AttachmentSizeLimit()
	}
	if r.ContentLength > limit {
		plainStatus(w, http.StatusRequestEntityTooLarge)
		return
	}
	lw := &limitWriter{ResponseWriter: w}
	r.Body = &limitBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit), w: lw}
	b.handler.ServeHTTP(lw, r)
}

// plainStatus answers with the bare status text and nothing else.
func plainStatus(w http.ResponseWriter, code int) {
	http.Error(w, http.StatusText(code), code)
}

// limitBody records that a read hit the body limit.
type limitBody struct {
	io.ReadCloser
	w *limitWriter
}

func (l *limitBody) Read(p []byte) (int, error) {
	n, err := l.ReadCloser.Read(p)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		l.w.overflow()
	}
	return n, err
}

// limitWriter replaces the adapter's status with 413 once the body
// overflowed before the response started.
type limitWriter struct {
	http.ResponseWriter

	mu       sync.Mutex
	over     bool
	wrote    bool
	replaced bool
}

func (l *limitWriter) overflow() {
	l.mu.Lock()
	l.over = true
	l.mu.Unlock()
}

func (l *limitWriter) WriteHeader(code int) {
	l.mu.Lock()
	if l.wrote {
		l.mu.Unlock()
		return
	}
	l.wrote = true
	l.replaced = l.over
	l.mu.Unlock()
	if l.replaced {
		plainStatus(l.ResponseWriter, http.StatusRequestEntityTooLarge)
		return
	}
	l.ResponseWriter.WriteHeader(code)
}

func (l *limitWriter) Write(p []byte) (int, error) {
	l.WriteHeader(http.StatusOK)
	l.mu.Lock()
	replaced := l.replaced
	l.mu.Unlock()
	if replaced {
		return len(p), nil
	}
	return l.ResponseWriter.Write(p)
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (l *limitWriter) Unwrap() http.ResponseWriter { return l.ResponseWriter }
