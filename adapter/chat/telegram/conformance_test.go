package telegram

import (
	"context"
	"hash/fnv"
	"net/http"
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

// The adapter passes the shared suite for receive_http, send, the message
// mutations and the interactions, once for each way a bot's chats divide into
// conversations: per room (a group chat for each peer), per thread (one forum
// topic of a group for each peer) and per contact (a private chat with each
// user), against the documented Bot API shapes of fake_test.go.
//
// Telegram has no marker to put on a message, so nothing it sends back says
// which operation made a call. The scripted bot is told, as a test double of
// the engine's own record: each effect the adapter performs is recorded with
// the requests it made, and the provider's calls are those requests under the
// operation and attempt of the effect.
//
// The suite drives a receiving adapter through Runtime.Receive and never
// through its webhook, and Telegram's two ways of receiving exclude each
// other, so it runs for each: as the webhook alone (the descriptor the suite
// is given leaves out receive, and the instance has no receiver) and as the
// long-polling receiver, which reads the updates the scripted bot queued.
func TestAdapterConformance(t *testing.T) {
	for _, transport := range []string{"webhook", "polling"} {
		for _, mode := range []string{model.ModePerRoom, model.ModePerThread, model.ModePerContact} {
			t.Run(transport+"/"+mode, func(t *testing.T) {
				sel := model.Selection{Mode: mode}
				if mode == model.ModePerContact {
					sel = model.Selection{} // the suite's default: per contact, locked to one conversation
				}
				caps := NewFactory(Options{}).Describe().Capabilities
				if transport == "webhook" {
					caps = slices.DeleteFunc(slices.Clone(caps), func(c model.Capability) bool { return c == model.CapabilityReceive })
				}
				conformance.RunAdapterSuite(t, conformance.AdapterHarness{
					New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
						p := &provider{mode: mode, polling: transport == "polling"}
						p.f = newFakeTelegram(p.now)
						return &recordingFactory{Factory: NewFactory(Options{PollInterval: time.Millisecond}), p: p, caps: caps}, p
					},
					Capabilities: caps,
					Selection:    sel,
				})
			})
		}
	}
}

// provider is the conformance view of the scripted bot: one account (an API
// root) per channel, and the way its peers map to Telegram, by mode:
//
//   - per room: a peer has a group chat of its own;
//   - per thread: peers share one forum supergroup and have a topic each;
//   - per contact: a peer is a user and has a private chat with the bot.
type provider struct {
	mode string
	// polling makes the adapter read updates with getUpdates: a delivered
	// event is queued at the scripted bot, not posted to the webhook.
	polling bool
	f       *fakeTelegram

	mu       sync.Mutex
	clock    ports.Clock
	prefixes map[model.ChannelRef]string
	numbers  map[string]int64
	hook     map[model.ChannelRef][]conformance.InboundEvent
	results  map[model.ChannelRef][]conformance.InboundResult
	effects  map[model.ChannelRef][]effectRecord
	// byMessage and byUpdate name the suite's event an envelope reports, for
	// the results of a receiver: by message ID for a new message, by update
	// ID for the rest.
	byMessage map[string]string
}

var (
	_ conformance.HTTPProvider    = (*provider)(nil)
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
	_ conformance.EventShaper     = (*provider)(nil)
)

// now is the engine's clock once an instance is open, so Telegram's dates
// follow the suite's.
func (p *provider) now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clock != nil {
		return p.clock.Now()
	}
	return testNow
}

// effectRecord is one effect the adapter performed and the requests it made
// to the bot for it, as a range of the account's calls.
type effectRecord struct {
	capability model.Capability
	eff        adapter.Effect
	content    model.Content
	emoji      string
	remove     bool
	mode       string
	followUp   bool
	from, to   int
}

// recordingFactory opens instances that tell the provider the clock and each
// effect they perform. It declares the capabilities of the run, which leave
// out receive for the webhook.
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
	r := &recordingInstance{x: inst.(*Instance), p: f.p, ch: ch.Ref}
	if f.p.polling {
		return &pollingInstance{recordingInstance: r}, nil
	}
	return r, nil
}

