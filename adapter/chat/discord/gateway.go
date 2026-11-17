package discord

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// The gateway, as "Gateway" and "Gateway Events" document it
// (https://docs.discord.com/developers/events/gateway,
// https://docs.discord.com/developers/events/gateway-events) and the close
// codes of "Opcodes and Status Codes"
// (https://docs.discord.com/developers/topics/opcodes-and-status-codes):
//
//   - Get Gateway Bot answers the URL to connect to and the session start
//     limit: 1000 Identify calls a day, past which "all active sessions for
//     the app will be terminated, the bot token will be reset". The
//     receiver never identifies with none remaining.
//   - The URL takes v=10 and encoding=json. Discord says Hello (op 10) with
//     heartbeat_interval; the first Heartbeat (op 1, d the last sequence
//     number or null) waits interval * jitter, then one every interval. A
//     Heartbeat ACK (op 11) missing before the next beat is a zombied
//     connection: close it with a code other than 1000 or 1001, then
//     resume. Discord may ask for a beat (op 1); it is sent at once.
//   - Identify (op 2): token, intents, properties {os, browser, device}.
//     READY carries session_id and resume_gateway_url. Resume (op 6):
//     token, session_id and seq, the last sequence number, on
//     resume_gateway_url; Discord replays what was missed and says RESUMED.
//   - Reconnect (op 7) and Invalid Session (op 9) with d true: resume.
//     Invalid Session with d false: identify again. Closing with 1000 or
//     1001 invalidates the session, so the receiver never does.
//   - Close codes 4004 (authentication failed), 4010 to 4014 are not to be
//     reconnected: 4014 is a privileged intent the application was not
//     granted. 4007 (invalid seq), 4009 (session timed out) and 4003 need a
//     new session; the others may resume.
//   - 120 gateway events per connection per 60 seconds; the receiver sends
//     an Identify or Resume and heartbeats, and answers Discord's requests
//     for a beat at most once a second.

// Gateway opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatAck   = 11
)

// Gateway intents ("Gateway Intents"). MESSAGE_CONTENT is privileged.
const (
	intentGuilds                 = 1 << 0
	intentGuildMessages          = 1 << 9
	intentGuildMessageReactions  = 1 << 10
	intentDirectMessages         = 1 << 12
	intentDirectMessageReactions = 1 << 13
	intentMessageContent         = 1 << 15
)

// intents are what Identify asks for: guilds (THREAD_UPDATE, which reports a
// thread's archive state), guild and direct messages and their reactions,
// and MESSAGE_CONTENT unless Config.MentionsOnly, where Discord
// fills the content the adapter reports without it.
func (c Config) intents() int {
	n := intentGuilds | intentGuildMessages | intentGuildMessageReactions | intentDirectMessages | intentDirectMessageReactions
	if !c.mentionsOnly() {
		n |= intentMessageContent
	}
	return n
}

// MaxGatewayMessageBytes bounds one gateway message.
const MaxGatewayMessageBytes = 8 << 20

// gatewayTiming bounds a connection: the time a new one has to dial and say
// hello, the time a write may take, the base wait before identifying again
// after an invalid session, and the base wait between reconnections.
type gatewayTiming struct {
	hello, write, invalid, retry time.Duration
	// attempts is how many connections in a row may fail before Run ends
	// for the engine to reconnect after its backoff.
	attempts int
}

var defaultGatewayTiming = gatewayTiming{hello: 10 * time.Second, write: 5 * time.Second, invalid: time.Second, retry: time.Second, attempts: 3}

func randomJitter() float64 { return rand.Float64() } // #nosec G404 -- heartbeat jitter, not a secret

// payload is one gateway message.
type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// session is what resuming needs, and what the receiver cursor keeps.
type session struct {
	ID  string `json:"id"`
	Seq int64  `json:"s"`
	URL string `json:"u"`
}

// cursor encodes a session as a receiver cursor.
func (s session) cursor() string {
	b, _ := json.Marshal(struct {
		V int `json:"v"`
		session
	}{1, s})
	return string(b)
}

// parseCursor reads a receiver cursor; ok is false for anything else.
func parseCursor(c string) (session, bool) {
	var v struct {
		V int `json:"v"`
		session
	}
	if json.Unmarshal([]byte(c), &v) != nil || v.V != 1 || v.ID == "" || len(v.ID) > 128 || v.Seq < 0 {
		return session{}, false
	}
	return v.session, true
}

