package runtime_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
)

const retryType model.TypeKey = "com.example.ingress.retry"

// scriptedWaiter is the injected clock of the supervisor: it records every
// wait without sleeping, and returns the context's error once cancelled.
type scriptedWaiter struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (w *scriptedWaiter) Wait(ctx context.Context, d time.Duration) error {
	if isRenewWait(d) {
		<-ctx.Done() // the lease renewal's wait: never fires in a test
		return ctx.Err()
	}
	w.mu.Lock()
	w.waits = append(w.waits, d)
	w.mu.Unlock()
	return ctx.Err()
}

// isRenewWait reports the lease renewal's wait, which also goes through the
// Waiter: between half the renewal period (the first, per-channel phase) and
// the longest jittered wait. No scripted backoff of these tests falls there.
func isRenewWait(d time.Duration) bool {
	return d >= runtime.ReceiverRenew/2 && d <= runtime.ReceiverRenewMax
}

func (w *scriptedWaiter) supervisorWaits() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.waits...)
}

// errDisconnect scripts a connection that returns without an error.
var errDisconnect = errors.New("disconnect")

// failingReceiver is a receive-capable adapter whose connections fail with
// the scripted errors in order, then wait to be stopped.
type failingReceiver struct {
	mu    sync.Mutex
	fails []error
	runs  int
	last  chan struct{} // closed on the first connection past the script
}

func (a *failingReceiver) Describe() adapter.Descriptor {
	return enginetest.IngressAdapter{Typ: retryType}.Describe()
}
func (*failingReceiver) Validate(model.Extension) []model.ValidationIssue { return nil }
func (a *failingReceiver) Open(context.Context, model.Channel, adapter.Dependencies) (adapter.Instance, error) {
	return &failingInstance{a: a}, nil
}

type failingInstance struct{ a *failingReceiver }

func (*failingInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{{Name: model.CapabilityReceive, Available: true}}, nil
}
func (*failingInstance) Close(context.Context) error { return nil }
func (i *failingInstance) Run(ctx context.Context, _ adapter.Sink) error {
	a := i.a
	a.mu.Lock()
	n := a.runs
	a.runs++
	var err error
	if n < len(a.fails) {
		err = a.fails[n]
	} else if n == len(a.fails) {
		close(a.last)
	}
	a.mu.Unlock()
	switch {
	case errors.Is(err, errDisconnect):
		return nil
	case err != nil:
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func statedWaitErr(after time.Duration) error {
	e := model.NewError(model.RateLimited, "slow down")
	e.RetryAfter = after
	return e
}

// receiveWaits runs Receive over an adapter failing with fails, under the
// retry policy, until the connection past the script (or Receive itself)
// ends, and returns the waits between connections and Receive's result.
func receiveWaits(t *testing.T, retry engine.RetryPolicy, fails []error) ([]time.Duration, error) {
	t.Helper()
	w := &scriptedWaiter{}
	a := &failingReceiver{fails: fails, last: make(chan struct{})}
	h := newHarness(t, retryType, func(c *engine.Config) {
		c.Adapters = append(c.Adapters, a)
		c.Waiter = w
		c.Retry = retry
	})
	h.RT.SetJitter(func(max time.Duration) time.Duration { return max })
	ctx, cancel := context.WithCancel(h.Ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.RT.Receive(ctx, runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "rx"}}, h.Channel.Ref, runtime.ReceiveOptions{})
	}()
	var err error
	select {
	case <-a.last:
		cancel()
		err = <-done
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("receiver neither reached its last connection nor ended")
	}
	return w.supervisorWaits(), err
}

