package microsoft

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// TestAdapterConformance runs the shared adapter suite against the fake
// Graph server.
func TestAdapterConformance(t *testing.T) {
	conformance.RunAdapterSuite(t, conformance.AdapterHarness{
		New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
			g := newFakeGraph(t)
			p := &graphProvider{g: g, rec: &sinkRecorder{}}
			f := NewFactory(Options{Endpoint: g.endpoint()})
			return &harnessFactory{Factory: f, p: p}, p
		},
		Capabilities: capabilities,
		Extensions:   NewFactory(Options{}).Extensions(),
		Action: model.Extension{
			Name: ExtensionCategories, Version: ExtensionVersion,
			Data: json.RawMessage(`{"set":["Desk","Follow up"]}`),
		},
		// A category named twice: the adapter's own rule.
		InvalidAction: model.Extension{
			Name: ExtensionCategories, Version: ExtensionVersion,
			Data: json.RawMessage(`{"set":["Desk","Desk"]}`),
		},
	})
}

// harnessFactory opens the adapter's own instances, wrapped (by embedding,
// so their interface set is unchanged) to see which inbound events the sink
// accepted, the provider-side acknowledgement the suite asks about: an
// accepted message advances the delta link, a refused one is fetched again.
type harnessFactory struct {
	*Factory
	p *graphProvider
}

func (h *harnessFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := h.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	return &recordingInstance{instance: inst.(*instance), p: h.p}, nil
}

type recordingInstance struct {
	*instance
	p *graphProvider
}

func (r *recordingInstance) Run(ctx context.Context, sink adapter.Sink) error {
	return r.instance.Run(ctx, &recordingSink{Sink: sink, p: r.p, ch: r.ch.Ref})
}

func (r *recordingInstance) Handler(sink adapter.Sink) http.Handler {
	return r.instance.Handler(&recordingSink{Sink: sink, p: r.p, ch: r.ch.Ref})
}

// Execute tells the fake which effect an update of the target message
// serves: Graph sees the message and the category list, not the
// operation, the audience or the action's unknown fields.
func (r *recordingInstance) Execute(ctx context.Context, req adapter.ExecuteRequest) (adapter.Outcome, error) {
	if id, ok := r.targetMessage(req.Effect); ok {
		mb := r.p.box(r.ch.Ref)
		r.p.g.mu.Lock()
		mb.executes[id] = fakeExecute{op: req.Operation, route: req.Route, audience: req.Audience, action: req.Action.Clone()}
		r.p.g.mu.Unlock()
	}
	return r.instance.Execute(ctx, req)
}

// Reconcile records the lookup of a state effect for the suite: the
// Graph calls of a state read-back do not name the effect.
func (r *recordingInstance) Reconcile(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	out, err := r.instance.Reconcile(ctx, req)
	if slices.Contains(stateCapabilities, req.Capability) {
		mb := r.p.box(r.ch.Ref)
		r.p.g.mu.Lock()
		mb.calls = append(mb.calls, conformance.ProviderCall{
			Capability: model.CapabilityReconcile, Operation: req.Operation, Route: req.Route, Applied: out.State == model.ProviderAccepted,
		})
		r.p.g.mu.Unlock()
	}
	return out, err
}

// note tells the fake which state effect the next calls on items serve:
// Graph sees the messages and the change, not the operation or the route.
func (r *recordingInstance) note(c model.Capability, eff adapter.Effect, items []model.ExternalRef) {
	mb := r.p.box(r.ch.Ref)
	r.p.g.mu.Lock()
	defer r.p.g.mu.Unlock()
	for _, it := range items {
		mb.notes[it.ID] = stateNote{cap: c, op: eff.Operation, route: eff.Route}
	}
}

func (r *recordingInstance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	r.note(model.CapabilityArchive, req.Effect, req.Items)
	return r.instance.Archive(ctx, req)
}

func (r *recordingInstance) Move(ctx context.Context, req adapter.MoveRequest) (adapter.Outcome, error) {
	r.note(model.CapabilityMove, req.Effect, req.Items)
	return r.instance.Move(ctx, req)
}

func (r *recordingInstance) SetReadState(ctx context.Context, req adapter.ReadStateRequest) (adapter.Outcome, error) {
	r.note(model.CapabilityReadState, req.Effect, req.Items)
	return r.instance.SetReadState(ctx, req)
}

func (r *recordingInstance) Flag(ctx context.Context, req adapter.FlagRequest) (adapter.Outcome, error) {
	r.note(model.CapabilityFlag, req.Effect, req.Items)
	return r.instance.Flag(ctx, req)
}

