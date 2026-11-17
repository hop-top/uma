package google

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// TestAdapterConformance runs the shared adapter suite against the fake
// Gmail API. Fixture senders and destinations are shaped as addresses; a
// delivered event is a raw message in the account's inbox, read by the
// receiver like any other.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			g := newFakeGmail(t)
			f := NewFactory(Options{Endpoint: g.endpoint(), CertsURL: g.certsURL()})
			p := &gmailProvider{g: g}
			return &harnessFactory{Factory: f, g: g, p: p}, p
		},
		Capabilities: capabilities,
		Extensions:   NewFactory(Options{}).Extensions(),
		// Star the peer's message and clear its unread mark: two system
		// labels every mailbox holds.
		Action: model.Extension{
			Name: ExtensionLabels, Version: ExtensionVersion,
			Data: json.RawMessage(`{"add":["STARRED"],"remove":["UNREAD"]}`),
		},
		// A label both added and removed: the adapter's own rule.
		InvalidAction: model.Extension{
			Name: ExtensionLabels, Version: ExtensionVersion,
			Data: json.RawMessage(`{"add":["STARRED"],"remove":["STARRED"]}`),
		},
	})
}

func mailboxOf(ch model.ChannelRef) string {
	return "desk@acct-" + strings.ToLower(string(ch.ID[len(ch.ID)-4:])) + ".test"
}

// harnessFactory opens the adapter's own instances, wrapped (by
// embedding, so their interface set is unchanged) to tell the fake which
// effect an outbound Message-ID serves: Gmail sees messages, not engine
// operations.
type harnessFactory struct {
	*Factory
	g *fakeGmail
	p *gmailProvider
}

func (h *harnessFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := h.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	in := inst.(*instance)
	// A report of the provider's state is dated by the provider: Gmail
	// stamps no label change, so the adapter dates it by its clock, which
	// the harness sets to the time the suite gave the report.
	base := in.now
	in.now = func() time.Time {
		if t := h.p.observedAt(); !t.IsZero() {
			return t
		}
		return base()
	}
	h.p.opened(in)
	return &recordingInstance{instance: in, g: h.g, p: h.p}, nil
}

type recordingInstance struct {
	*instance
	g *fakeGmail
	p *gmailProvider
}

// Run records which delivered events the sink accepted.
func (r *recordingInstance) Run(ctx context.Context, sink adapter.Sink) error {
	return r.instance.Run(ctx, &recordingSink{Sink: sink, p: r.p, ch: r.ch.Ref})
}

type recordingSink struct {
	adapter.Sink
	p  *gmailProvider
	ch model.ChannelRef
}

func (s *recordingSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	rec, err := s.Sink.Accept(ctx, in)
	if in.Envelope.Kind == model.EnvelopeConversationState && in.Envelope.State != nil {
		s.p.stateAccepted(s.ch, in.Envelope.State.Conversation.ID, err == nil)
		return rec, err
	}
	s.p.mu.Lock()
	if ev, ok := s.p.events[s.ch][in.Envelope.MessageID]; ok {
		s.p.results[s.ch] = append(s.p.results[s.ch], conformance.InboundResult{EventID: ev, Acked: err == nil})
	}
	s.p.mu.Unlock()
	return rec, err
}

func (r *recordingInstance) note(eff adapter.Effect) {
	b := r.g.box(r.cfg.User, r.ch.Ref)
	r.g.mu.Lock()
	b.attempts[r.messageID(eff.Operation, r.cfg.User)] = fakeAttempt{op: eff.Operation, route: eff.Route}
	r.g.mu.Unlock()
}

func (r *recordingInstance) Send(ctx context.Context, req adapter.SendRequest) (adapter.Outcome, error) {
	r.note(req.Effect)
	return r.instance.Send(ctx, req)
}

func (r *recordingInstance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	eff := req.Effect
	r.note(eff)
	out, err := r.instance.Reconcile(ctx, req)
	if slices.Contains(stateCapabilities, req.Capability) {
		// Gmail sees the thread read back, not the operation it serves.
		b := r.g.box(r.cfg.User, r.ch.Ref)
		r.g.mu.Lock()
		b.calls = append(b.calls, conformance.ProviderCall{
			Capability: model.CapabilityReconcile, Operation: eff.Operation, Route: eff.Route,
			Applied: err == nil && out.State == model.ProviderAccepted,
		})
		r.g.mu.Unlock()
	}
	return out, err
}

