package discord

import (
	"context"
	"encoding/json"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"hop.top/uma/model"
)

// gatewayConn is one connection, with the stop of its reader and heartbeat.
type gatewayConn struct {
	ws   *websocket.Conn
	stop context.CancelFunc
	// acked is set by a Heartbeat ACK and cleared by each beat; seq is the
	// last sequence number dispatched, -1 for none; beat is when the last
	// beat was sent, in Unix nanoseconds.
	acked atomic.Bool
	seq   atomic.Int64
	beat  atomic.Int64
	write time.Duration
}

// send writes one payload.
func (c *gatewayConn) send(ctx context.Context, op int, d any) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	b, err := json.Marshal(payload{Op: op, D: data})
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, c.write)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, b)
}

// heartbeatNow sends a beat with the last sequence number, or null.
func (c *gatewayConn) heartbeatNow(ctx context.Context) error {
	c.beat.Store(time.Now().UnixNano())
	var d any
	if s := c.seq.Load(); s >= 0 {
		d = s
	}
	return c.send(ctx, opHeartbeat, d)
}

// connection runs one connection until it ends. established reports that
// it reached READY or RESUMED; how is the way on after an end Run survives;
// err ends Run.
func (r *gatewayReceiver) connection(ctx context.Context) (established bool, how next, err error) {
	resume := r.sess.ID != ""
	raw := r.sess.URL
	if !resume || raw == "" || !r.x.trustedGateway(raw) {
		if raw, err = r.x.gatewayBot(ctx); err != nil {
			if model.IsCode(err, model.Transient) {
				return false, nextResume, nil
			}
			return false, 0, err
		}
	}
	ws, err := r.x.dial(ctx, raw)
	if err != nil {
		if ctx.Err() != nil {
			return false, 0, ctx.Err()
		}
		return false, nextResume, nil
	}
	cctx, stop := context.WithCancel(ctx)
	c := &gatewayConn{ws: ws, stop: stop, write: r.x.gw.write}
	c.seq.Store(-1)
	if resume {
		c.seq.Store(r.sess.Seq)
	}
	r.conn = c
	defer func() {
		r.x.link(false)
		stop()
		// No close frame: a close with 1000 or 1001 would end the session,
		// and it stays resumable.
		_ = ws.CloseNow()
		r.conn = nil
	}()
	r.wg.Go(func() { r.read(cctx, c) })

	hello := time.NewTimer(r.x.gw.hello)
	defer hello.Stop()
	said := false
	for {
		var helloC <-chan time.Time
		if !said {
			helloC = hello.C
		}
		events := r.events
		if r.held >= laneQueue {
			events = nil
		}
		var ev gatewayEvent
		select {
		case <-ctx.Done():
			return established, 0, ctx.Err()
		case <-helloC:
			return established, nextResume, nil
		case res := <-r.results:
			if err := r.finish(ctx, res); err != nil {
				return established, 0, err
			}
			continue
		case ev = <-events:
		}
		if ev.conn != c {
			continue
		}
		if ev.closed {
			how, err := closeCode(ev.err)
			return established, how, err
		}
		m := ev.msg
		switch m.Op {
		case opHello:
			if said {
				continue
			}
			var h struct {
				HeartbeatInterval int64 `json:"heartbeat_interval"`
			}
			if json.Unmarshal(m.D, &h) != nil || h.HeartbeatInterval <= 0 {
				return established, nextResume, nil
			}
			said = true
			r.wg.Go(func() { r.heartbeat(cctx, c, time.Duration(h.HeartbeatInterval)*time.Millisecond) })
			if err := r.start(ctx, c, resume); err != nil {
				return established, 0, err
			}
		case opHeartbeat:
			// Asked for a beat: at once, at most once a second.
			if time.Since(time.Unix(0, c.beat.Load())) >= time.Second {
				_ = c.heartbeatNow(cctx)
			}
		case opHeartbeatAck:
			c.acked.Store(true)
		case opReconnect:
			return established, nextResume, nil
		case opInvalidSession:
			var resumable bool
			_ = json.Unmarshal(m.D, &resumable)
			if resumable && r.sess.ID != "" {
				return established, nextResume, nil
			}
			return established, nextIdentify, nil
		case opDispatch:
			ok, err := r.dispatched(ctx, c, m)
			if err != nil {
				return established, 0, err
			}
			established = established || ok
		}
	}
}

