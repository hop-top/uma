package slack

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// gateSink records what it accepts, holding the Accept of a text it has a
// gate for until the gate is opened or ctx ends, without holding up the
// Accepts of others. It reports each Accept as it starts, and counts the
// Accepts in progress.
type gateSink struct {
	mu       sync.Mutex
	gates    map[string]chan struct{}
	opened   map[string]bool
	got      []string
	inflight int
	max      int
	// linger delays the end of an Accept whose ctx ended.
	linger  time.Duration
	started chan string
}

func newGateSink(texts ...string) *gateSink {
	s := &gateSink{gates: map[string]chan struct{}{}, opened: map[string]bool{}, started: make(chan string, 256)}
	for _, t := range texts {
		s.gates[t] = make(chan struct{})
	}
	return s
}

func (s *gateSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	text := textOf(in.Envelope)
	s.mu.Lock()
	s.inflight++
	s.max = max(s.max, s.inflight)
	gate := s.gates[text]
	s.mu.Unlock()
	s.started <- text
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			time.Sleep(s.linger)
			return model.IngressReceipt{}, ctx.Err()
		}
	}
	s.mu.Lock()
	s.got = append(s.got, text)
	s.mu.Unlock()
	return model.IngressReceipt{Committed: true, Durable: true}, nil
}

func (s *gateSink) EvaluateInteraction(context.Context, model.InteractionInput) (model.InteractionDecision, error) {
	return model.InteractionDecision{}, nil
}

// open opens the gate of text; opening it again does nothing.
func (s *gateSink) open(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.opened[text] {
		s.opened[text] = true
		close(s.gates[text])
	}
}

func (s *gateSink) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *gateSink) counts() (inflight, most int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight, s.max
}

// waitStarted waits for the Accept of text to start.
func (s *gateSink) waitStarted(t *testing.T, text string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-s.started:
			if got == text {
				return
			}
		case <-deadline:
			t.Fatalf("Accept of %q never started", text)
		}
	}
}

// runWith starts the rig's receiver with sink.
func (r *socketRig) runWith(t *testing.T, sink *gateSink) *running {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	run := &running{cancel: cancel, done: make(chan error, 1)}
	go func() { run.done <- r.x.Run(ctx, sink) }()
	t.Cleanup(func() { _ = run.stop(t) })
	return run
}

// roomEvent is a message event in room.
func roomEvent(eventID, room, text, ts string) map[string]any {
	return callback(eventID, messageEvent(testPerson, text, ts, map[string]any{"channel": room}))
}

func TestSocketModeSlowConversationHoldsBackOnlyItself(t *testing.T) {
	r := newSocketRig(t, Options{})
	sink := newGateSink("slow")
	t.Cleanup(func() { sink.open("slow") })
	r.runWith(t, sink)
	c := r.a.nextConn(t)
	slow := c.envelope(t, "events_api", roomEvent("Ev01", "C0SLOW", "slow", testTS), false)
	sink.waitStarted(t, "slow")
	// Another conversation's envelope is recorded and acknowledged while the
	// first is still being recorded, inside the deadline.
	fast := c.envelope(t, "events_api", roomEvent("Ev02", testRoom, "fast", testTS), false)
	if ack := c.ack(t); ack.EnvelopeID != fast || ack.lag >= AckDeadline {
		t.Fatalf("ack %+v after %v, want the other conversation's within %v", ack.socketAck, ack.lag, AckDeadline)
	}
	c.noAck(t, 50*time.Millisecond)
	sink.open("slow")
	if ack := c.ack(t); ack.EnvelopeID != slow {
		t.Fatalf("ack %+v, want the slow envelope's once recorded", ack.socketAck)
	}
}

