package host_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
	"hop.top/uma/store/memory"
)

// within fails the test if ch does not receive in time.
func within[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		panic("unreachable")
	}
}

// runHost runs comps under host.Run and returns a stop function that cancels
// the parent and returns Run's result.
func runHost(t *testing.T, o host.Options, comps ...host.Component) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx, o, comps...) }()
	return func() error { cancel(); return within(t, done, "host.Run to return") }
}

func dialFails(t *testing.T, addr string) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err == nil {
		_ = c.Close()
		t.Fatalf("dial %s succeeded after stop, want refused", addr)
	}
}

func get(t *testing.T, url string) string {
	t.Helper()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

func loopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestFuncSetsFields(t *testing.T) {
	var ran, shut atomic.Bool
	c := host.Func("f",
		func(context.Context) error { ran.Store(true); return nil },
		func(context.Context) error { shut.Store(true); return nil })
	if c.Name != "f" || c.Run == nil || c.Shutdown == nil {
		t.Fatalf("component = %+v", c)
	}
	_ = c.Run(t.Context())
	_ = c.Shutdown(t.Context())
	if !ran.Load() || !shut.Load() {
		t.Fatal("Func did not wire run and shutdown")
	}
}

func TestCloserShutdownRunsCloseOnce(t *testing.T) {
	var closes atomic.Int32
	c := host.Closer("streams", func(context.Context) error { closes.Add(1); return nil })
	if err := runHost(t, host.Options{}, c)(); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if n := closes.Load(); n != 1 {
		t.Fatalf("close ran %d times, want once", n)
	}
}

func TestCloserRunReturnsWhenContextEnds(t *testing.T) {
	c := host.Closer("streams", func(context.Context) error { return nil })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before its context ended", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := within(t, done, "Closer.Run"); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
}

func TestCloserErrorIsJoinedAndNamed(t *testing.T) {
	bad := errors.New("close failed")
	c := host.Closer("streams", func(context.Context) error { return bad })
	err := runHost(t, host.Options{}, c)()
	if !errors.Is(err, bad) || !strings.Contains(err.Error(), "component streams: shutdown") {
		t.Fatalf("err = %v, want streams shutdown failure", err)
	}
}

func TestWorkerDrainsOnShutdownAndKeepsRunContextUncancelled(t *testing.T) {
	rt := newFakeRuntime()
	boom := errors.New("boom")
	failing := host.Component{Name: "bot", Run: func(context.Context) error {
		<-rt.started
		return boom
	}}
	call := uma.RuntimeCall{Principal: model.Principal{Tenant: "t", Actor: "a"}}
	opts := uma.WorkerOptions{Owner: "host-1", Limit: 3}
	err := host.Run(t.Context(), host.Options{}, host.Worker(rt, call, opts), failing)
	if !errors.Is(err, boom) || errors.Is(err, errDraining) {
		t.Fatalf("err = %v, want only boom", err)
	}
	if _, runs, drains := rt.counts(); runs != 1 || drains != 1 {
		t.Fatalf("runs = %d, drains = %d, want 1 and 1", runs, drains)
	}
	if ctx := rt.runContext(); ctx.Err() != nil {
		t.Fatalf("worker context was cancelled (%v): claimed work would be cut short", ctx.Err())
	}
	if gotCall, gotOpts := rt.lastArgs(); gotCall != call || gotOpts.Owner != "host-1" || gotOpts.Limit != 3 {
		t.Fatalf("Run got %+v %+v, want the call and options given", gotCall, gotOpts)
	}
}

func TestWorkerRunIgnoresParentCancellation(t *testing.T) {
	rt := newFakeRuntime()
	ctx, cancel := context.WithCancel(t.Context())
	c := host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"})
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	within(t, rt.started, "worker start")
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v on cancellation, want it to wait for Drain", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := within(t, done, "Run after Drain"); err != nil {
		t.Fatalf("err = %v, want nil after Drain", err)
	}
}