// noteState tells the fake which state request serves the thread of each
// item: Gmail sees the thread and the label lists, not the operation, the
// route or the conversation's items.
func (r *recordingInstance) noteState(eff adapter.Effect, items []model.ExternalRef, c conformance.ProviderCall) {
	c.Operation, c.Route, c.Items = eff.Operation, eff.Route, slices.Clone(items)
	b := r.g.box(r.cfg.User, r.ch.Ref)
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	for _, it := range items {
		b.stateCalls[it.ID] = c
	}
}

func (r *recordingInstance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	r.noteState(req.Effect, req.Items, conformance.ProviderCall{Capability: model.CapabilityArchive, Undo: req.Undo})
	return r.instance.Archive(ctx, req)
}

func (r *recordingInstance) Move(ctx context.Context, req adapter.MoveRequest) (adapter.Outcome, error) {
	r.noteState(req.Effect, req.Items, conformance.ProviderCall{
		Capability: model.CapabilityMove, Destination: req.Destination, AddLabels: slices.Clone(req.Add), RemoveLabels: slices.Clone(req.Remove),
	})
	return r.instance.Move(ctx, req)
}

func (r *recordingInstance) SetReadState(ctx context.Context, req adapter.ReadStateRequest) (adapter.Outcome, error) {
	r.noteState(req.Effect, req.Items, conformance.ProviderCall{Capability: model.CapabilityReadState, Read: req.Read})
	return r.instance.SetReadState(ctx, req)
}

func (r *recordingInstance) Flag(ctx context.Context, req adapter.FlagRequest) (adapter.Outcome, error) {
	r.noteState(req.Effect, req.Items, conformance.ProviderCall{Capability: model.CapabilityFlag, FlagKind: req.Kind, Undo: req.Undo})
	return r.instance.Flag(ctx, req)
}

func (r *recordingInstance) Trash(ctx context.Context, req adapter.TrashRequest) (adapter.Outcome, error) {
	r.noteState(req.Effect, req.Items, conformance.ProviderCall{Capability: model.CapabilityTrash, Undo: req.Undo})
	return r.instance.Trash(ctx, req)
}

// Execute tells the fake which effect a modify of the target message
// serves: Gmail sees the message and the label lists, not the operation,
// the audience or the action's unknown fields.
func (r *recordingInstance) Execute(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if id, ok := r.targetMessage(req.Effect); ok {
		b := r.g.box(r.cfg.User, r.ch.Ref)
		r.g.mu.Lock()
		b.executes[id] = fakeExecute{
			fakeAttempt: fakeAttempt{op: req.Operation, route: req.Route}, audience: req.Audience, action: req.Action.Clone(),
		}
		r.g.mu.Unlock()
	}
	return r.instance.Execute(ctx, req)
}

// gmailProvider is the suite's view of the fake: one Gmail user per
// channel.
type gmailProvider struct {
	g *fakeGmail

	mu     sync.Mutex
	events map[model.ChannelRef]map[string]string // Gmail message ID to event ID
	// reports are the state reports queued per thread, oldest first; the
	// sink pairs each accepted conversation.state envelope with one.
	reports map[model.ChannelRef]map[string][]string
	// observed is the time of the latest report; inst the latest instance
	// opened per channel.
	observed time.Time
	inst     map[model.ChannelRef]*instance
	first    map[model.ChannelRef]map[string]string // sender to its first Gmail message ID
	results  map[model.ChannelRef][]conformance.InboundResult
}

// ShapeID gives senders a mailbox address and destinations the account's
// own domain; Gmail message IDs, event IDs and rooms take any token.
func (p *gmailProvider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return id + "@peers.test"
	case conformance.FixtureDestination:
		return id + "@" + domainOf(ch)
	}
	return id
}

func domainOf(ch model.ChannelRef) string {
	_, d, _ := strings.Cut(mailboxOf(ch), "@")
	return d
}

func (p *gmailProvider) box(ch model.ChannelRef) *fakeBox { return p.g.box(mailboxOf(ch), ch) }

// HTTPClient is the client the suite's engine injects.
func (p *gmailProvider) HTTPClient() ports.HTTPClient { return p.g.client() }

