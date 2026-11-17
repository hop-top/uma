package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The fake's Socket Mode side, in the shapes of "Using Socket Mode"
// (https://docs.slack.dev/apis/events-api/using-socket-mode/):
// apps.connections.open, called with the account's app-level token in the
// Authorization header, answers {"ok": true, "url": "wss://..."}; the URL
// leads to a WebSocket server (httptest, TLS) that says hello on every
// connection and records each acknowledgement. Tests send envelopes and
// disconnect messages and read the acknowledgements; the dial goes through
// the fake's HTTP client like every other request.

const fakeSocketHost = "wss.slack.test"

// fakeConn is one Socket Mode connection, from the server's side.
type fakeConn struct {
	ws    *websocket.Conn
	query string
	acks  chan fakeAck
	done  chan struct{}

	mu   sync.Mutex
	seq  int
	sent map[string]time.Time // envelope ID -> when it was sent
}

// fakeAck is an acknowledgement the server read, and how long after its
// envelope.
type fakeAck struct {
	socketAck
	lag time.Duration
}

// socketServer returns the fake's WebSocket server, started on first use.
func (f *fakeSlack) socketServer() *httptest.Server {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sockSrv == nil {
		f.sockSrv = httptest.NewTLSServer(http.HandlerFunc(f.serveSocket))
	}
	return f.sockSrv
}

// close stops the WebSocket server, closing every connection.
func (f *fakeSlack) close() {
	f.mu.Lock()
	srv := f.sockSrv
	f.mu.Unlock()
	if srv != nil {
		srv.CloseClientConnections()
		srv.Close()
	}
}

// socketRoundTrip forwards a dial of fakeSocketHost to the WebSocket server.
func (f *fakeSlack) socketRoundTrip(r *http.Request) (*http.Response, error) {
	srv := f.socketServer()
	out := r.Clone(r.Context())
	out.URL.Scheme, out.URL.Host, out.Host = "https", srv.Listener.Addr().String(), ""
	return srv.Client().Transport.RoundTrip(out)
}

// connectionsOpen answers apps.connections.open for a: the account's script
// of open answers first, then a URL with a fresh ticket naming the account.
func (f *fakeSlack) connectionsOpen(a *fakeAccount, w http.ResponseWriter, auth string) error {
	a.calls = append(a.calls, fakeCall{Method: "apps.connections.open", Auth: auth})
	if auth != "Bearer "+a.appToken || a.appToken == "" {
		writeAPI(w, map[string]any{"ok": false, "error": "invalid_auth"})
		return nil
	}
	if len(a.opens) > 0 {
		ans := a.opens[0]
		a.opens = a.opens[1:]
		switch {
		case ans.unreachable:
			return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		case ans.errCode != "":
			writeAPI(w, map[string]any{"ok": false, "error": ans.errCode})
			return nil
		case ans.status != 0:
			for k, v := range ans.header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(ans.status)
			return nil
		}
	}
	a.tickets++
	url := "wss://" + fakeSocketHost + "/link/?ticket=" + a.prefix + "." + strconv.Itoa(a.tickets)
	if a.socketURL != "" {
		url = a.socketURL
	}
	writeAPI(w, map[string]any{"ok": true, "url": url})
	return nil
}

func (f *fakeSlack) serveSocket(w http.ResponseWriter, r *http.Request) {
	prefix, _, _ := strings.Cut(r.URL.Query().Get("ticket"), ".")
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil || r.URL.Path != "/link/" {
		http.Error(w, "unknown ticket", http.StatusUnauthorized)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(MaxSocketMessageBytes)
	c := &fakeConn{ws: ws, query: r.URL.RawQuery, acks: make(chan fakeAck, 256), done: make(chan struct{}), sent: map[string]time.Time{}}
	f.mu.Lock()
	hello, onConnect, deaf := !a.noHello, a.onConnect, a.deafUntil
	a.conns = append(a.conns, c)
	f.mu.Unlock()
	if hello {
		_ = c.send(map[string]any{
			"type": "hello", "num_connections": 1, "connection_info": map[string]any{"app_id": testApp},
			"debug_info": map[string]any{"host": "applink-fake", "approximate_connection_time": 3600},
		})
	}
	select {
	case a.connected <- c:
	default:
	}
	if onConnect != nil {
		go onConnect(c)
	}
	defer close(c.done)
	if deaf != nil {
		<-deaf
		return
	}
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		var ack socketAck
		if json.Unmarshal(data, &ack) != nil {
			continue
		}
		c.mu.Lock()
		lag := time.Since(c.sent[ack.EnvelopeID])
		c.mu.Unlock()
		c.acks <- fakeAck{socketAck: ack, lag: lag}
	}
}

// send writes v as a text message.
func (c *fakeConn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, b)
}

// envelope sends payload as an envelope of typ and returns its ID.
func (c *fakeConn) envelope(t *testing.T, typ string, payload any, accepts bool) string {
	t.Helper()
	c.mu.Lock()
	c.seq++
	id := "env-" + strconv.Itoa(c.seq) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	c.sent[id] = time.Now()
	c.mu.Unlock()
	if err := c.send(map[string]any{"envelope_id": id, "type": typ, "payload": payload, "accepts_response_payload": accepts}); err != nil {
		t.Fatalf("send envelope: %v", err)
	}
	return id
}

func (c *fakeConn) disconnect(t *testing.T, reason string) {
	t.Helper()
	if err := c.send(map[string]any{"type": "disconnect", "reason": reason, "debug_info": map[string]any{"host": "wss-fake"}}); err != nil {
		t.Fatalf("send disconnect: %v", err)
	}
}

// ack waits for the next acknowledgement.
func (c *fakeConn) ack(t *testing.T) fakeAck {
	t.Helper()
	select {
	case a := <-c.acks:
		return a
	case <-time.After(5 * time.Second):
		t.Fatal("no acknowledgement")
	}
	return fakeAck{}
}

// noAck checks that nothing is acknowledged within d.
func (c *fakeConn) noAck(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case a := <-c.acks:
		t.Fatalf("acknowledged %+v, want none", a)
	case <-time.After(d):
	}
}

// closed reports whether the client ended the connection within d.
func (c *fakeConn) closed(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

// nextConn waits for the account's next connection.
func (a *fakeAccount) nextConn(t *testing.T) *fakeConn {
	t.Helper()
	select {
	case c := <-a.connected:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no Socket Mode connection")
	}
	return nil
}

// noConn checks that no connection is opened within d.
func (a *fakeAccount) noConn(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case <-a.connected:
		t.Fatal("a Socket Mode connection was opened, want none")
	case <-time.After(d):
	}
}
