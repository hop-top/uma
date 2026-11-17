package smtp_test

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/adapter/mail/smtp"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// TestAdapterConformance runs the shared adapter suite against smtp.mail,
// with an in-process relay per account. Fixture senders and destinations
// are shaped as addresses and message IDs as msg-ids; a delivered event is
// a mail the provider's MTA hands to the adapter's MX once its receiver
// listens, and again on each later listen until the MX accepts it.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			p := &provider{t: t, relays: map[model.ChannelRef]*relay{}, mtas: map[model.ChannelRef]*mtaQueue{}}
			inner := smtp.NewFactory(smtp.Options{
				Listen: func(_ context.Context, ch model.Channel) (net.Listener, error) {
					ln, err := net.Listen("tcp", "127.0.0.1:0")
					if err == nil {
						p.mta(ch.Ref).listening(ln.Addr().String())
					}
					return ln, err
				},
				Resolver: newZone(),
				RootCAs:  certs(t).roots,
			})
			return &observedFactory{Factory: inner, p: p}, p
		},
		Capabilities: []model.Capability{model.CapabilityReceive, model.CapabilitySend, model.CapabilityStartConversation, model.CapabilityReplyRecipients},
	})
}

// provider is the relay side of every account. A relay sees messages, not
// engine operations, so the factory wrapper tells it which effect the
// adapter is serving when a transaction starts.
type provider struct {
	t      *testing.T
	mu     sync.Mutex
	relays map[model.ChannelRef]*relay
	// calls maps relay transactions to the request the adapter served.
	calls map[model.ChannelRef][]callRecord
	// mtas deliver each account's inbound mail; events maps a delivered
	// Message-ID to its event ID, and results are what the sink answered.
	mtas    map[model.ChannelRef]*mtaQueue
	events  map[model.ChannelRef]map[string]string
	results map[model.ChannelRef][]conformance.InboundResult
}

func (p *provider) mta(ch model.ChannelRef) *mtaQueue {
	p.mu.Lock()
	defer p.mu.Unlock()
	q, ok := p.mtas[ch]
	if !ok {
		q = &mtaQueue{}
		p.mtas[ch] = q
	}
	return q
}

// ShapeID gives senders and destinations mailbox addresses (destinations
// on the account's domain) and message IDs the msg-id form.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return id + "@peers.test"
	case conformance.FixtureDestination:
		return id + "@" + domain(ch)
	case conformance.FixtureMessage:
		return "<" + id + "@peers.test>"
	}
	return id
}

type callRecord struct {
	req adapter.SendRequest
	tx  int
}

func (p *provider) relay(ch model.ChannelRef) *relay {
	p.mu.Lock()
	defer p.mu.Unlock()
	r, ok := p.relays[ch]
	if !ok {
		r = newRelay(p.t)
		r.anyCredential = true
		p.relays[ch] = r
	}
	return r
}

func domain(ch model.ChannelRef) string {
	return "acct-" + strings.ToLower(string(ch.ID[len(ch.ID)-4:])) + ".test"
}

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := smtp.Config{
		Hostname: "mx." + domain(ch),
		Recipients: []smtp.Recipient{
			{Address: "support@" + domain(ch), Destination: "support@" + domain(ch)},
			{Address: "only-b@" + domain(ch), Destination: "only-b@" + domain(ch)},
		},
		From:  "desk@" + domain(ch),
		Relay: smtp.Relay{Host: "127.0.0.1", StartTLSPort: p.relay(ch).port, CommandTimeoutMS: 5000, DataTimeoutMS: 1000},
	}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "relay-cred"}
}

func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return route(ch, peer)
}

// Deliver queues ev as a mail from its sender to its destination, handed
// to the adapter's MX by the account's MTA. Mail has no private audience;
// claims ride in Reply-To, which never authenticates anyone.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	p.mu.Lock()
	if p.events == nil {
		p.events, p.results = map[model.ChannelRef]map[string]string{}, map[model.ChannelRef][]conformance.InboundResult{}
	}
	if p.events[ch] == nil {
		p.events[ch] = map[string]string{}
	}
	p.events[ch][ev.MessageID] = ev.EventID
	p.mu.Unlock()
	extra := ""
	if len(ev.Claims) > 0 {
		extra = "Reply-To: " + strings.Join(ev.Claims, ", ") + "\n"
	}
	raw := "From: " + ev.Sender + "\nTo: " + ev.Destination + "\nSubject: hello\nMessage-ID: " + ev.MessageID + "\n" + extra + "\n" + ev.Text + "\n"
	p.mta(ch).queue(queued{from: ev.Sender, rcpt: ev.Destination, raw: crlf(raw)})
	return nil
}

// record notes the sink's answer to the delivered message in.
func (p *provider) record(ch model.ChannelRef, in model.Ingress, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev, ok := p.events[ch][in.Envelope.MessageID]; ok {
		p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: ev, Acked: err == nil})
	}
}

