package teams

import (
	"context"
	"hash/fnv"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// The adapter passes the shared suite for receive_http, send,
// start_conversation, the message mutations, signals and interaction
// reports, once for each way a bot's conversations divide: per room (a
// group chat for each peer), per thread (one channel, a thread for each
// peer) and per contact (a personal chat with each user), against the
// stand-in Bot Framework of fakeconnector_test.go.
//
// Teams puts no marker of the sender's own on a message, so nothing it
// sends back says which operation made a call. The stand-in is told, as a
// test double of the engine's own record: each effect the adapter performs
// is recorded with the requests it made, and the provider's calls are
// those requests under the operation and attempt of the effect.
func TestAdapterConformance(t *testing.T) {
	for _, mode := range []string{model.ModePerRoom, model.ModePerThread, model.ModePerContact} {
		t.Run(mode, func(t *testing.T) {
			sel := model.Selection{Mode: mode}
			if mode == model.ModePerContact {
				sel = model.Selection{} // the suite's default: per contact, locked to one conversation
			}
			conformance.RunAdapterSuite(t, conformance.AdapterHarness{
				New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
					p := &provider{mode: mode}
					p.f = newFakeTeams(p.now)
					return &recordingFactory{Factory: NewFactory(Options{OpenIDMetadataURL: fakeMetadataURL}), p: p}, p
				},
				Capabilities:    Capabilities(),
				Selection:       sel,
				NoResponseToken: true,
			})
		})
	}
}

// provider is the conformance view of the stand-in: one account (a service
// URL) per channel, and the way its peers map to Teams, by mode:
//
//   - per room: a peer has a group chat of its own;
//   - per thread: peers share one channel and have a thread each;
//   - per contact: a peer is a user with a personal chat with the bot.
type provider struct {
	mode string
	f    *fakeTeams

	mu       sync.Mutex
	clock    ports.Clock
	prefixes map[model.ChannelRef]string
	numbers  map[string]int64
	hook     map[model.ChannelRef][]conformance.InboundEvent
	results  map[model.ChannelRef][]conformance.InboundResult
	effects  map[model.ChannelRef][]effectRecord
}

var (
	_ conformance.HTTPProvider    = (*provider)(nil)
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
	_ conformance.EventShaper     = (*provider)(nil)
)

// now is the engine's clock once an instance is open, so the stand-in's
// tokens and timestamps follow the suite's.
func (p *provider) now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clock != nil {
		return p.clock.Now()
	}
	return testNow
}

// effectRecord is one effect the adapter performed and the requests it
// made for it, as a range of the account's calls.
type effectRecord struct {
	capability model.Capability
	eff        adapter.Effect
	content    model.Content
	emoji      string
	remove     bool
	kind       string
	from, to   int
}

// recordingFactory opens instances that tell the provider the clock and
// each effect they perform.
type recordingFactory struct {
	*Factory
	p *provider
}

func (f *recordingFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	inst, err := f.Factory.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	f.p.mu.Lock()
	f.p.clock = deps.Clock
	f.p.mu.Unlock()
	return &recordingInstance{x: inst.(*Instance), p: f.p, ch: ch.Ref}, nil
}

// recordingInstance forwards to the adapter's instance and implements
// exactly its interfaces.
type recordingInstance struct {
	x  *Instance
	p  *provider
	ch model.ChannelRef
}

func (i *recordingInstance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	return i.x.Capabilities(ctx)
}
func (i *recordingInstance) Close(ctx context.Context) error     { return i.x.Close(ctx) }
func (i *recordingInstance) Handler(s adapter.Sink) http.Handler { return i.x.Handler(s) }

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

func (i *recordingInstance) Signal(ctx context.Context, r adapter.SignalRequest) error {
	_, err := i.record(effectRecord{capability: model.CapabilitySignal, eff: adapter.Effect{Route: r.Route, Audience: r.Audience}, kind: r.Kind}, func() (adapter.Outcome, error) {
		return adapter.Outcome{}, i.x.Signal(ctx, r)
	})
	return err
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
		a := p.f.account(pre, testBot, testTenantID, conformance.CredentialSecret)
		a.lenient, a.assume = true, true
	}
	return pre
}

// HTTPClient is the client the suite's engine injects.
func (p *provider) HTTPClient() ports.HTTPClient { return p.f.client() }

func hashOf(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// conversationOf is the conversation a peer's messages are in, as Teams
// names it in an activity, and its conversation type.
func (p *provider) conversationOf(sender string) (string, string) {
	switch p.mode {
	case model.ModePerThread:
		return threadConversation(testTeamChannel, p.rootOf(sender)), "channel"
	case model.ModePerContact:
		return "a:1personal-" + sender[len("29:"):], "personal"
	}
	return "19:room-" + strconv.FormatUint(uint64(hashOf(sender)), 10) + "@thread.v2", "groupChat"
}

// rootOf is the root message of a peer's thread, in per-thread mode.
func (p *provider) rootOf(sender string) string {
	return strconv.FormatUint(1_600_000_000_000+uint64(hashOf(sender)%100_000), 10)
}

// Account configures the account's service URL.
func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	cfg := Config{AppID: testAppID, TenantID: testTenantID, ServiceURL: p.f.serviceURL(p.prefix(ch))}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "teams"}
}

