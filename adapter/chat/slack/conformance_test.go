package slack

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for receive or receive_http, send,
// the message mutations, reconcile, history and the interactions, once for
// each way a Slack workspace is divided into conversations: per room, per
// thread and per contact (a direct message with each user), against the
// documented Slack shapes of fakeslack_test.go.
//
// The suite drives a receiving adapter through Runtime.Receive and never
// through its request URL, and an app takes its payloads one way at a time,
// so it runs for each: as the request URL alone (the descriptor the suite is
// given leaves out receive, and the instance has no receiver) and as the
// Socket Mode receiver, which reads the events the fake workspace sends
// over its WebSocket (conformance_socket_test.go).
func TestAdapterConformance(t *testing.T) {
	for _, transport := range []ReceiveMode{ReceiveEventsAPI, ReceiveSocketMode} {
		for _, mode := range []string{model.ModePerRoom, model.ModePerThread, model.ModePerContact} {
			t.Run(string(transport)+"/"+mode, func(t *testing.T) {
				sel := model.Selection{Mode: mode}
				if mode == model.ModePerContact {
					sel = model.Selection{} // the suite's default: per contact, locked to one conversation
				}
				caps := NewFactory(Options{}).Describe().Capabilities
				if mode != model.ModePerRoom {
					// Only a channel is archivable: a thread or a direct message
					// is not an object Slack archives (TestArchiveRefusesThreadAndDirect).
					caps = slices.DeleteFunc(slices.Clone(caps), func(c model.Capability) bool { return c == model.CapabilityArchive })
				}
				if transport == ReceiveEventsAPI {
					caps = slices.DeleteFunc(slices.Clone(caps), func(c model.Capability) bool { return c == model.CapabilityReceive })
				}
				conformance.RunAdapterSuite(t, conformance.AdapterHarness{
					New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
						p := &provider{mode: mode, transport: transport}
						p.f = newFakeSlack(p.now)
						t.Cleanup(p.f.close)
						f := NewFactory(Options{ResponseBaseURL: p.f.responseBase()})
						return &recordingFactory{Factory: f, p: p, caps: caps}, p
					},
					Capabilities: caps,
					Selection:    sel,
					Extensions:   NewFactory(Options{}).Extensions(),
					// The suite's clock stands at testNow: the trigger_id lives
					// two seconds more.
					Action: ext(ExtensionViewsOpen, map[string]any{
						"trigger_id": testTrigger, "expires_at": testNow.Add(2 * time.Second).Format(time.RFC3339), "view": modal(),
					}),
					InvalidAction: ext(ExtensionViewsOpen, map[string]any{"trigger_id": testTrigger, "expires_at": testNow.Add(2 * time.Second).Format(time.RFC3339)}),
				})
			})
		}
	}
}

// provider is the conformance view of the fake workspace: one account (an
// API root) per channel, and the way its peers map to Slack, by mode:
//
//   - per room: a peer has a room of its own, "C" and the peer's ID;
//   - per thread: peers share one room and have a thread each, opened by a
//     message of the peer;
//   - per contact: a peer is a user and has a direct message, "D" and the
//     peer's ID.
type provider struct {
	mode      string
	transport ReceiveMode
	f         *fakeSlack

	mu       sync.Mutex
	clock    ports.Clock
	prefixes map[model.ChannelRef]string
	order    int
	stamps   map[string]string
	hook     map[model.ChannelRef][]conformance.InboundEvent
	results  map[model.ChannelRef][]conformance.InboundResult
	responds map[model.ChannelRef][]respondRecord
	executes map[model.ChannelRef][]executeRecord
	archives map[model.ChannelRef][]archiveRecord
	// refusals counts, per channel, the Accepts the engine refused while the
	// Socket Mode receiver ran.
	refusals map[model.ChannelRef]int
}

// respondRecord is what the engine handed the adapter for one response, and
// whether Slack saw a request for it: a response_url shows the provider
// neither the interaction nor the audience.
type respondRecord struct {
	req    adapter.RespondRequest
	called bool
}

// executeRecord is what the engine handed the adapter for one view effect,
// and whether Slack saw a request for it: a refused effect never reaches it.
type executeRecord struct {
	req    adapter.ExecuteRequest
	called bool
}

// archiveRecord is what the engine handed the adapter for one archive: the
// provider messages of the conversation Slack's one call stands for.
type archiveRecord struct {
	req    adapter.ArchiveRequest
	called bool
}

var (
	_ conformance.StateProvider   = (*provider)(nil)
	_ conformance.HTTPProvider    = (*provider)(nil)
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
	_ conformance.EventShaper     = (*provider)(nil)
	_ conformance.HistoryProvider = (*provider)(nil)
)

