package host_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
)

type trace struct {
	mu  sync.Mutex
	log []string
}

func (tr *trace) add(s string) { tr.mu.Lock(); tr.log = append(tr.log, s); tr.mu.Unlock() }
func (tr *trace) got() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return slices.Clone(tr.log)
}

// waits runs until ctx ends, then returns ctx's error: a well-behaved component.
func waits(name string, tr *trace) host.Component {
	return host.Component{
		Name:     name,
		Run:      func(ctx context.Context) error { <-ctx.Done(); tr.add("run-end " + name); return ctx.Err() },
		Shutdown: func(context.Context) error { tr.add("shutdown " + name); return nil },
	}
}

func TestFirstFailureStopsSiblingsInReverseOrder(t *testing.T) {
	tr := &trace{}
	boom := errors.New("boom")
	failing := host.Component{Name: "bot", Run: func(context.Context) error { return boom }}
	err := host.Run(t.Context(), host.Options{}, waits("worker", tr), failing, waits("server", tr))
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "bot") {
		t.Fatalf("err = %v, want boom naming bot", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v carries the cancellation the supervisor caused", err)
	}
	var shutdowns []string
	for _, s := range tr.got() {
		if strings.HasPrefix(s, "shutdown ") {
			shutdowns = append(shutdowns, s)
		}
	}
	if !slices.Equal(shutdowns, []string{"shutdown server", "shutdown worker"}) {
		t.Fatalf("shutdowns = %v, want reverse registration order", shutdowns)
	}
}

func TestParentCancelIsCleanShutdown(t *testing.T) {
	tr := &trace{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- host.Run(ctx, host.Options{}, waits("a", tr), waits("b", tr)) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("err = %v, want nil on parent cancellation", err)
	}
	var shutdowns []string
	for _, s := range tr.got() {
		if strings.HasPrefix(s, "shutdown ") {
			shutdowns = append(shutdowns, s)
		}
	}
	if !slices.Equal(shutdowns, []string{"shutdown b", "shutdown a"}) {
		t.Fatalf("shutdowns = %v, want both, in reverse registration order", shutdowns)
	}
}

// A parent that hits its deadline is a clean stop too: the components'
// DeadlineExceeded is the parent's doing, not a failure.
func TestParentDeadlineIsCleanShutdown(t *testing.T) {
	tr := &trace{}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := host.Run(ctx, host.Options{}, waits("a", tr), waits("b", tr)); err != nil {
		t.Fatalf("err = %v, want nil on parent deadline", err)
	}
}

// A component that returned by itself still has its Shutdown called: a
// cleanup registered as Shutdown must not be skipped because Run ended.
func TestShutdownRunsForFinishedComponent(t *testing.T) {
	tr := &trace{}
	boom := errors.New("boom")
	fin := host.Component{
		Name:     "fin",
		Run:      func(context.Context) error { return boom },
		Shutdown: func(context.Context) error { tr.add("shutdown fin"); return nil },
	}
	err := host.Run(t.Context(), host.Options{}, waits("other", tr), fin)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if !slices.Contains(tr.got(), "shutdown fin") {
		t.Fatalf("trace = %v, Shutdown skipped for a finished component", tr.got())
	}
}

func TestShutdownErrorIsJoinedAndNamed(t *testing.T) {
	bad := errors.New("close failed")
	c := host.Component{
		Name:     "db",
		Run:      func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		Shutdown: func(context.Context) error { return bad },
	}
	quits := host.Component{Name: "quits", Run: func(context.Context) error { return nil }}
	err := host.Run(t.Context(), host.Options{}, c, quits)
	if !errors.Is(err, bad) || !strings.Contains(err.Error(), "component db: shutdown: close failed") {
		t.Fatalf("err = %v, want it joined and named component db: shutdown", err)
	}
	if !errors.Is(err, host.ErrComponentExited) {
		t.Fatalf("err = %v, want the silent exit kept alongside", err)
	}
}

