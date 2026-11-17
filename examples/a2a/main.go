// Command a2a is a runnable A2A host: the a2a.messages adapter serves the
// Agent2Agent protocol over JSON-RPC on one local mux, remote agents
// authenticate with a static bearer token the host owns, and an executor
// loop in the same process does the work through Messages.Reply and
// Tasks.Act.
//
//	go run ./a2a -addr localhost:8080
//
// It prints the curl commands that talk to it. It uses uma's public
// packages only. The memory store is not durable: everything is lost on
// restart.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// demoToken is the bearer token of the one agent the demo knows. A real
// host verifies its own credentials in the Authenticator.
const demoToken = "demo-token"

// demoWait is how long a blocking SendMessage waits for the executor.
const demoWait = 3 * time.Second

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *addr, os.Stdout); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, addr string, out io.Writer) error {
	endpoint := "http://" + addr + mountPath
	h, err := newHost(ctx, options{
		PublicURL: endpoint, Tokens: map[string]string{demoToken: "agent-demo"},
		// "late" takes longer than the wait, so a blocking caller gives up
		// first; it holds the executor for that long.
		BlockingWait: demoWait, LateDelay: demoWait + 2*time.Second,
	})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ran := make(chan error, 1)
	go func() { ran <- h.Run(ctx) }()

	srv := &http.Server{Addr: addr, Handler: h.Handler(), ReadHeaderTimeout: 5 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe() }()
	_, _ = fmt.Fprintf(out, "A2A endpoint %s (memory store: nothing survives a restart)\n\n", endpoint)
	printCommands(out, endpoint, demoToken)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-served:
	}
	cancel()
	runErr := <-ran

	stopCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer done()
	if err := srv.Shutdown(stopCtx); err != nil && serveErr == nil {
		serveErr = err
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, runErr, h.Close(stopCtx))
}
