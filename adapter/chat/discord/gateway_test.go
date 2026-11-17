package discord

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
)

// gatewayRig is a rig whose instance runs fast: no heartbeat jitter, short
// waits between connections.
func gatewayRig(t *testing.T, mod ...func(*Config)) *rig {
	t.Helper()
	r := newRig(t, mod...)
	r.x.jitter = func() float64 { return 0 }
	r.x.gw = gatewayTiming{hello: 2 * time.Second, write: time.Second, invalid: 10 * time.Millisecond, retry: 10 * time.Millisecond, attempts: 3}
	return r
}

// running is one Run in the background.
type running struct {
	cancel context.CancelFunc
	done   chan error
}

func (r *rig) run(t *testing.T, sink *sinkRecorder) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ru := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { ru.done <- r.x.Run(ctx, sink) }()
	t.Cleanup(func() { cancel(); <-ru.done })
	return ru
}

// end waits for Run to return on its own.
func (ru *running) end(t *testing.T) error {
	t.Helper()
	select {
	case err := <-ru.done:
		ru.done <- err
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	return nil
}

// stop ends Run and returns what it returned.
func (ru *running) stop(t *testing.T) error {
	t.Helper()
	ru.cancel()
	return ru.end(t)
}

// handledLog records the dispatches the receiver handled.
type handledLog struct {
	mu   sync.Mutex
	seqs []int64
	errs []error
	ch   chan int64
}

func (r *rig) watch() *handledLog {
	h := &handledLog{ch: make(chan int64, 256)}
	r.x.handled = func(seq int64, err error) {
		h.mu.Lock()
		h.seqs, h.errs = append(h.seqs, seq), append(h.errs, err)
		h.mu.Unlock()
		h.ch <- seq
	}
	return h
}

func (h *handledLog) wait(t *testing.T, seq int64) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case s := <-h.ch:
			if s == seq {
				return
			}
		case <-deadline:
			t.Fatalf("dispatch %d was not handled", seq)
		}
	}
}

func dmEvent(id, text string) map[string]any {
	return msgEvent(id, "1100000000000000099", "", testPerson, text, nil)
}

func TestGatewayIdentifyHeartbeatDispatch(t *testing.T) {
	r := gatewayRig(t)
	r.acct.gw.heartbeat = 30 * time.Millisecond
	h := r.watch()
	sink := &sinkRecorder{}
	ru := r.run(t, sink)
	c := r.acct.gw.nextConn(t)
	ids, _, _ := r.acct.gw.counts()
	if ids != 1 {
		t.Fatalf("identifies = %d", ids)
	}
	id := r.acct.gw.identifies[0]
	props, _ := id["properties"].(map[string]any)
	if str(id, "token") != testToken || int(id["intents"].(float64)) != testConfig().intents() || props["os"] == nil || props["browser"] == nil {
		t.Fatalf("identify = %+v", id)
	}
	seq := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000001", "hello"))
	h.wait(t, seq)
	envs := sink.envelopes()
	if len(envs) != 1 || envs[0].Content.Parts[0].Text != "hello" {
		t.Fatalf("recorded %+v", envs)
	}
	cur, ok := parseCursor(sink.ingresses()[0].ReceiverCursor)
	if !ok || cur.ID != "sess-1" || cur.Seq != seq || cur.URL != "wss://"+fakeResumeHost+"/acct1" {
		t.Fatalf("cursor = %+v", cur)
	}
	// Heartbeats carry the last sequence number and are acknowledged.
	time.Sleep(100 * time.Millisecond)
	beats := c.beats()
	if len(beats) < 2 || beats[len(beats)-1] != float64(seq) {
		t.Fatalf("heartbeats = %v, want the last sequence %d", beats, seq)
	}
	if c.closed(50 * time.Millisecond) {
		t.Fatal("acknowledged heartbeats ended the connection")
	}
	if err := ru.stop(t); err != context.Canceled {
		t.Fatalf("Run = %v, want the context's end", err)
	}
	// The receiver only ever sends Identify, Resume and heartbeats; the
	// bot token goes nowhere else on the gateway.
	if calls := r.callsOf("GET", "gateway/bot"); len(calls) != 1 || calls[0].Agent != UserAgent || calls[0].Auth != "Bot "+testToken {
		t.Fatalf("Get Gateway Bot calls = %+v", calls)
	}
}

