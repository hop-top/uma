package smtp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"slices"
	"sync"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	mailstage "hop.top/uma/adapter/mail/stage"
	"hop.top/uma/model"
)

// Receiver timing.
const (
	receiverIOTimeout = 5 * time.Minute
	sinkTimeout       = 2 * time.Minute
)

// Run serves the MX receiver on the host's listener until ctx ends. It
// returns ctx's error then, and a retryable error when serving fails.
func (i *Instance) Run(ctx context.Context, sink adapter.Sink) error {
	if i.closed.Load() {
		return i.errClosed()
	}
	if i.opts.Listen == nil {
		return &model.Error{Code: model.Unavailable, SafeMessage: "smtp.mail: no listener configured", Retry: model.NoRetry}
	}
	ln, err := i.opts.Listen(ctx, i.ch)
	if err != nil {
		return model.NewError(model.Unavailable, "smtp.mail: listener unavailable")
	}
	be := &backend{i: i, sink: sink, ctx: ctx}
	srv := i.server(be)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	// Close drops open connections: a transaction cut before 250 stays with
	// the sending MTA, which retries. Deliveries already handed to the sink
	// finish before Run returns.
	select {
	case <-ctx.Done():
		_ = srv.Close()
		<-served
		be.stop()
		return ctx.Err()
	case <-served:
		_ = srv.Close()
		be.stop()
		return model.NewError(model.Unavailable, "smtp.mail: receiver stopped serving")
	}
}

func (i *Instance) server(be *backend) *gosmtp.Server {
	srv := gosmtp.NewServer(be)
	srv.Domain = i.cfg.Hostname
	srv.MaxMessageBytes = i.cfg.maxMessageBytes()
	srv.MaxRecipients = i.cfg.maxRecipients()
	srv.ReadTimeout = receiverIOTimeout
	srv.WriteTimeout = receiverIOTimeout
	srv.TLSConfig = i.opts.TLS
	srv.ErrorLog = log.New(io.Discard, "", 0)
	return srv
}

type backend struct {
	i        *Instance
	sink     adapter.Sink
	ctx      context.Context
	mu       sync.Mutex
	stopping bool
	wg       sync.WaitGroup
}

// begin registers one delivery; false once the receiver is stopping.
func (b *backend) begin() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopping {
		return false
	}
	b.wg.Add(1)
	return true
}

// stop refuses new deliveries and waits for the ones in flight.
func (b *backend) stop() {
	b.mu.Lock()
	b.stopping = true
	b.mu.Unlock()
	b.wg.Wait()
}

func (b *backend) NewSession(c *gosmtp.Conn) (gosmtp.Session, error) {
	return &session{b: b, conn: c}, nil
}

// session is one SMTP connection. It implements no AUTH: an MX takes mail
// from anyone and grades the sender by evidence.
type session struct {
	b        *backend
	conn     *gosmtp.Conn
	mailFrom string
	rcpts    []rcpt
}

type rcpt struct{ address, destination string }

var (
	errNoRecipient = &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "No such recipient here"}
	errTooBig      = &gosmtp.SMTPError{Code: 552, EnhancedCode: gosmtp.EnhancedCode{5, 3, 4}, Message: "Message too big"}
	errMalformed   = &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 6, 0}, Message: "Malformed message"}
	errNoFrom      = &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 7}, Message: "A valid From address is required"}
	errUnbound     = &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 1, 1}, Message: "Mailbox unavailable"}
	errDisabled    = &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 2, 1}, Message: "Mailbox disabled"}
	errTryLater    = &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "Temporary failure, try again later"}
	errShutdown    = &gosmtp.SMTPError{Code: 421, EnhancedCode: gosmtp.EnhancedCode{4, 3, 2}, Message: "Service shutting down"}
)

func (s *session) Mail(from string, opts *gosmtp.MailOptions) error {
	if opts != nil && opts.Size > s.b.i.cfg.maxMessageBytes() {
		return errTooBig
	}
	s.mailFrom, s.rcpts = from, nil
	return nil
}

// Rcpt accepts configured recipients only.
func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	dest, ok := s.b.i.cfg.destination(to)
	if !ok {
		return errNoRecipient
	}
	if !slices.ContainsFunc(s.rcpts, func(r rcpt) bool { return r.address == to }) {
		s.rcpts = append(s.rcpts, rcpt{address: to, destination: dest})
	}
	return nil
}

