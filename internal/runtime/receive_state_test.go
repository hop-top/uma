package runtime_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

const stateType model.TypeKey = "com.example.ingress.state"

// stateStep scripts one connection: it calls report with each of reports in
// order, accepts one ingress when accept is set, announces itself on
// stateAdapter.held, and returns err once the test releases it.
type stateStep struct {
	reports []bool
	accept  bool
	err     error
}

// stateAdapter is a receive adapter whose connections follow steps, then
// hold until stopped. With reporter set its instances implement
// adapter.ReceiverReporter and keep each connection's report function in
// reports; panicking makes ReportReceiver panic instead.
type stateAdapter struct {
	steps     []stateStep
	reporter  bool
	panicking bool

	mu      sync.Mutex
	n       int
	reports []func(bool)
	seq     int

	held    chan int      // the index of a connection holding in Run
	release chan struct{} // lets the holding connection return its step's error
}

func newStateAdapter(reporter bool, steps ...stateStep) *stateAdapter {
	return &stateAdapter{steps: steps, reporter: reporter, held: make(chan int, 16), release: make(chan struct{})}
}

func (a *stateAdapter) Describe() adapter.Descriptor {
	return enginetest.IngressAdapter{Typ: stateType}.Describe()
}
func (*stateAdapter) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *stateAdapter) Open(_ context.Context, ch model.Channel, _ adapter.Dependencies) (adapter.Instance, error) {
	in := &stateInstance{a: a, ch: ch.Ref}
	if a.reporter {
		return &reportingInstance{stateInstance: in}, nil
	}
	return in, nil
}

type stateInstance struct {
	a      *stateAdapter
	ch     model.ChannelRef
	report func(bool)
}

func (*stateInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}}, nil
}
func (*stateInstance) Close(context.Context) error { return nil }

func (i *stateInstance) Run(ctx context.Context, s adapter.Sink) error {
	a := i.a
	a.mu.Lock()
	n := a.n
	a.n++
	a.mu.Unlock()
	var step stateStep
	scripted := n < len(a.steps)
	if scripted {
		step = a.steps[n]
	}
	for _, c := range step.reports {
		i.report(c)
	}
	if step.accept {
		a.mu.Lock()
		a.seq++
		id := strconv.Itoa(a.seq)
		a.mu.Unlock()
		_, err := s.Accept(ctx, model.Ingress{Envelope: model.Envelope{
			EventID: "e" + id, MessageID: "m" + id, Destination: enginetest.Dest, Kind: model.EnvelopeMessage,
			Sender:    model.ExternalRef{Channel: i.ch, Namespace: ingressNS, Kind: "user", ID: "u1"},
			Assurance: "verified", Audience: model.Audience{Visibility: model.Public},
			Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi"}}}, OccurredAt: enginetest.T0,
		}})
		if err != nil {
			return err
		}
	}
	a.held <- n
	if !scripted {
		<-ctx.Done()
		return ctx.Err()
	}
	select {
	case <-a.release:
		return step.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// reportingInstance adds the optional reporter interface.
type reportingInstance struct{ *stateInstance }

func (i *reportingInstance) ReportReceiver(report func(bool)) {
	if i.a.panicking {
		panic("reporter exploded")
	}
	i.report = report
	i.a.mu.Lock()
	i.a.reports = append(i.a.reports, report)
	i.a.mu.Unlock()
}

func (a *stateAdapter) reportFn(n int) func(bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reports[n]
}

// stateWaiter is the injected clock of the supervisor: every wait of the
// supervisor is announced on waits and lasts until go_ is released.
type stateWaiter struct {
	waits chan time.Duration
	go_   chan struct{}
}

func newStateWaiter() *stateWaiter {
	return &stateWaiter{waits: make(chan time.Duration, 16), go_: make(chan struct{})}
}

func (w *stateWaiter) Wait(ctx context.Context, d time.Duration) error {
	if isRenewWait(d) {
		<-ctx.Done()
		return ctx.Err()
	}
	w.waits <- d
	select {
	case <-w.go_:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stateRig is a Receive call over a stateAdapter.
type stateRig struct {
	t      *testing.T
	h      *enginetest.Harness
	a      *stateAdapter
	w      *stateWaiter
	call   runtime.Call
	cancel context.CancelFunc
	done   chan error
}

func newStateRig(t *testing.T, a *stateAdapter, retry engine.RetryPolicy, opts ...func(*engine.Config)) *stateRig {
	t.Helper()
	w := newStateWaiter()
	h := newHarness(t, stateType, func(c *engine.Config) {
		c.Adapters = append(c.Adapters, a)
		c.Waiter = w
		c.Retry = retry
		for _, o := range opts {
			o(c)
		}
	})
	h.Configure(enginetest.PerContact(false), enginetest.Dest)
	h.RT.SetJitter(func(max time.Duration) time.Duration { return max })
	return &stateRig{t: t, h: h, a: a, w: w, call: runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "rx"}}}
}

func (r *stateRig) start(owner string) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(r.h.Ctx)
	r.cancel = cancel
	r.t.Cleanup(cancel)
	r.done = make(chan error, 1)
	go func() {
		r.done <- r.h.RT.Receive(ctx, r.call, r.h.Channel.Ref, runtime.ReceiveOptions{Owner: owner})
	}()
}

// held waits for a connection to be holding in Run and returns its index.
func (r *stateRig) held() int {
	r.t.Helper()
	select {
	case n := <-r.a.held:
		return n
	case err := <-r.done:
		r.t.Fatalf("Receive returned %v, want a holding connection", err)
	case <-time.After(10 * time.Second):
		r.t.Fatal("no connection held")
	}
	return -1
}

// waiting waits for the supervisor to be in its wait between connections.
func (r *stateRig) waiting() time.Duration {
	r.t.Helper()
	select {
	case d := <-r.w.waits:
		return d
	case err := <-r.done:
		r.t.Fatalf("Receive returned %v, want a wait", err)
	case <-time.After(10 * time.Second):
		r.t.Fatal("no wait")
	}
	return 0
}

func (r *stateRig) status() runtime.ReceiverStatus {
	r.t.Helper()
	st, err := r.h.RT.ReceiverStatus(r.h.Ctx, r.call, r.h.Channel.Ref)
	if err != nil {
		r.t.Fatalf("ReceiverStatus: %v", err)
	}
	return st
}

func (r *stateRig) result() error {
	r.t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(10 * time.Second):
		r.t.Fatal("Receive did not return")
	}
	return nil
}

