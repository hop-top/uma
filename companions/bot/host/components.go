package host

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// Func adapts plain functions to a Component.
func Func(name string, run, shutdown func(context.Context) error) Component {
	return Component{Name: name, Run: run, Shutdown: shutdown}
}

// Bot runs the dispatcher until its context ends. The cancellation that
// ends it is a clean stop; any other error is returned.
func Bot(b *bot.Bot) Component {
	return Component{Name: "bot", Run: func(ctx context.Context) error {
		if err := b.Run(ctx); !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}}
}

// Worker runs uma's delivery worker. Its Run does not stop on cancellation,
// so work already claimed finishes; Shutdown drains the runtime, within the
// stop bound, and that is what ends Run.
//
// Drain is permanent for the tenant on that runtime: it is never undone, and
// the runtime refuses a Run that registers after it with an Unavailable
// error. When that refusal follows this component's own Shutdown, the stop
// beat the start (a signal during startup, or another component failing at
// once) and Run returns nil. A drain issued from elsewhere still fails Run,
// as does any other error. Because Run always calls Shutdown, a worker whose
// own Run failed early is drained as well.
//
// The component remembers its own Shutdown, so build a Worker for each Run
// of a host: a second run on a drained tenant must report the refusal, unless
// that run is itself stopped before its worker starts.
func Worker(rt uma.Runtime, call uma.RuntimeCall, o uma.WorkerOptions) Component {
	var drained atomic.Bool
	return Component{
		Name: "worker",
		Run: func(ctx context.Context) error {
			err := rt.Run(context.WithoutCancel(ctx), call, o)
			if err != nil && drained.Load() && isDrainingRefusal(err) {
				return nil
			}
			return err
		},
		Shutdown: func(ctx context.Context) error {
			drained.Store(true) // before Drain, so a refusal it causes sees the flag
			return rt.Drain(ctx, call)
		},
	}
}

// isDrainingRefusal reports whether err is the refusal the runtime gives a
// Run that registers on a drained tenant: Unavailable, not retryable, with
// the engine's message. A different Unavailable error is a real failure.
func isDrainingRefusal(err error) bool {
	var me *model.Error
	return errors.As(err, &me) && me.Code == model.Unavailable &&
		me.Retry == model.NoRetry && me.SafeMessage == "runtime draining"
}

// Server serves srv on ln, which the application bound and owns. Shutdown
// is srv.Shutdown, so it honours the stop deadline; a Shutdown that came
// before Run makes Serve return http.ErrServerClosed at once, a clean stop.
func Server(srv *http.Server, ln net.Listener) Component {
	return Component{
		Name: "server",
		Run: func(context.Context) error {
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
		Shutdown: srv.Shutdown,
	}
}

// Closer is a component that only needs closing at shutdown, such as a
// widget's event streams, which must end before the server can drain. It
// runs until its context ends. Register it after the server so it closes
// first.
func Closer(name string, closeFn func(ctx context.Context) error) Component {
	return Component{
		Name:     name,
		Run:      func(ctx context.Context) error { <-ctx.Done(); return nil },
		Shutdown: closeFn,
	}
}

// ListenAndServe binds addr when it runs, not when it is constructed, and
// serves h until stopped. onListen, if set, receives the bound address.
func ListenAndServe(addr string, h http.Handler, onListen func(net.Addr)) Component {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	return Component{
		Name: "server",
		Run: func(context.Context) error {
			ln, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			if onListen != nil {
				onListen(ln.Addr())
			}
			if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
		Shutdown: srv.Shutdown,
	}
}