// Data reads the message (the server bounds its size) and hands it to the
// sink once per destination; 250 only when every destination accepted.
func (s *session) Data(r io.Reader) error {
	raw, err := io.ReadAll(r)
	if err != nil {
		if errors.Is(err, gosmtp.ErrDataTooLarge) {
			return errTooBig
		}
		return err
	}
	if !s.b.begin() {
		return errShutdown
	}
	defer s.b.wg.Done()
	return s.deliver(raw)
}

func (s *session) Reset() { s.mailFrom, s.rcpts = "", nil }

func (s *session) Logout() error { return nil }

func (s *session) deliver(raw []byte) error {
	i := s.b.i
	ctx := s.b.ctx
	if ctx.Err() != nil {
		return errShutdown
	}
	msg, err := mime.Parse(bytes.NewReader(raw), mailstage.Keep(mime.Limits{MaxTextBytes: model.DefaultContentLimits().MaxTextBytes}))
	if err != nil {
		if errors.Is(err, mime.ErrMalformed) {
			return errMalformed
		}
		return errTryLater
	}
	if len(msg.Header.From) == 0 {
		return errNoFrom
	}
	sender, err := model.NormalizeEmail(msg.Header.From[0].Address)
	if err != nil {
		return errNoFrom
	}
	ev := evaluate(ctx, i.opts.Resolver, evidenceInput{
		ip: remoteIP(s.conn), helo: s.conn.Hostname(), mailFrom: s.mailFrom,
		from: msg.Header.From, raw: raw, arc: arcFields(msg.Header),
	})
	_, tlsOn := s.conn.TLSConnectionState()
	tx := transaction{
		mailFrom: s.mailFrom, helo: s.conn.Hostname(), clientIP: remoteIP(s.conn), tls: tlsOn, rcpts: s.rcpts,
		sender: sender, raw: raw, msg: msg, ev: ev,
	}
	for _, dest := range destinations(s.rcpts) {
		if err := s.deliverTo(ctx, i, tx, dest); err != nil {
			return err
		}
	}
	return nil
}

// deliverTo hands the message to the sink for one destination: its
// attachments are staged first, in the destination's inbox, and named in
// the content next to their descriptors. A sink that cannot stage, or has no
// blob store, leaves them described only.
func (s *session) deliverTo(ctx context.Context, i *Instance, tx transaction, dest string) error {
	// Staging marks the attachments it staged: a destination works on its
	// own copy.
	m := *tx.msg
	m.Attachments = slices.Clone(m.Attachments)
	tx.msg = &m
	in, err := i.ingress(tx, dest)
	if err != nil {
		return errMalformed
	}
	actx, cancel := context.WithTimeout(ctx, sinkTimeout)
	defer cancel()
	parts, err := mailstage.Stage(actx, s.b.sink, mailstage.Request{Key: in.Envelope.EventID, Destination: dest, Audience: in.Envelope.Audience}, &m)
	if err != nil {
		return refusal(ctx, err)
	}
	if len(parts) > 0 {
		c, err := content(&m)
		if err != nil {
			return errMalformed
		}
		in.Envelope.Content = c
		in.Envelope.Content.Parts = append(in.Envelope.Content.Parts, parts...)
		if err := in.Envelope.Content.Validate(); err != nil {
			return errMalformed
		}
	}
	if _, err := s.b.sink.Accept(actx, in); err != nil {
		return refusal(ctx, err)
	}
	return nil
}

// refusal maps a sink failure to an SMTP reply: a destination bound to no
// inbox or a disabled one is permanent; anything else is temporary, so the
// sending MTA keeps the message.
func refusal(ctx context.Context, err error) error {
	switch {
	case ctx.Err() != nil:
		return errShutdown
	case model.IsCode(err, model.NotFound):
		return errUnbound
	case model.IsCode(err, model.Disabled):
		return errDisabled
	}
	return errTryLater
}

// destinations returns each recipient destination once, in RCPT order.
func destinations(rs []rcpt) []string {
	var out []string
	for _, r := range rs {
		if !slices.Contains(out, r.destination) {
			out = append(out, r.destination)
		}
	}
	return out
}

func remoteIP(c *gosmtp.Conn) net.IP {
	if a, ok := c.Conn().RemoteAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	host, _, err := net.SplitHostPort(c.Conn().RemoteAddr().String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// arcFields returns the ARC header fields in order, unverified.
func arcFields(h mime.Header) []mime.Field {
	var out []mime.Field
	for _, f := range h.Fields {
		switch {
		case equalFold(f.Key, "ARC-Seal"), equalFold(f.Key, "ARC-Message-Signature"), equalFold(f.Key, "ARC-Authentication-Results"):
			out = append(out, f)
		}
	}
	return out
}
