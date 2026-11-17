package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeGateway is a stand-in Discord gateway for one account, in the shapes
// of "Gateway" and "Gateway Events"
// (https://docs.discord.com/developers/events/gateway,
// https://docs.discord.com/developers/events/gateway-events): on every
// connection it says Hello with a heartbeat interval, ACKs heartbeats,
// answers Identify with READY (a new session, its resume_gateway_url and
// the bot user) and Resume with the dispatches the session missed and
// RESUMED, or Invalid Session for a session it does not know. Tests
// dispatch events, send opcodes and close connections with codes. The
// server is an httptest TLS server the fake's HTTP client forwards
// gateway.discord.test and resume.discord.test to; the path names the
// account.
type fakeGateway struct {
	f *fakeDiscord
	a *fakeAccount

	mu sync.Mutex
	// heartbeat is the interval Hello states; noAck drops ACKs; noHello
	// skips Hello; closeOn closes a connection with that code after its
	// Identify; refuseResume answers every Resume with Invalid Session.
	heartbeat    time.Duration
	noAck        bool
	noHello      bool
	closeOn      int
	refuseResume bool
	// readyUser is the user READY names; empty is the account's bot.
	readyUser  string
	sessions   map[string]*fakeSession
	identifies []map[string]any
	resumes    []map[string]any
	conns      []*fakeGWConn
	connected  chan *fakeGWConn
	// onReady runs when a connection reaches READY or RESUMED.
	onReady func(*fakeGWConn)
}

type fakeSession struct {
	id  string
	seq int64
	log []loggedDispatch
}

type loggedDispatch struct {
	seq  int64
	typ  string
	data json.RawMessage
}

// fakeGWConn is one gateway connection, from the server's side.
type fakeGWConn struct {
	g    *fakeGateway
	ws   *websocket.Conn
	path string
	done chan struct{}

	mu         sync.Mutex
	sess       *fakeSession
	heartbeats []any
}

func newFakeGateway(f *fakeDiscord, a *fakeAccount) *fakeGateway {
	return &fakeGateway{f: f, a: a, heartbeat: 41250 * time.Millisecond, sessions: map[string]*fakeSession{}, connected: make(chan *fakeGWConn, 64)}
}

// gatewayServer returns the WebSocket server, started on first use.
func (f *fakeDiscord) gatewayServer() *httptest.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gwSrv == nil {
		f.gwSrv = httptest.NewTLSServer(http.HandlerFunc(f.serveGateway))
	}
	return f.gwSrv
}

