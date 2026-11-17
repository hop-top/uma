package bot_test

import (
	"context"
	"errors"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

func config(c *fakeClient) bot.Config {
	return bot.Config{Client: c, Self: self, Subscription: sub, Idle: time.Millisecond, Backoff: time.Millisecond}
}

func mustNew(t *testing.T, cfg bot.Config) *bot.Bot {
	t.Helper()
	b, err := bot.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

func step(t *testing.T, b *bot.Bot) bot.Report {
	t.Helper()
	r, err := b.Step(t.Context())
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	return r
}

func outcomes(r bot.Report) []bot.Outcome {
	out := make([]bot.Outcome, len(r.Results))
	for i, x := range r.Results {
		out[i] = x.Outcome
	}
	return out
}

func routes(r bot.Report) []bot.Route {
	out := make([]bot.Route, len(r.Results))
	for i, x := range r.Results {
		out[i] = x.Route
	}
	return out
}

func mentioned(m model.Message) bool {
	return slices.ContainsFunc(m.Content.Parts, func(p model.Part) bool { return strings.Contains(p.Text, "@bot") })
}

func replying(text string) bot.Handler {
	return func(ctx context.Context, t *bot.Turn) error {
		_, err := t.Reply(ctx, model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}})
		return err
	}
}

func TestNewValidatesConfig(t *testing.T) {
	c := newFake()
	h := replying("x")
	for name, mut := range map[string]func(*bot.Config){
		"no client":            func(cfg *bot.Config) { cfg.Client = nil },
		"no self":              func(cfg *bot.Config) { cfg.Self = "" },
		"no subscription":      func(cfg *bot.Config) { cfg.Subscription = model.SubscriptionRef{} },
		"direct without test":  func(cfg *bot.Config) { cfg.Direct = h },
		"mention without test": func(cfg *bot.Config) { cfg.Mention = h },
		"watched without store": func(cfg *bot.Config) {
			cfg.Watched = h
		},
		"pattern without regexp": func(cfg *bot.Config) {
			cfg.Patterns = []bot.Pattern{{Name: "p", Handle: h}}
		},
		"duplicate pattern": func(cfg *bot.Config) {
			re := regexp.MustCompile("x")
			cfg.Patterns = []bot.Pattern{{Name: "p", Match: re, Handle: h}, {Name: "p", Match: re, Handle: h}}
		},
		"message kind handler": func(cfg *bot.Config) {
			cfg.Kinds = map[string]bot.Handler{model.EventMessageReceived: h}
		},
		"negative concurrency": func(cfg *bot.Config) { cfg.Concurrency = -1 },
	} {
		cfg := config(c)
		mut(&cfg)
		if _, err := bot.New(cfg); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
}

// Routing precedence: direct, then watched, then mention, then the first
// matching pattern; a message no route claims is ignored and acknowledged.
func TestRoutePrecedence(t *testing.T) {
	c := newFake()
	watches := bot.NewMemoryWatches()
	ctx := t.Context()
	dm, watched, room := conv(1), conv(2), conv(3)
	c.direct(dm)
	if err := watches.Add(ctx, bot.Watch{Bot: self, Conversation: watched}); err != nil {
		t.Fatal(err)
	}
	cfg := config(c)
	cfg.Watches = watches
	cfg.Direct, cfg.IsDirect = replying("dm"), func(v model.Conversation) bool { return v.Mode == model.ModePerContact }
	cfg.Watched = replying("watched")
	cfg.Mention, cfg.IsMention = replying("mention"), mentioned
	cfg.Patterns = []bot.Pattern{
		{Name: "help", Match: regexp.MustCompile(`^help`), Handle: replying("help")},
		{Name: "any-h", Match: regexp.MustCompile(`^h`), Handle: replying("h")},
	}
	b := mustNew(t, cfg)
	c.ev.push(
		c.message(t, 1, dm, "@bot help"),
		c.message(t, 2, watched, "@bot help"),
		c.message(t, 3, room, "@bot help"),
		c.message(t, 4, room, "help me"),
		c.message(t, 5, room, "hello"),
		c.message(t, 6, room, "nothing"),
	)
	r := step(t, b)
	want := []bot.Route{bot.RouteDirect, bot.RouteWatched, bot.RouteMention, bot.RoutePattern, bot.RoutePattern, ""}
	if got := routes(r); !slices.Equal(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	if got := outcomes(r); !slices.Equal(got, []bot.Outcome{bot.Handled, bot.Handled, bot.Handled, bot.Handled, bot.Handled, bot.Ignored}) {
		t.Fatalf("outcomes = %v", got)
	}
	if r.Results[3].Handler != "pattern/help" || r.Results[4].Handler != "pattern/any-h" {
		t.Fatalf("pattern handlers = %q, %q", r.Results[3].Handler, r.Results[4].Handler)
	}
	var texts []string
	for _, rep := range c.msgs.sent() {
		texts = append(texts, rep.Content.Parts[0].Text)
	}
	if !slices.Equal(texts, []string{"dm", "watched", "mention", "help", "h"}) {
		t.Fatalf("replies = %v", texts)
	}
	if !r.Acked || c.ev.acked() != 1 {
		t.Fatalf("acked = %v (%d acks), want one", r.Acked, c.ev.acked())
	}
}

// Non-message events route by kind; events without a handler are ignored.
func TestKindHandlers(t *testing.T) {
	c := newFake()
	var got []model.ConversationRef
	cfg := config(c)
	cfg.Kinds = map[string]bot.Handler{model.EventConversationChanged: func(_ context.Context, t *bot.Turn) error {
		got = append(got, t.Conversation)
		return nil
	}}
	b := mustNew(t, cfg)
	changed := model.Event{ID: "00000000-0000-7000-8000-000000000401", Kind: model.EventConversationChanged, Actor: human,
		Payload: model.Extension{Name: model.SchemaConversationChanged, Version: 1,
			Data: []byte(`{"conversation":{"inbox":{"tenant":"tenant","id":"` + string(inbox.ID) + `"},"id":"` + string(conv(7).ID) + `"}}`)}}
	delivery := model.Event{ID: "00000000-0000-7000-8000-000000000402", Kind: model.EventDeliveryUpdated, Actor: human}
	c.ev.push(changed, delivery)
	r := step(t, b)
	if o := outcomes(r); !slices.Equal(o, []bot.Outcome{bot.Handled, bot.Ignored}) {
		t.Fatalf("outcomes = %v", o)
	}
	if r.Results[0].Route != bot.RouteKind || r.Results[0].Handler != "kind/"+model.EventConversationChanged {
		t.Fatalf("result = %+v", r.Results[0])
	}
	if len(got) != 1 || got[0] != conv(7) {
		t.Fatalf("conversation = %v", got)
	}
}

// The batch is acknowledged only after every handler of the batch returned
// successfully; a retryable failure leaves it unacknowledged, and the
// redelivered batch reruns only the unsettled events.
func TestAckOnlyAfterHandlersSucceed(t *testing.T) {
	c := newFake()
	var calls sync.Map
	fail := atomic.Bool{}
	fail.Store(true)
	cfg := config(c)
	cfg.Mention, cfg.IsMention = func(ctx context.Context, t *bot.Turn) error {
		n, _ := calls.LoadOrStore(t.Event.ID, new(atomic.Int32))
		n.(*atomic.Int32).Add(1)
		c.msgs.j.add("handle " + string(t.Event.ID))
		if t.Conversation == conv(2) && fail.Load() {
			return model.NewError(model.Transient, "try later")
		}
		_, err := t.Reply(ctx, text("ok"))
		return err
	}, mentioned
	b := mustNew(t, cfg)
	e1, e2 := c.message(t, 1, conv(1), "@bot one"), c.message(t, 2, conv(2), "@bot two")
	c.ev.push(e1, e2)

	r := step(t, b)
	if r.Acked || c.ev.acked() != 0 {
		t.Fatalf("batch acknowledged despite a retryable failure: %+v", r)
	}
	if o := outcomes(r); !slices.Equal(o, []bot.Outcome{bot.Handled, bot.Retried}) {
		t.Fatalf("outcomes = %v", o)
	}

	fail.Store(false)
	r = step(t, b)
	if !r.Acked || c.ev.acked() != 1 {
		t.Fatalf("redelivered batch not acknowledged: %+v", r)
	}
	for _, id := range []model.ID{e1.ID, e2.ID} {
		n, _ := calls.Load(id)
		want := int32(1)
		if id == e2.ID {
			want = 2
		}
		if got := n.(*atomic.Int32).Load(); got != want {
			t.Errorf("event %s handled %d times, want %d", id, got, want)
		}
	}
	j := c.msgs.j.all()
	if j[len(j)-1] != "ack token-1" {
		t.Fatalf("journal = %v; ack must come last", j)
	}
	if slices.Index(j, "ack token-1") < slices.Index(j, "handle "+string(e2.ID)) {
		t.Fatalf("journal = %v; ack before handler", j)
	}
}

// Reply operation IDs derive from bot, event, handler and reply position:
// a redelivered event, even to a restarted dispatcher, reuses them.
func TestStableOperationIDsAcrossRedelivery(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Mention, cfg.IsMention = func(ctx context.Context, t *bot.Turn) error {
		for _, s := range []string{"first", "second"} {
			if _, err := t.Reply(ctx, text(s)); err != nil {
				return err
			}
		}
		return nil
	}, mentioned
	crash := errors.New("process died")
	c.ev.ackErr = func() error { return crash }
	c.ev.push(c.message(t, 1, conv(1), "@bot a"), c.message(t, 2, conv(2), "@bot b"))

	if _, err := mustNew(t, cfg).Step(t.Context()); !errors.Is(err, crash) {
		t.Fatalf("Step = %v, want the ack failure", err)
	}
	c.ev.ackErr = nil
	r := step(t, mustNew(t, cfg)) // restarted dispatcher, same batch
	if !r.Acked {
		t.Fatal("redelivered batch not acknowledged")
	}
	sent := c.msgs.sent()
	if len(sent) != 8 {
		t.Fatalf("%d reply submissions, want 8", len(sent))
	}
	if c.msgs.distinct() != 4 {
		t.Fatalf("%d distinct operations, want 4 (redelivery must reuse IDs)", c.msgs.distinct())
	}
	for i := range 4 {
		if sent[i].Operation != sent[i+4].Operation || sent[i].Content.Parts[0].Text != sent[i+4].Content.Parts[0].Text {
			t.Errorf("reply %d: %q then %q", i, sent[i].Operation, sent[i+4].Operation)
		}
	}
	other := config(c)
	other.Self = "other-bot"
	other.Mention, other.IsMention = cfg.Mention, mentioned
	c.ev.push(c.message(t, 1, conv(1), "@bot a"))
	step(t, mustNew(t, other))
	if c.msgs.distinct() != 6 {
		t.Fatalf("another bot reused operation IDs: %d distinct", c.msgs.distinct())
	}
}

// At most Concurrency handlers run at once; events of one conversation run
// one at a time, in batch order.
func TestBoundedConcurrency(t *testing.T) {
	c := newFake()
	var inFlight, peak atomic.Int32
	var mu sync.Mutex
	order := map[model.ConversationRef][]string{}
	busy := map[model.ConversationRef]bool{}
	var overlap atomic.Bool
	cfg := config(c)
	cfg.Concurrency = 2
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(`.`), Handle: func(_ context.Context, t *bot.Turn) error {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		mu.Lock()
		if busy[t.Conversation] {
			overlap.Store(true)
		}
		busy[t.Conversation] = true
		order[t.Conversation] = append(order[t.Conversation], t.Message.Content.Parts[0].Text)
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		busy[t.Conversation] = false
		mu.Unlock()
		return nil
	}}}
	b := mustNew(t, cfg)
	var evs []model.Event
	for i := range 8 {
		evs = append(evs, c.message(t, i+1, conv(i%4+1), "m"+pad(i)))
	}
	c.ev.push(evs...)
	r := step(t, b)
	if !r.Acked {
		t.Fatal("not acknowledged")
	}
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak concurrency = %d, want 2", p)
	}
	if overlap.Load() {
		t.Fatal("two handlers ran in one conversation at once")
	}
	for i := range 4 {
		got := order[conv(i+1)]
		want := []string{"m" + pad(i), "m" + pad(i+4)}
		if !slices.Equal(got, want) {
			t.Errorf("conversation %d order = %v, want %v", i+1, got, want)
		}
	}
}