func (p *gmailProvider) Account(ch model.ChannelRef) conformance.Account {
	p.box(ch)
	data, _ := json.Marshal(Config{User: mailboxOf(ch), Addresses: []string{"support@" + domainOf(ch), "only-b@" + domainOf(ch)}})
	return conformance.Account{
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
		CredentialRef: "cred-" + mailboxOf(ch),
	}
}

// Route is the address route to peer, replying to the Gmail message that
// opened the conversation. A mail conversation exists because the peer
// wrote, so before any delivery the fake holds that opening message, one
// no receiver round lists.
func (p *gmailProvider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	r := model.ExternalRef{Channel: ch, Namespace: IdentityNamespace, Kind: refKindAddress, ID: peer}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init(ch)
	if p.first[ch][peer] == "" {
		id := "opened-" + peer
		p.g.open(mailboxOf(ch), id, "thread-"+id, rawMessage(peer, "support@"+domainOf(ch), "<"+id+"@peers.test>", "opening message", "hello"))
		p.first[ch][peer] = id
	}
	r.ParentID = p.first[ch][peer]
	return r
}

// init prepares the per-channel maps; p.mu is held.
func (p *gmailProvider) init(ch model.ChannelRef) {
	if p.events == nil {
		p.events, p.first, p.results = map[model.ChannelRef]map[string]string{}, map[model.ChannelRef]map[string]string{}, map[model.ChannelRef][]conformance.InboundResult{}
	}
	if p.events[ch] == nil {
		p.events[ch], p.first[ch] = map[string]string{}, map[string]string{}
	}
}

// Deliver puts ev in the account's inbox as a raw message. Mail has no
// private audience, and claims have no header here.
func (p *gmailProvider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	p.mu.Lock()
	p.init(ch)
	p.events[ch][ev.MessageID] = ev.EventID
	if _, ok := p.first[ch][ev.Sender]; !ok {
		p.first[ch][ev.Sender] = ev.MessageID
	}
	p.mu.Unlock()
	p.box(ch)
	p.g.deliver(mailboxOf(ch), ev.MessageID, "thread-"+ev.Room, rawMessage(ev.Sender, ev.Destination, "<"+ev.MessageID+"@peers.test>", "hello", ev.Text))
	return nil
}

var _ conformance.RawProvider = (*gmailProvider)(nil)

// RawCopy is the raw form the fake holds of message id.
func (p *gmailProvider) RawCopy(ch model.ChannelRef, id string) ([]byte, error) {
	b := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	m, ok := b.messages[id]
	if !ok {
		return nil, conformance.ErrNotExpressible
	}
	return append([]byte(nil), m.raw...), nil
}

// HoldHistory puts ev in the account's inbox as mail received at at, which
// no receiver round lists. A message the receiver already took only
// changes its date.
func (p *gmailProvider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	p.mu.Lock()
	p.init(ch)
	p.events[ch][ev.MessageID] = ev.EventID
	p.mu.Unlock()
	p.box(ch)
	p.g.hold(mailboxOf(ch), ev.MessageID, "thread-"+ev.Room, rawMessage(ev.Sender, ev.Destination, "<"+ev.MessageID+"@peers.test>", "hello", ev.Text), at)
	return nil
}

func (p *gmailProvider) observedAt() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.observed
}

func (p *gmailProvider) opened(in *instance) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inst == nil {
		p.inst = map[model.ChannelRef]*instance{}
	}
	p.inst[in.ch.Ref] = in
}

// stateAccepted pairs an accepted conversation.state envelope about the
// message conv with the oldest queued report of its thread.
func (p *gmailProvider) stateAccepted(ch model.ChannelRef, conv string, acked bool) {
	b := p.box(ch)
	p.g.mu.Lock()
	var thread string
	if m, ok := b.messages[conv]; ok {
		thread = m.threadID
	}
	p.g.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	q := p.reports[ch][thread]
	if len(q) == 0 {
		return
	}
	p.reports[ch][thread] = q[1:]
	p.results[ch] = append(p.results[ch], conformance.InboundResult{EventID: q[0], Acked: acked})
}

func (p *gmailProvider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]conformance.InboundResult(nil), p.results[ch]...)
}

func (p *gmailProvider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	b := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	b.responses = append(b.responses, rs...)
	return nil
}

func (p *gmailProvider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	b := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	return append([]conformance.ProviderCall(nil), b.calls...)
}

