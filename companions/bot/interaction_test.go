package bot_test

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"testing"
	"time"

	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

var (
	public  = model.Audience{Visibility: model.Public}
	anyText = regexp.MustCompile(".")
)

// recording returns a handler that appends its name to *seen.
func recording(seen *[]string, name string) bot.Handler {
	return func(_ context.Context, t *bot.Turn) error {
		if t.Interaction == nil {
			return errors.New("turn has no interaction")
		}
		*seen = append(*seen, name)
		return nil
	}
}

// One handler per interaction kind: the route names the command, action or
// form schema, and the turn carries the interaction read through the
// bot's client and its conversation, but no message.
func TestBotCommandRoute(t *testing.T) {
	c := newFake()
	var seen []string
	var turn *bot.Turn
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{
		"triage": func(_ context.Context, t *bot.Turn) error { turn = t; return nil },
		"other":  recording(&seen, "other"),
	}
	b := mustNew(t, cfg)
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	r := step(t, b)
	if got := r.Results[0]; got.Outcome != bot.Handled || got.Route != bot.RouteCommand || got.Handler != "command/triage" || !r.Acked {
		t.Fatalf("result = %+v (acked %v), want handled by command/triage", got, r.Acked)
	}
	if turn == nil || turn.Interaction == nil || turn.Interaction.Command != "triage" || turn.Message != nil || turn.Conversation != conv(1) {
		t.Fatalf("turn = %+v, want the triage interaction in conversation 1 and no message", turn)
	}
	if len(seen) != 0 {
		t.Fatalf("handlers %v ran for another command", seen)
	}
}

func TestBotActionRoute(t *testing.T) {
	c := newFake()
	var seen []string
	cfg := config(c)
	cfg.Actions = map[string]bot.Handler{"approve": recording(&seen, "approve"), "reject": recording(&seen, "reject")}
	b := mustNew(t, cfg)
	c.ev.push(c.interaction(t, 1, conv(1)), c.interaction(t, 2, conv(1), func(i *model.Interaction) { i.Action = "reject" }))
	r := step(t, b)
	if got := routes(r); !slices.Equal(got, []bot.Route{bot.RouteAction, bot.RouteAction}) {
		t.Fatalf("routes = %v", got)
	}
	if r.Results[0].Handler != "action/approve" || r.Results[1].Handler != "action/reject" || !slices.Equal(seen, []string{"approve", "reject"}) {
		t.Fatalf("handlers = %q, %q; ran %v", r.Results[0].Handler, r.Results[1].Handler, seen)
	}
}

func TestBotSubmissionRoute(t *testing.T) {
	c := newFake()
	var seen []string
	cfg := config(c)
	cfg.Submissions = map[string]bot.Handler{"feedback": recording(&seen, "feedback")}
	b := mustNew(t, cfg)
	c.ev.push(c.interaction(t, 1, conv(1), asSubmission("feedback")), c.interaction(t, 2, conv(1), asSubmission("unknown")))
	r := step(t, b)
	if got := outcomes(r); !slices.Equal(got, []bot.Outcome{bot.Handled, bot.Ignored}) {
		t.Fatalf("outcomes = %v, want the unmatched schema ignored", got)
	}
	if r.Results[0].Route != bot.RouteSubmission || r.Results[0].Handler != "submission/feedback" || !r.Acked {
		t.Fatalf("result = %+v (acked %v)", r.Results[0], r.Acked)
	}
}

