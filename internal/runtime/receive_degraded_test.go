package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

// degradeOnlyInstance is an instance with the degradation reporter alone.
type degradeOnlyInstance struct {
	*stateInstance
	a *degradeAdapter
}

func (i *degradeOnlyInstance) ReportReceiverDegraded(report func(string)) { i.a.set(nil, report) }

// bothInstance has both reporters.
type bothInstance struct {
	*stateInstance
	a *degradeAdapter
}

func (i *bothInstance) ReportReceiver(report func(bool))           { i.a.set(report, nil); i.report = report }
func (i *bothInstance) ReportReceiverDegraded(report func(string)) { i.a.set(nil, report) }

type degradeAdapter struct {
	*stateAdapter
	both bool

	mu   sync.Mutex
	link func(bool)
	deg  func(string)
}

func (a *degradeAdapter) set(l func(bool), d func(string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l != nil {
		a.link = l
	}
	if d != nil {
		a.deg = d
	}
}

func (a *degradeAdapter) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	in := &stateInstance{a: a.stateAdapter, ch: ch.Ref}
	if a.both {
		return &bothInstance{stateInstance: in, a: a}, nil
	}
	in.report = func(bool) {}
	return &degradeOnlyInstance{stateInstance: in, a: a}, nil
}

func (a *degradeAdapter) fns() (func(bool), func(string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.link, a.deg
}

// newStateRigFor is a state rig whose channel is served by d.
func newStateRigFor(t *testing.T, d *degradeAdapter) *stateRig {
	return newStateRig(t, d.stateAdapter, statePolicy, func(c *engine.Config) { c.Adapters[len(c.Adapters)-1] = d })
}

// A degradation shows the link as degraded with its code on both status
// reads, over the reported link; recovery restores connected.
func TestReceiverDegradedLink(t *testing.T) {
	a := &degradeAdapter{stateAdapter: newStateAdapter(false), both: true}
	r := newStateRigFor(t, a)
	r.start("host-a")
	r.held()
	link, deg := a.fns()
	link(true)
	if st := r.status(); st.Link != runtime.ReceiverConnected || st.Detail != "" {
		t.Fatalf("link %q detail %q, want connected", st.Link, st.Detail)
	}
	deg("google.mail.push_denied")
	st := r.status()
	if st.Link != runtime.ReceiverDegraded || st.Detail != "google.mail.push_denied" || st.Reason != "" || st.Phase != runtime.ReceiverRunning {
		t.Fatalf("status %+v, want degraded with the code and no reason", st)
	}
	mem, ok := r.h.RT.ReceiverState(r.call, r.h.Channel.Ref)
	if !ok || mem.Link != runtime.ReceiverDegraded || mem.Detail != "google.mail.push_denied" {
		t.Fatalf("ReceiverState %+v %v, want degraded", mem, ok)
	}
	// A link report while degraded keeps the degradation and moves the base.
	link(false)
	if st := r.status(); st.Link != runtime.ReceiverDegraded {
		t.Fatalf("link %q after report(false) while degraded", st.Link)
	}
	deg("")
	if st := r.status(); st.Link != runtime.ReceiverConnecting || st.Detail != "" {
		t.Fatalf("link %q detail %q after recovery, want connecting", st.Link, st.Detail)
	}
	link(true)
	if st := r.status(); st.Link != runtime.ReceiverConnected {
		t.Fatalf("link %q, want connected", st.Link)
	}
}

// An instance with the degradation reporter alone starts at connecting and
// shows degraded too.
func TestReceiverDegradedWithoutLinkReporter(t *testing.T) {
	a := &degradeAdapter{stateAdapter: newStateAdapter(false)}
	r := newStateRigFor(t, a)
	r.start("host-a")
	r.held()
	_, deg := a.fns()
	if st := r.status(); st.Link != runtime.ReceiverConnecting {
		t.Fatalf("link %q, want connecting", st.Link)
	}
	deg("x.code")
	if st := r.status(); st.Link != runtime.ReceiverDegraded || st.Detail != "x.code" {
		t.Fatalf("status %+v, want degraded", st)
	}
}

// A degradation reported through a connection that ended does not count,
// and a new connection starts clean.
func TestReceiverDegradedDoesNotSurviveConnection(t *testing.T) {
	a := &degradeAdapter{stateAdapter: newStateAdapter(false, stateStep{err: model.NewError(model.Unavailable, "gone")}), both: true}
	r := newStateRigFor(t, a)
	r.start("host-a")
	r.held()
	_, deg := a.fns()
	deg("x.code")
	a.release <- struct{}{}
	r.waiting()
	if st := r.status(); st.Link != "" || st.Detail != "" {
		t.Fatalf("link %q detail %q while retrying, want none", st.Link, st.Detail)
	}
	deg("late.code") // old connection's report
	r.w.go_ <- struct{}{}
	r.held()
	if st := r.status(); st.Link != runtime.ReceiverConnecting || st.Detail != "" {
		t.Fatalf("new connection link %q detail %q, want connecting and clean", st.Link, st.Detail)
	}
}

// StartSpread delays the first connection by a stable offset in
// [0, StartSpread) and no later one; zero does not wait.
func TestReceiveStartSpread(t *testing.T) {
	const spread = 6 * time.Hour
	a := newStateAdapter(false, stateStep{err: model.NewError(model.Unavailable, "gone")})
	r := newStateRig(t, a, statePolicy)
	ctx, cancel := context.WithCancel(r.h.Ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- r.h.RT.Receive(ctx, r.call, r.h.Channel.Ref, runtime.ReceiveOptions{Owner: "o", StartSpread: spread})
	}()
	d := r.waiting()
	if d < 0 || d >= spread {
		t.Fatalf("start wait %v outside [0, %v)", d, spread)
	}
	if d2 := runtime.StartOffset(r.h.Channel.Ref, spread); d2 != d {
		t.Fatalf("start wait %v, want the channel's stable offset %v", d, d2)
	}
	select {
	case <-a.held:
		t.Fatal("connected before the start wait ended")
	case <-time.After(100 * time.Millisecond):
	}
	r.w.go_ <- struct{}{}
	r.held() // first connection
	a.release <- struct{}{}
	if d := r.waiting(); d != time.Second {
		t.Fatalf("reconnect wait %v, want the plain backoff, no start offset", d)
	}
	cancel()
	<-done
}

// Offsets are stable per channel, spread over the window and within it.
func TestStartOffsetStableAndSpread(t *testing.T) {
	const spread = time.Hour
	seen := map[time.Duration]bool{}
	for n := range 200 {
		ref := renewRef(n)
		d := runtime.StartOffset(ref, spread)
		if d < 0 || d >= spread {
			t.Fatalf("offset %v outside [0, %v)", d, spread)
		}
		if runtime.StartOffset(ref, spread) != d {
			t.Fatal("offset not stable")
		}
		seen[d] = true
	}
	if len(seen) < 100 {
		t.Fatalf("200 channels share only %d offsets", len(seen))
	}
}

func TestReceiveRefusesNegativeStartSpread(t *testing.T) {
	r := newStateRig(t, newStateAdapter(false), statePolicy)
	err := r.h.RT.Receive(r.h.Ctx, r.call, r.h.Channel.Ref, runtime.ReceiveOptions{Owner: "o", StartSpread: -time.Second})
	if !model.IsCode(err, model.Validation) {
		t.Fatalf("err = %v, want validation", err)
	}
}
