package discord

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

func (r *rig) watchLink() *linkLog {
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

// The receiver says connected at READY and RESUMED, not connected when the
// connection ends, and not connected once Run returns.
func TestGatewayReportsItsLink(t *testing.T) {
	r := gatewayRig(t)
	link := r.watchLink()
	ru := r.run(t, &sinkRecorder{})
	c := r.acct.gw.nextConn(t) // READY
	if !link.next(t) {
		t.Fatal("first report is not connected, want it at READY")
	}
	c.closeWith(4000) // a resumable close: the session survives
	if link.next(t) {
		t.Fatal("report after the connection closed is connected, want not connected")
	}
	r.acct.gw.nextConn(t) // RESUMED
	if !link.next(t) {
		t.Fatal("report after resuming is not connected")
	}
	_ = ru.stop(t)
	if got, want := link.reports(), []bool{true, false, true, false}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v, the last before Run returned", got, want)
	}
}

// Dialling and a Hello are not a link: a gateway that never says READY is
// never reported connected.
func TestGatewayNeverConnectedWithoutReady(t *testing.T) {
	r := gatewayRig(t)
	r.x.gw.hello = 50 * time.Millisecond
	r.acct.gw.noHello = true
	link := r.watchLink()
	ru := r.run(t, &sinkRecorder{})
	if err := ru.end(t); !model.IsCode(err, model.Unavailable) {
		t.Fatalf("Run = %v, want unavailable after the connections kept failing", err)
	}
	if got := link.reports(); slices.Contains(got, true) || len(got) == 0 {
		t.Fatalf("reports = %v, want only not connected", got)
	}
}

// A close the receiver cannot survive ends the link too: Run returns with
// the link reported down.
func TestGatewayReportsDownWhenRunEnds(t *testing.T) {
	r := gatewayRig(t)
	link := r.watchLink()
	ru := r.run(t, &sinkRecorder{})
	c := r.acct.gw.nextConn(t)
	if !link.next(t) {
		t.Fatal("not connected at READY")
	}
	c.closeWith(4004) // authentication failed: Run ends, no reconnect
	if err := ru.end(t); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("Run = %v, want unauthorized", err)
	}
	if got := link.reports(); got[len(got)-1] {
		t.Fatalf("reports = %v, want the last to be not connected", got)
	}
}