// A retryable failure holds back later events of the same conversation.
func TestFailureHoldsBackItsConversation(t *testing.T) {
	c := newFake()
	var seen []string
	cfg := config(c)
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(`.`), Handle: func(_ context.Context, t *bot.Turn) error {
		seen = append(seen, t.Message.Content.Parts[0].Text)
		if t.Message.Content.Parts[0].Text == "a1" {
			return errors.New("flaky")
		}
		return nil
	}}}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "a1"), c.message(t, 2, conv(1), "a2"), c.message(t, 3, conv(2), "b1"))
	r := step(t, b)
	if o := outcomes(r); !slices.Equal(o, []bot.Outcome{bot.Retried, bot.Retried, bot.Handled}) {
		t.Fatalf("outcomes = %v", o)
	}
	if slices.Contains(seen, "a2") {
		t.Fatalf("a2 ran after a1 failed: %v", seen)
	}
}

func TestClassify(t *testing.T) {
	base := errors.New("x")
	for _, tc := range []struct {
		err  error
		want bot.Outcome
	}{
		{nil, bot.Handled},
		{bot.Drop(base), bot.Dropped},
		{bot.Retry(model.NewError(model.Validation, "v")), bot.Retried},
		{model.NewError(model.Validation, "v"), bot.Dropped},
		{model.NewError(model.Unauthorized, "u"), bot.Dropped},
		{&model.Error{Code: model.NotFound}, bot.Dropped},
		{model.NewError(model.Transient, "t"), bot.Retried},
		{model.NewError(model.OutcomeUnknown, "o"), bot.Retried},
		{context.Canceled, bot.Retried},
		{base, bot.Retried},
	} {
		if got := bot.Classify(tc.err); got != tc.want {
			t.Errorf("Classify(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
	if err := bot.Drop(base); !errors.Is(err, base) {
		t.Error("Drop does not wrap")
	}
}

// Dropped events are acknowledged with the batch; an event still failing
// after MaxAttempts runs is dropped.
func TestErrorOutcomes(t *testing.T) {
	c := newFake()
	var observed []bot.Result
	cfg := config(c)
	cfg.MaxAttempts = 2
	cfg.Observe = func(r bot.Result) { observed = append(observed, r) }
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(`.`), Handle: func(_ context.Context, t *bot.Turn) error {
		switch t.Message.Content.Parts[0].Text {
		case "drop":
			return bot.Drop(errors.New("unprocessable"))
		case "flaky":
			return model.NewError(model.Unavailable, "down")
		}
		return nil
	}}}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "drop"), c.message(t, 2, conv(2), "flaky"), c.message(t, 3, conv(3), "fine"))
	r := step(t, b)
	if r.Acked || !slices.Equal(outcomes(r), []bot.Outcome{bot.Dropped, bot.Retried, bot.Handled}) {
		t.Fatalf("first attempt = %v acked=%v", outcomes(r), r.Acked)
	}
	r = step(t, b)
	if !r.Acked || !slices.Equal(outcomes(r), []bot.Outcome{bot.Dropped, bot.Dropped, bot.Handled}) {
		t.Fatalf("second attempt = %v acked=%v", outcomes(r), r.Acked)
	}
	if r.Results[1].Attempt != 2 || r.Results[1].Err == nil || model.CodeOf(r.Results[1].Err) != model.Unavailable {
		t.Fatalf("exhausted result = %+v", r.Results[1])
	}
	if len(observed) != 6 {
		t.Fatalf("observed %d results, want 6", len(observed))
	}
}