// An interaction that names a command, an action and form fields takes the
// first route that has a handler for it: command, action, submission, then
// the Kinds fallback. An interaction no route claims is ignored.
func TestBotInteractionPrecedence(t *testing.T) {
	both := func(i *model.Interaction) {
		i.Command, i.Action = "run", "go"
		i.Fields = model.Extension{Name: "form", Version: 1}
	}
	var seen []string
	all := map[string]bot.Handler{}
	record := func(name string) bot.Handler { return recording(&seen, name) }
	steps := []struct {
		name string
		cfg  func(*bot.Config)
		want bot.Route
	}{
		{"all four", func(cfg *bot.Config) {
			cfg.Commands, cfg.Actions = map[string]bot.Handler{"run": record("command")}, map[string]bot.Handler{"go": record("action")}
			cfg.Submissions, cfg.Kinds = map[string]bot.Handler{"form": record("submission")}, all
		}, bot.RouteCommand},
		{"no command handler", func(cfg *bot.Config) {
			cfg.Commands, cfg.Actions = map[string]bot.Handler{"elsewhere": record("command")}, map[string]bot.Handler{"go": record("action")}
			cfg.Submissions, cfg.Kinds = map[string]bot.Handler{"form": record("submission")}, all
		}, bot.RouteAction},
		{"submission and fallback", func(cfg *bot.Config) {
			cfg.Submissions, cfg.Kinds = map[string]bot.Handler{"form": record("submission")}, all
		}, bot.RouteSubmission},
		{"fallback only", func(cfg *bot.Config) { cfg.Kinds = all }, bot.RouteKind},
		{"nothing", func(cfg *bot.Config) { cfg.Commands = map[string]bot.Handler{"elsewhere": record("command")} }, ""},
	}
	all[model.EventInteractionReceived] = record("kind")
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			seen = nil
			c := newFake()
			cfg := config(c)
			s.cfg(&cfg)
			b := mustNew(t, cfg)
			c.ev.push(c.interaction(t, 1, conv(1), both))
			r := step(t, b)
			if r.Results[0].Route != s.want {
				t.Fatalf("route = %q, want %q (ran %v)", r.Results[0].Route, s.want, seen)
			}
			if s.want == "" {
				if r.Results[0].Outcome != bot.Ignored || len(seen) != 0 {
					t.Fatalf("result = %+v, ran %v; want ignored", r.Results[0], seen)
				}
				return
			}
			if len(seen) != 1 || r.Results[0].Outcome != bot.Handled {
				t.Fatalf("ran %v, outcome %s; want exactly the one route", seen, r.Results[0].Outcome)
			}
		})
	}
}

// Another bot's interactions in a shared inbox are not answered, whatever
// route would claim them; the interaction is still read to decide.
func TestBotIgnoresForeignBot(t *testing.T) {
	c := newFake()
	var seen []string
	cfg := config(c)
	cfg.Actions = map[string]bot.Handler{"approve": recording(&seen, "action")}
	cfg.Kinds = map[string]bot.Handler{model.EventInteractionReceived: recording(&seen, "kind")}
	ours := model.ExternalRef{Namespace: "org.example.fake", Kind: "bot", ID: "mine"}
	cfg.IsAddressed = func(in model.Interaction) bool { return in.Bot != nil && *in.Bot == ours }
	b := mustNew(t, cfg)
	foreign := ours
	foreign.ID = "theirs"
	c.ev.push(
		c.interaction(t, 1, conv(1), func(i *model.Interaction) { i.Bot = &foreign }),
		c.interaction(t, 2, conv(1)),
		c.interaction(t, 3, conv(1), func(i *model.Interaction) { i.Bot = &ours }),
	)
	r := step(t, b)
	if got := outcomes(r); !slices.Equal(got, []bot.Outcome{bot.Ignored, bot.Ignored, bot.Handled}) {
		t.Fatalf("outcomes = %v, want only our own interaction handled", got)
	}
	if !slices.Equal(seen, []string{"action"}) || !r.Acked {
		t.Fatalf("ran %v, acked %v", seen, r.Acked)
	}
}

// Events the bot caused are ignored on the interaction path too.
func TestBotIgnoresItsOwnInteractionEvents(t *testing.T) {
	c := newFake()
	var seen []string
	cfg := config(c)
	cfg.Actions = map[string]bot.Handler{"approve": recording(&seen, "action")}
	b := mustNew(t, cfg)
	ev := c.interaction(t, 1, conv(1))
	ev.Actor = self
	c.ev.push(ev)
	if r := step(t, b); r.Results[0].Outcome != bot.Ignored || len(seen) != 0 {
		t.Fatalf("result = %+v, ran %v", r.Results[0], seen)
	}
}