// start identifies, or resumes the session.
func (r *gatewayReceiver) start(ctx context.Context, c *gatewayConn, resume bool) error {
	tok, err := r.x.token(ctx, false)
	if err != nil {
		return err
	}
	if resume {
		return ignoreWrite(c.send(ctx, opResume, map[string]any{"token": tok, "session_id": r.sess.ID, "seq": r.sess.Seq}))
	}
	return ignoreWrite(c.send(ctx, opIdentify, map[string]any{
		"token": tok, "intents": r.x.cfg.intents(),
		"properties": map[string]string{"os": runtime.GOOS, "browser": "uma", "device": "uma"},
	}))
}

// ignoreWrite drops a failed write: the reader then sees the connection end
// and the receiver goes on from there.
func ignoreWrite(error) error { return nil }

// dispatched handles one dispatch: READY and RESUMED establish the
// connection; the events the adapter consumes go to their lanes.
func (r *gatewayReceiver) dispatched(ctx context.Context, c *gatewayConn, m *payload) (bool, error) {
	if m.S != nil {
		c.seq.Store(*m.S)
		if r.sess.ID != "" {
			r.sess.Seq = *m.S
		}
	}
	switch m.T {
	case "READY":
		var rd struct {
			SessionID        string `json:"session_id"`
			ResumeGatewayURL string `json:"resume_gateway_url"`
			User             struct {
				ID string `json:"id"`
			} `json:"user"`
			Application struct {
				ID string `json:"id"`
			} `json:"application"`
		}
		if json.Unmarshal(m.D, &rd) != nil || rd.SessionID == "" {
			return false, nil
		}
		if rd.User.ID != r.x.cfg.BotUserID || (rd.Application.ID != "" && rd.Application.ID != r.x.cfg.ApplicationID) {
			return false, final(model.Permanent, "the bot token belongs to another bot than bot_user_id and application_id name")
		}
		var seq int64
		if m.S != nil {
			seq = *m.S
		}
		r.gen++
		r.outstanding = map[int64]int{}
		r.sess = session{ID: rd.SessionID, Seq: seq}
		if r.x.trustedGateway(rd.ResumeGatewayURL) {
			r.sess.URL = rd.ResumeGatewayURL
		}
		r.x.link(true)
		return true, nil
	case "RESUMED":
		r.x.link(true)
		return true, nil
	case "MESSAGE_CREATE", "MESSAGE_UPDATE", "MESSAGE_DELETE", "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE", "THREAD_UPDATE", "INTERACTION_CREATE":
		if m.S == nil {
			return false, nil
		}
		r.dispatch(ctx, laneJob{gen: r.gen, seq: *m.S, session: r.sess.ID, url: r.sess.URL, typ: m.T, data: m.D})
	}
	return false, nil
}

func (r *gatewayReceiver) read(ctx context.Context, c *gatewayConn) {
	for {
		typ, data, err := c.ws.Read(ctx)
		if err != nil {
			r.post(ctx, gatewayEvent{conn: c, closed: true, err: err})
			return
		}
		var m payload
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
			continue
		}
		if !r.post(ctx, gatewayEvent{conn: c, msg: &m}) {
			return
		}
	}
}

func (r *gatewayReceiver) post(ctx context.Context, ev gatewayEvent) bool {
	select {
	case r.events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

// heartbeat beats every interval, the first after interval * jitter. A beat
// whose predecessor got no ACK finds the connection zombied and ends it,
// without a close frame; the reader then reports the end.
func (r *gatewayReceiver) heartbeat(ctx context.Context, c *gatewayConn, interval time.Duration) {
	c.acked.Store(true)
	wait := time.Duration(r.x.jitter() * float64(interval))
	for {
		if r.x.wait(ctx, wait) != nil {
			return
		}
		if !c.acked.Load() {
			_ = c.ws.CloseNow()
			return
		}
		c.acked.Store(false)
		if c.heartbeatNow(ctx) != nil {
			return
		}
		wait = interval
	}
}