// Events the bot caused and messages that echo its own operations are
// ignored; another actor's operation is not an echo.
func TestOwnEffectsAreIgnored(t *testing.T) {
	c := newFake()
	var handled []string
	cfg := config(c)
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(`.`), Handle: func(_ context.Context, t *bot.Turn) error {
		handled = append(handled, t.Message.Content.Parts[0].Text)
		return nil
	}}}
	b := mustNew(t, cfg)
	own := c.message(t, 1, conv(1), "own event")
	own.Actor = self
	echo := c.message(t, 2, conv(1), "echo")
	foreign := c.message(t, 3, conv(1), "colleague")
	for ref, m := range c.msgs.byRef {
		switch m.Content.Parts[0].Text {
		case "echo":
			m.Origin = &model.Origin{Actor: self}
		case "colleague":
			m.Origin = &model.Origin{Actor: "other-bot"}
		}
		c.msgs.byRef[ref] = m
	}
	c.ev.push(own, echo, foreign)
	r := step(t, b)
	if o := outcomes(r); !slices.Equal(o, []bot.Outcome{bot.Ignored, bot.Ignored, bot.Handled}) {
		t.Fatalf("outcomes = %v", o)
	}
	if !slices.Equal(handled, []string{"colleague"}) {
		t.Fatalf("handled = %v", handled)
	}
}