// A public reply to an interaction names the interaction for correlation
// and answers no message.
func TestBotReplyCorrelates(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{"triage": replying("on it")}
	b := mustNew(t, cfg)
	ev := c.interaction(t, 1, conv(1), asCommand("triage"))
	c.ev.push(ev)
	step(t, b)
	sent := c.msgs.sent()
	if len(sent) != 1 || sent[0].InResponseTo == nil || *sent[0].InResponseTo != c.ints.refOf(t, ev) || sent[0].InReplyTo != nil || sent[0].Conversation != conv(1) {
		t.Fatalf("replies = %+v, want one public reply correlated with the interaction", sent)
	}
}

func (f *fakeInteractions) refOf(t *testing.T, ev model.Event) model.InteractionRef {
	t.Helper()
	for ref := range f.byRef {
		if ref.Ref() == ev.Resource {
			return ref
		}
	}
	t.Fatalf("no interaction for event %s", ev.ID)
	return model.InteractionRef{}
}

func responding(mode string, followUp bool) bot.Handler {
	return func(ctx context.Context, t *bot.Turn) error {
		aud := public
		if mode == model.ResponseModeEphemeral {
			aud.Visibility = model.Restricted
		}
		respond := t.Respond
		if followUp {
			respond = t.FollowUp
		}
		_, err := respond(ctx, mode, aud, text("done"))
		return err
	}
}

// A handler's response is one operation derived from the bot, event,
// handler and position: a redelivered event, even to a restarted dispatcher,
// resubmits it and uma replays it, one provider call in all.
func TestBotRespondReplaysOnRedelivery(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{"triage": func(ctx context.Context, t *bot.Turn) error {
		if _, err := t.Respond(ctx, model.ResponseModeMessage, public, text("one")); err != nil {
			return err
		}
		_, err := t.FollowUp(ctx, model.ResponseModeEphemeral, model.Audience{Visibility: model.Restricted}, text("two"))
		return err
	}}
	crash := errors.New("process died")
	c.ev.ackErr = func() error { return crash }
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	if _, err := mustNew(t, cfg).Step(t.Context()); !errors.Is(err, crash) {
		t.Fatalf("Step = %v, want the ack failure", err)
	}
	c.ev.ackErr = nil
	if r := step(t, mustNew(t, cfg)); !r.Acked || r.Results[0].Outcome != bot.Handled {
		t.Fatalf("redelivered batch: %+v", r)
	}
	got := c.ints.submissions()
	if len(got) != 4 || c.ints.calls() != 2 {
		t.Fatalf("%d submissions and %d operations, want 4 and 2: redelivery must reuse operation IDs", len(got), c.ints.calls())
	}
	if got[0].Operation == got[1].Operation {
		t.Fatal("the response and the follow-up share an operation ID")
	}
	for i := range 2 {
		if got[i].Operation != got[i+2].Operation || got[i].Mode != got[i+2].Mode || got[i].FollowUp != got[i+2].FollowUp || !got[i].ExpiresAt.Equal(got[i+2].ExpiresAt) {
			t.Errorf("submission %d: %+v then %+v", i, got[i], got[i+2])
		}
	}
	if got[0].FollowUp || !got[1].FollowUp || got[0].Mode != model.ResponseModeMessage || got[1].Mode != model.ResponseModeEphemeral {
		t.Fatalf("submissions = %+v, want an initial message then an ephemeral follow-up", got)
	}
	other := config(c)
	other.Self = "other-bot"
	other.Commands = cfg.Commands
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	if r := step(t, mustNew(t, other)); r.Results[0].Outcome != bot.Ignored {
		t.Fatalf("another bot's initial response lost the claim: %+v", r.Results[0])
	}
}

