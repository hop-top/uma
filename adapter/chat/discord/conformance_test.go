package discord

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
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

// The adapter passes the shared suite once per transport and per way a
// bot's channels divide into conversations: per room, per thread (each
// peer a thread of one channel) and per contact (each peer a direct
// message), against the stand-in Discord of fakediscord_test.go and
// fakegateway_test.go.
//
// The suite drives a receiving adapter through Runtime.Receive, so it runs
// as the gateway receiver and as the Interactions Endpoint URL alone (the
// descriptor it is given leaves out receive). Over the endpoint only
// interactions arrive: the suite's message cases are not expressible there.
//
// The descriptor the suite is given also leaves out send_ephemeral,
// everywhere: Discord shows an ephemeral message only in answer to an
// interaction, and the suite's ephemeral case names none (the package's
// respond tests cover it); and membership and archive outside per thread:
// only a thread has members the bot can change, and only a thread archives.
func TestAdapterConformance(t *testing.T) {
	for _, transport := range []ReceiveMode{ReceiveGateway, ReceiveInteractions} {
		for _, mode := range []string{model.ModePerRoom, model.ModePerThread, model.ModePerContact} {
			t.Run(string(transport)+"/"+mode, func(t *testing.T) {
				sel := model.Selection{Mode: mode}
				if mode == model.ModePerContact {
					sel = model.Selection{} // the suite's default: per contact, locked to one conversation
				}
				caps := suiteCapabilities(transport, mode)
				conformance.RunAdapterSuite(t, conformance.AdapterHarness{
					New: func(t *testing.T) (adapter.Factory, conformance.Provider) {
						p := &provider{mode: mode, transport: transport}
						p.f = newFakeDiscord(p.now)
						t.Cleanup(p.f.close)
						return &recordingFactory{Factory: NewFactory(), p: p, caps: caps}, p
					},
					Capabilities: caps,
					Selection:    sel,
				})
			})
		}
	}
}

func suiteCapabilities(transport ReceiveMode, mode string) []model.Capability {
	return slices.DeleteFunc(slices.Clone(Capabilities()), func(c model.Capability) bool {
		return c == model.CapabilitySendEphemeral ||
			((c == model.CapabilityMembership || c == model.CapabilityArchive) && mode != model.ModePerThread) ||
			(c == model.CapabilityReceive && transport == ReceiveInteractions)
	})
}

// suiteNow is when the suite's clock starts.
var suiteNow = time.Unix(1_700_000_000, 0).UTC()

// suiteKey is the application key the stand-in signs interactions with;
// the suite's host resolves its own secret for every purpose, so the
// harness answers the public key purpose with this key's public half.
var suiteKey = ed25519.NewKeyFromSeed(sha256Sum(conformance.CredentialSecret))

type provider struct {
	mode      string
	transport ReceiveMode
	f         *fakeDiscord

	mu       sync.Mutex
	clock    ports.Clock
	prefixes map[model.ChannelRef]string
	order    uint64
	ids      map[string]string
	queued   map[model.ChannelRef][]conformance.InboundEvent
	seqs     map[model.ChannelRef]map[int64]string
	results  map[model.ChannelRef][]conformance.InboundResult

	// effects records, per account, which effect each run of calls
	// served, by the index of its first call.
	effects map[string][]recorded
	// serial runs one effect at a time, so its calls are its own.
	serial sync.Mutex
}

// recorded is one effect the engine handed the adapter and the calls it
// made, [from, to).
type recorded struct {
	from, to   int
	capability model.Capability
	effect     adapter.Effect
	mode       string
	followUp   bool
	emoji      string
	remove     bool
	kind       string
	member     model.ExternalRef
	route      model.ExternalRef
	items      []model.ExternalRef
	undo       bool
}

var (
	_ conformance.HTTPProvider    = (*provider)(nil)
	_ conformance.WebhookProvider = (*provider)(nil)
	_ conformance.FixtureShaper   = (*provider)(nil)
	_ conformance.EventShaper     = (*provider)(nil)
	_ conformance.HistoryProvider = (*provider)(nil)
	_ conformance.StateProvider   = (*provider)(nil)
)

func (p *provider) now() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.clock != nil {
		return p.clock.Now()
	}
	return suiteNow
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
		a := p.f.account(pre, conformance.CredentialSecret, testBotUser, testApp)
		a.lenient = true
		a.gw.onReady = func(c *fakeGWConn) { p.pushGateway(ch, c) }
	}
	return pre
}

func (p *provider) HTTPClient() ports.HTTPClient { return p.f.client() }

// snowflakeOf maps a fixture value to a snowflake under base, stable and
// distinct per value.
func snowflakeOf(base uint64, s string) string {
	h := fnv.New64a()
	h.Write([]byte(s))
	return strconv.FormatUint(base+h.Sum64()%1_000_000_000_000_000, 10)
}

