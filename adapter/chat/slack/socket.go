package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// Socket Mode, as "Using Socket Mode" documents it
// (https://docs.slack.dev/apis/events-api/using-socket-mode/):
//
//   - apps.connections.open, called with an app-level token "in the
//     Authorization header, not as a parameter", answers {"ok": true, "url":
//     "wss://..."}; the URL "refreshes regularly", so each connection asks
//     for a new one (https://docs.slack.dev/reference/methods/apps.connections.open/).
//   - Once connected, "Slack will send a hello message" ({"type": "hello",
//     "num_connections": ..., "debug_info": {...}}).
//   - Events, interactions and slash commands arrive as envelopes
//     {"payload", "envelope_id", "type", "accepts_response_payload"}, type
//     events_api, interactive or slash_commands. "Your app still needs to
//     acknowledge receiving each event so that Slack knows whether to
//     retry": the acknowledgement is {"envelope_id", "payload"}, the payload
//     optional, and it is where the answer of a slash command, a modal
//     submission or an options request goes. Over the socket "there's no
//     need to verify or validate inbound events, because you're receiving
//     the events over a pre-authenticated WebSocket".
//   - {"type": "disconnect", "reason": ...}: link_disabled when Socket Mode
//     is turned off, warning "about 10 seconds before the disconnect", and
//     refresh_requested; "you'll need to handle connection refreshes once
//     every few hours". An app may hold up to 10 connections, and "each
//     payload may be sent to any of the connections".
//   - "&debug_reconnects=true" on the URL makes a connection last 360
//     seconds (Options.DebugReconnects).
//
// The page states no deadline for an acknowledgement; the adapter holds it
// to the three seconds (AckDeadline) the Events API and interactions state
// for their HTTP answers.

// MaxSocketMessageBytes bounds one message read from the socket: the
// engine's default webhook body limit, 1 MiB, as for the request URL.
const MaxSocketMessageBytes = 1 << 20

// socketTiming bounds a connection: the time a new one has to dial and say
// hello, the pace of the receiver's pings (RFC 6455 section 5.5.2: "Upon
// receipt of a Ping frame, an endpoint MUST send a Pong frame in response")
// and how long a pong and an acknowledgement's write may take.
type socketTiming struct {
	hello, ping, pong, write time.Duration
}

var defaultSocketTiming = socketTiming{hello: 10 * time.Second, ping: 30 * time.Second, pong: 10 * time.Second, write: AckDeadline}

// socketQueue is how many messages the connections read ahead of the
// receiver, and how many envelopes the receiver holds, waiting or being
// handled; past either the connections stop reading until it catches up.
const socketQueue = 64

// socketLanes is how many envelopes are handled at once: the envelopes of
// one conversation one at a time, in the order they were read, and up to
// socketLanes conversations side by side.
const socketLanes = 8

// Observation kinds of the Socket Mode receiver. Neither names the app, a
// connection or an envelope.
const (
	// ObservationSocketConnections is a gauge reported on each hello: Value
	// is num_connections, the app's open Socket Mode connections as Slack
	// counts them, the one saying hello included. It is 1 for a receiver
	// alone and 2 at the hello of a refresh, the replaced connection still
	// open.
	ObservationSocketConnections = string(Type) + ".socket_connections"
	// ObservationSocketShared counts a hello whose num_connections exceeds
	// the connections the receiver holds (the one saying hello, and the one
	// it replaces): another consumer of the same app-level token holds a
	// connection, and Slack sends it a share of the envelopes.
	ObservationSocketShared = string(Type) + ".socket_shared"
)

// socketMessage is one message Slack sends over the socket: hello,
// disconnect or an envelope.
type socketMessage struct {
	Type                   string          `json:"type"`
	Reason                 string          `json:"reason"`
	EnvelopeID             string          `json:"envelope_id"`
	Payload                json.RawMessage `json:"payload"`
	AcceptsResponsePayload bool            `json:"accepts_response_payload"`
	NumConnections         int             `json:"num_connections"`
}