// Shutdown is called even when the worker's own Run already failed.
func TestWorkerRunFailureIsNamedAndStillDrained(t *testing.T) {
	rt := newFakeRuntime()
	rt.runErr = errors.New("lease lost")
	err := host.Run(t.Context(), host.Options{}, host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"}))
	if !errors.Is(err, rt.runErr) || !strings.Contains(err.Error(), "component worker") {
		t.Fatalf("err = %v, want worker's failure named", err)
	}
	if _, _, drains := rt.counts(); drains != 1 {
		t.Fatalf("drains = %d, want Shutdown to drain once", drains)
	}
}

// Drain is permanent, and the runtime refuses a Run that registers after it.
// When the refusal follows the worker's own Shutdown, the stop beat the
// start: a clean stop.
func TestWorkerShutdownBeforeRunIsACleanStop(t *testing.T) {
	rt := newFakeRuntime()
	c := host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"})
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := c.Run(t.Context()); err != nil {
		t.Fatalf("err = %v, want nil: the refusal is the stop", err)
	}
	if _, runs, drains := rt.counts(); runs != 1 || drains != 1 {
		t.Fatalf("runs = %d, drains = %d, want 1 and 1", runs, drains)
	}
}

// A drain from elsewhere is not the worker's own stop and still fails Run.
func TestWorkerForeignDrainStillFailsRun(t *testing.T) {
	rt := newFakeRuntime()
	c := host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"})
	if err := rt.Drain(t.Context(), uma.RuntimeCall{}); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(t.Context()); !errors.Is(err, errDraining) {
		t.Fatalf("err = %v, want the draining error", err)
	}
}

// Only the draining refusal is a stop: another failure after Shutdown is
// still a failure.
func TestWorkerOtherFailureAfterShutdownIsReturned(t *testing.T) {
	rt := newFakeRuntime()
	rt.runErr = model.NewError(model.Conflict, "lease lost")
	c := host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"})
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := c.Run(t.Context()); !errors.Is(err, rt.runErr) {
		t.Fatalf("err = %v, want %v", err, rt.runErr)
	}
}

// Only the engine's draining refusal is a stop: any other Unavailable error
// after Shutdown is a failure, even one that is not retryable.
func TestWorkerOtherUnavailableAfterShutdownIsReturned(t *testing.T) {
	for name, runErr := range map[string]error{
		"store unavailable":      model.NewError(model.Unavailable, "store unavailable"),
		"no retry, other reason": &model.Error{Code: model.Unavailable, SafeMessage: "disk full", Retry: model.NoRetry},
	} {
		t.Run(name, func(t *testing.T) {
			rt := newFakeRuntime()
			rt.runErr = runErr
			c := host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"})
			if err := c.Shutdown(t.Context()); err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
			if err := c.Run(t.Context()); !errors.Is(err, runErr) {
				t.Fatalf("err = %v, want %v returned, not swallowed", err, runErr)
			}
		})
	}
}

// The same two outcomes against the real engine, which pins its refusal
// to what the component recognises as its own stop.
func TestWorkerAgainstEngineDrainRefusal(t *testing.T) {
	call := uma.RuntimeCall{Principal: principal}
	opts := uma.WorkerOptions{Owner: "host-1", Limit: 1, Poll: time.Millisecond}

	own := newEngine(t, memory.New(memory.Options{}), 31).Runtime()
	c := host.Worker(own, call, opts)
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := c.Run(t.Context()); err != nil {
		t.Fatalf("Run after own Shutdown = %v, want nil", err)
	}

	foreign := newEngine(t, memory.New(memory.Options{}), 32).Runtime()
	if err := foreign.Drain(t.Context(), call); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if err := host.Worker(foreign, call, opts).Run(t.Context()); err == nil {
		t.Fatal("Run after a foreign Drain = nil, want the refusal")
	}
}

func TestWorkerDrainErrorIsJoinedAndNamed(t *testing.T) {
	rt := newFakeRuntime()
	rt.drainErr = errors.New("drain failed")
	stop := runHost(t, host.Options{}, host.Worker(rt, uma.RuntimeCall{}, uma.WorkerOptions{Owner: "o"}))
	within(t, rt.started, "worker start")
	err := stop()
	if !errors.Is(err, rt.drainErr) || !strings.Contains(err.Error(), "component worker: shutdown") {
		t.Fatalf("err = %v, want worker shutdown failure", err)
	}
}

func TestServerServesAndStopsOnCancel(t *testing.T) {
	ln := loopback(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }), ReadHeaderTimeout: time.Second}
	stop := runHost(t, host.Options{}, host.Server(srv, ln))
	if body := get(t, "http://"+ln.Addr().String()); body != "ok" {
		t.Fatalf("body = %q", body)
	}
	if err := stop(); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	dialFails(t, ln.Addr().String())
}

// Shutdown before Serve is the http.ErrServerClosed path: a clean nil.
func TestServerShutdownBeforeServeIsNil(t *testing.T) {
	c := host.Server(&http.Server{ReadHeaderTimeout: time.Second}, loopback(t))
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := c.Run(t.Context()); err != nil {
		t.Fatalf("Run after Shutdown = %v, want nil", err)
	}
}

func TestServerServeFailureIsReturned(t *testing.T) {
	ln := loopback(t)
	_ = ln.Close() // Serve on a closed listener fails with an accept error, not ErrServerClosed
	c := host.Server(&http.Server{ReadHeaderTimeout: time.Second}, ln)
	if err := c.Run(t.Context()); err == nil {
		t.Fatal("Run on a closed listener = nil, want the accept error")
	}
}

// blockedServer serves a handler that never finishes. inHandler is closed
// when a request is inside it.
func blockedServer(t *testing.T) (srv *http.Server, ln net.Listener, inHandler <-chan struct{}) {
	t.Helper()
	ln = loopback(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	in := make(chan struct{})
	srv = &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(in)
		<-release
	})}
	return srv, ln, in
}