// Run receives over the gateway and hands what Discord dispatches to sink
// until ctx ends, which it returns.
//
// MESSAGE_CREATE, MESSAGE_UPDATE, MESSAGE_DELETE, MESSAGE_REACTION_ADD,
// MESSAGE_REACTION_REMOVE, THREAD_UPDATE and INTERACTION_CREATE are handled in lanes: the
// events of one channel one at a time, in the order dispatched, so an edit,
// a deletion or a reaction is never recorded ahead of its message, and up
// to gatewayLanes channels at once. An interaction has a lane of its own:
// its three seconds are not spent behind a channel's records. The gateway
// has no acknowledgement; each ingress carries the receiver cursor (the
// session, its resume URL and the highest sequence number below which
// every dispatch was handled), so a restarted Run resumes the session from
// there and Discord replays the rest, which core's duplicate check absorbs.
//
// A sink refusal that is retryable, or a denial, ends Run with it: the
// engine reconnects after a retryable one (resuming from the cursor, before
// the refused dispatch) and stops on a denial. Any other refusal drops the
// dispatch.
//
// Reconnect, Invalid Session, a missing heartbeat ACK and a connection that
// closes with a resumable code are handled within Run: it resumes, or
// identifies again, without the engine. Close code 4014 (a privileged
// intent not granted) ends Run with model.Disabled, 4004 with
// model.Unauthorized, the other codes Discord says not to reconnect after
// with model.Permanent; none is retried. gw.attempts connections failing in
// a row end Run as model.Unavailable.
//
// Under Config.Receive ReceiveInteractions the channel has no gateway: Run
// returns model.Disabled at once and calls nothing.
func (x *Instance) Run(ctx context.Context, sink adapter.Sink) error {
	if !x.cfg.offers(model.CapabilityReceive) {
		return final(model.Disabled, "the channel receives at its Interactions Endpoint URL (configuration receive is \"interactions\")")
	}
	if err := x.ready(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &gatewayReceiver{
		x: x, sink: sink, events: make(chan gatewayEvent, laneQueue), results: make(chan laneResult),
		lanes: map[string]*lane{}, outstanding: map[int64]int{},
	}
	if cs, ok := sink.(adapter.CursorSink); ok {
		if c, err := cs.ReceiverCursor(ctx); err == nil {
			if s, ok := parseCursor(c); ok && x.trustedGateway(s.URL) {
				r.sess = s
			}
		}
	}
	defer r.wg.Wait()
	defer cancel()
	return r.run(ctx)
}

// gatewayReceiver is one Run.
type gatewayReceiver struct {
	x    *Instance
	sink adapter.Sink
	// events carries what the current connection read; results the end of
	// each dispatch's handling.
	events  chan gatewayEvent
	results chan laneResult
	wg      sync.WaitGroup

	// sess is the session to resume, empty before READY; gen counts
	// sessions, so a dispatch of an earlier one never moves the cursor of
	// the current one.
	sess session
	gen  int
	// lanes, ready, running and held as in socket lanes; outstanding counts
	// the current session's dispatches by sequence number, read and not yet
	// handled.
	lanes         map[string]*lane
	ready         []*lane
	running, held int
	outstanding   map[int64]int

	conn *gatewayConn
}

// gatewayEvent is a payload read on conn, or its end with the close error.
type gatewayEvent struct {
	conn   *gatewayConn
	msg    *payload
	closed bool
	err    error
}

// next says how the receiver goes on after a connection ended.
type next int

const (
	nextResume next = iota
	nextIdentify
)

func (r *gatewayReceiver) run(ctx context.Context) error {
	failures := 0
	how := nextResume
	for {
		if how == nextIdentify || r.sess.ID == "" {
			r.sess = session{}
		}
		established, how2, err := r.connection(ctx)
		if err != nil {
			return err
		}
		how = how2
		if established {
			failures = 0
		} else if failures++; failures >= r.x.gw.attempts {
			return model.NewError(model.Unavailable, "Discord gateway connections keep failing")
		}
		wait := r.x.gw.retry * time.Duration(failures)
		if how == nextIdentify {
			// "wait a random amount of time between 1 and 5 seconds" is
			// what Discord's earlier guide asked; the current one states
			// no wait. A short random one keeps a fleet from identifying
			// at once.
			wait = r.x.gw.invalid + time.Duration(r.x.jitter()*float64(4*r.x.gw.invalid))
		}
		if wait > 0 {
			if err := r.waitResults(ctx, wait); err != nil {
				return err
			}
		}
	}
}

// waitResults waits d while still finishing dispatches; a refusal that ends
// Run ends the wait.
func (r *gatewayReceiver) waitResults(ctx context.Context, d time.Duration) error {
	done := make(chan error, 1)
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { done <- r.x.wait(wctx, d) }()
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			_ = err
			return nil
		case res := <-r.results:
			if err := r.finish(ctx, res); err != nil {
				return err
			}
		}
	}
}