// now is the engine's clock once an instance is open, so Slack's timestamps
// follow the suite's.
func (p *provider) now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clock != nil {
		return p.clock.Now()
	}
	return testNow
}

// recordingFactory opens instances that tell the provider the clock and
// each response the engine hands them, declaring caps.
type recordingFactory struct {
	*Factory
	p    *provider
	caps []model.Capability
}

func (f *recordingFactory) Describe() adapter.Descriptor {
	d := f.Factory.Describe()
	d.Capabilities = f.caps
	return d
}

func (f *recordingFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	f.p.mu.Lock()
	f.p.clock = deps.Clock
	f.p.mu.Unlock()
	r := &recordingInstance{Instance: inst.(*Instance), p: f.p, ch: ch.Ref}
	if f.p.transport == ReceiveSocketMode {
		if f.p.mode != model.ModePerRoom {
			return &socketNoArchive{socketInstance: &socketInstance{recordingInstance: r}}, nil
		}
		return &socketInstance{recordingInstance: r}, nil
	}
	if f.p.mode != model.ModePerRoom {
		return &urlNoArchive{requestURLInstance: &requestURLInstance{r: r}}, nil
	}
	return &requestURLInstance{r: r}, nil
}

// Only a channel is archivable: a thread or a direct message is no object
// Slack archives, so the per-thread and per-contact runs declare no archive
// and these instances show none. The field Archive hides the method the
// embedded instance has (TestArchiveRefusesThreadAndDirect covers the
// refusal itself).
type socketNoArchive struct {
	*socketInstance
	Archive struct{}
}

func (i *socketNoArchive) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := i.socketInstance.Capabilities(ctx)
	return slices.DeleteFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityArchive }), err
}

type urlNoArchive struct {
	*requestURLInstance
	Archive struct{}
}

func (i *urlNoArchive) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := i.requestURLInstance.Capabilities(ctx)
	return slices.DeleteFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityArchive }), err
}

type recordingInstance struct {
	*Instance
	p  *provider
	ch model.ChannelRef
}

func (i *recordingInstance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	before := len(i.p.f.calls(i.p.prefix(i.ch)))
	out, err := i.Instance.Respond(ctx, r)
	after := i.p.f.calls(i.p.prefix(i.ch))
	called := false
	for _, c := range after[before:] {
		called = called || c.Method == "response_url"
	}
	i.p.mu.Lock()
	if i.p.responds == nil {
		i.p.responds = map[model.ChannelRef][]respondRecord{}
	}
	i.p.responds[i.ch] = append(i.p.responds[i.ch], respondRecord{req: r, called: called})
	i.p.mu.Unlock()
	return out, err
}

func (i *recordingInstance) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	before := len(i.p.f.calls(i.p.prefix(i.ch)))
	r.Action = r.Action.Clone()
	out, err := i.Instance.Execute(ctx, r)
	called := len(i.p.f.calls(i.p.prefix(i.ch))) > before
	i.p.mu.Lock()
	if i.p.executes == nil {
		i.p.executes = map[model.ChannelRef][]executeRecord{}
	}
	i.p.executes[i.ch] = append(i.p.executes[i.ch], executeRecord{req: r, called: called})
	i.p.mu.Unlock()
	return out, err
}

func (i *recordingInstance) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	before := len(i.p.f.calls(i.p.prefix(i.ch)))
	out, err := i.Instance.Archive(ctx, r)
	called := len(i.p.f.calls(i.p.prefix(i.ch))) > before
	i.p.mu.Lock()
	if i.p.archives == nil {
		i.p.archives = map[model.ChannelRef][]archiveRecord{}
	}
	i.p.archives[i.ch] = append(i.p.archives[i.ch], archiveRecord{req: r, called: called})
	i.p.mu.Unlock()
	return out, err
}

func (p *provider) prefix(ch model.ChannelRef) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefixes == nil {
		p.prefixes = map[model.ChannelRef]string{}
	}
	pre, ok := p.prefixes[ch]
	if !ok {
		pre = "acct" + strconv.Itoa(len(p.prefixes)+1)
		p.prefixes[ch] = pre
		a := p.f.account(pre, testTeam, testBotUser, conformance.CredentialSecret)
		a.lenient = true
		// The suite cannot know the trigger_ids the fake sends.
		a.views.anyTrigger = true
		if p.transport == ReceiveSocketMode {
			a.appToken = conformance.CredentialSecret
			a.onConnect = func(c *fakeConn) { p.pushSocket(ch, c) }
		}
	}
	return pre
}