const (
	baseUser    = 1_100_000_000_000_000_000
	baseRoom    = 1_300_000_000_000_000_000
	baseThread  = 1_500_000_000_000_000_000
	baseDM      = 1_600_000_000_000_000_000
	baseEvent   = 1_700_000_000_000_000_000
	threadRoom  = "1400000000000000001"
	defaultRoom = "1400000000000000002"
)

// roomOf is the channel of a peer's messages: its room, its thread of the
// shared room, or its direct message.
func (p *provider) roomOf(sender string) string {
	switch p.mode {
	case model.ModePerThread:
		return snowflakeOf(baseThread, sender)
	case model.ModePerContact:
		return snowflakeOf(baseDM, sender)
	}
	return snowflakeOf(baseRoom, sender)
}

// Account configures the account: every message reported (the suite's
// messages mention no bot), and the peers' channels for channel-wide
// history.
func (p *provider) Account(ch model.ChannelRef) conformance.Account {
	pre := p.prefix(ch)
	all := false
	cfg := Config{ApplicationID: testApp, BotUserID: testBotUser, APIURL: p.f.apiURL(pre), GatewayURL: "wss://" + fakeGatewayHost,
		MentionsOnly: &all, Receive: p.transport}
	for _, peer := range []string{"peer-1", "peer-2", "peer-3", "peer-4", "peer-5"} {
		sender := p.ShapeID(ch, conformance.FixtureSender, peer)
		p.ensure(pre, sender)
		cfg.HistoryChannels = append(cfg.HistoryChannels, p.roomOf(sender))
	}
	return conformance.Account{Configuration: cfg.Extension(), CredentialRef: "discord"}
}

// ensure makes the peer's channel exist.
func (p *provider) ensure(pre, sender string) {
	conv := p.roomOf(sender)
	switch p.mode {
	case model.ModePerThread:
		p.f.thread(pre, threadRoom, conv)
	case model.ModePerContact:
		p.f.holdDM(pre, sender, conv)
	default:
		p.f.mu.Lock()
		p.f.accounts[pre].channel(conv)
		p.f.mu.Unlock()
	}
}

func (p *provider) Route(ch model.ChannelRef, sender string) model.ExternalRef {
	p.ensure(p.prefix(ch), sender)
	x := &Instance{channel: model.Channel{Ref: ch}}
	switch p.mode {
	case model.ModePerThread:
		return x.threadRef(threadRoom, p.roomOf(sender))
	case model.ModePerContact:
		return x.userRef(sender)
	}
	return x.roomRef(p.roomOf(sender))
}

// Revoke: a bot's permissions show only in Discord's answers.
func (p *provider) Revoke(model.ChannelRef, model.Capability) error {
	return conformance.ErrNotExpressible
}