// recordingInstance forwards to the adapter's instance, so that it
// implements exactly the interfaces of a webhook-only adapter.
type recordingInstance struct {
	x  *Instance
	p  *provider
	ch model.ChannelRef
}

func (i *recordingInstance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := i.x.Capabilities(ctx)
	if !i.p.polling {
		st = slices.DeleteFunc(st, func(s model.CapabilityStatus) bool { return s.Name == model.CapabilityReceive })
	}
	return st, err
}
func (i *recordingInstance) Close(ctx context.Context) error     { return i.x.Close(ctx) }
func (i *recordingInstance) Handler(s adapter.Sink) http.Handler { return i.x.Handler(s) }

// pollingInstance is the receiving adapter: it also runs the receiver, over
// a sink that tells the provider what the engine accepted.
type pollingInstance struct{ *recordingInstance }

func (i *pollingInstance) Run(ctx context.Context, sink adapter.Sink) error {
	return i.x.Run(ctx, &resultSink{Sink: sink, p: i.p, ch: i.ch})
}

// resultSink records which delivered events the engine accepted: what a
// webhook shows by its status, a receiver shows by what it handed over and
// what the sink said.
type resultSink struct {
	adapter.Sink
	p  *provider
	ch model.ChannelRef
}

func (s *resultSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	rec, err := s.Sink.Accept(ctx, in)
	s.p.mu.Lock()
	if ev, ok := s.p.eventOf(in.Envelope); ok {
		if s.p.results == nil {
			s.p.results = map[model.ChannelRef][]conformance.InboundResult{}
		}
		s.p.results[s.ch] = append(s.p.results[s.ch], conformance.InboundResult{EventID: ev, Acked: err == nil})
	}
	s.p.mu.Unlock()
	return rec, err
}

// eventOf is the suite's event an envelope reports; p.mu is held. An update's
// ID is the event ID of the suite, a new message is known by its message ID.
func (p *provider) eventOf(env model.Envelope) (string, bool) {
	for _, prefix := range []string{"update:", "callback:"} {
		if rest, ok := strings.CutPrefix(env.EventID, prefix); ok {
			id, _, _ := strings.Cut(rest, ":")
			return id, true
		}
	}
	ev, ok := p.byMessage[env.MessageID]
	return ev, ok
}

func (i *recordingInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	return i.record(effectRecord{capability: model.CapabilitySend, eff: r.Effect, content: r.Content}, func() (adapter.Outcome, error) { return i.x.Send(ctx, r) })
}

func (i *recordingInstance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	return i.record(effectRecord{capability: model.CapabilityEdit, eff: r.Effect, content: r.Content}, func() (adapter.Outcome, error) { return i.x.Edit(ctx, r) })
}

func (i *recordingInstance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	return i.record(effectRecord{capability: model.CapabilityDelete, eff: r.Effect}, func() (adapter.Outcome, error) { return i.x.Delete(ctx, r) })
}

func (i *recordingInstance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	return i.record(effectRecord{capability: model.CapabilityReact, eff: r.Effect, emoji: r.Emoji, remove: r.Remove}, func() (adapter.Outcome, error) { return i.x.React(ctx, r) })
}

func (i *recordingInstance) Respond(ctx context.Context, r adapter.RespondRequest) (adapter.Outcome, error) {
	return i.record(effectRecord{capability: model.CapabilityRespond, eff: r.Effect, content: r.Content, mode: r.Mode, followUp: r.FollowUp}, func() (adapter.Outcome, error) { return i.x.Respond(ctx, r) })
}