func wantState(t *testing.T, what string, got, want runtime.ReceiverStatus) {
	t.Helper()
	got.Owner, got.Until = "", time.Time{}
	if got != want {
		t.Fatalf("%s: status = %+v, want %+v", what, got, want)
	}
}

var statePolicy = engine.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: time.Hour}

// The status follows the supervisor through connect, failure, a stated
// wait, a recovery that accepts ingress, and the end: phase, counters,
// reason, the retry time, and the adapter's link.
func TestReceiverStateFollowsTheSupervisor(t *testing.T) {
	a := newStateAdapter(true,
		stateStep{reports: []bool{true}, err: model.NewError(model.Unavailable, "gateway gone")},
		stateStep{err: statedWaitErr(2 * time.Hour)},
		stateStep{reports: []bool{true}, accept: true},
	)
	r := newStateRig(t, a, statePolicy)
	t0 := r.h.Clock.Now()
	r.start("host-a")

	if r.held() != 0 {
		t.Fatal("first connection is not index 0")
	}
	wantState(t, "first connection, connected", r.status(), runtime.ReceiverStatus{
		Phase: runtime.ReceiverRunning, Since: t0, Link: runtime.ReceiverConnected,
	})
	if st := r.status(); st.Owner != "host-a" || st.Until.IsZero() {
		t.Fatalf("lease fields = %q until %v, want host-a with an expiry", st.Owner, st.Until)
	}

	a.release <- struct{}{}
	if d := r.waiting(); d != time.Second {
		t.Fatalf("first wait = %v, want the backoff of one second", d)
	}
	wantState(t, "after a retryable failure", r.status(), runtime.ReceiverStatus{
		Phase: runtime.ReceiverRetrying, Since: t0, Failures: 1, Reason: model.Unavailable, RetryAt: t0.Add(time.Second),
	})

	t1 := r.h.Clock.Advance(5 * time.Second)
	r.w.go_ <- struct{}{}
	r.held()
	wantState(t, "reconnecting after a failure", r.status(), runtime.ReceiverStatus{
		Phase: runtime.ReceiverRunning, Since: t1, Failures: 1, Reason: model.Unavailable, Link: runtime.ReceiverConnecting,
	})

	a.release <- struct{}{}
	if d := r.waiting(); d != 2*time.Hour {
		t.Fatalf("second wait = %v, want the stated two hours", d)
	}
	wantState(t, "after a stated wait", r.status(), runtime.ReceiverStatus{
		Phase: runtime.ReceiverRetrying, Since: t1, Failures: 2, Reason: model.RateLimited, RetryAt: t1.Add(2 * time.Hour),
	})

	t2 := r.h.Clock.Advance(10 * time.Second) // within the lease: an expired one reads as no call
	r.w.go_ <- struct{}{}
	r.held()
	wantState(t, "recovered: accepted ingress", r.status(), runtime.ReceiverStatus{
		Phase: runtime.ReceiverRunning, Since: t2, Link: runtime.ReceiverConnected,
	})

	r.cancel()
	if err := r.result(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v, want the cancellation", err)
	}
	if st := r.status(); st != (runtime.ReceiverStatus{}) {
		t.Fatalf("status after Receive returned = %+v, want none", st)
	}
}