func (p *provider) Respond(ch model.ChannelRef, rs ...conformance.Response) error {
	var as []answer
	for _, r := range rs {
		switch r.Outcome {
		case conformance.OutcomeAccepted:
			as = append(as, accepted)
		case conformance.OutcomeRejected:
			as = append(as, refusal(http.StatusBadRequest, codeInvalidFormBody))
		case conformance.OutcomeUnauthorized:
			as = append(as, refusal(http.StatusForbidden, codeEditOthers))
		case conformance.OutcomeTransient:
			as = append(as, unreachable)
		case conformance.OutcomeRateLimited:
			as = append(as, rateLimitedFor(r.RetryAfter, false))
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

// historyOffsets are the times, from the start of the history cases'
// window, of the messages those cases hold, in the order they need: a
// snowflake is a message's ID and its time at once, and the suite names IDs
// before times.
var historyOffsets = map[string]time.Duration{
	"msg-h-old": -time.Minute, "msg-h-1": 0, "msg-live": 90 * time.Minute, "msg-h-2": 3 * time.Hour, "msg-h-3": 4 * time.Hour,
	"msg-h-4": 5 * time.Hour, "msg-h-5": 6 * time.Hour, "msg-a-1": 7 * time.Hour, "msg-b-1": 8 * time.Hour, "msg-a-2": 9 * time.Hour,
}

// ShapeID gives fixture values Discord's forms: snowflakes, and a message
// ID "<channel>:<id>" in the default room, which ShapeEvent moves to its
// peer's channel.
func (p *provider) ShapeID(_ model.ChannelRef, kind conformance.FixtureKind, id string) string {
	switch kind {
	case conformance.FixtureSender:
		return snowflakeOf(baseUser, id)
	case conformance.FixtureDestination:
		return testApp
	case conformance.FixtureRoom:
		return defaultRoom
	case conformance.FixtureEvent:
		return snowflakeOf(baseEvent, id)
	case conformance.FixtureMessage:
		return defaultRoom + ":" + p.stamp(id)
	}
	return id
}

func (p *provider) stamp(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ids == nil {
		p.ids = map[string]string{}
	}
	s, ok := p.ids[id]
	if !ok {
		p.order++
		at := suiteNow.Add(time.Duration(len(p.ids)+1) * time.Millisecond)
		if off, held := historyOffsets[id]; held {
			at = suiteNow.Add(-24*time.Hour + off)
		}
		s = sid(at, p.order)
		p.ids[id] = s
	}
	return s
}

// ShapeEvent places an event in its peer's channel.
func (p *provider) ShapeEvent(_ model.ChannelRef, ev conformance.InboundEvent) conformance.InboundEvent {
	if ev.Sender == "" {
		return ev
	}
	conv := p.roomOf(ev.Sender)
	ev.Room = conv
	if ev.MessageID != "" {
		_, id, ok := strings.Cut(ev.MessageID, ":")
		if !ok {
			id = ev.MessageID
		}
		ev.MessageID = conv + ":" + id
	}
	return ev
}

// HoldHistory stores a past message in its sender's channel. Its snowflake
// was given by ShapeID; its timestamp is at.
func (p *provider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	pre := p.prefix(ch)
	p.ensure(pre, ev.Sender)
	conv, id, _ := strings.Cut(ev.MessageID, ":")
	if p.f.find(pre, conv, id) == nil {
		m := p.f.put(pre, conv, ev.Sender, ev.Text, id)
		m.at = at
	}
	return nil
}

// Calls maps the account's requests to capabilities, by the effect that
// made them: the request that applies an effect is its call; the reads of a
// reconciliation are one reconcile call; history reads, thread and direct
// message lookups and interaction acknowledgements are no capability's.
func (p *provider) Calls(ch model.ChannelRef) []conformance.ProviderCall {
	pre := p.prefix(ch)
	calls := p.f.calls(pre)
	p.mu.Lock()
	effects := slices.Clone(p.effects[pre])
	p.mu.Unlock()
	var out []conformance.ProviderCall
	for _, e := range effects {
		reading := false
		for i := e.from; i < e.to && i < len(calls); i++ {
			c := calls[i]
			if e.capability == model.CapabilityReconcile {
				if c.Method == http.MethodGet && !reading {
					out = append(out, conformance.ProviderCall{Capability: model.CapabilityReconcile, Operation: e.effect.Operation})
					reading = true
				}
				continue
			}
			if pc, ok := p.callOf(ch, e, c); ok {
				out = append(out, pc)
			}
		}
	}
	return out
}

func (p *provider) callOf(ch model.ChannelRef, e recorded, c fakeCall) (conformance.ProviderCall, bool) {
	segs := strings.Split(c.Path, "/")
	var args map[string]any
	_ = json.Unmarshal(c.Body, &args)
	pc := conformance.ProviderCall{
		Capability: e.capability, Operation: e.effect.Operation, Attempt: e.effect.Attempt, Route: e.route,
		Applied: c.Applied, Audience: e.effect.Audience,
	}
	if e.effect.Target != nil {
		pc.Target = *e.effect.Target
	}
	content, _ := args["content"].(string)
	switch {
	case e.capability == model.CapabilitySend && c.Method == http.MethodPost && len(segs) == 3 && segs[2] == "messages":
		pc.Content = text(content)
		if c.Applied {
			pc.References = []model.ExternalRef{model.ProviderMessageRef(ch, messageID(segs[1], c.Message))}
		}
	case e.capability == model.CapabilityEdit && c.Method == http.MethodPatch:
		pc.Content = text(content)
		if c.Applied {
			pc.References = []model.ExternalRef{pc.Target}
		}
	case e.capability == model.CapabilityDelete && c.Method == http.MethodDelete:
	case e.capability == model.CapabilityReact && len(segs) == 7 && segs[4] == "reactions":
		pc.Emoji, pc.Remove = e.emoji, e.remove
	case e.capability == model.CapabilitySignal && len(segs) == 3 && segs[2] == "typing":
		pc.Kind = e.kind
	case e.capability == model.CapabilityMembership && len(segs) == 4 && segs[2] == "thread-members" && c.Method != http.MethodGet:
		pc.Participant, pc.Remove = e.member, e.remove
	case e.capability == model.CapabilityArchive && c.Method == http.MethodPatch && len(segs) == 2:
		pc.Items, pc.Undo = e.items, e.undo
	case e.capability == model.CapabilityRespond && segs[0] == "webhooks" && c.Method != http.MethodGet:
		pc.Mode, pc.FollowUp, pc.Content = e.mode, e.followUp, text(content)
		if ic := e.effect.Interaction; ic != nil {
			pc.Interaction, pc.Token = ic.Ref, ic.Token
		}
	default:
		return pc, false
	}
	return pc, true
}

// suiteCredentials answers the interactions purpose with suiteKey's public
// half, after the suite's own port answered it, so a revoked consent still
// refuses.
type suiteCredentials struct{ next ports.Credentials }

func (s suiteCredentials) Resolve(ctx context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	c, err := s.next.Resolve(ctx, r)
	if err != nil || r.Purpose != CredentialPurposeInteractions {
		return c, err
	}
	return ports.Secret{Bytes: []byte(hex.EncodeToString(suiteKey.Public().(ed25519.PublicKey)))}, nil
}
