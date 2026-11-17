package imap_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	gosmtp "github.com/emersion/go-smtp"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/imap"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

// TestAdapterConformance runs the shared adapter suite against imap.mail,
// with an in-process IMAP server and submission server per account. The
// provider saves sent copies (as Gmail and Microsoft 365 do), so reconcile
// can tell an applied submission from a lost one. Fixture senders are
// shaped as addresses and message IDs as msg-ids; a delivered event is a
// mail appended to the account's INBOX, read by the receiver like any
// other.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			p := &provider{t: t, accounts: map[model.ChannelRef]*account{}}
			return &observedFactory{Factory: newFactory(t), p: p}, p
		},
		Capabilities: []model.Capability{
			model.CapabilityReceive, model.CapabilitySend, model.CapabilityReconcile, model.CapabilityStartConversation,
			model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash, model.CapabilityHistory,
			model.CapabilityReplyRecipients, model.CapabilityRawRead,
			model.CapabilityExtensionQuery, model.CapabilityExtensionExecute, imap.ExtensionMailboxes, imap.ExtensionMailboxCreate,
		},
		Extensions: imap.NewFactory(imap.Options{}).Extensions(),
		// A move is a copy into the destination mailbox: the message has a
		// new UID there, so a new reference.
		MoveChangesReference: true,
	})
}

// account is one provider mailbox: its IMAP and submission servers. The
// server moves mail and has the special-use mailboxes a mail provider has,
// and the folders the state cases move to.
type account struct {
	srv   *mailServer
	relay *relay
}

type provider struct {
	t        *testing.T
	mu       sync.Mutex
	accounts map[model.ChannelRef]*account
	calls    map[model.ChannelRef][]callRecord
	// events maps a delivered Message-ID to its event ID, and results are
	// what the sink answered.
	raws    map[model.ChannelRef]map[string][]byte
	events  map[model.ChannelRef]map[string]string
	results map[model.ChannelRef][]conformance.InboundResult
}

// ShapeID gives senders mailbox addresses and message IDs the msg-id form.
// A mailbox serves one destination, the configured "support".
func (p *provider) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return id + "@peers.test"
	case conformance.FixtureMessage:
		return "<" + id + "@peers.test>"
	}
	return id
}

// callRecord is one adapter call the provider saw: a send mapped to the
// submission transactions it made, or a reconcile.
type callRecord struct {
	capability model.Capability
	effect     adapter.Effect
	content    model.Content
	tx         int
	// state is the request of a provider state call.
	state *stateCall
}

// stateCall is a provider state call and whether the provider applied it.
type stateCall struct {
	applied     bool
	items       []model.ExternalRef
	undo, read  bool
	kind        string
	destination model.MoveDestination
	add, remove []string
}

func (p *provider) account(ch model.ChannelRef) *account {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.accounts[ch]
	if !ok {
		a = &account{
			srv:   newMailServer(p.t, serverOptions{sec: implicitTLS, mechs: []string{"XOAUTH2", "PLAIN"}, tokenOK: func(string) bool { return true }, anyUser: true, state: true}),
			relay: newRelay(p.t),
		}
		a.srv.create("Sent", goimap.MailboxAttrSent)
		a.srv.create("Archive", goimap.MailboxAttrArchive)
		a.srv.create("Trash", goimap.MailboxAttrTrash)
		a.srv.create("Junk", goimap.MailboxAttrJunk)
		a.srv.create("conformance-folder-a")
		a.srv.create("conformance-folder-b")
		a.relay.anyCred = true
		a.relay.saved = func(b []byte) { a.srv.deliver("Sent", b, time.Now()) }
		p.accounts[ch] = a
	}
	return a
}

func domain(ch model.ChannelRef) string {
	return "acct-" + strings.ToLower(string(ch.ID[len(ch.ID)-4:])) + ".test"
}

func route(ch model.ChannelRef, addr string) model.ExternalRef {
	return model.ExternalRef{Channel: ch, Namespace: imap.Namespace, Kind: imap.RouteKind, ID: addr}
}

func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	a := p.account(ch)
	cfg := imap.Config{
		Address: "desk@" + domain(ch), Destination: "support",
		IMAP:       a.srv.endpoint(),
		Submission: imap.Server{Host: "127.0.0.1", StartTLSPort: a.relay.port, CommandTimeoutMS: 5000, DataTimeoutMS: 1000},
		Sent:       imap.Sent{Mailbox: "Sent", SavedByProvider: true},
	}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "mailbox-cred"}
}