func requestBlocked(t *testing.T, ln net.Listener) {
	t.Helper()
	go func() {
		if resp, err := (&http.Client{Timeout: 10 * time.Second}).Get("http://" + ln.Addr().String()); err == nil {
			_ = resp.Body.Close()
		}
	}()
}

// Server's Shutdown is srv.Shutdown, so it honours the context it is given:
// a request that never finishes ends it with the context's error.
func TestServerShutdownHonoursItsContext(t *testing.T) {
	srv, ln, inHandler := blockedServer(t)
	c := host.Server(srv, ln)
	go func() { _ = c.Run(t.Context()) }()
	requestBlocked(t, ln)
	within(t, inHandler, "request to reach the handler")
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := c.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want the context's deadline", err)
	}
}

// A request that never finishes cannot hold the host past StopTimeout, and
// the server is named in the result.
func TestServerStuckRequestCannotHoldHostPastStopTimeout(t *testing.T) {
	srv, ln, inHandler := blockedServer(t)
	stop := runHost(t, host.Options{StopTimeout: 100 * time.Millisecond}, host.Server(srv, ln))
	requestBlocked(t, ln)
	within(t, inHandler, "request to reach the handler")
	start := time.Now()
	err := stop()
	if time.Since(start) > 3*time.Second {
		t.Fatal("stop waited past the stop timeout")
	}
	if err == nil || !strings.Contains(err.Error(), "server") || !strings.Contains(err.Error(), "shutdown") {
		t.Fatalf("err = %v, want it to name the server's shutdown", err)
	}
}

func TestListenAndServeBindsInRunAndStopsOnCancel(t *testing.T) {
	addrs := make(chan net.Addr, 1)
	c := host.ListenAndServe("127.0.0.1:0",
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("hello")) }),
		func(a net.Addr) { addrs <- a })
	select {
	case a := <-addrs:
		t.Fatalf("onListen called at construction with %v", a)
	default:
	}
	stop := runHost(t, host.Options{}, c)
	addr := within(t, addrs, "onListen").String()
	if body := get(t, "http://"+addr); body != "hello" {
		t.Fatalf("body = %q", body)
	}
	if err := stop(); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	dialFails(t, addr)
}

func TestListenAndServeBindFailureIsReturned(t *testing.T) {
	taken := loopback(t)
	defer func() { _ = taken.Close() }()
	c := host.ListenAndServe(taken.Addr().String(), http.NotFoundHandler(), nil)
	if err := c.Run(t.Context()); err == nil {
		t.Fatal("Run on a taken address = nil, want the bind error")
	}
}

// Shutdown before Run: Run is a clean nil and the port it bound is closed.
func TestListenAndServeShutdownBeforeRunIsNilAndReleasesPort(t *testing.T) {
	addrs := make(chan net.Addr, 1)
	c := host.ListenAndServe("127.0.0.1:0", http.NotFoundHandler(), func(a net.Addr) { addrs <- a })
	if err := c.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := c.Run(t.Context()); err != nil {
		t.Fatalf("Run after Shutdown = %v, want nil", err)
	}
	dialFails(t, within(t, addrs, "onListen").String())
}

// pollClient is a uma.Client whose event poll blocks until its context ends
// and then fails with that context's error, or fails at once with pollErr.
type pollClient struct {
	uma.Client
	pollErr error
}

func (c pollClient) Events() uma.Events { return pollEvents{pollErr: c.pollErr} }

type pollEvents struct {
	uma.Events
	pollErr error
}

func (e pollEvents) Poll(ctx context.Context, _ model.Poll) (model.EventBatch, error) {
	if e.pollErr != nil {
		return model.EventBatch{}, e.pollErr
	}
	<-ctx.Done()
	return model.EventBatch{}, ctx.Err()
}

func newBot(t *testing.T, c pollClient) *bot.Bot {
	t.Helper()
	b, err := bot.New(bot.Config{
		Client: c, Self: "bot",
		Subscription: model.SubscriptionRef{Tenant: "t", ID: "00000000-0000-7000-8000-0000000000f1"},
	})
	if err != nil {
		t.Fatalf("bot.New: %v", err)
	}
	return b
}

func TestBotRunsUntilCancelledAndCancellationIsNil(t *testing.T) {
	c := host.Bot(newBot(t, pollClient{}))
	if c.Name != "bot" || c.Run == nil || c.Shutdown != nil {
		t.Fatalf("component = %+v, want name bot, a Run and no Shutdown", c)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("Run returned %v before cancellation", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := within(t, done, "Bot.Run"); err != nil {
		t.Fatalf("err = %v, want nil on cancellation", err)
	}
}

func TestBotFailureIsReturned(t *testing.T) {
	boom := errors.New("poll failed")
	c := host.Bot(newBot(t, pollClient{pollErr: boom}))
	if err := c.Run(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the poll failure", err)
	}
}
