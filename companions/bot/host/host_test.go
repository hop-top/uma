package host_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
)

// countingEvents counts the calls that would start consumption. Poll serves
// batch once if set; otherwise, and after, it waits for its context as a long
// poll does, so a running bot idles until stopped.
type countingEvents struct {
	fakeEvents
	polls atomic.Int32

	mu     sync.Mutex
	batch  *model.EventBatch
	polled []model.SubscriptionRef
	acked  []model.SubscriptionRef
}

func (e *countingEvents) Poll(ctx context.Context, p model.Poll) (model.EventBatch, error) {
	e.polls.Add(1)
	e.mu.Lock()
	e.polled = append(e.polled, p.Subscription)
	b := e.batch
	e.batch = nil
	e.mu.Unlock()
	if b != nil {
		return *b, nil
	}
	<-ctx.Done()
	return model.EventBatch{}, ctx.Err()
}

func (e *countingEvents) Ack(_ context.Context, a model.Acknowledge) (model.SubscriptionResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.acked = append(e.acked, a.Subscription)
	return model.SubscriptionResult{}, nil
}

type countingClient struct {
	uma.Client
	ev *countingEvents
}

func (c countingClient) Events() uma.Events { return c.ev }

var runtimePrincipal = model.Principal{Tenant: "acme", Actor: "runtime"}

// hostFixture is a valid Config over counting fakes.
type hostFixture struct {
	cfg host.Config
	rt  *fakeRuntime
	ev  *countingEvents
}

func newFixture() hostFixture {
	rt, ev := newFakeRuntime(), &countingEvents{}
	return hostFixture{
		rt: rt, ev: ev,
		cfg: host.Config{
			Client:      countingClient{ev: ev},
			Principal:   principal,
			Runtime:     rt,
			RuntimeCall: uma.RuntimeCall{Principal: runtimePrincipal},
			Worker:      uma.WorkerOptions{Owner: "support-helper-worker"},
			Prepared: host.Prepared{
				Marker:       marker(),
				Subscription: model.Subscription{Ref: refA, Checkpoint: "cp-1"},
			},
			Bindings: []host.Binding{{Pattern: "POST /widget/", Channel: widget()}},
			Routes:   []host.Route{{Pattern: "GET /widget/events", Handler: appHandler("app")}},
		},
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*host.Config)
		want string
	}{
		"nil client":        {func(c *host.Config) { c.Client = nil }, "client required"},
		"nil runtime":       {func(c *host.Config) { c.Runtime = nil }, "runtime required"},
		"zero principal":    {func(c *host.Config) { c.Principal = model.Principal{} }, "bot principal required"},
		"no actor":          {func(c *host.Config) { c.Principal.Actor = "" }, "bot principal required"},
		"zero runtime call": {func(c *host.Config) { c.RuntimeCall = uma.RuntimeCall{} }, "runtime principal required"},
		"no worker owner":   {func(c *host.Config) { c.Worker.Owner = "" }, "worker owner required"},
		"no subscription":   {func(c *host.Config) { c.Prepared.Subscription = model.Subscription{} }, "prepared subscription required"},
		"marker other actor": {func(c *host.Config) {
			c.Prepared.Marker.Actor = "someone"
		}, "another principal"},
		"marker other tenant": {func(c *host.Config) {
			c.Prepared.Marker.Tenant = "other"
		}, "another principal"},
		"bot client preset": {func(c *host.Config) { c.Bot.Client = c.Client }, "set by the host"},
		"bot self preset":   {func(c *host.Config) { c.Bot.Self = "helper" }, "set by the host"},
		"bot subscription preset": {func(c *host.Config) {
			c.Bot.Subscription = refA
		}, "set by the host"},
		"empty binding pattern": {func(c *host.Config) {
			c.Bindings = append(c.Bindings, host.Binding{Channel: widget()})
		}, "requires a pattern"},
		"conflicting route": {func(c *host.Config) {
			c.Routes = append(c.Routes, host.Route{Pattern: "POST /widget/", Handler: appHandler("dup")})
		}, "POST /widget/"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture()
			// A bot route that bot.New would refuse with its own message:
			// the host's check must come first and say so.
			f.cfg.Bot.Direct = func(context.Context, *bot.Turn) error { return nil }
			tc.mut(&f.cfg)
			h, err := host.New(t.Context(), f.cfg)
			if model.CodeOf(err) != model.Validation {
				t.Fatalf("err = %v, want validation", err)
			}
			if h != nil {
				t.Error("host returned with an error")
			}
			if !strings.Contains(err.Error(), "host: ") || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want host error containing %q", err, tc.want)
			}
			if w, _, _ := f.rt.counts(); w != 0 {
				t.Errorf("Webhook called %d times before the config was validated", w)
			}
		})
	}
}

