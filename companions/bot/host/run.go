package host

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"hop.top/uma/model"
)

// ErrComponentExited reports a component that returned without an error
// while the host was still meant to run: a bot, worker or server that
// stopped on its own is a failure of the host, not a clean stop.
var ErrComponentExited = errors.New("host: component exited unexpectedly")

// DefaultStopTimeout bounds shutdown when Options.StopTimeout is zero.
const DefaultStopTimeout = 10 * time.Second

// Component is one part of a host: something to run and, optionally, a
// bounded way to stop it. Run must return once its context ends or
// Shutdown was called. Shutdown should honour its context, which carries the
// stop deadline; one that does not is abandoned and named when
// Options.StopTimeout passes, and keeps running in the background.
type Component struct {
	Name     string
	Run      func(ctx context.Context) error
	Shutdown func(ctx context.Context) error
}

// Options configures Run.
type Options struct {
	// StopTimeout bounds every Shutdown and the wait for Runs to return
	// together: when it passes, a Shutdown or Run still going is abandoned
	// and named in the result. Zero is DefaultStopTimeout; negative is
	// invalid.
	StopTimeout time.Duration
}

type exit struct {
	i   int
	err error
}

// Run starts every component, waits for ctx to end or the first component
// to return, then stops the rest: it cancels their context, calls Shutdown
// in reverse order of registration and waits for the Runs, all within
// StopTimeout. It returns the joined failures, each named by component. A
// cancelled ctx, or one past its deadline, is a clean stop: the cancellation
// is not reported, and a component that stopped because of it is not blamed
// (its context.Canceled, or the parent's own error, is not reported). Run
// returns nil unless a component or a Shutdown failed or the stop overran
// StopTimeout. Run starts and stops only the components it was given: it
// never closes an engine, a store or a server the application owns. The
// Shutdowns run in one goroutine; a Shutdown that overruns the bound is named and abandoned along
// with that goroutine, so the Shutdowns registered before it may still be
// running when Run returns. A Run that ignores cancellation is likewise
// named and abandoned.
func Run(ctx context.Context, o Options, comps ...Component) error {
	if err := check(o, comps); err != nil {
		return err
	}
	timeout := o.StopTimeout
	if timeout == 0 {
		timeout = DefaultStopTimeout
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	exits := make(chan exit, len(comps))
	for i, c := range comps {
		go func() { exits <- exit{i, c.Run(runCtx)} }()
	}
	errs := make([]error, len(comps))
	done := make([]bool, len(comps))
	record := func(e exit, stopping bool) {
		done[e.i] = true
		if e.err == nil {
			return
		}
		// What the supervisor's cancel, or a parent that was cancelled or
		// ran out of time, made a component return is a stop, not a failure.
		if stopping && errors.Is(e.err, context.Canceled) {
			return
		}
		if pe := ctx.Err(); pe != nil && errors.Is(e.err, pe) {
			return
		}
		errs[e.i] = fmt.Errorf("component %s: %w", comps[e.i].Name, e.err)
	}
	remaining := len(comps)
	select {
	case e := <-exits:
		// A cancelled parent may win this select against ctx.Done: its
		// components' Canceled errors are then the clean stop, not failures.
		record(e, ctx.Err() != nil)
		if ctx.Err() == nil && e.err == nil {
			errs[e.i] = fmt.Errorf("component %s: %w", comps[e.i].Name, ErrComponentExited)
		}
		remaining--
	case <-ctx.Done():
	}
	cancel()
	stopCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer stop()
	// Shutdowns run one after another in their own goroutine so that one
	// ignoring its context cannot hold Run past the bound.
	var (
		mu       sync.Mutex
		current  string
		shutdown []error
	)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for i := len(comps) - 1; i >= 0; i-- {
			sd := comps[i].Shutdown
			if sd == nil {
				continue
			}
			mu.Lock()
			current = comps[i].Name
			mu.Unlock()
			if err := sd(stopCtx); err != nil {
				mu.Lock()
				shutdown = append(shutdown, fmt.Errorf("component %s: shutdown: %w", comps[i].Name, err))
				mu.Unlock()
			}
		}
	}()
	select {
	case <-finished:
	case <-stopCtx.Done():
		select {
		case <-finished:
		default:
			mu.Lock()
			shutdown = append(shutdown, fmt.Errorf("host: shutdown of %s did not return within %s", current, timeout))
			mu.Unlock()
		}
	}
	// Past the bound, still take what has already returned: a component
	// that exited in time must not be blamed because both channels are ready.
	drain := func() {
		for remaining > 0 {
			select {
			case e := <-exits:
				record(e, true)
				remaining--
			default:
				return
			}
		}
	}
	for remaining > 0 {
		select {
		case e := <-exits:
			record(e, true)
			remaining--
		case <-stopCtx.Done():
			drain()
			if remaining > 0 {
				var stuck []string
				for i, d := range done {
					if !d {
						stuck = append(stuck, comps[i].Name)
					}
				}
				mu.Lock()
				shutdown = append(shutdown, fmt.Errorf("host: %s did not stop within %s", strings.Join(stuck, ", "), timeout))
				mu.Unlock()
			}
			remaining = 0
		}
	}
	mu.Lock()
	defer mu.Unlock()
	return errors.Join(append(errs, shutdown...)...)
}

func check(o Options, comps []Component) error {
	invalid := func(msg string) error { return model.NewError(model.Validation, "host: "+msg) }
	if o.StopTimeout < 0 {
		return invalid("negative stop timeout")
	}
	if len(comps) == 0 {
		return invalid("no components")
	}
	seen := map[string]bool{}
	for _, c := range comps {
		switch {
		case c.Name == "" || c.Run == nil:
			return invalid("component requires a name and a Run")
		case seen[c.Name]:
			return invalid("duplicate component " + c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}
