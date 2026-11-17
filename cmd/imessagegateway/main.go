// Command imessagegateway runs the reference iMessage gateway for the
// relay.messages adapter: uma's callback and lookup in front of a Blooio or
// BlueBubbles backend. See the module README for the flags, the environment
// and how to wire the uma side.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	gateway "hop.top/uma/gateway/imessage"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "imessagegateway:", err)
		os.Exit(1)
	}
}

// options are the parsed flags; secrets are read from the environment or
// from files, never from flags, so they stay out of process listings.
type options struct {
	addr, backend, data, umaURL, callbackHeader string
	retention                                   time.Duration
	blooioBase, blooioFrom                      string
	bbURL, bbAccount                            string
	files                                       map[string]*string
}

// secretVars name each secret's environment variable; the -<name>-file flag
// reads it from a file instead.
var secretVars = map[string]string{ // #nosec G101 -- variable names, not values
	"callback-secret":       "IMESSAGE_GATEWAY_CALLBACK_SECRET",
	"webhook-secret":        "IMESSAGE_GATEWAY_WEBHOOK_SECRET",
	"webhook-token":         "IMESSAGE_GATEWAY_WEBHOOK_TOKEN",
	"blooio-api-key":        "BLOOIO_API_KEY",
	"blooio-webhook-secret": "BLOOIO_WEBHOOK_SECRET",
	"bluebubbles-password":  "BLUEBUBBLES_PASSWORD",
}

func parse(args []string, getenv func(string) string) (options, error) {
	env := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	fs := flag.NewFlagSet("imessagegateway", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := options{files: map[string]*string{}}
	fs.StringVar(&o.addr, "addr", env("IMESSAGE_GATEWAY_ADDR", "127.0.0.1:8787"), "listen address")
	fs.StringVar(&o.backend, "backend", env("IMESSAGE_GATEWAY_BACKEND", ""), "blooio or bluebubbles")
	fs.StringVar(&o.data, "data", env("IMESSAGE_GATEWAY_DATA", "imessagegateway-data"), "directory of the Idempotency-Key log")
	fs.DurationVar(&o.retention, "retention", gateway.DefaultRetention, "how long a key is kept after its last change")
	fs.StringVar(&o.umaURL, "uma-webhook-url", env("IMESSAGE_GATEWAY_UMA_WEBHOOK_URL", ""), "uma's webhook URL for the channel")
	fs.StringVar(&o.callbackHeader, "callback-header", env("IMESSAGE_GATEWAY_CALLBACK_HEADER", ""), "header carrying the callback secret (channel callback.auth \"header\"); empty: Authorization: Bearer")
	fs.StringVar(&o.blooioBase, "blooio-base-url", env("BLOOIO_BASE_URL", gateway.BlooioBaseURL), "Blooio v4 API base URL")
	fs.StringVar(&o.blooioFrom, "blooio-from", env("BLOOIO_FROM", ""), "Blooio sender number, E.164")
	fs.StringVar(&o.bbURL, "bluebubbles-url", env("BLUEBUBBLES_URL", ""), "BlueBubbles Server URL")
	fs.StringVar(&o.bbAccount, "bluebubbles-account", env("BLUEBUBBLES_ACCOUNT", ""), "Apple ID address the Mac sends from")
	for name, v := range secretVars {
		o.files[name] = fs.String(name+"-file", "", "file holding the "+name+" (else $"+v+")")
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, errors.New("unexpected arguments: " + strings.Join(fs.Args(), " "))
	}
	return o, nil
}

// secret reads a secret from its file flag, else its environment variable.
func (o options) secret(name string, getenv func(string) string) (string, error) {
	if f := *o.files[name]; f != "" {
		b, err := os.ReadFile(f) // #nosec G304 -- operator-named secret file
		if err != nil {
			return "", fmt.Errorf("%s file: %w", name, err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	return getenv(secretVars[name]), nil
}

func run(ctx context.Context, args []string, getenv func(string) string, logw io.Writer) error {
	o, err := parse(args, getenv)
	if err != nil {
		return err
	}
	sec := map[string]string{}
	for name := range secretVars {
		if sec[name], err = o.secret(name, getenv); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewTextHandler(logw, nil))
	client := &http.Client{
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var be gateway.Backend
	switch o.backend {
	case "blooio":
		be, err = gateway.NewBlooio(gateway.BlooioOptions{
			BaseURL: o.blooioBase, APIKey: sec["blooio-api-key"], From: o.blooioFrom,
			WebhookSecret: []byte(sec["blooio-webhook-secret"]), Client: client,
		})
	case "bluebubbles":
		be, err = gateway.NewBlueBubbles(gateway.BlueBubblesOptions{
			URL: o.bbURL, Password: sec["bluebubbles-password"], Account: o.bbAccount, Client: client,
		})
	default:
		return errors.New("-backend must be blooio or bluebubbles")
	}
	if err != nil {
		return err
	}
	keys, err := gateway.OpenKeyStore(o.data, o.retention, nil)
	if err != nil {
		return err
	}
	defer func() { _ = keys.Close() }()
	gw, err := gateway.New(gateway.Config{
		Backend: be, Keys: keys,
		CallbackSecret: []byte(sec["callback-secret"]), CallbackHeader: o.callbackHeader,
		WebhookURL: o.umaURL, WebhookSecret: []byte(sec["webhook-secret"]), WebhookToken: sec["webhook-token"],
		Logger: log,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr: o.addr, Handler: gw.Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", o.addr, "backend", be.Name())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shut)
}