// trustedGateway reports whether a gateway URL may be dialled with the bot
// token: wss, no user info or fragment, on the configured gateway's host or
// a host of its parent domain (resume_gateway_url names a regional host,
// such as gateway-us-east1-b.discord.gg for gateway.discord.gg).
func (x *Instance) trustedGateway(raw string) bool {
	if !cleanURL(raw, "wss", true) {
		return false
	}
	u, _ := url.Parse(raw)
	base, err := url.Parse(x.cfg.gatewayURL())
	if err != nil {
		return false
	}
	host, want := u.Hostname(), base.Hostname()
	if host == want {
		return true
	}
	_, parent, ok := strings.Cut(want, ".")
	return ok && strings.Contains(parent, ".") && strings.HasSuffix(host, "."+parent)
}

// gatewayDialURL adds the version and encoding to a gateway URL.
func gatewayDialURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("v", "10")
	q.Set("encoding", "json")
	u.RawQuery = q.Encode()
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String()
}

// gatewayBot calls Get Gateway Bot: the URL to connect to, and refuses to
// identify when the session start limit has none left.
func (x *Instance) gatewayBot(ctx context.Context) (string, error) {
	var out struct {
		URL               string `json:"url"`
		SessionStartLimit struct {
			Remaining  int     `json:"remaining"`
			ResetAfter float64 `json:"reset_after"`
		} `json:"session_start_limit"`
	}
	if err := x.call(ctx, http.MethodGet, "gateway/bot", nil, &out); err != nil {
		_, ferr := failure(ctx, err, readCall)
		return "", ferr
	}
	if out.SessionStartLimit.Remaining <= 0 {
		return "", rateLimited(time.Duration(out.SessionStartLimit.ResetAfter) * time.Millisecond)
	}
	if out.URL == "" {
		return x.cfg.gatewayURL(), nil
	}
	if !x.trustedGateway(out.URL) {
		return "", final(model.Permanent, "Get Gateway Bot answered a gateway outside the configured one's domain")
	}
	return out.URL, nil
}

// dial opens a WebSocket to raw through the host's HTTP client.
func (x *Instance) dial(ctx context.Context, raw string) (*websocket.Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, x.gw.hello)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, gatewayDialURL(raw), &websocket.DialOptions{HTTPClient: x.socketClient()})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, model.NewError(model.Transient, "Discord gateway connection failed")
	}
	ws.SetReadLimit(MaxGatewayMessageBytes)
	return ws, nil
}

// socketClient is the host's HTTP client as the WebSocket dial takes it. It
// follows no redirect: only the URL checked is dialled.
func (x *Instance) socketClient() *http.Client {
	return &http.Client{
		Transport:     portTransport{x},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type portTransport struct{ x *Instance }

func (t portTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("User-Agent", UserAgent)
	return t.x.http.Do(r)
}

// closeCode classifies the end of a connection: how to go on, or the error
// that ends Run.
func closeCode(err error) (next, error) {
	switch websocket.CloseStatus(err) {
	case 4004:
		return 0, final(model.Unauthorized, "Discord gateway authentication failed (close 4004)")
	case 4014:
		return 0, final(model.Disabled, "a privileged intent is not granted to the application (close 4014): turn on MESSAGE_CONTENT or set mentions_only")
	case 4010, 4011, 4012, 4013:
		return 0, final(model.Permanent, "Discord gateway refused the session; not reconnected")
	case 4003, 4007, 4009:
		return nextIdentify, nil
	}
	return nextResume, nil
}

// sleep waits d, or until ctx ends, whose error it returns.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