// A failed message read is classified like a handler error.
func TestReadFailureIsClassified(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(`.`), Handle: replying("x")}}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "hi"))
	c.msgs.getErr = model.NewError(model.Unavailable, "store down")
	if r := step(t, b); r.Acked || r.Results[0].Outcome != bot.Retried {
		t.Fatalf("report = %+v", r)
	}
	c.msgs.getErr = model.NewError(model.NotFound, "gone")
	if r := step(t, b); !r.Acked || r.Results[0].Outcome != bot.Dropped {
		t.Fatalf("report = %+v", r)
	}
}

// Watch and Unwatch change routing for later events only, scoped to the
// bot and conversation.
func TestWatchFromHandler(t *testing.T) {
	c := newFake()
	watches := bot.NewMemoryWatches()
	cfg := config(c)
	cfg.Watches = watches
	cfg.Mention, cfg.IsMention = func(ctx context.Context, t *bot.Turn) error { return t.Watch(ctx) }, mentioned
	cfg.Watched = func(ctx context.Context, t *bot.Turn) error {
		if t.Message.Content.Parts[0].Text == "stop" {
			return t.Unwatch(ctx)
		}
		return nil
	}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "@bot hi"))
	c.ev.push(c.message(t, 2, conv(1), "follow-up"), c.message(t, 3, conv(2), "follow-up"))
	c.ev.push(c.message(t, 4, conv(1), "stop"))
	c.ev.push(c.message(t, 5, conv(1), "after"))
	var got []bot.Route
	for range 4 {
		got = append(got, routes(step(t, b))...)
	}
	want := []bot.Route{bot.RouteMention, bot.RouteWatched, "", bot.RouteWatched, ""}
	if !slices.Equal(got, want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	ok, err := watches.Has(t.Context(), bot.Watch{Bot: "other-bot", Conversation: conv(1)})
	if err != nil || ok {
		t.Fatalf("watch leaked to another bot: %v %v", ok, err)
	}
}