// holdRelease is a store that parks the release of a receiver lease until
// the test lets it go, so the phase of a call that is ending can be read.
type holdRelease struct {
	store.Store
	parked, resume chan struct{}
}

func (s *holdRelease) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		return fn(releaseHook{Tx: tx, s: s})
	})
}

type releaseHook struct {
	store.Tx
	s *holdRelease
}

func (h releaseHook) ReleaseLease(ctx context.Context, l store.Lease) error {
	h.s.parked <- struct{}{}
	<-h.s.resume
	return h.Tx.ReleaseLease(ctx, l)
}

// A call that ends reports ended with the reason until it has released its
// lease, whatever ended it: exhaustion, a failure retrying cannot cure, a
// stop.
func TestReceiverStateEnded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		steps  []stateStep
		retry  engine.RetryPolicy
		stop   bool
		reason model.ErrorCode
		fails  int
		code   model.ErrorCode // Receive's own failure, "" for none
	}{
		{"exhausted", []stateStep{
			{err: model.NewError(model.Transient, "a")}, {err: model.NewError(model.Transient, "b")},
		}, engine.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second}, false, model.Transient, 2, model.Transient},
		{"permanent failure", []stateStep{
			{err: model.NewError(model.Disabled, "off")},
		}, statePolicy, false, model.Disabled, 0, model.Disabled},
		{"stopped", []stateStep{{}}, statePolicy, true, "", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hold := &holdRelease{parked: make(chan struct{}, 1), resume: make(chan struct{})}
			a := newStateAdapter(false, tc.steps...)
			r := newStateRig(t, a, tc.retry, func(c *engine.Config) {
				hold.Store = c.Store
				c.Store = hold
			})
			var tEnd time.Time
			r.start("host-a")
			for i := range tc.steps {
				r.held()
				if i == len(tc.steps)-1 {
					tEnd = r.h.Clock.Advance(3 * time.Second)
				}
				if tc.stop && i == len(tc.steps)-1 {
					r.cancel()
					continue
				}
				a.release <- struct{}{}
				if i < len(tc.steps)-1 {
					r.waiting()
					r.w.go_ <- struct{}{}
				}
			}
			select {
			case <-hold.parked:
			case <-time.After(10 * time.Second):
				t.Fatal("call did not reach the release of its lease")
			}
			wantState(t, "while releasing", r.status(), runtime.ReceiverStatus{
				Phase: runtime.ReceiverEnded, Since: tEnd, Failures: tc.fails, Reason: tc.reason,
			})
			close(hold.resume)
			err := r.result()
			switch {
			case tc.code == "" && !errors.Is(err, context.Canceled):
				t.Fatalf("Receive = %v, want the cancellation", err)
			case tc.code != "" && !model.IsCode(err, tc.code):
				t.Fatalf("Receive = %v, want %s", err, tc.code)
			}
			if st := r.status(); st != (runtime.ReceiverStatus{}) {
				t.Fatalf("status after Receive returned = %+v, want none", st)
			}
		})
	}
}