func TestSocketModeKeepsTheOrderOfAConversation(t *testing.T) {
	r := newSocketRig(t, Options{})
	sink := newGateSink("first")
	t.Cleanup(func() { sink.open("first") })
	r.runWith(t, sink)
	c := r.a.nextConn(t)
	first := c.envelope(t, "events_api", roomEvent("Ev01", testRoom, "first", testTS), false)
	sink.waitStarted(t, "first")
	second := c.envelope(t, "events_api", callback("Ev02", map[string]any{
		"type": "reaction_added", "user": testPerson, "reaction": "thumbsup", "event_ts": "1700000001.000100",
		"item": map[string]any{"type": "message", "channel": testRoom, "ts": testTS},
	}), false)
	// The reaction waits for the message it reacts to.
	c.noAck(t, 200*time.Millisecond)
	if got := sink.recorded(); len(got) != 0 {
		t.Fatalf("recorded %q while the conversation's first envelope was held", got)
	}
	sink.open("first")
	if a, b := c.ack(t), c.ack(t); a.EnvelopeID != first || b.EnvelopeID != second {
		t.Fatalf("acks %s, %s, want %s then %s", a.EnvelopeID, b.EnvelopeID, first, second)
	}
}

func TestSocketModeBoundsConcurrentHandling(t *testing.T) {
	r := newSocketRig(t, Options{})
	texts := make([]string, socketLanes+1)
	for i := range texts {
		texts[i] = "hold-" + strconv.Itoa(i)
	}
	sink := newGateSink(texts...)
	t.Cleanup(func() {
		for _, s := range texts {
			sink.open(s)
		}
	})
	r.runWith(t, sink)
	c := r.a.nextConn(t)
	for i, s := range texts[:socketLanes] {
		c.envelope(t, "events_api", roomEvent("Ev"+strconv.Itoa(i), "C0ROOM"+strconv.Itoa(i), s, testTS), false)
		sink.waitStarted(t, s)
	}
	last := texts[socketLanes]
	c.envelope(t, "events_api", roomEvent("EvLast", "C0LAST", last, testTS), false)
	select {
	case s := <-sink.started:
		t.Fatalf("Accept of %q started with %d already in progress", s, socketLanes)
	case <-time.After(200 * time.Millisecond):
	}
	sink.open(texts[0])
	sink.waitStarted(t, last)
	if _, most := sink.counts(); most > socketLanes {
		t.Errorf("%d Accepts at once, want at most %d", most, socketLanes)
	}
}

func TestSocketModeStopWaitsForEnvelopesInProgress(t *testing.T) {
	r := newSocketRig(t, Options{})
	sink := newGateSink("held")
	sink.linger = 100 * time.Millisecond
	run := r.runWith(t, sink)
	c := r.a.nextConn(t)
	c.envelope(t, "events_api", roomEvent("Ev01", testRoom, "held", testTS), false)
	sink.waitStarted(t, "held")
	if err := run.stop(t); err == nil {
		t.Fatal("Run returned nil after its context ended")
	}
	if n, _ := sink.counts(); n != 0 {
		t.Errorf("%d Accepts still in progress after Run returned", n)
	}
	c.noAck(t, 50*time.Millisecond)
}

func TestSocketModeReplacedConnectionAnswersWhatItHolds(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.a.noHello = true
	sink := newGateSink("held")
	t.Cleanup(func() { sink.open("held") })
	r.runWith(t, sink)
	c1 := r.a.nextConn(t)
	if err := c1.send(map[string]any{"type": "hello", "num_connections": 1}); err != nil {
		t.Fatal(err)
	}
	id := c1.envelope(t, "events_api", roomEvent("Ev01", testRoom, "held", testTS), false)
	sink.waitStarted(t, "held")
	c1.disconnect(t, "warning")
	c2 := r.a.nextConn(t)
	if err := c2.send(map[string]any{"type": "hello", "num_connections": 2}); err != nil {
		t.Fatal(err)
	}
	if c1.closed(100 * time.Millisecond) {
		t.Fatal("replaced connection closed with an envelope still being recorded")
	}
	sink.open("held")
	if ack := c1.ack(t); ack.EnvelopeID != id {
		t.Fatalf("ack %+v, want the held envelope's on the connection that delivered it", ack.socketAck)
	}
	if !c1.closed(2 * time.Second) {
		t.Error("replaced connection still open once it had nothing to answer")
	}
}