func (r *recordingInstance) Trash(ctx context.Context, req adapter.TrashRequest) (adapter.Outcome, error) {
	r.note(model.CapabilityTrash, req.Effect, req.Items)
	return r.instance.Trash(ctx, req)
}

type recordingSink struct {
	adapter.Sink
	p  *graphProvider
	ch model.ChannelRef
}

func (s *recordingSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	rec, err := s.Sink.Accept(ctx, in)
	key := in.Envelope.MessageID
	if in.Envelope.State != nil {
		key = "state:" + in.Envelope.State.Conversation.ID
	}
	s.p.rec.add(s.ch, conformance.InboundResult{EventID: s.p.eventOf(s.ch, key), Acked: err == nil})
	return rec, err
}

type sinkRecorder struct {
	mu  sync.Mutex
	got map[model.ChannelRef][]conformance.InboundResult
}

func (r *sinkRecorder) add(ch model.ChannelRef, res conformance.InboundResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.got == nil {
		r.got = map[model.ChannelRef][]conformance.InboundResult{}
	}
	r.got[ch] = append(r.got[ch], res)
}

func (r *sinkRecorder) list(ch model.ChannelRef) []conformance.InboundResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]conformance.InboundResult(nil), r.got[ch]...)
}

// graphProvider is the suite's view of the fake: one mailbox per channel.
type graphProvider struct {
	g   *fakeGraph
	rec *sinkRecorder
}

func mailboxOf(ch model.ChannelRef) string { return "mbx-" + string(ch.ID[len(ch.ID)-4:]) }

func (p *graphProvider) box(ch model.ChannelRef) *fakeMailbox { return p.g.mailbox(mailboxOf(ch), ch) }

// HTTPClient is the client the suite's engine injects.
func (p *graphProvider) HTTPClient() ports.HTTPClient { return p.g.srv.Client() }

func (p *graphProvider) Account(ch model.ChannelRef) conformance.Account {
	mb := p.box(ch)
	// The suite moves messages to these folders and then has the provider
	// report on them: a configured folder is watched.
	watched := []string{"conformance-folder-a", "conformance-folder-b"}
	p.g.mu.Lock()
	for _, f := range watched {
		mb.folders[f] = true
	}
	p.g.mu.Unlock()
	data, _ := json.Marshal(Config{Mailbox: mailboxOf(ch), Addresses: []string{"support", "only-b"}, WatchedFolders: watched})
	return conformance.Account{
		Configuration: model.Extension{Name: ConfigSchema, Version: ConfigVersion, Data: data},
		CredentialRef: "cred-" + mailboxOf(ch),
	}
}

// Route is the sender reference of peer's first message on channel, as
// normalize builds it. A mail conversation exists because the peer wrote,
// so before any delivery the fake holds the message that opened it, one
// no receiver round lists.
func (p *graphProvider) Route(ch model.ChannelRef, peer string) model.ExternalRef {
	mb := p.box(ch)
	r := model.ExternalRef{Channel: ch, Namespace: IdentityNamespace, Kind: refKindAddress, ID: peer}
	if id := p.firstFrom(mb, peer); id != "" {
		r.ParentID = id
		return r
	}
	id := "opened-" + peer
	p.g.open(mb.name, gMessage{
		ID: id, InternetMessageID: "<" + id + "@peer.test>", ConversationID: "conv-" + peer, Subject: "opening message",
		From:         &gRecipient{EmailAddress: gEmailAddress{Address: peer}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: "support@" + mb.name}}},
		Body:         &gItemBody{ContentType: "text", Content: "hello"},
	})
	r.ParentID = id
	return r
}

func (p *graphProvider) firstFrom(mb *fakeMailbox, peer string) string {
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	for _, id := range mb.order {
		if fm := mb.messages[id]; fm.folder == "inbox" && fm.m.From != nil && fm.m.From.EmailAddress.Address == peer {
			return id
		}
	}
	return ""
}