// Run stops when its context ends: in-flight handlers see the
// cancellation, Run waits for them, leaves the batch unacknowledged and
// leaks no goroutine.
func TestRunStopsOnCancel(t *testing.T) {
	before := runtime.NumGoroutine()
	c := newFake()
	started := make(chan struct{}, 4)
	cfg := config(c)
	cfg.Concurrency = 4
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(`.`), Handle: func(ctx context.Context, _ *bot.Turn) error {
		started <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}}}
	b := mustNew(t, cfg)
	for i := range 4 {
		c.ev.push(c.message(t, i+1, conv(i+1), "x"))
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- b.Run(ctx) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if c.ev.acked() != 0 {
		t.Fatal("cancelled batch acknowledged")
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines: %d before, %d after", before, runtime.NumGoroutine())
		}
		time.Sleep(time.Millisecond)
	}
}

// Run keeps polling through empty batches and stops on a permanent poll
// failure.
func TestRunReturnsPermanentPollFailure(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Subscription = model.SubscriptionRef{Tenant: tenant, ID: "00000000-0000-7000-8000-0000000000f9"}
	b := mustNew(t, cfg)
	if err := b.Run(t.Context()); model.CodeOf(err) != model.NotFound {
		t.Fatalf("Run = %v, want not_found", err)
	}
}

// A lost ACK race (another poller of the same subscription won, or the
// lease expired) is not an error: the next poll decides.
func TestLostAckIsNotAnError(t *testing.T) {
	c := newFake()
	b := mustNew(t, config(c))
	c.ev.push(c.message(t, 1, conv(1), "x"))
	c.ev.ackErr = func() error { return model.NewError(model.Conflict, "stale") }
	r := step(t, b)
	if r.Acked {
		t.Fatal("reported acknowledged")
	}
	c.ev.ackErr = func() error {
		return &model.Error{Code: model.Conflict, SafeMessage: "subscription closed", Retry: model.NoRetry}
	}
	if _, err := b.Step(t.Context()); model.CodeOf(err) != model.Conflict {
		t.Fatalf("closed subscription: err = %v", err)
	}
}
