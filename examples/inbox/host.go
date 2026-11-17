package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/adapter/web/widget/storelog"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store/postgres"
)

// Actors of the host. The host, not uma, decides who they are: the service
// actor runs the receivers and workers, the setup actor configures channels
// and inboxes, and operators are whoever presents an operator token.
const (
	serviceActor  model.ActorID = "inbox-service"
	setupActor    model.ActorID = "inbox-setup"
	operatorActor model.ActorID = "operator"
)

// Config is everything the host needs. Every value is owned by the caller:
// Start never closes Pool, and Shutdown closes only the HTTP listener.
type Config struct {
	// Pool is the database. Start migrates it, then opens the store on it.
	Pool *pgxpool.Pool
	// Tenant is the one tenant this host serves.
	Tenant model.TenantID
	// HTTP serves the widget receiver and event stream, the operator view
	// and the demo page.
	HTTP net.Listener
	// Site is the widget site visitors chat on.
	Site string
	// MailAddr is the SMTP receiver's listen address, such as ":2525".
	MailAddr string
	// MailHostname is the receiver's greeting name.
	MailHostname string
	// MailAddress is the address the receiver accepts mail for; replies are
	// sent from it.
	MailAddress string
	// MailResolver answers sender evidence lookups; nil uses the system
	// resolver.
	MailResolver smtp.Resolver
	// Relay is the submission server of email replies.
	Relay smtp.Relay
	// Credentials resolves the widget session key and the relay login.
	Credentials ports.Credentials
	// OperatorToken is the bearer token of the operator view.
	OperatorToken string
	// WidgetLog keeps visitor event logs; nil keeps them in the uma store.
	WidgetLog widget.Log
	// Workers is the number of Runtime.Run loops; zero runs two.
	Workers int
	// Retry bounds provider attempts; zero fields take uma's defaults.
	Retry uma.RetryPolicy
	// Poll is the idle wait of workers and of the standby mail receiver;
	// zero waits one second.
	Poll time.Duration
	// Logger receives host events; nil uses slog.Default.
	Logger *slog.Logger
}

// Host is one running inbox process.
type Host struct {
	cfg     Config
	log     *slog.Logger
	engine  uma.Engine
	widget  *widget.Factory
	server  *http.Server
	service uma.RuntimeCall
	ids     ports.IDs
	// instance names this host in worker lease owners.
	instance string

	widgetChannel, mailChannel model.ChannelRef
	widgetInbox, mailInbox     model.InboxRef

	// quit is closed when Shutdown starts; the restart loops stop on it.
	quit chan struct{}
	// abort cancels the background loops when Shutdown runs out of time.
	abort context.CancelFunc
	loops sync.WaitGroup
	serve chan error

	mu       sync.Mutex
	mailBind string
}