// socketAck acknowledges an envelope.
type socketAck struct {
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload,omitempty"`
}

// Run receives the app's payloads over Socket Mode and hands them to sink
// until ctx ends, which it returns. It is the other way to receive: with
// Socket Mode on, Slack sends nothing to the request URL.
//
// Each envelope goes through the parsing of the request URL (an
// events_api payload is an Events API delivery, an interactive one an
// interaction payload, a slash_commands one the fields of a command form)
// and is acknowledged once the sink recorded it, with the answer a request
// would have had in its body: the options of an options request, the field
// errors of a modal submission. What the request URL answers 2xx is
// acknowledged; anything else is not, so Slack retries an event and shows
// an error for an interaction, as for a non-2xx answer. An envelope the
// sink refused is never acknowledged: it stays with Slack. A refusal that
// is retryable, or a denial, also ends the connection: the engine
// reconnects after a retryable one and stops on a denial. Any other
// refusal (one retrying cannot cure, such as a conversation the selection
// cannot place) leaves the connection up, so one envelope does not stop the
// channel; Slack has no way to be told not to retry over the socket, so it
// delivers the envelope again a few times and then gives up.
//
// Envelopes of one conversation (the Slack conversation an event, an
// interaction or a command names; a modal submission's view; one lane for
// the rest) are handled one at a time, in the order they were read, so an
// edit, a deletion or a reaction is never recorded ahead of the message it
// changes. Up to socketLanes conversations are handled at once, each
// acknowledgement written as its record completes, so a slow record holds
// back only its own conversation. A replaced connection is closed once the
// envelopes it delivered are answered.
//
// Each hello reports ObservationSocketConnections, and
// ObservationSocketShared when Slack counts more connections than the
// receiver holds: another consumer of the app-level token then takes a
// share of the envelopes. Neither stops the receiver.
//
// A disconnect of reason warning or refresh_requested opens the next
// connection while the old one still delivers, and closes the old one once
// the new one said hello; nothing returns to the engine. link_disabled
// ends Run with model.Disabled: Socket Mode was turned off for the app. Any
// other reason, a connection that closes unasked, a missing hello and a
// missing pong end it as model.Unavailable, which the engine reconnects
// after its backoff. A rate-limited apps.connections.open is waited out;
// a refused app-level token is model.Unauthorized. Slack keeps no cursor:
// what was not acknowledged is Slack's to deliver again, and the engine's
// duplicate check absorbs a delivery handed twice.
//
// The WebSocket is dialled through the host's HTTP client, which must
// return the body of a 101 Switching Protocols answer writable, as
// net/http does.
//
// Under Config.Receive ReceiveEventsAPI the channel has no socket: Run
// returns model.Disabled at once and calls nothing. The engine never gets
// there, as the instance reports receive disabled.
func (x *Instance) Run(ctx context.Context, sink adapter.Sink) error {
	if !x.cfg.offers(model.CapabilityReceive) {
		return final(model.Disabled, "the channel receives at its request URL (configuration receive is \"events_api\")")
	}
	if err := x.ready(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	r := &socketReceiver{
		x: x, sink: sink, events: make(chan socketEvent, socketQueue), results: make(chan socketResult),
		lanes: map[string]*socketLane{},
	}
	defer x.link(false)
	defer r.wg.Wait()
	defer r.closeAll()
	defer cancel()
	return r.run(ctx)
}

// socketReceiver is one Run: its connections and their events.
type socketReceiver struct {
	x    *Instance
	sink adapter.Sink
	// events carries what the connections read and the outcome of opening
	// a replacement.
	events chan socketEvent
	// results carries the end of each envelope's handling.
	results chan socketResult
	wg      sync.WaitGroup

	// lanes holds the envelopes read and not yet answered, by lane; ready
	// the lanes with one waiting and none being handled, in the order they
	// became so. running counts the envelopes being handled, held those
	// read and not yet answered.
	lanes         map[string]*socketLane
	ready         []*socketLane
	running, held int

	conns []*socketConn
	// current is the connection that said hello last; pending a new one
	// that has not yet. dialing is set while a replacement is being opened.
	current, pending *socketConn
	dialing          bool
}

// socketConn is one connection, with the stop of its reader and pinger.
// held counts its envelopes not yet answered; a retired connection is
// closed once it is zero.
type socketConn struct {
	ws               *websocket.Conn
	stop             context.CancelFunc
	held             int
	retired, closing bool
}

// socketLane is the envelopes of one conversation waiting their turn; busy
// while one of them is handled.
type socketLane struct {
	key   string
	queue []socketJob
	busy  bool
}

// socketJob is one envelope and the connection to acknowledge it on.
type socketJob struct {
	conn *socketConn
	msg  socketMessage
}

// socketResult ends job on lane; err, when set, ends Run.
type socketResult struct {
	lane *socketLane
	job  socketJob
	err  error
}

// socketEvent is a message read on conn, the end of conn (closed), or the
// outcome of opening a replacement (dialed, err).
type socketEvent struct {
	conn   *socketConn
	msg    *socketMessage
	closed bool
	dialed bool
	ws     *websocket.Conn
	err    error
}

func (r *socketReceiver) run(ctx context.Context) error {
	ws, err := r.x.openSocket(ctx)
	if err != nil {
		return err
	}
	r.pending = r.start(ctx, ws)
	hello := time.NewTimer(r.x.sock.hello)
	defer hello.Stop()
	for {
		var ev socketEvent
		var helloC <-chan time.Time
		if r.pending != nil {
			helloC = hello.C
		}
		events := r.events
		if r.held >= socketQueue {
			events = nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-helloC:
			return model.NewError(model.Unavailable, "Slack sent no hello on a new socket connection")
		case res := <-r.results:
			if err := r.finish(ctx, res); err != nil {
				return err
			}
			continue
		case ev = <-events:
		}
		armed := r.pending != nil
		if err := r.on(ctx, ev); err != nil {
			return err
		}
		if !armed && r.pending != nil {
			hello.Reset(r.x.sock.hello)
		}
	}
}

// live is the connection whose disconnect counts: the current one, or the
// first connection before its hello.
func (r *socketReceiver) live() *socketConn {
	if r.current == nil && !r.dialing {
		return r.pending
	}
	return r.current
}

// on handles one event; an error ends Run.
func (r *socketReceiver) on(ctx context.Context, ev socketEvent) error {
	switch {
	case ev.dialed:
		r.dialing = false
		if ev.err != nil {
			return ev.err
		}
		r.pending = r.start(ctx, ev.ws)
		return nil
	case ev.closed:
		switch ev.conn {
		case r.current:
			r.current = nil
			r.x.link(false)
			if r.pending == nil && !r.dialing {
				return model.NewError(model.Unavailable, "Slack closed the socket connection")
			}
		case r.pending:
			return model.NewError(model.Unavailable, "socket connection closed before its hello")
		}
		return nil
	}
	m := ev.msg
	switch m.Type {
	case "hello":
		if ev.conn == r.pending {
			r.counted(ctx, m.NumConnections)
			old := r.current
			r.current, r.pending = r.pending, nil
			r.x.link(true)
			if old != nil {
				r.retire(old)
			}
		}
		return nil
	case "disconnect":
		if ev.conn != r.live() {
			return nil
		}
		switch m.Reason {
		case "link_disabled":
			return final(model.Disabled, "Socket Mode is turned off for the app (disconnect link_disabled)")
		case "warning", "refresh_requested":
			if r.pending == nil && !r.dialing {
				r.dialing = true
				r.wg.Go(func() { r.replace(ctx) })
			}
			return nil
		}
		msg := "Slack disconnected the socket connection"
		if codeToken.MatchString(m.Reason) {
			msg += ": " + m.Reason
		}
		return model.NewError(model.Unavailable, msg)
	}
	if m.EnvelopeID == "" {
		return nil
	}
	r.dispatch(ctx, socketJob{conn: ev.conn, msg: *m})
	return nil
}

// counted reports a hello's num_connections, and whether Slack counts more
// connections than the receiver holds: the one saying hello and the one it
// replaces.
func (r *socketReceiver) counted(ctx context.Context, n int) {
	o := r.x.observer
	if o == nil {
		return
	}
	o.Observe(ctx, ports.Observation{Kind: ObservationSocketConnections, Adapter: Type, Value: int64(n)})
	mine := 1
	if r.current != nil {
		mine++
	}
	if n > mine {
		o.Observe(ctx, ports.Observation{Kind: ObservationSocketShared, Adapter: Type})
	}
}

// dispatch puts job in its lane and starts what may start.
func (r *socketReceiver) dispatch(ctx context.Context, job socketJob) {
	key := laneKey(job.msg)
	l := r.lanes[key]
	if l == nil {
		l = &socketLane{key: key}
		r.lanes[key] = l
	}
	l.queue = append(l.queue, job)
	r.held++
	job.conn.held++
	if !l.busy && len(l.queue) == 1 {
		r.ready = append(r.ready, l)
	}
	r.startReady(ctx)
}

// startReady handles the next envelope of each ready lane while fewer than
// socketLanes are being handled.
func (r *socketReceiver) startReady(ctx context.Context) {
	for r.running < socketLanes && len(r.ready) > 0 {
		l := r.ready[0]
		r.ready = r.ready[1:]
		job := l.queue[0]
		l.queue = l.queue[1:]
		l.busy = true
		r.running++
		r.wg.Go(func() {
			res := socketResult{lane: l, job: job, err: r.handle(ctx, job.conn, job.msg)}
			select {
			case r.results <- res:
			case <-ctx.Done():
			}
		})
	}
}

// finish ends one envelope's handling: the next of its lane becomes ready,
// a retired connection with nothing left to answer is closed, and an error
// ends Run.
func (r *socketReceiver) finish(ctx context.Context, res socketResult) error {
	l, c := res.lane, res.job.conn
	r.running--
	r.held--
	c.held--
	l.busy = false
	r.closeIfDone(c)
	if res.err != nil {
		return res.err
	}
	if len(l.queue) > 0 {
		r.ready = append(r.ready, l)
	} else {
		delete(r.lanes, l.key)
	}
	r.startReady(ctx)
	return nil
}

// laneKey is the lane of an envelope: the Slack conversation its payload
// names, which a message and every edit, deletion and reaction of it
// share; for a modal submission without one, its view; else the one lane
// of everything else. Only the fields naming it are read here; the payload
// is parsed in full when it is handled.
func laneKey(m socketMessage) string {
	var p struct {
		Event *struct {
			Channel json.RawMessage `json:"channel"`
			Item    *struct {
				Channel string `json:"channel"`
			} `json:"item"`
		} `json:"event"`
		Channel   json.RawMessage `json:"channel"`
		ChannelID string          `json:"channel_id"`
		Container *struct {
			ChannelID string `json:"channel_id"`
		} `json:"container"`
		View *struct {
			ID string `json:"id"`
		} `json:"view"`
	}
	if json.Unmarshal(m.Payload, &p) != nil {
		return ""
	}
	var conv string
	switch m.Type {
	case "events_api":
		if p.Event != nil {
			// A channel that is an object (channel_created) names none.
			_ = json.Unmarshal(p.Event.Channel, &conv)
			if conv == "" && p.Event.Item != nil {
				conv = p.Event.Item.Channel
			}
		}
	case "interactive":
		var ch struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(p.Channel, &ch)
		conv = ch.ID
		if conv == "" && p.Container != nil {
			conv = p.Container.ChannelID
		}
		if conv == "" && p.View != nil && p.View.ID != "" {
			return "view:" + p.View.ID
		}
	case "slash_commands":
		conv = p.ChannelID
	}
	if conv == "" {
		return ""
	}
	return "conversation:" + conv
}

// handle answers one envelope and acknowledges it when its answer says so.
func (r *socketReceiver) handle(ctx context.Context, c *socketConn, m socketMessage) error {
	v := r.x.envelope(ctx, r.sink, m)
	if ctx.Err() != nil {
		// Run is ending; nothing is acknowledged.
		return nil
	}
	ack, err := acknowledges(v)
	if !ack {
		return err
	}
	a := socketAck{EnvelopeID: m.EnvelopeID}
	if m.AcceptsResponsePayload {
		a.Payload = v.json()
	}
	b, err := json.Marshal(a)
	if err != nil {
		return nil
	}
	wctx, cancel := context.WithTimeout(ctx, r.x.sock.write)
	defer cancel()
	// Write sends one whole message at a time ("All methods may be called
	// concurrently except for Reader and Read"), so the acknowledgements of
	// several lanes never interleave. A failed write ends the connection;
	// its reader reports that, and Slack delivers the envelope again.
	_ = c.ws.Write(wctx, websocket.MessageText, b)
	return nil
}

// envelope answers one envelope as the request URL answers the request
// that would have carried its payload.
func (x *Instance) envelope(ctx context.Context, sink adapter.Sink, m socketMessage) verdict {
	switch m.Type {
	case "events_api":
		return x.event(ctx, sink, m.Payload)
	case "interactive":
		return x.interaction(ctx, sink, m.Payload, x.now(), m.Payload)
	case "slash_commands":
		f, ok := commandFields(m.Payload)
		if !ok {
			return statusAnswer(http.StatusBadRequest)
		}
		return x.command(ctx, sink, f, x.now())
	}
	// A kind of envelope the adapter does not consume: acknowledged, as an
	// event it does not consume is.
	return statusAnswer(http.StatusOK)
}

// acknowledges maps a verdict to the acknowledgement: given for what the
// request URL answers 2xx, withheld otherwise, a sink refusal always. A
// retryable refusal or a denial also ends the connection, with the refusal.
func acknowledges(v verdict) (bool, error) {
	if v.refusal != nil {
		code := model.CodeOf(v.refusal)
		if code.DefaultRetry() == model.RetrySameOperation || code == model.Unauthorized {
			return false, v.refusal
		}
	}
	return v.refusal == nil && v.status >= 200 && v.status < 300, nil
}

// commandFields reads a slash_commands payload, the JSON object of the
// fields Slack posts as a form to a request URL, as those fields.
func commandFields(payload []byte) (url.Values, bool) {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil || m == nil {
		return nil, false
	}
	f := url.Values{}
	for k, v := range m {
		if s, ok := v.(string); ok {
			f.Set(k, s)
		}
	}
	return f, true
}

// start reads and pings ws until it ends or Run does.
func (r *socketReceiver) start(ctx context.Context, ws *websocket.Conn) *socketConn {
	cctx, stop := context.WithCancel(ctx) // #nosec G118 -- closeAll and retire call c.stop
	c := &socketConn{ws: ws, stop: stop}
	r.conns = append(r.conns, c)
	r.wg.Go(func() { r.read(cctx, c) })
	r.wg.Go(func() { r.ping(cctx, c) })
	return c
}

func (r *socketReceiver) read(ctx context.Context, c *socketConn) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			r.post(ctx, socketEvent{conn: c, closed: true})
			return
		}
		var m socketMessage
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
			continue
		}
		if !r.post(ctx, socketEvent{conn: c, msg: &m}) {
			return
		}
	}
}