// HTTPClient is the client the suite's engine injects.
func (p *provider) HTTPClient() ports.HTTPClient { return p.f.client() }

const threadRoom = "C0THREADS"

// upper is s as a Slack ID tail: letters and digits, uppercase.
func upper(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// roomOf is the conversation of a peer's messages.
func (p *provider) roomOf(sender string) string {
	switch p.mode {
	case model.ModePerThread:
		return threadRoom
	case model.ModePerContact:
		return "D" + sender[1:]
	}
	return "C" + sender[1:]
}

// threadOf is the ts of a peer's thread, per-thread mode only.
func threadOf(sender string) string {
	h := fnv.New32a()
	h.Write([]byte(sender))
	return formatTS(time.Unix(1_699_000_000+int64(h.Sum32()%100_000), 0))
}

// Account configures the account's API root, and the conversations of the
// peers a channel-wide history read covers.
func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := Config{TeamID: testTeam, BotUserID: testBotUser, BotID: testBotID, APIURL: p.f.apiURL(p.prefix(ch)), Receive: p.transport}
	if p.mode != model.ModePerThread {
		for _, peer := range []string{"peer-1", "peer-2", "peer-3", "peer-4", "peer-5"} {
			room := p.roomOf(p.ShapeID(ch, conformance.FixtureSender, peer))
			p.f.touch(p.prefix(ch), room)
			cfg.HistoryChannels = append(cfg.HistoryChannels, room)
		}
	}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "slack"}
}

// Route is where a peer's conversation lives. Slack holds it from the
// start: the account's copy of the room, the direct message or the thread's
// first message exists.
func (p *provider) Route(ch model.ChannelRef, sender string) model.ExternalRef {
	pre := p.prefix(ch)
	x := &Instance{channel: model.Channel{Ref: ch}}
	switch p.mode {
	case model.ModePerThread:
		ts := threadOf(sender)
		if p.f.find(pre, threadRoom, ts) == nil {
			p.f.put(pre, threadRoom, sender, "thread of "+sender, ts, "")
		}
		return x.threadRef(threadRoom, ts)
	case model.ModePerContact:
		p.f.holdDM(pre, sender, p.roomOf(sender))
		return x.userRef(sender)
	}
	p.f.touch(pre, p.roomOf(sender))
	return x.roomRef(p.roomOf(sender))
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []answer
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, failWith("invalid_arguments"))
		case conformance.OutcomeUnauthorized:
			as = append(as, answer{errCode: "restricted_action", byMethod: map[string]string{
				"chat.update": "cant_update_message", "chat.delete": "cant_delete_message",
			}})
		case conformance.OutcomeTransient:
			as = append(as, unreachable)
		case conformance.OutcomeRateLimited:
			as = append(as, rateLimitedFor(r.RetryAfter))
		case conformance.OutcomeAnswerLost:
			as = append(as, answerLost)
		case conformance.OutcomeRequestLost:
			as = append(as, requestLost)
		default:
			return conformance.ErrNotExpressible
		}
	}
	p.f.respond(p.prefix(ch), as...)
	return nil
}

// Revoke: the scopes of a Slack token are visible only in Slack's answers,
// never before a call.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// HoldHistory stores a past message. Slack's ts is a message's ID and its
// time at once, and the suite names IDs before it names times: ShapeID gives
// the IDs of the history cases times in the order those cases need, so the
// message is held at its own ts. A thread's window starts with its parent
// message, which the suite does not hold: not expressible.
func (p *provider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	if p.mode == model.ModePerThread {
		return conformance.ErrNotExpressible
	}
	pre := p.prefix(ch)
	conv, ts, _ := strings.Cut(ev.MessageID, ":")
	if p.mode == model.ModePerContact {
		p.f.holdDM(pre, ev.Sender, conv)
	}
	if p.f.find(pre, conv, ts) == nil {
		p.f.put(pre, conv, ev.Sender, ev.Text, ts, "")
	}
	return nil
}

// ShapeID gives fixture values Slack's forms, stable per value. A message
// ID is "<room>:<ts>" in the default room; ShapeEvent moves it to its
// peer's.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return "U0" + upper(id)
	case conformance.FixtureDestination:
		return testTeam
	case conformance.FixtureRoom:
		return "C0ROOM1"
	case conformance.FixtureMessage:
		return "C0ROOM1:" + p.stamp(id)
	}
	return id
}

