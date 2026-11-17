// Command bot is the quickstart uma bot host: a browser chat widget
// (native.webwidget) as the channel, the bot companion answering visitor
// messages and the interactions of a /help command and a Help button, and
// the in-memory store, served on one local HTTP address.
//
//	go run ./bot -addr localhost:8080
//
// It composes the parts with the host package and uses uma's public packages
// only. See ../botexplicit for the same bot wired by hand. The memory store
// is not durable: everything is lost on restart.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *addr); err != nil {
		log.Fatal(err)
	}
}

func serve(ctx context.Context, addr string) error {
	// The application owns the listener and the address it binds.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	a, err := newApp(ctx, options{})
	if err != nil {
		return errors.Join(err, ln.Close())
	}
	log.Printf("chat at http://%s/ (memory store: nothing survives a restart)", ln.Addr())
	return a.run(ctx, ln)
}