// ping checks that the peer still answers; a missing pong closes the
// connection, which its reader then reports.
func (r *socketReceiver) ping(ctx context.Context, c *socketConn) {
	t := time.NewTicker(r.x.sock.ping)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		pctx, cancel := context.WithTimeout(ctx, r.x.sock.pong)
		err := c.ws.Ping(pctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				_ = c.ws.CloseNow()
			}
			return
		}
	}
}

func (r *socketReceiver) post(ctx context.Context, ev socketEvent) bool {
	select {
	case r.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// replace opens the next connection and reports it.
func (r *socketReceiver) replace(ctx context.Context) {
	ws, err := r.x.openSocket(ctx)
	if !r.post(ctx, socketEvent{dialed: true, ws: ws, err: err}) && ws != nil {
		_ = ws.CloseNow()
	}
}

// retire closes a replaced connection once the envelopes it delivered are
// answered.
func (r *socketReceiver) retire(c *socketConn) {
	c.retired = true
	r.closeIfDone(c)
}

// closeIfDone closes a retired connection with nothing left to answer, once,
// with a close handshake; an envelope it delivers meanwhile is Slack's to
// deliver again.
func (r *socketReceiver) closeIfDone(c *socketConn) {
	if !c.retired || c.held > 0 || c.closing {
		return
	}
	c.closing = true
	r.wg.Go(func() {
		_ = c.ws.Close(websocket.StatusNormalClosure, "")
		c.stop()
	})
}

func (r *socketReceiver) closeAll() {
	for _, c := range r.conns {
		c.stop()
		_ = c.ws.CloseNow()
	}
}

// openSocket asks Slack for a connection URL and dials it.
func (x *Instance) openSocket(ctx context.Context) (*websocket.Conn, error) {
	u, err := x.connectionURL(ctx)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, x.sock.hello)
	defer cancel()
	ws, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPClient: x.socketClient()})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, model.NewError(model.Transient, "Slack socket connection failed")
	}
	ws.SetReadLimit(MaxSocketMessageBytes)
	return ws, nil
}

