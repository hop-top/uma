// Command triage is a mailbox triage loop on uma's provider conversation
// state. It follows a Gmail mailbox (google.mail) and an IMAP mailbox
// (imap.mail), fetches their mail into conversations and, on a timer,
// applies one rule to each: junk is trashed, mail asking for attention is
// flagged, bulk mail is archived. Every change is made at the provider, in
// the account's own view, and never touches uma's conversation status or
// labels.
//
// Configuration comes from the environment; a mailbox is followed when its
// address is set:
//
//	GMAIL_ADDRESS        Gmail mailbox address
//	GMAIL_ACCESS_TOKEN   OAuth2 access token with gmail.modify (renew it outside)
//	IMAP_ADDRESS         IMAP mailbox address
//	IMAP_HOST            IMAP server host (implicit TLS on 993)
//	IMAP_USER            login, default IMAP_ADDRESS
//	IMAP_PASSWORD        password or app password
//	TENANT               tenant key (default demo)
//	TRIAGE_INTERVAL      time between passes (default 5m)
//	TRIAGE_WARMUP        wait before the first pass, for the first fetch (default 15s)
//	TRIAGE_ONCE          set to run one pass and exit
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/google"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/model"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "triage:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	secrets := secretsFromEnv()
	boxes := mailboxesFromEnv()
	if len(boxes) == 0 {
		return errors.New("set GMAIL_ADDRESS or IMAP_ADDRESS")
	}
	h, err := Start(ctx, Config{
		Tenant:      model.TenantID(env("TENANT", "demo")),
		Mailboxes:   boxes,
		Adapters:    []adapter.Factory{google.NewFactory(google.Options{}), imap.NewFactory(imap.Options{})},
		Credentials: secrets,
		HTTP:        &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		return err
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Close(shutdown); err != nil {
			slog.Error("close", "err", err)
		}
	}()

	interval, warmup := duration("TRIAGE_INTERVAL", 5*time.Minute), duration("TRIAGE_WARMUP", 15*time.Second)
	wait := warmup
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		outcomes, err := h.Pass(ctx)
		for _, o := range outcomes {
			slog.Info("triage", "mailbox", o.Mailbox, "conversation", o.Action.Conversation.ID,
				"verb", o.Action.Verb, "kind", o.Action.Kind, "state", o.State, "failure", o.Failure)
		}
		if err != nil {
			slog.Error("pass", "err", err)
		}
		if os.Getenv("TRIAGE_ONCE") != "" {
			return err
		}
		wait = interval
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func duration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil && d > 0 {
		return d
	}
	return def
}