func TestGatewayResumesAfterDrop(t *testing.T) {
	for name, drop := range map[string]func(c *fakeGWConn){
		"resumable close": func(c *fakeGWConn) { c.closeWith(4000) },
		"reconnect":       func(c *fakeGWConn) { _ = c.send(opReconnect, nil, nil, "") },
		"invalid session resumable": func(c *fakeGWConn) {
			_ = c.send(opInvalidSession, true, nil, "")
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := gatewayRig(t)
			h := r.watch()
			sink := &sinkRecorder{}
			r.run(t, sink)
			c := r.acct.gw.nextConn(t)
			first := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000001", "one"))
			h.wait(t, first)
			drop(c)
			// Discord logs what the session misses while it is away.
			r.acct.gw.mu.Lock()
			s := r.acct.gw.sessions["sess-1"]
			missed := s.seq + 1
			s.seq = missed
			s.log = append(s.log, loggedDispatch{seq: missed, typ: "MESSAGE_CREATE", data: mustJSON(dmEvent("1200000000000000002", "missed"))})
			r.acct.gw.mu.Unlock()
			c2 := r.acct.gw.nextConn(t)
			h.wait(t, missed)
			ids, resumes, _ := r.acct.gw.counts()
			if ids != 1 || resumes != 1 {
				t.Fatalf("identifies %d resumes %d, want one of each", ids, resumes)
			}
			res := r.acct.gw.resumes[0]
			if str(res, "session_id") != "sess-1" || res["seq"] != float64(first) || str(res, "token") != testToken || c2.path != fakeResumeHost {
				t.Fatalf("resume = %+v on %s, want sess-1 at %d on the resume URL", res, c2.path, first)
			}
			if envs := sink.envelopes(); len(envs) != 2 || envs[1].Content.Parts[0].Text != "missed" {
				t.Fatalf("recorded %+v", envs)
			}
		})
	}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestGatewayIdentifiesAgain(t *testing.T) {
	for name, end := range map[string]func(c *fakeGWConn){
		"invalid session": func(c *fakeGWConn) { _ = c.send(opInvalidSession, false, nil, "") },
		"close 4007":      func(c *fakeGWConn) { c.closeWith(4007) },
		"close 4009":      func(c *fakeGWConn) { c.closeWith(4009) },
	} {
		t.Run(name, func(t *testing.T) {
			r := gatewayRig(t)
			r.run(t, &sinkRecorder{})
			end(r.acct.gw.nextConn(t))
			r.acct.gw.nextConn(t)
			if ids, resumes, _ := r.acct.gw.counts(); ids != 2 || resumes != 0 {
				t.Fatalf("identifies %d resumes %d, want a new session", ids, resumes)
			}
		})
	}
}

func TestGatewayCloseCodesEndRun(t *testing.T) {
	for code, want := range map[int]model.ErrorCode{4014: model.Disabled, 4004: model.Unauthorized, 4013: model.Permanent, 4011: model.Permanent} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			r := gatewayRig(t)
			r.acct.gw.closeOn = code
			ru := r.run(t, &sinkRecorder{})
			if err := ru.end(t); !model.IsCode(err, want) {
				t.Fatalf("close %d: Run = %v, want %s", code, err, want)
			}
			if ids, _, conns := r.acct.gw.counts(); ids != 1 || conns != 1 {
				t.Fatalf("close %d: identifies %d connections %d, want one, never retried", code, ids, conns)
			}
		})
	}
}

func TestGatewayZombieConnectionResumes(t *testing.T) {
	r := gatewayRig(t)
	r.acct.gw.heartbeat = 20 * time.Millisecond
	r.acct.gw.noAck = true
	r.run(t, &sinkRecorder{})
	c := r.acct.gw.nextConn(t)
	if !c.closed(2 * time.Second) {
		t.Fatal("a connection with no heartbeat ACK stayed open")
	}
	r.acct.gw.mu.Lock()
	r.acct.gw.noAck = false
	r.acct.gw.mu.Unlock()
	r.acct.gw.nextConn(t)
	if _, resumes, _ := r.acct.gw.counts(); resumes < 1 {
		t.Fatal("a zombied connection was not resumed")
	}
}

func TestGatewayHeartbeatOnRequest(t *testing.T) {
	r := gatewayRig(t)
	r.run(t, &sinkRecorder{})
	c := r.acct.gw.nextConn(t)
	time.Sleep(1100 * time.Millisecond) // past the once-a-second bound of the first beat
	before := len(c.beats())
	_ = c.send(opHeartbeat, nil, nil, "")
	deadline := time.Now().Add(2 * time.Second)
	for len(c.beats()) == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(c.beats()) == before {
		t.Fatal("no heartbeat sent when Discord asked for one")
	}
}

// A restarted Run resumes the session from the receiver cursor, and the
// cursor never passes a dispatch that was not recorded.
func TestGatewayRestartResumesFromCursor(t *testing.T) {
	r := gatewayRig(t)
	h := r.watch()
	refuse := true
	sink := &sinkRecorder{fail: func(in model.Ingress) error {
		if refuse && in.Envelope.Content.Parts[0].Text == "two" {
			return model.NewError(model.Transient, "store busy")
		}
		return nil
	}}
	ru := r.run(t, sink)
	c := r.acct.gw.nextConn(t)
	one := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000001", "one"))
	h.wait(t, one)
	c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000002", "two"))
	if err := ru.end(t); !model.IsCode(err, model.Transient) {
		t.Fatalf("Run after a retryable refusal = %v, want it", err)
	}
	if cur, _ := parseCursor(sink.cursor); cur.Seq != one {
		t.Fatalf("cursor at %d, want %d: the refused dispatch is not passed", cur.Seq, one)
	}
	refuse = false
	r.run(t, sink)
	r.acct.gw.nextConn(t)
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.envelopes()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if envs := sink.envelopes(); len(envs) != 2 || envs[1].Content.Parts[0].Text != "two" {
		t.Fatalf("recorded %+v, want the refused dispatch replayed", envs)
	}
	if ids, resumes, _ := r.acct.gw.counts(); ids != 1 || resumes != 1 {
		t.Fatalf("identifies %d resumes %d, want the restart to resume", ids, resumes)
	}
}

