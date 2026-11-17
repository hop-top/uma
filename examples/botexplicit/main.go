// Command botexplicit is a runnable uma bot host: a browser chat widget
// (native.webwidget) as the channel, the bot companion answering visitor
// messages and the interactions of a /help command and a Help button, and
// the in-memory store, served on one local HTTP mux.
//
//	go run ./botexplicit -addr localhost:8080
//
// It wires every part by hand, to show what a host owns, and uses uma's
// public packages only. The memory store is not durable: everything is lost
// on restart.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *addr); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, addr string) error {
	// The application composes everything the host needs; demoOptions{} is
	// the example as shipped.
	d, err := newDemo(ctx, demoOptions{})
	if err != nil {
		return err
	}
	// The application owns the listener and the address it binds.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		stopCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer done()
		d.widget.Close()
		return errors.Join(err, d.Close(stopCtx))
	}
	log.Printf("chat at http://%s/ (memory store: nothing survives a restart)", ln.Addr())
	return serve(ctx, d, ln)
}

// serve is the supervisor: the application owns it, not uma or the bot
// companion. It runs d and serves it on ln until ctx ends or either part
// stops: the bot and delivery worker, or the HTTP server. The first to stop takes
// the other down, so a dead bot never leaves a chat page answering nobody.
// It closes ln and d.
func serve(ctx context.Context, d *demo, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- d.Run(ctx) }()

	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	var serveErr, runErr error
	select {
	case <-ctx.Done():
		runErr = <-ran
	case serveErr = <-served:
		cancel()
		runErr = <-ran
	case runErr = <-ran:
	}

	// Shut down within a bound: end the event streams first, or
	// Shutdown waits on them.
	stopCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	d.widget.Close()
	if err := srv.Shutdown(stopCtx); err != nil && serveErr == nil {
		serveErr = err
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, runErr, d.Close(stopCtx))
}