func (p *provider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	return route(ch, peer)
}

// Deliver appends ev to the account's INBOX as a mail from its sender. A
// mailbox has one destination, no private audience, and claims ride in
// Reply-To, which never authenticates anyone.
func (p *provider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Destination != "support" {
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
		extra = "Reply-To: " + strings.Join(ev.Claims, ", ") + "\r\n"
	}
	raw := "From: " + ev.Sender + "\r\nTo: desk@" + domain(ch) + "\r\nSubject: hello\r\nMessage-ID: " + ev.MessageID + "\r\n" + extra + "\r\n" + ev.Text + "\r\n"
	p.mu.Lock()
	if p.raws == nil {
		p.raws = map[model.ChannelRef]map[string][]byte{}
	}
	if p.raws[ch] == nil {
		p.raws[ch] = map[string][]byte{}
	}
	p.raws[ch][ev.MessageID] = []byte(raw)
	p.mu.Unlock()
	p.account(ch).srv.deliver("INBOX", []byte(raw), time.Now())
	return nil
}

var _ conformance.RawProvider = (*provider)(nil)

// RawCopy is the text delivered under Message-ID id.
func (p *provider) RawCopy(ch model.ChannelRef, id string) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, ok := p.raws[ch][id]
	if !ok {
		return nil, conformance.ErrNotExpressible
	}
	return append([]byte(nil), raw...), nil
}

// HoldHistory appends ev to the account's INBOX as mail received at at: the
// arrival date and the Date header both. An event already delivered is held
// already: one message, with the bytes it arrived with.
func (p *provider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	if ev.Kind != "" || (ev.Visibility != "" && ev.Visibility != model.Public) || ev.Destination != "support" || len(ev.Claims) > 0 {
		return conformance.ErrNotExpressible
	}
	p.mu.Lock()
	_, delivered := p.events[ch][ev.MessageID]
	p.mu.Unlock()
	if delivered {
		return nil // the mailbox holds it already, once
	}
	raw := "From: " + ev.Sender + "\r\nTo: desk@" + domain(ch) + "\r\nSubject: hello\r\nMessage-ID: " + ev.MessageID +
		"\r\nDate: " + at.UTC().Format(time.RFC1123Z) + "\r\n\r\n" + ev.Text + "\r\n"
	p.account(ch).srv.deliver("INBOX", []byte(raw), at)
	return nil
}

// recordInbound notes the sink's answer to the delivered message in.
func (p *provider) recordInbound(ch model.ChannelRef, in model.Ingress, err error) {
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

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var script []reply
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			script = append(script, reply{})
		case conformance.OutcomeRejected:
			script = append(script, reply{mailErr: &gosmtp.SMTPError{Code: 550, EnhancedCode: gosmtp.EnhancedCode{5, 7, 1}, Message: "refused"}})
		case conformance.OutcomeTransient:
			script = append(script, reply{mailErr: &gosmtp.SMTPError{Code: 451, EnhancedCode: gosmtp.EnhancedCode{4, 3, 0}, Message: "try later"}})
		case conformance.OutcomeAnswerLost:
			script = append(script, reply{applied: true})
		case conformance.OutcomeRequestLost:
			script = append(script, reply{lost: true})
		default:
			return conformance.ErrNotExpressible // SMTP states no Retry-After
		}
	}
	p.account(ch).relay.respond(script...)
	// The same answers script the mailbox's state commands, the call each
	// state verb makes.
	var state []stateReply
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			state = append(state, stateReply{apply: true})
		case conformance.OutcomeRejected:
			state = append(state, stateReply{refuse: &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeNoPerm, Text: "refused"}})
		case conformance.OutcomeTransient:
			state = append(state, stateReply{refuse: &goimap.Error{Type: goimap.StatusResponseTypeNo, Code: goimap.ResponseCodeUnavailable, Text: "try later"}})
		case conformance.OutcomeAnswerLost:
			state = append(state, stateReply{lostReply: true})
		case conformance.OutcomeRequestLost:
			state = append(state, stateReply{cut: true})
		}
	}
	p.account(ch).srv.respond(state...)
	return nil
}