func TestGatewayRefusalsThatDoNotEndRun(t *testing.T) {
	r := gatewayRig(t)
	h := r.watch()
	sink := &sinkRecorder{fail: func(in model.Ingress) error {
		if in.Envelope.Content.Parts[0].Text == "bad" {
			return model.NewError(model.Validation, "no inbox")
		}
		return nil
	}}
	ru := r.run(t, sink)
	c := r.acct.gw.nextConn(t)
	bad := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000001", "bad"))
	good := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000002", "good"))
	h.wait(t, bad)
	h.wait(t, good)
	if envs := sink.envelopes(); len(envs) != 1 {
		t.Fatalf("recorded %+v", envs)
	}
	select {
	case err := <-ru.done:
		t.Fatalf("Run ended with %v on a refusal retrying cannot cure", err)
	default:
	}
}

func TestGatewayIdentifyGuards(t *testing.T) {
	t.Run("no session starts left", func(t *testing.T) {
		r := gatewayRig(t)
		r.acct.startsLeft = 0
		err := r.run(t, &sinkRecorder{}).end(t)
		if me, ok := err.(*model.Error); !ok || me.Code != model.RateLimited || me.RetryAfter != time.Hour {
			t.Fatalf("Run = %v, want rate_limited for the reset", err)
		}
		if _, _, conns := r.acct.gw.counts(); conns != 0 {
			t.Fatal("connected with no identify left")
		}
	})
	t.Run("token of another bot", func(t *testing.T) {
		r := gatewayRig(t)
		r.acct.gw.readyUser = testOther
		if err := r.run(t, &sinkRecorder{}).end(t); !model.IsCode(err, model.Permanent) {
			t.Fatalf("Run = %v, want permanent", err)
		}
	})
	t.Run("connections keep failing", func(t *testing.T) {
		r := gatewayRig(t)
		r.acct.gw.noHello = true
		r.x.gw.hello = 50 * time.Millisecond
		if err := r.run(t, &sinkRecorder{}).end(t); !model.IsCode(err, model.Unavailable) {
			t.Fatalf("Run = %v, want unavailable", err)
		}
	})
	t.Run("untrusted resume URL in the cursor", func(t *testing.T) {
		r := gatewayRig(t)
		sink := &sinkRecorder{cursor: session{ID: "sess-9", Seq: 3, URL: "wss://evil.example/"}.cursor()}
		r.run(t, sink)
		r.acct.gw.nextConn(t)
		if ids, resumes, _ := r.acct.gw.counts(); ids != 1 || resumes != 0 {
			t.Fatalf("identifies %d resumes %d: a cursor naming another host is ignored", ids, resumes)
		}
	})
}

// Dispatches of one channel are recorded in order; another channel's go
// on while one is held.
func TestGatewayLanes(t *testing.T) {
	r := gatewayRig(t)
	h := r.watch()
	held := make(chan struct{})
	sink := &sinkRecorder{}
	var mu sync.Mutex
	var order []string
	sink.fail = func(in model.Ingress) error {
		mu.Lock()
		order = append(order, in.Envelope.Content.Parts[0].Text)
		mu.Unlock()
		return nil
	}
	gate := &gatedSink{sinkRecorder: sink, block: "slow", release: held}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.x.Run(ctx, gate) }()
	c := r.acct.gw.nextConn(t)
	slow := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000001", "slow"))
	after := c.dispatch("MESSAGE_CREATE", dmEvent("1200000000000000002", "after"))
	other := c.dispatch("MESSAGE_CREATE", msgEvent("1200000000000000003", "1100000000000000098", "", testPerson, "other", nil))
	h.wait(t, other)
	// The other channel's record may not move the cursor past the dispatch
	// still held: a restart must replay it.
	if cur, _ := parseCursor(sink.cursor); cur.Seq != slow-1 {
		t.Fatalf("cursor at %d while %d is held, want %d", cur.Seq, slow, slow-1)
	}
	close(held)
	h.wait(t, slow)
	h.wait(t, after)
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "other" || order[1] != "slow" || order[2] != "after" {
		t.Fatalf("record order %v, want the other channel first and one channel in order", order)
	}
	cancel()
	<-done
}

// gatedSink holds the record of the message whose text is block until
// release closes.
type gatedSink struct {
	*sinkRecorder
	block   string
	release chan struct{}
}

func (g *gatedSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	if in.Envelope.Content.Parts[0].Text == g.block {
		select {
		case <-g.release:
		case <-ctx.Done():
			return model.IngressReceipt{}, ctx.Err()
		}
	}
	return g.sinkRecorder.Accept(ctx, in)
}