func (p *graphProvider) Deliver(ch model.ChannelRef, ev conformance.InboundEvent) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible // mail has no private messages
	}
	mb := p.box(ch)
	m := gMessage{
		ID: ev.MessageID, InternetMessageID: "<" + ev.MessageID + "@peer.test>", ConversationID: "conv-" + ev.Sender,
		Subject: "subject " + ev.MessageID,
		// Claims ride in the display name: unauthenticated text.
		From:         &gRecipient{EmailAddress: gEmailAddress{Name: claimsName(ev.Claims), Address: ev.Sender}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: ev.Destination}}},
		Body:         &gItemBody{ContentType: "text", Content: ev.Text},
	}
	p.g.deliver(mb.name, m)
	p.g.mu.Lock()
	mb.events[ev.MessageID] = ev.EventID
	if p.g.mimes == nil {
		p.g.mimes = map[string][]byte{}
	}
	p.g.mimes[ev.MessageID] = []byte("From: " + ev.Sender + "\r\nTo: " + ev.Destination + "\r\nSubject: subject " + ev.MessageID +
		"\r\nMessage-ID: <" + ev.MessageID + "@peer.test>\r\n\r\n" + ev.Text + "\r\n")
	p.g.mu.Unlock()
	return nil
}

var _ conformance.RawProvider = (*graphProvider)(nil)

// RawCopy is the MIME the fake serves for message id.
func (p *graphProvider) RawCopy(_ model.ChannelRef, id string) ([]byte, error) {
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	raw, ok := p.g.mimes[id]
	if !ok {
		return nil, conformance.ErrNotExpressible
	}
	return append([]byte(nil), raw...), nil
}

func claimsName(c []string) string {
	out := ""
	for _, s := range c {
		out += s + " "
	}
	return out
}

func (p *graphProvider) eventOf(ch model.ChannelRef, msgID string) string {
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	return mb.events[msgID]
}

func (p *graphProvider) Inbound(ch model.ChannelRef) []conformance.InboundResult {
	return p.rec.list(ch)
}

func (p *graphProvider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	mb.responses = append(mb.responses, rs...)
	return nil
}

func (p *graphProvider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	return append([]conformance.ProviderCall(nil), mb.calls...)
}

var _ conformance.StateProvider = (*graphProvider)(nil)

// ItemState is what the mailbox holds for the message ref names.
func (p *graphProvider) ItemState(ch model.ChannelRef, ref model.ExternalRef) (conformance.ItemState, bool) {
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	fm, ok := mb.messages[ref.ID]
	if !ok {
		return conformance.ItemState{}, false
	}
	it := conformance.ItemState{
		Read: fm.m.IsRead, Archived: fm.folder == "archive", Trashed: fm.folder == "deleteditems",
		Folder: &model.Folder{ID: folderID(fm.folder), Name: folderDisplay(fm.folder), Role: knownFolders[fm.folder].role},
	}
	if fm.m.Flag != nil && fm.m.Flag.FlagStatus == flagFlagged {
		it.Flags = []string{model.FlagStar}
	}
	if fm.m.Importance == importanceHigh {
		it.Flags = append(it.Flags, model.FlagImportant)
	}
	return it, true
}

// DeliverState changes the message the way the report says, in Graph's
// terms: a folder, read, the follow-up flag and importance. A state Graph
// cannot hold, archived and trashed at once or in a folder that is neither,
// is not expressible.
func (p *graphProvider) DeliverState(ch model.ChannelRef, ev conformance.StateEvent) error {
	st := ev.State
	if st.Archived && st.Trashed || (st.Archived || st.Trashed) && st.Folder != nil || len(st.Labels) > 0 {
		return conformance.ErrNotExpressible
	}
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	fm, ok := mb.messages[ev.MessageID]
	if !ok {
		// A conversation uma does not hold.
		fm = &fakeMessage{folder: "inbox", m: gMessage{ID: ev.MessageID}}
		mb.messages[ev.MessageID] = fm
		mb.order = append(mb.order, ev.MessageID)
		mb.flog["inbox"] = append(mb.flog["inbox"], fchange{id: ev.MessageID})
	}
	switch {
	case st.Archived:
		mb.relocate(fm, "archive")
	case st.Trashed:
		mb.relocate(fm, "deleteditems")
	case st.Folder != nil && canonFolder(st.Folder.ID) != fm.folder:
		mb.relocate(fm, canonFolder(st.Folder.ID))
	}
	if st.Read != nil {
		fm.m.IsRead = *st.Read
	}
	fm.m.Flag = &gFlag{FlagStatus: flagNotFlagged}
	if slices.Contains(st.Flags, model.FlagStar) {
		fm.m.Flag.FlagStatus = flagFlagged
	}
	fm.m.Importance = importanceNormal
	if slices.Contains(st.Flags, model.FlagImportant) {
		fm.m.Importance = importanceHigh
	}
	mb.touch(fm)
	fm.modified = st.ObservedAt
	mb.events["state:"+ev.MessageID] = ev.EventID
	return nil
}

// Revoke is not expressible: Graph has no documented pre-call permission
// check; a revoked permission is a 403 on the call itself.
func (p *graphProvider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}