// Revoke is not expressible: Gmail has no documented pre-call scope
// check; a missing scope is a 403 on the call itself.
func (p *gmailProvider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// ItemState is what the mailbox holds for message ref names.
func (p *gmailProvider) ItemState(ch model.ChannelRef, ref model.ExternalRef) (conformance.ItemState, bool) {
	b := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	m, ok := b.messages[ref.ID]
	if !ok {
		return conformance.ItemState{}, false
	}
	has := func(l string) bool { return slices.Contains(m.labels, l) }
	it := conformance.ItemState{
		Trashed: has("TRASH"), Read: !has("UNREAD"),
		Archived: !has(labelInbox),
	}
	if has("STARRED") {
		it.Flags = append(it.Flags, model.FlagStar)
	}
	if has("IMPORTANT") {
		it.Flags = append(it.Flags, model.FlagImportant)
	}
	for _, l := range m.labels {
		if !slices.Contains(systemLabels, l) {
			it.Labels = append(it.Labels, l)
		}
	}
	return it, true
}

// DeliverState has the mailbox change as another client would, so that the
// thread of ev.MessageID shows ev.State, and notes the report for the
// receiver's history round to pick up. A thread the mailbox does not hold
// is made, so a report of an unknown conversation can be told apart. The
// change always leaves one history record, even when the mailbox already
// shows the state, as a redelivered history record does. The receiver is
// primed first when it has not synchronized: a change before the first
// synchronization is not a history record to read.
func (p *gmailProvider) DeliverState(ch model.ChannelRef, ev conformance.StateEvent) error {
	user := mailboxOf(ch)
	b := p.box(ch)
	p.mu.Lock()
	in := p.inst[ch]
	p.mu.Unlock()
	if in != nil {
		if st, err := in.loadState(context.Background()); err == nil && st.HistoryID == 0 {
			p.g.mu.Lock()
			h := b.historyID
			p.g.mu.Unlock()
			st.HistoryID = int64s(h)
			if err := in.saveState(context.Background(), st); err != nil {
				return err
			}
		}
	}
	p.g.mu.Lock()
	m, ok := b.messages[ev.MessageID]
	p.g.mu.Unlock()
	if !ok {
		p.g.open(user, ev.MessageID, "thread-"+ev.MessageID, rawMessage("stranger@peers.test", ev.Destination, "<"+ev.MessageID+"@peers.test>", "unknown", "hello"))
		p.g.mu.Lock()
		m = b.messages[ev.MessageID]
		p.g.mu.Unlock()
	}
	thread := m.threadID
	st := ev.State
	var add, remove []string
	put := func(on bool, label string) {
		if on {
			add = append(add, label)
		} else {
			remove = append(remove, label)
		}
	}
	put(!st.Archived && !st.Trashed, labelInbox)
	put(st.Trashed, "TRASH")
	// Gmail always holds a read state: a report that names none shows the
	// thread unread.
	put(st.Read == nil || !*st.Read, "UNREAD")
	put(slices.Contains(st.Flags, model.FlagStar), "STARRED")
	put(slices.Contains(st.Flags, model.FlagImportant), "IMPORTANT")
	want := map[string]bool{}
	for _, l := range st.Labels {
		want[l.ID] = true
		add = append(add, l.ID)
	}
	p.g.mu.Lock()
	for _, x := range p.g.threadMessages(b, thread) {
		for _, l := range x.labels {
			if !slices.Contains(systemLabels, l) && !want[l] {
				remove = append(remove, l)
			}
		}
	}
	p.g.mu.Unlock()
	p.g.relabelThread(user, thread, add, remove)
	p.g.mu.Lock()
	b.historyID++
	b.history = append(b.history, fakeHistory{id: b.historyID, msg: ev.MessageID, thread: thread, kind: "labelAdded", labels: slices.Clone(b.messages[ev.MessageID].labels), changed: []string{"UNREAD"}})
	p.g.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init(ch)
	if p.reports == nil {
		p.reports = map[model.ChannelRef]map[string][]string{}
	}
	if p.reports[ch] == nil {
		p.reports[ch] = map[string][]string{}
	}
	p.reports[ch][thread] = append(p.reports[ch][thread], ev.EventID)
	p.observed = ev.State.ObservedAt
	return nil
}

var (
	_ conformance.StateProvider   = (*gmailProvider)(nil)
	_ conformance.HistoryProvider = (*gmailProvider)(nil)
)