// historyOffsets are the times, from the start of the history cases'
// window, of the messages those cases hold: in the order they need.
var historyOffsets = map[string]time.Duration{
	"msg-h-old": -time.Minute, "msg-h-1": 0, "msg-live": 90 * time.Minute, "msg-h-2": 3 * time.Hour, "msg-h-3": 4 * time.Hour,
	"msg-h-4": 5 * time.Hour, "msg-h-5": 6 * time.Hour, "msg-a-1": 7 * time.Hour, "msg-b-1": 8 * time.Hour, "msg-a-2": 9 * time.Hour,
}

func (p *provider) stamp(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stamps == nil {
		p.stamps = map[string]string{}
	}
	ts, ok := p.stamps[id]
	if !ok {
		if off, held := historyOffsets[id]; held {
			ts = formatTS(testNow.Add(-24*time.Hour + off))
		} else {
			p.order++
			ts = formatTS(testNow.Add(time.Duration(p.order) * time.Millisecond)) // the fake's own messages count microseconds
		}
		p.stamps[id] = ts
	}
	return ts
}

// ShapeEvent places an event in its peer's conversation.
func (p *provider) ShapeEvent(ch model.ChannelRef, ev conformance.InboundEvent) conformance.InboundEvent {
	if len(ev.Sender) < 2 {
		return ev
	}
	conv := p.roomOf(ev.Sender)
	ev.Room = conv
	if p.mode == model.ModePerThread {
		ev.Room = threadOf(ev.Sender)
	}
	if ev.MessageID != "" {
		_, ts, ok := strings.Cut(ev.MessageID, ":")
		if !ok {
			ts = ev.MessageID
		}
		ev.MessageID = conv + ":" + ts
	}
	return ev
}

// Calls maps requests to capabilities: a message posted is send, an update
// edit, a deletion delete, a reaction call react, a post to a response URL
// respond; reading a message with its metadata or reactions is reconcile,
// however many requests it takes. Opening a direct message and reading
// history to import are no capability's call.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	pre := p.prefix(ch)
	var out []conformance.ProviderCall
	reading := false
	p.mu.Lock()
	records := append([]respondRecord(nil), p.responds[ch]...)
	p.mu.Unlock()
	var called []respondRecord
	for _, r := range records {
		if r.called {
			called = append(called, r)
		}
	}
	responds := 0
	p.mu.Lock()
	var executed []executeRecord
	for _, r := range p.executes[ch] {
		if r.called {
			executed = append(executed, r)
		}
	}
	executes := 0
	var archived []archiveRecord
	for _, r := range p.archives[ch] {
		if r.called {
			archived = append(archived, r)
		}
	}
	p.mu.Unlock()
	archives := 0
	for _, c := range p.f.calls(pre) {
		pc, kind := p.callOf(ch, pre, c, called, &responds)
		if strings.HasPrefix(c.Method, "views.") && executes < len(executed) {
			r := executed[executes]
			executes++
			pc = conformance.ProviderCall{
				Capability: model.CapabilityExtensionExecute, Applied: c.Applied, Route: r.req.Route, Audience: r.req.Audience,
				Operation: r.req.Operation, Attempt: r.req.Attempt, Action: ptr(r.req.Action),
			}
			if c.Applied {
				pc.References = []model.ExternalRef{{Channel: ch, Namespace: Namespace, Kind: RefKindView, ID: "V1001"}}
			}
			kind = ""
		}
		if (c.Method == "conversations.archive" || c.Method == "conversations.unarchive") && archives < len(archived) {
			r := archived[archives]
			archives++
			pc = conformance.ProviderCall{
				Capability: model.CapabilityArchive, Applied: c.Applied, Route: r.req.Route, Audience: r.req.Audience,
				Operation: r.req.Operation, Attempt: r.req.Attempt, Items: r.req.Items, Undo: r.req.Undo,
			}
			kind = ""
		}
		switch kind {
		case "skip":
		case "read":
			if !reading {
				out = append(out, conformance.ProviderCall{Capability: model.CapabilityReconcile})
			}
			reading = true
		default:
			reading = false
			out = append(out, pc)
		}
	}
	return out
}

