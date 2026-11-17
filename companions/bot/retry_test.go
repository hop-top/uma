package bot_test

import (
	"regexp"
	"testing"

	"hop.top/uma/companions/bot"
	"hop.top/uma/model"
)

// A unit that lost to a concurrent unit fails with model.Transient: the
// same request is safe to resubmit. Step resubmits a transient poll, reply
// or acknowledgment a bounded number of times, under the same operation ID,
// before reporting it.
func TestStepResubmitsTransientUnits(t *testing.T) {
	conflict := func() error { return model.NewError(model.Transient, "concurrent unit committed; retry the unit") }
	for _, tc := range []struct {
		name  string
		arm   func(c *fakeClient, n int) *int
		fails int
	}{
		{"poll", func(c *fakeClient, n int) *int {
			hook, calls := failing(n, conflict())
			c.ev.pollErr = hook
			return calls
		}, 3},
		{"reply", func(c *fakeClient, n int) *int {
			hook, calls := failing(n, conflict())
			c.msgs.replyErr = hook
			return calls
		}, 3},
		{"ack", func(c *fakeClient, n int) *int {
			hook, calls := failing(n, conflict())
			c.ev.ackErr = hook
			return calls
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newFake()
			p := &pacer{}
			cfg := config(c)
			cfg.Waiter = p
			cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(""), Handle: replying("ok")}}
			b := mustNew(t, cfg)
			c.ev.push(c.message(t, 1, conv(1), "hi"))
			calls := tc.arm(c, tc.fails)
			r := step(t, b)
			if !r.Acked || len(r.Results) != 1 || r.Results[0].Outcome != bot.Handled {
				t.Fatalf("report = %+v, want one handled event, acknowledged", r)
			}
			if *calls != tc.fails+1 {
				t.Fatalf("%d calls, want %d", *calls, tc.fails+1)
			}
			if len(p.waits) != tc.fails {
				t.Fatalf("%d pauses between resubmissions, want %d", len(p.waits), tc.fails)
			}
			if c.msgs.distinct() != 1 {
				t.Fatalf("%d reply operations, want 1", c.msgs.distinct())
			}
		})
	}
}

// The resubmissions are bounded: a unit still transient after them is
// reported, from Poll and Ack as Step's error and from a reply as a
// retried event.
func TestStepTransientResubmissionIsBounded(t *testing.T) {
	conflict := model.NewError(model.Transient, "concurrent unit committed")
	c := newFake()
	cfg := config(c)
	cfg.Waiter = &pacer{}
	cfg.Patterns = []bot.Pattern{{Name: "all", Match: regexp.MustCompile(""), Handle: replying("ok")}}
	b := mustNew(t, cfg)
	c.ev.push(c.message(t, 1, conv(1), "hi"))

	poll, polls := failing(100, conflict)
	c.ev.pollErr = poll
	if _, err := b.Step(t.Context()); model.CodeOf(err) != model.Transient || *polls != bot.UnitAttempts {
		t.Fatalf("poll: err %v after %d calls, want transient after %d", err, *polls, bot.UnitAttempts)
	}
	c.ev.pollErr = nil

	reply, replies := failing(100, conflict)
	c.msgs.replyErr = reply
	if r := step(t, b); r.Acked || r.Results[0].Outcome != bot.Retried || *replies != bot.UnitAttempts {
		t.Fatalf("reply: report %+v after %d calls", r, *replies)
	}
	c.msgs.replyErr = nil

	ack, acks := failing(100, conflict)
	c.ev.ackErr = ack
	if _, err := b.Step(t.Context()); model.CodeOf(err) != model.Transient || *acks != bot.UnitAttempts {
		t.Fatalf("ack: err %v after %d calls, want transient after %d", err, *acks, bot.UnitAttempts)
	}
}

// Only model.Transient is resubmitted in place: other retryable codes
// (unavailable, rate limited) and permanent failures are reported at once.
func TestStepDoesNotResubmitOtherFailures(t *testing.T) {
	for _, err := range []error{
		model.NewError(model.Unavailable, "down"),
		model.NewError(model.RateLimited, "slow down"),
		model.NewError(model.Validation, "bad"),
		&model.Error{Code: model.Transient, Retry: model.NoRetry},
	} {
		c := newFake()
		cfg := config(c)
		cfg.Waiter = &pacer{}
		b := mustNew(t, cfg)
		hook, calls := failing(1, err)
		c.ev.pollErr = hook
		if _, got := b.Step(t.Context()); got == nil || *calls != 1 {
			t.Errorf("%v: err %v after %d calls, want reported after 1", err, got, *calls)
		}
	}
}