// close stops the WebSocket server, closing every connection.
func (f *fakeDiscord) close() {
	f.mu.Lock()
	srv := f.gwSrv
	f.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

func (f *fakeDiscord) gatewayRoundTrip(r *http.Request) (*http.Response, error) {
	srv := f.gatewayServer()
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = "https", srv.Listener.Addr().String(), ""
	out.Header.Set("X-Fake-Host", r.URL.Host)
	return srv.Client().Transport.RoundTrip(out)
}

// gatewayURL is the account's gateway, as Get Gateway Bot answers it.
func (a *fakeAccount) gatewayURL() string { return "wss://" + fakeGatewayHost + "/" + a.prefix }

func (f *fakeDiscord) serveGateway(w http.ResponseWriter, r *http.Request) {
	prefix := strings.Trim(r.URL.Path, "/")
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil || r.URL.Query().Get("v") != "10" || r.URL.Query().Get("encoding") != "json" {
		http.Error(w, "unknown gateway", http.StatusNotFound)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	g := a.gw
	c := &fakeGWConn{g: g, ws: ws, path: r.Header.Get("X-Fake-Host"), done: make(chan struct{})}
	g.mu.Lock()
	g.conns = append(g.conns, c)
	hello, interval := !g.noHello, g.heartbeat
	g.mu.Unlock()
	defer close(c.done)
	if hello {
		_ = c.send(opHello, map[string]any{"heartbeat_interval": interval.Milliseconds()}, nil, "")
	}
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		var p payload
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal(p.D, &d)
		switch p.Op {
		case opHeartbeat:
			var seq any
			_ = json.Unmarshal(p.D, &seq)
			c.mu.Lock()
			c.heartbeats = append(c.heartbeats, seq)
			c.mu.Unlock()
			g.mu.Lock()
			ack := !g.noAck
			g.mu.Unlock()
			if ack {
				_ = c.send(opHeartbeatAck, nil, nil, "")
			}
		case opIdentify:
			g.identify(c, d)
		case opResume:
			g.resume(c, d)
		}
	}
}

func (g *fakeGateway) identify(c *fakeGWConn, d map[string]any) {
	g.mu.Lock()
	g.identifies = append(g.identifies, d)
	code := g.closeOn
	user := g.readyUser
	if user == "" {
		user = g.a.botUser
	}
	g.mu.Unlock()
	if code != 0 {
		_ = c.ws.Close(websocket.StatusCode(code), "closed by the fake")
		return
	}
	if str(d, "token") != g.a.token {
		_ = c.ws.Close(websocket.StatusCode(4004), "Authentication failed.")
		return
	}
	g.mu.Lock()
	s := &fakeSession{id: "sess-" + strconv.Itoa(len(g.sessions)+1)}
	g.sessions[s.id] = s
	g.mu.Unlock()
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	c.dispatchLogged("READY", map[string]any{
		"v": 10, "session_id": s.id, "resume_gateway_url": "wss://" + fakeResumeHost + "/" + g.a.prefix,
		"user": map[string]any{"id": user, "bot": true}, "application": map[string]any{"id": g.a.app}, "guilds": []any{},
	}, false)
	g.ready(c)
}

func (g *fakeGateway) resume(c *fakeGWConn, d map[string]any) {
	g.mu.Lock()
	g.resumes = append(g.resumes, d)
	s := g.sessions[str(d, "session_id")]
	refuse := g.refuseResume
	g.mu.Unlock()
	seq, _ := d["seq"].(float64)
	if s == nil || refuse || str(d, "token") != g.a.token {
		_ = c.send(opInvalidSession, false, nil, "")
		return
	}
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	g.mu.Lock()
	var replay []loggedDispatch
	for _, l := range s.log {
		if l.seq > int64(seq) {
			replay = append(replay, l)
		}
	}
	g.mu.Unlock()
	for _, l := range replay {
		_ = c.send(opDispatch, l.data, &l.seq, l.typ)
	}
	c.dispatchLogged("RESUMED", map[string]any{}, false)
	g.ready(c)
}

func (g *fakeGateway) ready(c *fakeGWConn) {
	select {
	case g.connected <- c:
	default:
	}
	g.mu.Lock()
	on := g.onReady
	g.mu.Unlock()
	if on != nil {
		go on(c)
	}
}

// send writes one payload.
func (c *fakeGWConn) send(op int, d any, s *int64, t string) error {
	var raw json.RawMessage
	if d != nil {
		if r, ok := d.(json.RawMessage); ok {
			raw = r
		} else {
			raw, _ = json.Marshal(d)
		}
	} else {
		raw = json.RawMessage("null")
	}
	b, _ := json.Marshal(payload{Op: op, D: raw, S: s, T: t})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// dispatchLogged sends a dispatch with the session's next sequence number,
// logging it for replay when log is set; it returns the number.
func (c *fakeGWConn) dispatchLogged(typ string, d any, log bool) int64 {
	raw, _ := json.Marshal(d)
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	c.g.mu.Lock()
	s.seq++
	seq := s.seq
	if log {
		s.log = append(s.log, loggedDispatch{seq: seq, typ: typ, data: raw})
	}
	c.g.mu.Unlock()
	_ = c.send(opDispatch, json.RawMessage(raw), &seq, typ)
	return seq
}

// dispatch sends an event and returns its sequence number.
func (c *fakeGWConn) dispatch(typ string, d any) int64 { return c.dispatchLogged(typ, d, true) }

// closeWith closes the connection with code.
func (c *fakeGWConn) closeWith(code int) { _ = c.ws.Close(websocket.StatusCode(code), "") }

func (c *fakeGWConn) closed(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

func (c *fakeGWConn) beats() []any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.heartbeats...)
}

// nextConn waits for the next connection to reach READY or RESUMED.
func (g *fakeGateway) nextConn(t *testing.T) *fakeGWConn {
	t.Helper()
	select {
	case c := <-g.connected:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no gateway session")
	}
	return nil
}

func (g *fakeGateway) counts() (identifies, resumes, conns int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.identifies), len(g.resumes), len(g.conns)
}