func (p *provider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

// queued is one inbound mail waiting for the MX.
type queued struct{ from, rcpt, raw string }

// mtaQueue is the provider's MTA for one account: it hands queued mail to
// the MX at the address it last listened on, in order, keeping a mail the
// MX did not accept for the next listen, as an MTA retries.
type mtaQueue struct {
	mu      sync.Mutex
	addr    string
	pending []queued
	running bool
}

func (q *mtaQueue) listening(addr string) {
	q.mu.Lock()
	q.addr = addr
	q.mu.Unlock()
	q.flush()
}

func (q *mtaQueue) queue(m queued) {
	q.mu.Lock()
	q.pending = append(q.pending, m)
	q.mu.Unlock()
	q.flush()
}

// flush starts one delivery run unless one is running or nothing listens.
func (q *mtaQueue) flush() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.running || q.addr == "" || len(q.pending) == 0 {
		return
	}
	q.running = true
	go q.run()
}

func (q *mtaQueue) run() {
	for {
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			q.mu.Unlock()
			return
		}
		m, addr := q.pending[0], q.addr
		q.mu.Unlock()
		if err := transfer(addr, m); err != nil {
			q.mu.Lock()
			q.running = false
			if q.addr == addr {
				// Retried on the next listen.
				q.addr = ""
			}
			q.mu.Unlock()
			q.flush()
			return
		}
		q.mu.Lock()
		q.pending = q.pending[1:]
		q.mu.Unlock()
	}
}

// transfer is one SMTP transaction to the MX at addr.
func transfer(addr string, m queued) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	c := gosmtp.NewClient(conn)
	defer func() { _ = c.Close() }()
	if err := c.Hello("mta.peers.test"); err != nil {
		return err
	}
	if err := c.Mail(m.from, nil); err != nil {
		return err
	}
	if err := c.Rcpt(m.rcpt, nil); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(m.raw)); err != nil {
		return err
	}
	return w.Close()
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var script []reply
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			script = append(script, reply{})
		case conformance.OutcomeRejected:
			script = append(script, reply{at: "MAIL", err: &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "refused"}})
		case conformance.OutcomeTransient:
			script = append(script, reply{at: "MAIL", err: &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "try later"}})
		case conformance.OutcomeAnswerLost:
			script = append(script, reply{drop: true})
		default:
			// SMTP states no Retry-After, and without reconcile a lost
			// request is indistinguishable from a lost answer.
			return conformance.ErrNotExpressible
		}
	}
	p.relay(ch).respond(script...)
	return nil
}

func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	r := p.relay(ch)
	txs := r.transactions()
	p.mu.Lock()
	recs := append([]callRecord(nil), p.calls[ch]...)
	p.mu.Unlock()
	var out []conformance.ProviderCall
	for _, rec := range recs {
		if rec.tx < 0 || rec.tx >= len(txs) {
			continue
		}
		tx := txs[rec.tx]
		call := conformance.ProviderCall{
			Capability: model.CapabilitySend, Operation: rec.req.Operation, Audience: rec.req.Audience,
			Applied: tx.applied, Content: rec.req.Content,
		}
		if len(tx.rcpts) > 0 {
			call.Route = route(ch, tx.rcpts[0])
		}
		if tx.applied {
			if m, err := mime.Parse(bytes.NewReader(tx.data), mime.Limits{}); err == nil {
				call.References = []model.ExternalRef{model.ProviderMessageRef(ch, m.Header.MessageID)}
			}
		}
		out = append(out, call)
	}
	return out
}

func (p *provider) Revoke(ch model.ChannelRef, c model.Capability) error {
	if c != model.CapabilitySend {
		return conformance.ErrNotExpressible
	}
	p.relay(ch).setAuth(false)
	return nil
}

// observedFactory opens the real adapter and notes which send request each
// relay transaction served. It changes no behavior.
type observedFactory struct {
	*smtp.Factory
	p *provider
}

func (f *observedFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return &observedInstance{Instance: inst.(*smtp.Instance), p: f.p, ch: ch.Ref}, nil
}

type observedInstance struct {
	*smtp.Instance
	p  *provider
	ch model.ChannelRef
}

// Run records the sink's answer to each delivered mail.
func (o *observedInstance) Run(ctx context.Context, sink adapter.Sink) error {
	return o.Instance.Run(ctx, &recordingSink{Sink: sink, p: o.p, ch: o.ch})
}

type recordingSink struct {
	adapter.Sink
	p  *provider
	ch model.ChannelRef
}

func (s *recordingSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	r, err := s.Sink.Accept(ctx, in)
	s.p.record(s.ch, in, err)
	return r, err
}

func (o *observedInstance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	r := o.p.relay(o.ch)
	before := len(r.transactions())
	out, err := o.Instance.Send(ctx, req)
	if after := len(r.transactions()); after > before {
		o.p.mu.Lock()
		if o.p.calls == nil {
			o.p.calls = map[model.ChannelRef][]callRecord{}
		}
		for i := before; i < after; i++ {
			o.p.calls[o.ch] = append(o.p.calls[o.ch], callRecord{req: req, tx: i})
		}
		o.p.mu.Unlock()
	}
	return out, err
}
