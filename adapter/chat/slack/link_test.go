package slack

import (
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
)

// linkLog records what Run reports to the host about its provider link.
type linkLog struct {
	mu  sync.Mutex
	all []bool
	ch  chan bool
}

func (r *socketRig) watchLink() *linkLog {
	l := &linkLog{ch: make(chan bool, 64)}
	r.x.ReportReceiver(func(connected bool) {
		l.mu.Lock()
		l.all = append(l.all, connected)
		l.mu.Unlock()
		l.ch <- connected
	})
	return l
}

func (l *linkLog) next(t *testing.T) bool {
	t.Helper()
	select {
	case c := <-l.ch:
		return c
	case <-time.After(5 * time.Second):
		t.Fatal("no link report")
	}
	return false
}

func (l *linkLog) reports() []bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.all)
}

// The receiver says connected at the hello of a connection, stays connected
// across a refresh that replaces it, and says not connected when Run ends.
func TestSocketModeReportsItsLink(t *testing.T) {
	r := newSocketRig(t, Options{})
	link := r.watchLink()
	run := r.run(t)
	c1 := r.a.nextConn(t)
	if !link.next(t) {
		t.Fatal("first report is not connected, want it at the hello")
	}
	c1.disconnect(t, "refresh_requested")
	r.a.nextConn(t)
	if !link.next(t) {
		t.Fatal("the replacement's hello did not report connected")
	}
	_ = run.stop(t)
	if got, want := link.reports(), []bool{true, true, false}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v: no gap across the refresh, down once Run returned", got, want)
	}
}

// A connection that says no hello is no link.
func TestSocketModeNeverConnectedWithoutHello(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.a.noHello = true
	r.x.sock.hello = 100 * time.Millisecond
	link := r.watchLink()
	run := r.run(t)
	r.a.nextConn(t)
	if err := run.ended(t); !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Run = %v, want unavailable", err)
	}
	if got := link.reports(); slices.Contains(got, true) {
		t.Fatalf("reports = %v, want no connected before a hello", got)
	}
}

// The connection standing closes while its replacement has not said hello:
// there is no link until it does, and the report does not wait for Run to
// give up on the replacement.
func TestSocketModeReportsDownWhenTheStandingConnectionClosesDuringARefresh(t *testing.T) {
	r := newSocketRig(t, Options{})
	r.x.sock.hello = 400 * time.Millisecond
	link := r.watchLink()
	run := r.run(t)
	c1 := r.a.nextConn(t)
	if !link.next(t) {
		t.Fatal("not connected at the hello")
	}
	r.f.mu.Lock()
	r.a.noHello = true // the replacement stays silent
	r.f.mu.Unlock()
	c1.disconnect(t, "refresh_requested")
	r.a.nextConn(t)
	_ = c1.ws.CloseNow()
	if link.next(t) {
		t.Fatal("report after the standing connection closed is connected")
	}
	if !run.still(50 * time.Millisecond) {
		t.Fatalf("Run returned before the report: %v", run.ended(t))
	}
	if err := run.ended(t); !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Run = %v, want unavailable once the replacement said no hello", err)
	}
}

// Slack closing the only connection ends Run, reported down.
func TestSocketModeReportsDownWhenTheConnectionCloses(t *testing.T) {
	r := newSocketRig(t, Options{})
	link := r.watchLink()
	run := r.run(t)
	c := r.a.nextConn(t)
	if !link.next(t) {
		t.Fatal("not connected at the hello")
	}
	_ = c.ws.CloseNow()
	if link.next(t) {
		t.Fatal("report after the close is connected")
	}
	if err := run.ended(t); !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Run = %v, want unavailable", err)
	}
}