// The runtime principal is policy: it is never taken from the bot's.
func TestNewDoesNotDefaultTheRuntimePrincipal(t *testing.T) {
	f := newFixture()
	f.cfg.RuntimeCall = uma.RuntimeCall{Call: model.Call{Correlation: "x"}}
	if _, err := host.New(t.Context(), f.cfg); model.CodeOf(err) != model.Validation {
		t.Fatalf("err = %v, want validation", err)
	}
}

func TestNewStartsNothing(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if h.Handler() == nil || h.Bot() == nil {
		t.Fatalf("handler = %v, bot = %v", h.Handler(), h.Bot())
	}
	if w, runs, drains := f.rt.counts(); runs != 0 || drains != 0 || w != 1 {
		t.Errorf("webhooks, runs, drains = %d, %d, %d; want 1, 0, 0", w, runs, drains)
	}
	if n := f.ev.polls.Load(); n != 0 {
		t.Errorf("Poll called %d times", n)
	}
	if n := len(f.ev.opens); n != 0 {
		t.Errorf("Open called %d times: New must not open the subscription", n)
	}
}

func TestNewAppliesBotRoutesAndHostFields(t *testing.T) {
	f := newFixture()
	f.cfg.Bot.Mention = func(context.Context, *bot.Turn) error { return nil }
	f.cfg.Bot.IsMention = func(model.Message) bool { return true }
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatalf("a bot with routes only: %v", err)
	}
	if h.Bot() == nil {
		t.Fatal("no bot")
	}
}

func TestNewReturnsBotConstructionErrors(t *testing.T) {
	f := newFixture()
	f.cfg.Bot.Direct = func(context.Context, *bot.Turn) error { return nil } // no IsDirect
	_, err := host.New(t.Context(), f.cfg)
	if model.CodeOf(err) != model.Validation || !strings.Contains(err.Error(), "IsDirect") {
		t.Fatalf("err = %v, want the bot's validation", err)
	}
	if w, _, _ := f.rt.counts(); w != 0 {
		t.Errorf("Webhook called %d times after the bot failed", w)
	}
}

func TestNewReturnsMuxErrors(t *testing.T) {
	f := newFixture()
	f.cfg.Bindings = []host.Binding{{Pattern: "POST /x/", Channel: model.ChannelRef{ID: "bad"}}}
	if _, err := host.New(t.Context(), f.cfg); model.CodeOf(err) != model.NotFound {
		t.Fatalf("err = %v, want the runtime's not found", err)
	}
}

func TestHandlerServesBindingsAndRoutes(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := serve(h.Handler(), http.MethodPost, "/widget/").Body.String(); got != "hook:widget" {
		t.Errorf("binding body = %q", got)
	}
	if got := serve(h.Handler(), http.MethodGet, "/widget/events").Body.String(); got != "app" {
		t.Errorf("route body = %q", got)
	}
	call, _ := f.rt.lastArgs()
	if call.Principal != runtimePrincipal {
		t.Errorf("Webhook principal = %+v, want the explicit runtime principal", call.Principal)
	}
}

func TestComponentsAreWorkerThenBot(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range h.Components() {
		names = append(names, c.Name)
	}
	if !slices.Equal(names, []string{"worker", "bot"}) {
		t.Errorf("components = %v, want [worker bot]", names)
	}
}

func TestSubscriptionIsThePreparedOne(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Subscription(); got.Ref != refA || got.Checkpoint != "cp-1" {
		t.Errorf("subscription = %+v", got)
	}
}