// connectionURL calls apps.connections.open, waiting out a rate limit.
func (x *Instance) connectionURL(ctx context.Context) (string, error) {
	for {
		var out struct {
			URL string `json:"url"`
		}
		err := x.callWith(ctx, x.appToken, "apps.connections.open", struct{}{}, &out)
		if err != nil {
			_, ferr := failure(ctx, err, readCall)
			if me, ok := errors.AsType[*model.Error](ferr); ok && me.Code == model.RateLimited {
				if werr := x.wait(ctx, me.RetryAfter); werr != nil {
					return "", werr
				}
				continue
			}
			return "", ferr
		}
		u, ok := socketURL(out.URL, x.opts.DebugReconnects)
		if !ok {
			return "", final(model.Permanent, "apps.connections.open answered no wss URL")
		}
		return u, nil
	}
}

// socketURL checks the URL apps.connections.open answered: wss, with a host
// and no user info or fragment. debug appends debug_reconnects=true.
func socketURL(raw string, debug bool) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", false
	}
	if debug {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += "debug_reconnects=true"
	}
	return u.String(), true
}

// appToken resolves the app-level token for one apps.connections.open call.
// Its shape is Slack's to check: a bot token stored under the purpose by
// mistake is refused there (not_allowed_token_type), as model.Unauthorized.
func (x *Instance) appToken(ctx context.Context, fresh bool) (string, error) {
	c, err := x.creds.Resolve(ctx, ports.CredentialRequest{
		Channel: x.channel.Ref, Reference: x.channel.CredentialRef, Purpose: CredentialPurposeSocket,
		Accept: []ports.CredentialKind{ports.KindSecret}, Fresh: fresh,
	})
	if err != nil {
		return "", err
	}
	s, ok := c.(ports.Secret)
	if !ok || len(s.Bytes) == 0 {
		return "", final(model.Unsupported, "credential is not a secret")
	}
	return string(s.Bytes), nil
}

// socketClient is the host's HTTP client as the WebSocket dial takes it. It
// follows no redirect: the URL Slack answered is the only one dialled.
func (x *Instance) socketClient() *http.Client {
	return &http.Client{
		Transport:     portTransport{x.http},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

type portTransport struct{ c ports.HTTPClient }

func (t portTransport) RoundTrip(r *http.Request) (*http.Response, error) { return t.c.Do(r) }

// json is the body of a JSON verdict, nil for any other: what a Socket Mode
// acknowledgement carries as its payload.
func (v verdict) json() json.RawMessage {
	if v.contentType != "application/json" || len(v.body) == 0 {
		return nil
	}
	return v.body
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