// A wait the failed connection states replaces the backoff from the first
// failure on, not only late in the budget: seven failures each stating a
// wait longer than the backoff ceiling are each waited out in full.
func TestReceiveHonoursStatedWaitOnEveryAttempt(t *testing.T) {
	var fails []error
	var want []time.Duration
	for i := range 7 {
		after := time.Duration(i+1) * time.Hour
		fails = append(fails, statedWaitErr(after))
		want = append(want, after)
	}
	waits, err := receiveWaits(t, engine.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: 4 * time.Second}, fails)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v, want cancellation after the last connection", err)
	}
	if !equalDurations(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// A stated wait shorter than the backoff ceiling does not shorten the
// backoff, and a failure stating none keeps the plain backoff.
func TestReceiveKeepsBackoffWithoutALongerStatedWait(t *testing.T) {
	fails := []error{
		model.NewError(model.Transient, "no wait stated"),
		statedWaitErr(time.Millisecond),
		model.NewError(model.Unavailable, "no wait stated"),
		statedWaitErr(0),
	}
	waits, err := receiveWaits(t, engine.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: 5 * time.Minute}, fails)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v, want cancellation after the last connection", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	if !equalDurations(waits, want) {
		t.Fatalf("waits = %v, want the plain backoff %v", waits, want)
	}
}

// An untyped failure and a disconnect state no wait.
func TestReceiveUntypedFailureKeepsBackoff(t *testing.T) {
	fails := []error{errors.New("boom"), errDisconnect}
	waits, err := receiveWaits(t, engine.RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second}, fails)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive = %v, want cancellation after the last connection", err)
	}
	if want := []time.Duration{time.Second, 2 * time.Second}; !equalDurations(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// A stated wait beyond a day is waited as a day, whatever the policy's own
// ceiling says.
func TestReceiveCapsStatedWait(t *testing.T) {
	fails := []error{statedWaitErr(30 * 24 * time.Hour), statedWaitErr(24 * time.Hour), statedWaitErr(23 * time.Hour)}
	waits, _ := receiveWaits(t, engine.RetryPolicy{MaxAttempts: 8}, fails)
	want := []time.Duration{24 * time.Hour, 24 * time.Hour, 23 * time.Hour}
	if !equalDurations(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// The budget is unchanged: the failure that exhausts Retry.MaxAttempts
// ends the call with itself, with no wait for it, and the stated waits
// before it are all honoured.
func TestReceiveStatedWaitsStillCountAgainstTheBudget(t *testing.T) {
	var fails []error
	for range 5 {
		fails = append(fails, statedWaitErr(time.Hour))
	}
	waits, err := receiveWaits(t, engine.RetryPolicy{}, fails)
	requireCode(t, err, model.RateLimited, "slow down")
	if want := []time.Duration{time.Hour, time.Hour, time.Hour, time.Hour}; !equalDurations(waits, want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
}

// Stopping the call interrupts a stated wait at once: a host's cancel does
// not wait out a provider's hour.
func TestReceiveCancellationInterruptsStatedWait(t *testing.T) {
	// Buffered: the wait may start before the test reaches its receive, and a
	// report dropped for want of a receiver would never be sent again.
	entered := make(chan struct{}, 1)
	w := blockingWaiter{entered: entered}
	a := &failingReceiver{fails: []error{statedWaitErr(24 * time.Hour)}, last: make(chan struct{})}
	h := newHarness(t, retryType, func(c *engine.Config) {
		c.Adapters = append(c.Adapters, a)
		c.Waiter = w
	})
	ctx, cancel := context.WithCancel(h.Ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.RT.Receive(ctx, runtime.Call{Principal: h.Service(), Call: model.Call{Correlation: "rx"}}, h.Channel.Ref, runtime.ReceiveOptions{})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("supervisor did not start its wait")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Receive = %v, want the cancellation", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Receive did not return after cancel")
	}
	a.mu.Lock()
	n := a.runs
	a.mu.Unlock()
	if n != 1 {
		t.Fatalf("connections = %d, want none after the cancelled wait", n)
	}
}

// blockingWaiter reports the first wait of a day or more and blocks every
// wait until the context ends.
type blockingWaiter struct{ entered chan struct{} }

func (w blockingWaiter) Wait(ctx context.Context, d time.Duration) error {
	if d >= time.Hour {
		select {
		case w.entered <- struct{}{}:
		default:
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