// runUntilWorkerStarted runs the host, cancels its context once the worker
// is running and returns Run's result. For tests that need the worker
// running when the stop begins.
func runUntilWorkerStarted(t *testing.T, f hostFixture, h *host.Host, o host.Options, extra ...host.Component) error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, o, extra...) }()
	select {
	case <-f.rt.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
		return nil
	}
}

// A context cancelled before Run is a clean stop, whichever of the worker's
// Run and its Shutdown's Drain reaches the runtime first, and the runtime is
// drained once.
func TestHostRunStopsCleanlyAndDrainsOnce(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.Run(ctx, host.Options{}); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if _, _, drains := f.rt.counts(); drains != 1 {
		t.Errorf("drains = %d, want 1", drains)
	}
}

// The same, with the drain made to reach the runtime before the worker's Run
// looks at it, which the previous test only does by chance.
func TestHostRunDrainBeatingTheWorkerIsACleanStop(t *testing.T) {
	f := newFixture()
	f.rt.gate = make(chan struct{})
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, host.Options{}) }()
	within(t, f.rt.started, "worker start")
	within(t, f.rt.drained, "drain")
	close(f.rt.gate)
	if err := within(t, done, "Run"); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if _, runs, drains := f.rt.counts(); runs != 1 || drains != 1 {
		t.Errorf("runs, drains = %d, %d; want 1, 1", runs, drains)
	}
}

// Drain is permanent for the tenant: a second Run on the same host has the
// worker refused, and says so.
func TestHostRunAgainOnDrainedTenantFails(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := runUntilWorkerStarted(t, f, h, host.Options{}); err != nil {
		t.Fatalf("first Run = %v, want nil", err)
	}
	// A live context: the worker is refused before any Shutdown could be
	// blamed, and its refusal ends the group.
	if err := h.Run(t.Context(), host.Options{}); !errors.Is(err, errDraining) {
		t.Fatalf("second Run = %v, want the draining error", err)
	}
}

// Extras stop first and the worker drains last.
func TestHostRunStopsExtrasBeforeTheWorkerDrains(t *testing.T) {
	f := newFixture()
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	var drainedWhenExtraStopped atomic.Bool
	extra := host.Closer("extra", func(context.Context) error {
		select {
		case <-f.rt.drained:
			drainedWhenExtraStopped.Store(true)
		default:
		}
		return nil
	})
	if err := runUntilWorkerStarted(t, f, h, host.Options{}, extra); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if drainedWhenExtraStopped.Load() {
		t.Error("the runtime was drained before the extra component stopped")
	}
	if _, _, drains := f.rt.counts(); drains != 1 {
		t.Errorf("drains = %d, want 1", drains)
	}
}

// The host, not the application, binds the bot to the prepared subscription
// and to the principal's actor: it polls and acknowledges refA and ignores
// events its own actor caused.
func TestBotUsesTheHostSetSubscriptionAndSelf(t *testing.T) {
	f := newFixture()
	var handled []model.ActorID
	f.cfg.Bot.Kinds = map[string]bot.Handler{"app.ping": func(_ context.Context, tn *bot.Turn) error {
		handled = append(handled, tn.Event.Actor)
		return nil
	}}
	f.ev.batch = &model.EventBatch{AckToken: "tok", Events: []model.Event{
		{ID: "00000000-0000-7000-8000-000000000e01", Kind: "app.ping", Actor: principal.Actor},
		{ID: "00000000-0000-7000-8000-000000000e02", Kind: "app.ping", Actor: "human"},
	}}
	h, err := host.New(t.Context(), f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := h.Bot().Step(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !r.Acked || !slices.Equal(handled, []model.ActorID{"human"}) {
		t.Errorf("acked = %v, handled = %v; want acked, [human] (own actor ignored)", r.Acked, handled)
	}
	f.ev.mu.Lock()
	defer f.ev.mu.Unlock()
	if !slices.Equal(f.ev.polled, []model.SubscriptionRef{refA}) || !slices.Equal(f.ev.acked, []model.SubscriptionRef{refA}) {
		t.Errorf("polled = %v, acked = %v; want [%v] each", f.ev.polled, f.ev.acked, refA)
	}
}
