// Command inbox is a maintained durable inbox host on uma: a website chat
// (native.webwidget) and an email address (smtp.mail) feeding one inbox
// model in PostgreSQL, with an operator view to list conversations, read
// messages, reply and add internal notes.
//
// It shows the host's side of the contract end to end: the host migrates
// and opens the store, composes the engine with RequireDurable, owns the
// HTTP mux, the SMTP port, credentials, workers and shutdown. It uses only
// uma's public packages.
//
// Configuration comes from the environment:
//
//	DATABASE_URL     PostgreSQL connection string (required)
//	WIDGET_KEY       widget session signing key, at least 32 bytes (required)
//	OPERATOR_TOKEN   bearer token of the operator view, at least 16 bytes (required)
//	HTTP_ADDR        HTTP listen address (default 127.0.0.1:8080)
//	SMTP_ADDR        SMTP receiver listen address (default 127.0.0.1:2525)
//	MAIL_ADDRESS     address the receiver accepts (default support@example.com)
//	MAIL_HOSTNAME    receiver greeting name (default localhost)
//	RELAY_HOST       submission relay host (default 127.0.0.1)
//	RELAY_PORT       relay STARTTLS port; unset uses plaintext loopback RELAY_PLAIN_PORT
//	RELAY_PLAIN_PORT relay plaintext port, loopback only (default 1025)
//	RELAY_USER, RELAY_PASSWORD  relay login
//	TENANT           tenant key (default demo)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/model"
)

func main() {
	if err := run(); err != nil {
		slog.Error("inbox host", "err", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is required")
	}
	key := os.Getenv("WIDGET_KEY")
	if len(key) < 32 {
		return errors.New("WIDGET_KEY of at least 32 bytes is required")
	}
	relay, err := relayFromEnv()
	if err != nil {
		return err
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	ln, err := net.Listen("tcp", env("HTTP_ADDR", "127.0.0.1:8080"))
	if err != nil {
		return err
	}

	h, err := Start(ctx, Config{
		Pool:          pool,
		Tenant:        model.TenantID(env("TENANT", "demo")),
		HTTP:          ln,
		Site:          "support",
		MailAddr:      env("SMTP_ADDR", "127.0.0.1:2525"),
		MailHostname:  env("MAIL_HOSTNAME", "localhost"),
		MailAddress:   env("MAIL_ADDRESS", "support@example.com"),
		Relay:         relay,
		OperatorToken: os.Getenv("OPERATOR_TOKEN"),
		Credentials: &Secrets{
			WidgetKey: []byte(key), RelayUser: os.Getenv("RELAY_USER"), RelayPassword: []byte(os.Getenv("RELAY_PASSWORD")),
		},
	})
	if err != nil {
		_ = ln.Close()
		return err
	}
	<-ctx.Done()
	slog.Info("shutting down")
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return h.Shutdown(shutdown)
}

func relayFromEnv() (smtp.Relay, error) {
	r := smtp.Relay{Host: env("RELAY_HOST", "127.0.0.1")}
	if p := os.Getenv("RELAY_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return r, fmt.Errorf("RELAY_PORT: %w", err)
		}
		r.StartTLSPort = n
		return r, nil
	}
	n, err := strconv.Atoi(env("RELAY_PLAIN_PORT", "1025"))
	if err != nil {
		return r, fmt.Errorf("RELAY_PLAIN_PORT: %w", err)
	}
	r.PlainPort, r.AllowPlaintextLoopback = n, true
	return r, nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