// The response expires with the provider's respond window, or, when the
// provider states none, a fixed span after the interaction was recorded:
// never at a time read when the handler runs, so a rerun resubmits the
// same expiry.
func TestBotRespondExpiry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window time.Duration
		mod    func(*model.Interaction)
		want   time.Time
	}{
		{"provider window", 0, func(*model.Interaction) {}, interactionAt.Add(15 * time.Minute)},
		{"no window, default", 0, func(i *model.Interaction) { i.RespondUntil = time.Time{} }, interactionAt.Add(bot.DefaultRespondWindow)},
		{"no window, configured", time.Hour, func(i *model.Interaction) { i.RespondUntil = time.Time{} }, interactionAt.Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFake()
			cfg := config(c)
			cfg.RespondWindow = tc.window
			cfg.Commands = map[string]bot.Handler{"triage": responding(model.ResponseModeMessage, false)}
			b := mustNew(t, cfg)
			c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage"), tc.mod))
			step(t, b)
			if got := c.ints.submissions(); len(got) != 1 || !got[0].ExpiresAt.Equal(tc.want) {
				t.Fatalf("submissions = %+v, want expiry %v", got, tc.want)
			}
		})
	}
}

// The initial response is claimed once: of two workers answering one
// interaction, the loser is told Conflict and its event is ignored, not
// retried and not dropped as a failure.
func TestBotRespondLosesInitialClaim(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{"triage": responding(model.ResponseModeMessage, false)}
	b := mustNew(t, cfg)
	ev := c.interaction(t, 1, conv(1), asCommand("triage"))
	c.ints.initial = "bot.another-worker"
	c.ev.push(ev)
	r := step(t, b)
	if got := r.Results[0]; got.Outcome != bot.Ignored || got.Err != nil || !r.Acked || r.Retried() {
		t.Fatalf("result = %+v (acked %v), want ignored without error, acknowledged", got, r.Acked)
	}
	if n := len(c.ints.submissions()); n != 1 {
		t.Fatalf("%d submissions, want the losing one only: a lost claim is never retried", n)
	}
}

// A follow-up that conflicts is a failure, not a lost claim.
func TestBotFollowUpConflictIsNotALostClaim(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{"triage": responding(model.ResponseModeEphemeral, true)}
	b := mustNew(t, cfg)
	c.ints.respErr = func() error { return model.NewError(model.Conflict, "redacted") }
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	if got := step(t, b).Results[0]; got.Outcome == bot.Ignored {
		t.Fatalf("result = %+v: only the initial claim loser is ignored", got)
	}
}

// A response after the respond window is dropped: waiting cannot help.
func TestBotRespondExpiredDrops(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Commands = map[string]bot.Handler{"triage": responding(model.ResponseModeMessage, false)}
	b := mustNew(t, cfg)
	c.ints.respErr = func() error { return model.NewError(model.DeadlineExpired, "respond window ended") }
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	r := step(t, b)
	if got := r.Results[0]; got.Outcome != bot.Dropped || model.CodeOf(got.Err) != model.DeadlineExpired || !r.Acked {
		t.Fatalf("result = %+v (acked %v), want dropped as expired, acknowledged", got, r.Acked)
	}
	if n := len(c.ints.submissions()); n != 1 {
		t.Fatalf("%d submissions, want one", n)
	}
}

// A response that lost a race to a concurrent unit (Transient) is
// resubmitted in place under the same operation ID, as replies are.
func TestBotRespondResubmitsTransient(t *testing.T) {
	c := newFake()
	p := &pacer{}
	cfg := config(c)
	cfg.Waiter = p
	cfg.Commands = map[string]bot.Handler{"triage": responding(model.ResponseModeMessage, false)}
	b := mustNew(t, cfg)
	hook, calls := failing(2, model.NewError(model.Transient, "concurrent unit committed; retry the unit"))
	c.ints.respErr = hook
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	r := step(t, b)
	if !r.Acked || r.Results[0].Outcome != bot.Handled || *calls != 3 || c.ints.calls() != 1 {
		t.Fatalf("report %+v after %d calls and %d operations; want handled on the third, one operation", r, *calls, c.ints.calls())
	}
	got := c.ints.submissions()
	if got[0].Operation != got[2].Operation {
		t.Fatalf("resubmission changed the operation ID: %q then %q", got[0].Operation, got[2].Operation)
	}
}