func (p *provider) callOf(ch model.ChannelRef, pre string, c fakeCall, responds []respondRecord, nextRespond *int) (conformance.ProviderCall, string) {
	var args map[string]any
	_ = json.Unmarshal(c.Body, &args)
	x := &Instance{channel: model.Channel{Ref: ch}}
	pc := conformance.ProviderCall{Applied: c.Applied, Audience: model.Audience{Visibility: model.Public}}
	target := func() model.ExternalRef {
		return model.ProviderMessageRef(ch, str(args, "channel")+":"+firstOf(str(args, "ts"), str(args, "timestamp")))
	}
	switch c.Method {
	case "chat.postMessage":
		pc.Capability = model.CapabilitySend
		pc.Route = p.routeOf(x, pre, str(args, "channel"), str(args, "thread_ts"))
		pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: unescape(str(args, "text"))}}}
		if md, ok := args["metadata"].(map[string]any); ok {
			payload, _ := md["event_payload"].(map[string]any)
			pc.Operation = model.OperationRef{Tenant: ch.Tenant, ID: model.ID(str(payload, "operation"))}
			pc.Attempt = model.ID(str(payload, "attempt"))
		}
		if c.Applied {
			pc.References = []model.ExternalRef{model.ProviderMessageRef(ch, c.Channel+":"+c.TS)}
		}
	case "chat.update", "chat.delete":
		pc.Capability, pc.Target = model.CapabilityEdit, target()
		if c.Method == "chat.delete" {
			pc.Capability = model.CapabilityDelete
		}
		pc.Route = p.routeOfMessage(x, pre, str(args, "channel"), str(args, "ts"))
		if c.Applied && pc.Capability == model.CapabilityEdit {
			pc.References = []model.ExternalRef{pc.Target}
		}
		pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: unescape(str(args, "text"))}}}
		if md, ok := args["metadata"].(map[string]any); ok {
			payload, _ := md["event_payload"].(map[string]any)
			pc.Operation = model.OperationRef{Tenant: ch.Tenant, ID: model.ID(str(payload, "operation"))}
			pc.Attempt = model.ID(str(payload, "attempt"))
		}
	case "reactions.add", "reactions.remove":
		pc.Capability, pc.Target, pc.Remove = model.CapabilityReact, target(), c.Method == "reactions.remove"
		pc.Emoji, _ = reactionEmoji(str(args, "name"))
		pc.Route = p.routeOfMessage(x, pre, str(args, "channel"), str(args, "timestamp"))
	case "response_url":
		pc.Capability = model.CapabilityRespond
		if *nextRespond < len(responds) {
			r := responds[*nextRespond]
			*nextRespond++
			pc.Mode, pc.FollowUp, pc.Audience = r.req.Mode, r.req.FollowUp, r.req.Audience
			if ic := r.req.Interaction; ic != nil {
				pc.Interaction, pc.Token = ic.Ref, ic.Token
			}
			if r.req.Target != nil {
				pc.Target = *r.req.Target
			}
			pc.Operation = r.req.Operation
		}
	case "reactions.get", "conversations.replies", "conversations.info":
		return pc, "read"
	case "conversations.history":
		if args["include_all_metadata"] == true {
			return pc, "read"
		}
		return pc, "skip"
	default:
		return pc, "skip"
	}
	return pc, ""
}

func firstOf(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// routeOf is the route of a message posted to conversation (and thread).
func (p *provider) routeOf(x *Instance, pre, conversation, thread string) model.ExternalRef {
	switch p.mode {
	case model.ModePerThread:
		if thread != "" {
			return x.threadRef(conversation, thread)
		}
	case model.ModePerContact:
		if user := p.f.userOfDM(pre, conversation); user != "" {
			return x.userRef(user)
		}
	}
	return x.roomRef(conversation)
}

func (p *provider) routeOfMessage(x *Instance, pre, conversation, ts string) model.ExternalRef {
	thread := ""
	if m := p.f.find(pre, conversation, ts); m != nil {
		thread = m.threadTS
	}
	return p.routeOf(x, pre, conversation, thread)
}

// unescape reads the entities text carries.
func unescape(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
}

// ItemState is what the account holds for the message ref names: Slack
// archives the channel, so every message of it is archived at once.
func (p *provider) ItemState(ch model.ChannelRef, ref model.ExternalRef) (conformance.ItemState, bool) {
	conv, ts, ok := parseMessageID(ref.ID)
	if !ok || p.f.find(p.prefix(ch), conv, ts) == nil {
		return conformance.ItemState{}, false
	}
	return conformance.ItemState{Archived: p.f.archivedChannel(p.prefix(ch), conv)}, true
}

// DeliverState: Slack reports an archive as channel_archive and
// channel_unarchive, naming the channel and no message, and the adapter
// reports the channel's room as the conversation. The suite's conversation
// is seeded and sent into, never selected by a room, and the engine
// resolves a room reference only through the room's selection candidates,
// so a report of the room finds no conversation here: the inbound cases
// skip. TestArchiveEvents covers the events.
func (p *provider) DeliverState(model.ChannelRef, conformance.StateEvent) error {
	return conformance.ErrNotExpressible
}