// Only the process running a call has its state: a runtime that does not
// reads the owner and expiry from the lease and nothing else.
func TestReceiverStateIsEmptyForAnotherRuntime(t *testing.T) {
	a := newStateAdapter(true, stateStep{reports: []bool{true}})
	r := newStateRig(t, a, statePolicy)
	r.start("host-a")
	r.held()
	if st := r.status(); st.Phase != runtime.ReceiverRunning || st.Link != runtime.ReceiverConnected {
		t.Fatalf("owner's status = %+v, want running and connected", st)
	}
	other := runtime.New(r.h.E)
	st, err := other.ReceiverStatus(r.h.Ctx, r.call, r.h.Channel.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if st.Owner != "host-a" || st.Until.IsZero() {
		t.Fatalf("other runtime's lease fields = %+v, want host-a with an expiry", st)
	}
	st.Owner, st.Until = "", time.Time{}
	if st != (runtime.ReceiverStatus{}) {
		t.Fatalf("other runtime's status = %+v, want no phase, counters or link", st)
	}
}

// An adapter without the reporter interface has no link.
func TestReceiverStateNoLinkWithoutReporter(t *testing.T) {
	a := newStateAdapter(false, stateStep{})
	r := newStateRig(t, a, statePolicy)
	r.start("")
	r.held()
	if st := r.status(); st.Phase != runtime.ReceiverRunning || st.Link != "" {
		t.Fatalf("status = %+v, want running with no link", st)
	}
}

// The link is the adapter's word, per connection: it reads connecting until
// the adapter says connected, follows it back, ends with the connection,
// and a report that outlives its connection changes nothing, not even the
// next connection's link.
func TestReceiverLinkFollowsTheAdapterPerConnection(t *testing.T) {
	a := newStateAdapter(true,
		stateStep{reports: []bool{true, false, true, false}, err: model.NewError(model.Transient, "drop")},
		stateStep{},
	)
	r := newStateRig(t, a, statePolicy)
	r.start("")
	r.held()
	if st := r.status(); st.Link != runtime.ReceiverConnecting {
		t.Fatalf("link = %q after a last report of false, want connecting", st.Link)
	}
	report0 := a.reportFn(0)
	report0(true)
	if st := r.status(); st.Link != runtime.ReceiverConnected {
		t.Fatalf("link = %q after report(true), want connected", st.Link)
	}
	a.release <- struct{}{}
	r.waiting()
	if st := r.status(); st.Phase != runtime.ReceiverRetrying || st.Link != "" {
		t.Fatalf("status between connections = %+v, want retrying with no link", st)
	}
	report0(true) // late: the connection is over
	if st := r.status(); st.Link != "" {
		t.Fatalf("link = %q after a late report between connections, want none", st.Link)
	}
	r.w.go_ <- struct{}{}
	r.held()
	report0(true) // stale: an earlier connection's function
	if st := r.status(); st.Link != runtime.ReceiverConnecting {
		t.Fatalf("link = %q after a stale report, want the new connection's connecting", st.Link)
	}
	a.reportFn(1)(true)
	if st := r.status(); st.Link != runtime.ReceiverConnected {
		t.Fatalf("link = %q after the new connection's report, want connected", st.Link)
	}
}

// The signal is advisory: a receiver reporting connected on every
// connection is retried, waited and ended exactly as one that reports
// nothing, and so is one whose ReportReceiver panics.
func TestReceiverReportDoesNotChangeSupervision(t *testing.T) {
	fails := []error{
		model.NewError(model.Transient, "a"), errDisconnect, statedWaitErr(time.Hour), model.NewError(model.Unavailable, "d"),
	}
	retry := engine.RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 5 * time.Minute}
	run := func(a *stateAdapter) ([]time.Duration, error) {
		r := newStateRig(t, a, retry)
		r.start("")
		var waits []time.Duration
		for i := range fails {
			r.held()
			a.release <- struct{}{}
			if i < len(fails)-1 {
				waits = append(waits, r.waiting())
				r.w.go_ <- struct{}{}
			}
		}
		return waits, r.result()
	}
	steps := func(report bool) []stateStep {
		var out []stateStep
		for _, f := range fails {
			s := stateStep{err: f}
			if errors.Is(f, errDisconnect) {
				s.err = nil
			}
			if report {
				s.reports = []bool{true}
			}
			out = append(out, s)
		}
		return out
	}
	plainWaits, plainErr := run(newStateAdapter(false, steps(false)...))
	repWaits, repErr := run(newStateAdapter(true, steps(true)...))
	boom := newStateAdapter(true, steps(false)...)
	boom.panicking = true
	boomWaits, boomErr := run(boom)
	if !equalDurations(plainWaits, repWaits) || !equalDurations(plainWaits, boomWaits) {
		t.Fatalf("waits: plain %v, reporting %v, panicking %v; want all equal", plainWaits, repWaits, boomWaits)
	}
	want := []time.Duration{time.Second, 2 * time.Second, time.Hour}
	if !equalDurations(plainWaits, want) {
		t.Fatalf("waits = %v, want %v", plainWaits, want)
	}
	for name, err := range map[string]error{"plain": plainErr, "reporting": repErr, "panicking": boomErr} {
		requireCode(t, err, model.Unavailable, "d")
		_ = name
	}
}