// record runs do and notes the requests it made.
func (i *recordingInstance) record(rec effectRecord, do func() (adapter.Outcome, error)) (adapter.Outcome, error) {
	pre := i.p.prefix(i.ch)
	rec.from = len(i.p.f.calls(pre))
	out, err := do()
	rec.to = len(i.p.f.calls(pre))
	if rec.to > rec.from {
		i.p.mu.Lock()
		if i.p.effects == nil {
			i.p.effects = map[model.ChannelRef][]effectRecord{}
		}
		i.p.effects[i.ch] = append(i.p.effects[i.ch], rec)
		i.p.mu.Unlock()
	}
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
		a := p.f.account(pre, testBotID, conformance.CredentialSecret)
		a.lenient, a.assume = true, true
	}
	return pre
}

// HTTPClient is the client the suite's engine injects.
func (p *provider) HTTPClient() ports.HTTPClient { return p.f.client() }

// userOf is the Telegram user ID of a fixture peer: a stable number. The
// suite shapes a peer once (ShapeID) and hands the shaped value on, so every
// other function takes the ID as a decimal string.
func userOf(peer string) int64 {
	h := fnv.New32a()
	h.Write([]byte(peer))
	return 1_000_000 + int64(h.Sum32()%900_000_000)
}

const forumChat int64 = -1001000000000

// chatOf is the chat a peer's messages are in, and topicOf its topic, in
// per-thread mode only.
func (p *provider) chatOf(sender string) int64 {
	switch p.mode {
	case model.ModePerThread:
		return forumChat
	case model.ModePerContact:
		return userID(sender)
	}
	return -1_000_000_000_000 - userID(sender)
}

func (p *provider) topicOf(sender string) int64 {
	if p.mode != model.ModePerThread {
		return 0
	}
	return 2 + userID(sender)%100_000
}

// Account configures the account's API root.
func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := Config{BotID: testBotID, BotUsername: testBotName, APIURL: p.f.apiURL(p.prefix(ch))}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "telegram"}
}

// Route is where a peer's conversation lives, as the adapter normalizes it.
func (p *provider) Route(ch model.ChannelRef, sender string) model.ExternalRef {
	x := &Instance{channel: model.Channel{Ref: ch}}
	pre := p.prefix(ch)
	chat := p.chatOf(sender)
	p.f.mu.Lock()
	p.f.accounts[pre].chat(chat)
	p.f.mu.Unlock()
	switch p.mode {
	case model.ModePerThread:
		return x.threadRef(chat, p.topicOf(sender))
	case model.ModePerContact:
		return x.userRef(userID(sender))
	}
	return x.roomRef(chat)
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []scripted
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, failWith(400, "Bad Request: can't parse entities: Can't find end of the entity starting at byte offset 0"))
		case conformance.OutcomeUnauthorized:
			as = append(as, scripted{status: 403, body: `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked from the supergroup chat"}`, byMethod: map[string]scripted{
				"editMessageText": failWith(400, "Bad Request: message can't be edited"),
				"deleteMessage":   failWith(400, "Bad Request: message can't be deleted"),
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

// Revoke: a bot token carries no scopes; what it may do shows only in
// Telegram's answers, never before a call.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// number is a stable number for a fixture value, increasing in the order
// values are first named: Telegram's message_id and update_id grow.
func (p *provider) number(kind, id string, base int64) int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.numbers == nil {
		p.numbers = map[string]int64{}
	}
	k := kind + "/" + id
	n, ok := p.numbers[k]
	if !ok {
		p.numbers["next/"+kind]++
		n = base + p.numbers["next/"+kind]
		p.numbers[k] = n
	}
	return n
}

// ShapeID gives fixture values Telegram's forms, stable per value. A message
// ID is "<chat ID>:<message_id>" in the default chat; ShapeEvent moves it to
// its peer's.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return strconv.FormatInt(userOf(id), 10)
	case conformance.FixtureDestination:
		return idString(testBotID)
	case conformance.FixtureRoom:
		return idString(forumChat)
	case conformance.FixtureEvent:
		return strconv.FormatInt(p.number("update", id, 9000), 10)
	case conformance.FixtureMessage:
		return idString(forumChat) + ":" + strconv.FormatInt(p.number("message", id, 100), 10)
	}
	return id
}

