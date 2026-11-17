package host_test

import (
	"context"
	"net/http"
	"sync"

	"hop.top/uma"
	"hop.top/uma/model"
)

// errDraining is what fakeRuntime.Run returns when the tenant was already
// drained, as the engine's Run does.
// It matches the engine's error: Unavailable, no retry.
var errDraining error = &model.Error{Code: model.Unavailable, SafeMessage: "runtime draining", Retry: model.NoRetry}

// fakeRuntime is a uma.Runtime with the three methods the host uses; any
// other call panics on the nil embedded interface. Run blocks until Drain,
// and a Drain issued before Run makes Run return errDraining, because a
// drain is permanent.
type fakeRuntime struct {
	uma.Runtime

	// runErr, when set before Run starts, is what Run returns at once.
	runErr error
	// drainErr is what Drain returns.
	drainErr error
	// started is closed when Run begins; drained when Drain is first called.
	started, drained chan struct{}
	// gate, when set, holds Run after it began until gate is closed, before
	// it looks at Drain: a test closes it after Drain to make the drain
	// reach the runtime first.
	gate chan struct{}

	mu       sync.Mutex
	webhooks int
	runs     int
	drains   int
	runCtx   context.Context
	call     uma.RuntimeCall
	opts     uma.WorkerOptions
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{started: make(chan struct{}), drained: make(chan struct{})}
}

// Webhook returns a handler that writes "hook:" and the channel ID, or an
// error for the channel ID "bad".
func (f *fakeRuntime) Webhook(_ context.Context, call uma.RuntimeCall, ref model.ChannelRef) (http.Handler, error) {
	f.mu.Lock()
	f.webhooks++
	f.call = call
	f.mu.Unlock()
	if ref.ID == "bad" {
		return nil, model.NewError(model.NotFound, "no such channel")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hook:" + string(ref.ID)))
	}), nil
}

func (f *fakeRuntime) Run(ctx context.Context, call uma.RuntimeCall, o uma.WorkerOptions) error {
	f.mu.Lock()
	f.runs++
	f.runCtx, f.call, f.opts = ctx, call, o
	if f.runs == 1 {
		close(f.started)
	}
	f.mu.Unlock()
	if f.runErr != nil {
		return f.runErr
	}
	if f.gate != nil {
		<-f.gate
	}
	select {
	case <-f.drained:
		return errDraining
	default:
	}
	<-f.drained
	return nil
}

func (f *fakeRuntime) Drain(context.Context, uma.RuntimeCall) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drains++
	if f.drains == 1 {
		close(f.drained)
	}
	return f.drainErr
}

// counts reports how many times each method was called.
func (f *fakeRuntime) counts() (webhooks, runs, drains int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.webhooks, f.runs, f.drains
}

// runContext is the context the first Run got.
func (f *fakeRuntime) runContext() context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runCtx
}

// lastArgs is the call and options of the latest Run or Webhook.
func (f *fakeRuntime) lastArgs() (uma.RuntimeCall, uma.WorkerOptions) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.call, f.opts
}