// A panicking ReportReceiver leaves the call running with no link.
func TestReceiverPanickingReporterIsAbsent(t *testing.T) {
	a := newStateAdapter(true, stateStep{})
	a.panicking = true
	r := newStateRig(t, a, statePolicy)
	r.start("")
	r.held()
	if st := r.status(); st.Phase != runtime.ReceiverRunning || st.Link != "" {
		t.Fatalf("status = %+v, want running with no link", st)
	}
}

// ReceiverStatus is read from other goroutines while the supervisor writes
// and the adapter reports from several: run with -race.
func TestReceiverStateConcurrentReads(t *testing.T) {
	var fails []stateStep
	for range 6 {
		fails = append(fails, stateStep{reports: []bool{true, false}, err: model.NewError(model.Transient, "x")})
	}
	a := newStateAdapter(true, fails...)
	r := newStateRig(t, a, engine.RetryPolicy{MaxAttempts: 20, BaseDelay: time.Second, MaxDelay: 8 * time.Second})
	r.start("host-a")
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reads atomic.Int64
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				st, err := r.h.RT.ReceiverStatus(r.h.Ctx, r.call, r.h.Channel.Ref)
				if err != nil {
					t.Errorf("ReceiverStatus: %v", err)
					return
				}
				switch st.Phase {
				case "", runtime.ReceiverRunning, runtime.ReceiverRetrying, runtime.ReceiverEnded:
				default:
					t.Errorf("phase = %q", st.Phase)
				}
				reads.Add(1)
			}
		})
	}
	for i := range fails {
		n := r.held()
		report := a.reportFn(n)
		for range 3 {
			wg.Go(func() {
				for j := range 200 {
					report(j%2 == 0)
				}
			})
		}
		a.release <- struct{}{}
		r.waiting()
		r.w.go_ <- struct{}{}
		_ = i
	}
	r.held()
	r.cancel()
	_ = r.result()
	close(stop)
	wg.Wait()
	if reads.Load() == 0 {
		t.Fatal("no status was read")
	}
}

// A lease that expired is no call's: the state of a call that failed to
// renew is not reported as live.
func TestReceiverStateIsEmptyOnceTheLeaseExpired(t *testing.T) {
	a := newStateAdapter(true, stateStep{reports: []bool{true}})
	r := newStateRig(t, a, statePolicy)
	r.start("host-a")
	r.held()
	r.h.Clock.Advance(2 * time.Minute)
	if st := r.status(); st != (runtime.ReceiverStatus{}) {
		t.Fatalf("status past the lease's expiry = %+v, want none", st)
	}
}

// The state is the call holding the lease's: a lease another owner now
// holds reads as that owner's, with none of this call's state.
func TestReceiverStateIsEmptyForAnotherOwnersLease(t *testing.T) {
	a := newStateAdapter(true, stateStep{reports: []bool{true}})
	r := newStateRig(t, a, statePolicy)
	r.start("host-a")
	r.held()
	ref := r.h.Channel.Ref.Ref()
	r.h.Tx(func(tx store.Tx) error {
		l, err := tx.Lease(r.h.Ctx, ref)
		if err != nil {
			return err
		}
		if err := tx.ReleaseLease(r.h.Ctx, l); err != nil {
			return err
		}
		_, err = tx.AcquireLease(r.h.Ctx, store.LeaseRequest{Resource: ref, Owner: "host-b", Until: r.h.Clock.Now().Add(time.Minute)})
		return err
	})
	st := r.status()
	if st.Owner != "host-b" {
		t.Fatalf("owner = %q, want host-b", st.Owner)
	}
	st.Owner, st.Until = "", time.Time{}
	if st != (runtime.ReceiverStatus{}) {
		t.Fatalf("status = %+v under another owner's lease, want none of the call's state", st)
	}
}