// A transient failure that outlasts the resubmissions leaves the batch for
// redelivery, like any retryable failure.
func TestBotRespondTransientExhaustedIsRetried(t *testing.T) {
	c := newFake()
	cfg := config(c)
	cfg.Waiter = &pacer{}
	cfg.Commands = map[string]bot.Handler{"triage": responding(model.ResponseModeMessage, false)}
	b := mustNew(t, cfg)
	c.ints.respErr = func() error { return model.NewError(model.Transient, "concurrent unit committed; retry the unit") }
	c.ev.push(c.interaction(t, 1, conv(1), asCommand("triage")))
	r := step(t, b)
	if r.Acked || r.Results[0].Outcome != bot.Retried {
		t.Fatalf("report = %+v, want retried and unacknowledged", r)
	}
}

// Respond and FollowUp answer the turn's interaction: a turn without one
// refuses.
func TestBotRespondNeedsAnInteraction(t *testing.T) {
	c := newFake()
	cfg := config(c)
	var got error
	cfg.Kinds = map[string]bot.Handler{model.EventConversationChanged: func(ctx context.Context, t *bot.Turn) error {
		_, got = t.Respond(ctx, model.ResponseModeMessage, public, text("x"))
		return nil
	}}
	b := mustNew(t, cfg)
	c.ev.push(model.Event{ID: "00000000-0000-7000-8000-000000000401", Kind: model.EventConversationChanged, Actor: human})
	step(t, b)
	if model.CodeOf(got) != model.Validation {
		t.Fatalf("Respond on a turn without an interaction = %v, want validation", got)
	}
}

// Events of one conversation keep their order across kinds: an interaction
// runs after the earlier message of its conversation.
func TestBotInteractionJoinsItsConversationLane(t *testing.T) {
	c := newFake()
	var order []string
	cfg := config(c)
	cfg.Concurrency = 4
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: anyText, Handle: func(_ context.Context, t *bot.Turn) error {
		time.Sleep(20 * time.Millisecond)
		order = append(order, "message")
		return nil
	}}}
	cfg.Actions = map[string]bot.Handler{"approve": func(_ context.Context, t *bot.Turn) error {
		order = append(order, "action")
		return nil
	}}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "hi"), c.interaction(t, 2, conv(1)))
	step(t, b)
	if !slices.Equal(order, []string{"message", "action"}) {
		t.Fatalf("order = %v, want the message first: one conversation runs one event at a time", order)
	}
}

func TestNewValidatesInteractionRoutes(t *testing.T) {
	c := newFake()
	for name, mut := range map[string]func(*bot.Config){
		"nil command handler":   func(cfg *bot.Config) { cfg.Commands = map[string]bot.Handler{"x": nil} },
		"empty action name":     func(cfg *bot.Config) { cfg.Actions = map[string]bot.Handler{"": replying("x")} },
		"nil submission":        func(cfg *bot.Config) { cfg.Submissions = map[string]bot.Handler{"x": nil} },
		"negative respond span": func(cfg *bot.Config) { cfg.RespondWindow = -1 },
	} {
		cfg := config(c)
		mut(&cfg)
		if _, err := bot.New(cfg); model.CodeOf(err) != model.Validation {
			t.Errorf("%s: err = %v, want validation", name, err)
		}
	}
	cfg := config(c)
	cfg.Kinds = map[string]bot.Handler{model.EventInteractionReceived: replying("x")}
	if _, err := bot.New(cfg); err != nil {
		t.Errorf("interaction.received fallback refused: %v", err)
	}
}