// Start migrates the database, composes the engine, configures the widget
// and mail channels and their inboxes, mounts the HTTP surfaces and starts
// the workers and the mail receiver. ctx bounds startup only; the host runs
// until Shutdown.
func Start(ctx context.Context, cfg Config) (*Host, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	if cfg.Poll == 0 {
		cfg.Poll = time.Second
	}
	h := &Host{
		cfg: cfg, log: cfg.Logger, quit: make(chan struct{}), serve: make(chan error, 1),
		service: uma.RuntimeCall{
			Principal: model.Principal{Tenant: cfg.Tenant, Actor: serviceActor},
			Call:      model.Call{Correlation: "inbox-host"},
		},
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	h.ids = ports.NewUUIDv7(ports.SystemClock{})
	id, err := h.ids.New(ctx)
	if err != nil {
		return nil, err
	}
	h.instance = string(id)

	// The host owns migrations; Open never migrates. The visitor logs live
	// in uma's schema, so one migration covers them.
	if err := postgres.Migrate(ctx, cfg.Pool); err != nil {
		return nil, err
	}
	st, err := postgres.Open(ctx, cfg.Pool, postgres.Options{})
	if err != nil {
		return nil, err
	}

	// Visitor logs are recipient logs of the uma store, so erasing a contact
	// (Privacy.Redact with Cascade) deletes its visitors' logs in the same
	// unit; a log kept outside the store would survive the erasure.
	visitors := cfg.WidgetLog
	if visitors == nil {
		if visitors, err = storelog.New(st, storelog.Options{}); err != nil {
			return nil, err
		}
	}
	site := cfg.Site
	h.widget, err = widget.New(widget.Options{Log: visitors, Verifier: widget.VerifierFunc(
		// Every visitor is anonymous on the one site. A host with its own
		// login returns AssuranceVerified and the user's ID here instead.
		func(*http.Request, model.ChannelRef) (widget.Visitor, error) {
			return widget.Visitor{Assurance: widget.AssuranceAnonymous, Site: site}, nil
		})})
	if err != nil {
		return nil, err
	}
	mail := smtp.NewFactory(smtp.Options{Listen: h.listenMail, Resolver: cfg.MailResolver})

	h.engine, err = uma.New(uma.Config{
		Store:          st,
		Authorizer:     roles{tenant: cfg.Tenant},
		Adapters:       []adapter.Factory{h.widget, mail},
		Credentials:    cfg.Credentials,
		Clock:          ports.SystemClock{},
		IDs:            h.ids,
		Retry:          cfg.Retry,
		RequireDurable: true,
	})
	if err != nil {
		h.widget.Close()
		return nil, err
	}
	if err := h.setup(ctx); err != nil {
		return nil, h.abandon(err)
	}
	webhook, err := h.engine.Runtime().Webhook(ctx, h.service, h.widgetChannel)
	if err != nil {
		return nil, h.abandon(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/widget/", webhook)
	mux.Handle("GET /widget/events", h.widget.Events(h.widgetChannel))
	mux.Handle("/operator/", h.operatorAPI())
	mux.HandleFunc("GET /{$}", servePage)
	h.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	run, abort := context.WithCancel(context.WithoutCancel(ctx)) // #nosec G118 -- Shutdown calls h.abort
	h.abort = abort
	for i := range cfg.Workers {
		h.loops.Add(1)
		go h.work(run, i)
	}
	h.loops.Add(1)
	go h.receiveMail(run)
	go func() { h.serve <- h.server.Serve(cfg.HTTP) }()
	h.log.Info("inbox host started", "http", cfg.HTTP.Addr().String(),
		"widget_inbox", h.widgetInbox.ID, "mail_inbox", h.mailInbox.ID)
	return h, nil
}

func (c Config) validate() error {
	switch {
	case c.Pool == nil:
		return errors.New("config: pool required")
	case c.Tenant.Validate() != nil:
		return errors.New("config: tenant invalid")
	case c.HTTP == nil:
		return errors.New("config: http listener required")
	case c.Site == "" || c.MailAddr == "" || c.MailHostname == "" || c.MailAddress == "":
		return errors.New("config: site, mail address, hostname and listen address required")
	case c.Credentials == nil:
		return errors.New("config: credentials required")
	case len(c.OperatorToken) < 16:
		return errors.New("config: operator token of at least 16 bytes required")
	}
	return nil
}

// abandon releases what Start built before it failed.
func (h *Host) abandon(err error) error {
	h.widget.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(err, h.engine.Close(ctx))
}

// Shutdown stops the host in order: event streams end, the HTTP server
// finishes in-flight requests, Drain lets workers finish the work they hold
// and stops the mail receiver, then the engine closes. Work that cannot
// finish before ctx ends keeps its lease until expiry and is picked up by
// the next host; nothing accepted is lost. Call it once.
func (h *Host) Shutdown(ctx context.Context) error {
	close(h.quit)
	// Event streams are long-lived; ending them first lets the HTTP server
	// shut down without waiting on browsers.
	h.widget.Close()
	errs := []error{h.server.Shutdown(ctx)}
	if err := <-h.serve; !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, err)
	}
	errs = append(errs, h.engine.Runtime().Drain(ctx, h.service))
	stopped := make(chan struct{})
	go func() { h.loops.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	// Past the deadline, cancel whatever still runs; its claims expire.
	h.abort()
	<-stopped
	errs = append(errs, h.engine.Close(ctx))
	return errors.Join(errs...)
}

// work runs one worker until the tenant drains, restarting it after an
// unexpected failure.
func (h *Host) work(ctx context.Context, n int) {
	defer h.loops.Done()
	owner := "inbox-host/" + h.instance + "/worker-" + strconv.Itoa(n)
	for {
		err := h.engine.Runtime().Run(ctx, h.service, uma.WorkerOptions{Owner: owner, Limit: 8, Poll: h.cfg.Poll})
		if !h.retry(ctx, err) {
			return
		}
		h.log.Warn("worker stopped; restarting", "owner", owner, "err", err)
	}
}

// receiveMail runs the SMTP receiver. One Receive owns the channel across
// every process sharing the database: while another host owns it this one
// stands by and takes over once that host stops.
func (h *Host) receiveMail(ctx context.Context) {
	defer h.loops.Done()
	for {
		err := h.engine.Runtime().Receive(ctx, h.service, h.mailChannel)
		if !h.retry(ctx, err) {
			return
		}
		if model.IsCode(err, model.Conflict) {
			h.log.Info("mail receiver owned by another host; standing by")
		} else {
			h.log.Warn("mail receiver stopped; restarting", "err", err)
		}
	}
}

// retry reports whether a loop should run again after err, waiting one
// poll interval first. A nil error is a drain or close.
func (h *Host) retry(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil {
		return false
	}
	select {
	case <-h.quit:
		return false
	case <-ctx.Done():
		return false
	case <-time.After(h.cfg.Poll):
		return true
	}
}

// listenMail is the smtp.mail Listen hook: the host binds the port.
func (h *Host) listenMail(ctx context.Context, _ model.Channel) (net.Listener, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", h.cfg.MailAddr)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.mailBind = ln.Addr().String()
	h.mu.Unlock()
	return ln, nil
}

// MailAddr returns the address the mail receiver last listened on, empty
// before it first started.
func (h *Host) MailAddr() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.mailBind
}