func TestLaneKey(t *testing.T) {
	env := func(typ string, payload map[string]any) socketMessage {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return socketMessage{Type: typ, Payload: b}
	}
	room := "conversation:" + testRoom
	submission := submissionPayload()
	delete(submission["view"].(map[string]any), "response_urls")
	for name, c := range map[string]struct {
		m    socketMessage
		want string
	}{
		"message":          {env("events_api", eventPayload("Ev1", testPerson, "hi", testTS)), room},
		"reaction":         {env("events_api", callback("Ev2", map[string]any{"type": "reaction_added", "item": map[string]any{"channel": testRoom, "ts": testTS}})), room},
		"edit":             {env("events_api", callback("Ev3", map[string]any{"type": "message", "subtype": "message_changed", "channel": testRoom})), room},
		"channel object":   {env("events_api", callback("Ev4", map[string]any{"type": "channel_created", "channel": map[string]any{"id": "C9"}})), ""},
		"button":           {env("interactive", buttonPayload()), room},
		"submission":       {env("interactive", submission), "view:VNHU13V36"},
		"command":          {env("slash_commands", commandPayload()), room},
		"unknown envelope": {env("other", map[string]any{"channel_id": testRoom}), ""},
		"not JSON":         {socketMessage{Type: "events_api", Payload: []byte("[")}, ""},
	} {
		if got := laneKey(c.m); got != c.want {
			t.Errorf("%s: lane %q, want %q", name, got, c.want)
		}
	}
}

// observations records what the receiver observes.
type observations struct {
	mu  sync.Mutex
	got []ports.Observation
}

func (o *observations) Observe(_ context.Context, ob ports.Observation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.got = append(o.got, ob)
}

// take returns what was observed since the last take.
func (o *observations) take() []ports.Observation {
	o.mu.Lock()
	defer o.mu.Unlock()
	got := o.got
	o.got = nil
	return got
}

func TestSocketModeReportsTheConnectionsSlackCounts(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.a.noHello = true
	obs := &observations{}
	r.x.observer = obs
	run := r.run(t)
	gauge := func(n int64) ports.Observation {
		return ports.Observation{Kind: ObservationSocketConnections, Adapter: Type, Value: n}
	}
	shared := ports.Observation{Kind: ObservationSocketShared, Adapter: Type}
	hello := func(c *fakeConn, n int) {
		t.Helper()
		if err := c.send(map[string]any{"type": "hello", "num_connections": n}); err != nil {
			t.Fatal(err)
		}
	}
	// waitFor waits for the observations of one hello.
	waitFor := func(want ...ports.Observation) {
		t.Helper()
		var got []ports.Observation
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if got = append(got, obs.take()...); len(got) >= len(want) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		got = append(got, obs.take()...)
		if !slices.Equal(got, want) {
			t.Fatalf("observed %+v, want %+v", got, want)
		}
	}
	c1 := r.a.nextConn(t)
	hello(c1, 1)
	waitFor(gauge(1))
	// A refresh: the replaced connection still counts.
	c1.disconnect(t, "refresh_requested")
	c2 := r.a.nextConn(t)
	hello(c2, 2)
	waitFor(gauge(2))
	// Slack counts one connection more than the receiver holds.
	c2.disconnect(t, "refresh_requested")
	c3 := r.a.nextConn(t)
	hello(c3, 3)
	waitFor(gauge(3), shared)
	if !run.still(50 * time.Millisecond) {
		t.Fatalf("Run ended on a shared app-level token: %v", run.ended(t))
	}
	// The receiver keeps receiving what Slack sends it.
	c3.envelope(t, "events_api", eventPayload("Ev01", testPerson, "still here", testTS), false)
	c3.ack(t)
}

func TestSocketModeFirstHelloOfASharedToken(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.a.noHello = true
	obs := &observations{}
	r.x.observer = obs
	r.run(t)
	c := r.a.nextConn(t)
	if err := c.send(map[string]any{"type": "hello", "num_connections": 2}); err != nil {
		t.Fatal(err)
	}
	c.envelope(t, "events_api", eventPayload("Ev01", testPerson, "hi", testTS), false)
	c.ack(t) // handled after the hello
	got := obs.take()
	want := []ports.Observation{{Kind: ObservationSocketConnections, Adapter: Type, Value: 2}, {Kind: ObservationSocketShared, Adapter: Type}}
	if !slices.Equal(got, want) {
		t.Fatalf("observed %+v, want %+v", got, want)
	}
}