func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	txs := p.account(ch).relay.transactions()
	p.mu.Lock()
	recs := append([]callRecord(nil), p.calls[ch]...)
	p.mu.Unlock()
	var out []conformance.ProviderCall
	for _, rec := range recs {
		call := conformance.ProviderCall{Capability: rec.capability, Operation: rec.effect.Operation, Audience: rec.effect.Audience, Route: rec.effect.Route}
		if st := rec.state; st != nil {
			call.Applied, call.Items, call.Undo, call.Read, call.FlagKind = st.applied, st.items, st.undo, st.read, st.kind
			call.Destination, call.AddLabels, call.RemoveLabels = st.destination, st.add, st.remove
		}
		if rec.capability == model.CapabilitySend {
			if rec.tx < 0 || rec.tx >= len(txs) {
				continue
			}
			tx := txs[rec.tx]
			call.Applied, call.Content = tx.applied, rec.content
			if len(tx.rcpts) > 0 {
				call.Route = route(ch, tx.rcpts[0])
			}
			if tx.applied {
				if m, err := mime.Parse(bytes.NewReader(tx.data), mime.Limits{}); err == nil {
					call.References = []model.ExternalRef{model.ProviderMessageRef(ch, m.Header.MessageID)}
				}
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
	p.account(ch).relay.setAuth(false)
	return nil
}

func (p *provider) record(ch model.ChannelRef, rec callRecord) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.calls == nil {
		p.calls = map[model.ChannelRef][]callRecord{}
	}
	p.calls[ch] = append(p.calls[ch], rec)
}

// observedFactory opens the real adapter and notes which effect each
// submission transaction and reconcile served. It changes no behavior and,
// by embedding, keeps the instance's interface set.
type observedFactory struct {
	*imap.Factory
	p *provider
}

func (f *observedFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return &observedInstance{Instance: inst.(*imap.Instance), p: f.p, ch: ch.Ref}, nil
}

type observedInstance struct {
	*imap.Instance
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
	s.p.recordInbound(s.ch, in, err)
	return r, err
}

func (o *observedInstance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	r := o.p.account(o.ch).relay
	before := len(r.transactions())
	out, err := o.Instance.Send(ctx, req)
	for i := before; i < len(r.transactions()); i++ {
		o.p.record(o.ch, callRecord{capability: model.CapabilitySend, effect: req.Effect, content: req.Content, tx: i})
	}
	return out, err
}

func (o *observedInstance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	e := req.Effect
	o.p.record(o.ch, callRecord{capability: model.CapabilityReconcile, effect: e})
	return o.Instance.Reconcile(ctx, req)
}

// recordState notes a state call: what was asked and whether the provider
// applied it. A command refused or lost before it ran applied nothing; one
// whose answer was lost applied what it was asked.
func (o *observedInstance) recordState(c model.Capability, e adapter.Effect, st stateCall, out adapter.Outcome) {
	scripted := o.p.account(o.ch).srv.takeScripted()
	st.applied = out.State == model.ProviderAccepted || scripted.lostReply
	o.p.record(o.ch, callRecord{capability: c, effect: e, state: &st})
}

func (o *observedInstance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	out, err := o.Instance.Archive(ctx, req)
	o.recordState(model.CapabilityArchive, req.Effect, stateCall{items: req.Items, undo: req.Undo}, out)
	return out, err
}

func (o *observedInstance) Move(ctx context.Context, req adapter.MoveRequest) (adapter.Outcome, error) {
	out, err := o.Instance.Move(ctx, req)
	o.recordState(model.CapabilityMove, req.Effect, stateCall{items: req.Items, destination: req.Destination, add: req.Add, remove: req.Remove}, out)
	return out, err
}

func (o *observedInstance) SetReadState(ctx context.Context, req adapter.ReadStateRequest) (adapter.Outcome, error) {
	out, err := o.Instance.SetReadState(ctx, req)
	o.recordState(model.CapabilityReadState, req.Effect, stateCall{items: req.Items, read: req.Read}, out)
	return out, err
}

func (o *observedInstance) Flag(ctx context.Context, req adapter.FlagRequest) (adapter.Outcome, error) {
	out, err := o.Instance.Flag(ctx, req)
	o.recordState(model.CapabilityFlag, req.Effect, stateCall{items: req.Items, kind: req.Kind, undo: req.Undo}, out)
	return out, err
}

func (o *observedInstance) Trash(ctx context.Context, req adapter.TrashRequest) (adapter.Outcome, error) {
	out, err := o.Instance.Trash(ctx, req)
	o.recordState(model.CapabilityTrash, req.Effect, stateCall{items: req.Items, undo: req.Undo}, out)
	return out, err
}