// A component that returns nil by itself still ends the host: a bot that
// stopped must not leave a server answering nobody.
func TestSilentExitStopsGroup(t *testing.T) {
	tr := &trace{}
	quits := host.Component{Name: "quits", Run: func(context.Context) error { return nil }}
	err := host.Run(t.Context(), host.Options{StopTimeout: 2 * time.Second}, waits("server", tr), quits)
	if !errors.Is(err, host.ErrComponentExited) || !strings.Contains(err.Error(), "quits") {
		t.Fatalf("err = %v, want ErrComponentExited naming quits", err)
	}
	// The server must have been told to stop, not abandoned at the bound.
	if strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("err = %v, server was not cancelled", err)
	}
	if !slices.Contains(tr.got(), "run-end server") {
		t.Fatalf("trace = %v, server not stopped", tr.got())
	}
}

// A component deaf to cancellation cannot hold Run past StopTimeout.
func TestStopTimeoutBoundsShutdown(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	deaf := host.Component{Name: "deaf", Run: func(context.Context) error { <-release; return nil }}
	quits := host.Component{Name: "quits", Run: func(context.Context) error { return nil }}
	start := time.Now()
	err := host.Run(t.Context(), host.Options{StopTimeout: 100 * time.Millisecond}, deaf, quits)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Run waited past the stop timeout")
	}
	if err == nil || !strings.Contains(err.Error(), "deaf") {
		t.Fatalf("err = %v, want it to name deaf", err)
	}
}

func TestRunValidatesComponents(t *testing.T) {
	ok := func(context.Context) error { return nil }
	for name, comps := range map[string][]host.Component{
		"none":     nil,
		"no name":  {{Run: ok}},
		"no run":   {{Name: "x"}},
		"dup name": {{Name: "x", Run: ok}, {Name: "x", Run: ok}},
	} {
		if err := host.Run(t.Context(), host.Options{}, comps...); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want a Validation error", name, err)
		}
	}
	err := host.Run(t.Context(), host.Options{StopTimeout: -1}, host.Component{Name: "x", Run: ok})
	if model.CodeOf(err) != model.Validation {
		t.Errorf("negative stop timeout: err = %v, want a Validation error", err)
	}
}

// A Shutdown that ignores its context cannot hold Run past StopTimeout: it
// is abandoned and named.
func TestStopTimeoutBoundsDeafShutdown(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	hung := host.Component{
		Name:     "hung",
		Run:      func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() },
		Shutdown: func(context.Context) error { <-release; return nil },
	}
	quits := host.Component{Name: "quits", Run: func(context.Context) error { return nil }}
	start := time.Now()
	err := host.Run(t.Context(), host.Options{StopTimeout: 100 * time.Millisecond}, hung, quits)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Run waited past the stop timeout for a deaf Shutdown")
	}
	if err == nil || !strings.Contains(err.Error(), "shutdown of hung did not return within") {
		t.Fatalf("err = %v, want it to name hung and its shutdown overrun", err)
	}
	if strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("err = %v, hung's Run returned on cancel and is not stuck", err)
	}
}

// When the bound passes, components that already returned are not reported
// as stuck, whatever order the supervisor looks at its channels in. Shutdown
// waits for slow's Run to have returned, then for the deadline, so both
// channels are ready when the bound passes.
func TestExpiredBoundDoesNotBlameFinishedComponents(t *testing.T) {
	for range 20 {
		exited := make(chan struct{})
		slow := host.Component{
			Name:     "slow",
			Run:      func(ctx context.Context) error { <-ctx.Done(); defer close(exited); return ctx.Err() },
			Shutdown: func(ctx context.Context) error { <-exited; <-ctx.Done(); return ctx.Err() },
		}
		quits := host.Component{Name: "quits", Run: func(context.Context) error { return nil }}
		err := host.Run(t.Context(), host.Options{StopTimeout: 50 * time.Millisecond}, slow, quits)
		if err == nil || strings.Contains(err.Error(), "did not stop") {
			t.Fatalf("err = %v, finished components reported as stuck", err)
		}
	}
}