// ShapeEvent places an event in its peer's conversation.
func (p *provider) ShapeEvent(ch model.ChannelRef, ev conformance.InboundEvent) conformance.InboundEvent {
	if len(ev.Sender) < 2 {
		return ev
	}
	chat := p.chatOf(ev.Sender)
	ev.Room = idString(chat)
	if p.mode == model.ModePerThread {
		ev.Room = idString(p.topicOf(ev.Sender))
	}
	if ev.MessageID != "" {
		_, id, ok := strings.Cut(ev.MessageID, ":")
		if !ok {
			id = ev.MessageID
		}
		ev.MessageID = idString(chat) + ":" + id
	}
	return ev
}

// Calls maps the effects the adapter performed to provider calls: a message
// posted is send, an edit edit, a deletion delete, a reaction call react and
// the calls of a response respond, however many requests it takes.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	pre := p.prefix(ch)
	all := p.f.calls(pre)
	p.mu.Lock()
	records := append([]effectRecord(nil), p.effects[ch]...)
	p.mu.Unlock()
	var out []conformance.ProviderCall
	for _, rec := range records {
		out = append(out, p.callOf(ch, pre, rec, all[rec.from:rec.to]))
	}
	return out
}

func (p *provider) callOf(ch model.ChannelRef, pre string, rec effectRecord, calls []fakeCall) conformance.ProviderCall {
	x := &Instance{channel: model.Channel{Ref: ch}}
	first := calls[0]
	args := first.args()
	pc := conformance.ProviderCall{
		Capability: rec.capability, Operation: rec.eff.Operation, Attempt: rec.eff.Attempt, Applied: first.Applied,
		Audience: model.Audience{Visibility: model.Public}, Route: p.routeOf(x, pre, first.Chat, num(args, "message_thread_id"), first.Message),
	}
	switch rec.capability {
	case model.CapabilitySend:
		pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: str(args, "text")}}}
		if first.Applied {
			pc.References = []model.ExternalRef{model.ProviderMessageRef(ch, messageID(first.Chat, first.Message))}
		}
	case model.CapabilityEdit, model.CapabilityDelete:
		pc.Target = model.ProviderMessageRef(ch, messageID(first.Chat, first.Message))
		if first.Applied && rec.capability == model.CapabilityEdit {
			pc.References = []model.ExternalRef{pc.Target}
		}
		if rec.capability == model.CapabilityEdit {
			pc.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: str(args, "text")}}}
		}
	case model.CapabilityReact:
		pc.Target = model.ProviderMessageRef(ch, messageID(first.Chat, first.Message))
		pc.Emoji, pc.Remove = rec.emoji, rec.remove
	case model.CapabilityRespond:
		pc.Mode, pc.FollowUp, pc.Audience = rec.mode, rec.followUp, rec.eff.Audience
		if ic := rec.eff.Interaction; ic != nil {
			pc.Interaction, pc.Token = ic.Ref, ic.Token
		}
		if rec.eff.Target != nil {
			pc.Target = *rec.eff.Target
		}
		pc.Route = rec.eff.Route
		if rec.mode == model.ResponseModeMessage && first.Applied {
			pc.References = []model.ExternalRef{model.ProviderMessageRef(ch, messageID(first.Chat, first.Message))}
		}
	}
	return pc
}

// routeOf is the route of a message in a chat (and topic): what the adapter
// stores for its conversation.
func (p *provider) routeOf(x *Instance, pre string, chat, thread, msg int64) model.ExternalRef {
	if thread == 0 && msg != 0 {
		if m := p.f.find(pre, chat, msg); m != nil {
			thread = m.thread
		}
	}
	switch p.mode {
	case model.ModePerThread:
		if thread != 0 {
			return x.threadRef(chat, thread)
		}
	case model.ModePerContact:
		if chat > 0 {
			return x.userRef(chat)
		}
	}
	return x.roomRef(chat)
}