// Route is where a peer's conversation lives, as the adapter normalizes
// it. In per-contact mode the app is installed for the peer, so the
// Connector creates its personal chat.
func (p *provider) Route(ch model.ChannelRef, sender string) model.ExternalRef {
	x := &Instance{channel: model.Channel{Ref: ch}}
	pre := p.prefix(ch)
	conv, _ := p.conversationOf(sender)
	p.f.mu.Lock()
	a := p.f.accounts[pre]
	a.conv(conv)
	if p.mode == model.ModePerContact {
		a.installed[sender] = true
		a.personal[sender] = conv
	}
	p.f.mu.Unlock()
	switch p.mode {
	case model.ModePerThread:
		return x.threadRef(testTeamChannel, p.rootOf(sender))
	case model.ModePerContact:
		return x.userRef(sender)
	}
	return x.roomRef(conv)
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []scripted
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, failWith(http.StatusBadRequest, "BadArgument"))
		case conformance.OutcomeUnauthorized:
			as = append(as, failWith(http.StatusForbidden, "ForbiddenOperationException"))
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

// Revoke: a bot's rights are its installation, not scopes on its token;
// what it may do shows only in the Connector's answers.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

// number is a stable number for a fixture value, increasing in the order
// values are first named.
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

// ShapeID gives fixture values Teams' forms, stable per value. A message
// ID is "<group chat>|<activity ID>"; ShapeEvent moves it to its peer's
// conversation.
func (p *provider) ShapeID(ch model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return "29:1u" + strconv.FormatUint(uint64(hashOf(id)), 36)
	case conformance.FixtureDestination:
		return testTenantID
	case conformance.FixtureRoom:
		return testTeamChannel
	case conformance.FixtureEvent:
		return "f:" + strconv.FormatInt(p.number("event", id, 9000), 10)
	case conformance.FixtureMessage:
		return testGroupChat + "|" + strconv.FormatInt(p.number("message", id, 1_650_000_000_000), 10)
	}
	return id
}

// ShapeEvent places an event in its peer's conversation.
func (p *provider) ShapeEvent(ch model.ChannelRef, ev conformance.InboundEvent) conformance.InboundEvent {
	if len(ev.Sender) < 4 {
		return ev
	}
	conv, _ := p.conversationOf(ev.Sender)
	ev.Room = conv
	if ev.MessageID != "" {
		_, id, ok := parseMessageID(ev.MessageID)
		if !ok {
			id = ev.MessageID
		}
		ev.MessageID = baseConversation(conv) + idSeparator + id
	}
	return ev
}

// Calls maps the effects the adapter performed to provider calls: the
// Connector request that carried each effect (the message posted after a
// personal chat was created), however many requests it took.
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
	main := calls[len(calls)-1]
	pc := conformance.ProviderCall{
		Capability: rec.capability, Operation: rec.eff.Operation, Attempt: rec.eff.Attempt, Applied: main.Applied,
		Audience: model.Audience{Visibility: model.Public}, Route: p.routeOf(x, pre, main),
	}
	target := model.ProviderMessageRef(ch, messageID(main.Conversation, main.Activity))
	switch rec.capability {
	case model.CapabilitySend:
		pc.Content = text(str(main.args(), "text"))
		if main.Applied {
			pc.References = []model.ExternalRef{target}
		}
	case model.CapabilityEdit, model.CapabilityDelete:
		pc.Target = target
		if rec.capability == model.CapabilityEdit {
			pc.Content = text(str(main.args(), "text"))
			if main.Applied {
				pc.References = []model.ExternalRef{target}
			}
		}
	case model.CapabilityReact:
		pc.Target = target
		pc.Emoji, pc.Remove = rec.emoji, rec.remove
	case model.CapabilitySignal:
		pc.Kind, pc.Applied, pc.Audience = rec.kind, main.Op == "typing", rec.eff.Audience
	}
	return pc
}

// routeOf is the route of the conversation a call reached: what the
// adapter stores for it.
func (p *provider) routeOf(x *Instance, pre string, c fakeCall) model.ExternalRef {
	conv := c.Conversation
	if u := p.f.userOfPersonal(pre, conv); u != "" && p.mode == model.ModePerContact {
		return x.userRef(u)
	}
	root := threadRoot(conv)
	if root == "" && c.Activity != "" {
		if m := p.f.find(pre, conv, c.Activity); m != nil {
			root = m.root
		}
	}
	if p.mode == model.ModePerThread && root != "" {
		return x.threadRef(conv, root)
	}
	return x.roomRef(conv)
}
