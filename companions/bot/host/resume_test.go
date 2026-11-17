package host_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store/memory"
)

// These tests run the real engine. Every engine gets its own ID seed:
// engines that share one mint equal operation refs, which would make a
// replacement store look like the original to Resume's Operation-ref check.
// Production IDs are UUIDv7 and never collide this way.

const consumer = "support-helper"

// newEngine returns an engine over store, closed when the test ends.
// Closing an engine neither closes nor empties its store, so several
// engines may share one: that is a restart.
func newEngine(t *testing.T, store *memory.Store, seed uint32) uma.Engine {
	t.Helper()
	clock := portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))
	e, err := uma.New(uma.Config{
		Store: store,
		Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
			return policy.Decision{Allow: true}, nil
		}),
		Clock: clock,
		IDs:   portstest.NewSequenceIDs(seed),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close(context.WithoutCancel(t.Context())) })
	return e
}

func client(t *testing.T, e uma.Engine) uma.Client {
	t.Helper()
	c, err := e.For(principal)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// prepareFilter runs the real Prepare against e with f as the filter.
func prepareFilter(t *testing.T, e uma.Engine, f model.Filter, in host.Intent) (host.Prepared, error) {
	t.Helper()
	return host.Prepare(t.Context(), host.PrepareConfig{
		Client: client(t, e), Principal: principal, Consumer: consumer, Filter: f, Intent: in,
	})
}

// prepare is prepareFilter with the package's shared filter.
func prepare(t *testing.T, e uma.Engine, in host.Intent) (host.Prepared, error) {
	t.Helper()
	return prepareFilter(t, e, filter, in)
}

func mustPrepare(t *testing.T, e uma.Engine, in host.Intent) host.Prepared {
	t.Helper()
	p, err := prepare(t, e, in)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return p
}

// A restarted process: a new engine over the same store replays the Open
// the marker records.
func TestResumeAfterRestart(t *testing.T) {
	store := memory.New(memory.Options{})
	e1 := newEngine(t, store, 1)
	first := mustPrepare(t, e1, host.Bootstrap(model.StartBeginning, ""))
	if first.Replayed {
		t.Fatal("bootstrap replayed")
	}
	if err := e1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	e2 := newEngine(t, store, 2)
	again, err := prepare(t, e2, host.Resume(first.Marker))
	if err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	if !again.Replayed || again.Subscription.Ref != first.Subscription.Ref || again.Marker.Operation != first.Marker.Operation {
		t.Fatalf("Resume = %+v, want a replay of %+v", again, first)
	}
}

// A StartTail bootstrap resumes without a payload conflict: Resume sends the
// marker's request unchanged, so it carries the ID and the fingerprint of the
// Bootstrap's.
func TestResumeAfterTailBootstrap(t *testing.T) {
	store := memory.New(memory.Options{})
	e1 := newEngine(t, store, 1)
	first := mustPrepare(t, e1, host.Bootstrap(model.StartTail, ""))
	if err := e1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	e2 := newEngine(t, store, 2)
	again, err := prepare(t, e2, host.Resume(first.Marker))
	if model.CodeOf(err) == model.Conflict {
		t.Fatalf("Resume = payload conflict %v, want a replay", err)
	}
	if err != nil {
		t.Fatalf("Resume after tail bootstrap: %v", err)
	}
	if !again.Replayed || again.Subscription.Ref != first.Subscription.Ref {
		t.Fatalf("Resume = %+v, want a replay of %+v", again, first)
	}
}

// The Open command ID covers the whole request. A second Bootstrap of the
// same consumer at another Start is a different request: it must get its own
// operation and be accepted (not replayed), never a payload conflict from an
// ID shared with the first: uma fingerprints Start, After, Filter and
// PayloadMajor, so an ID derived from the identity alone would be refused.
func TestOtherOpenRequestIsAnotherOperation(t *testing.T) {
	e := newEngine(t, memory.New(memory.Options{}), 1)
	tail := mustPrepare(t, e, host.Bootstrap(model.StartTail, ""))
	beginning, err := prepare(t, e, host.Bootstrap(model.StartBeginning, ""))
	if err != nil {
		t.Fatalf("Bootstrap at another start = %v, want its own operation", err)
	}
	if beginning.Replayed || beginning.Marker.Operation == tail.Marker.Operation {
		t.Fatalf("Bootstrap at another start = %+v, want a new operation, not a replay of %+v", beginning, tail)
	}
	// The first marker still resumes: its request is its own operation.
	if again, err := prepare(t, e, host.Resume(tail.Marker)); err != nil || !again.Replayed {
		t.Fatalf("Resume of the first marker = %+v, %v, want a replay", again, err)
	}
}

// A replaced store has no record of the Open the marker names. The ref is
// derived from tenant, actor and consumer, so it is identical in the new
// store and proves nothing; the operation does. The first lost Resume's Open
// records a new operation in the replacement, so a repeat is a replay too,
// but of that operation, not the marker's: it must fail again.
func TestResumeAgainstReplacedStore(t *testing.T) {
	e1 := newEngine(t, memory.New(memory.Options{}), 1)
	first := mustPrepare(t, e1, host.Bootstrap(model.StartTail, ""))

	e3 := newEngine(t, memory.New(memory.Options{}), 3)
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := prepare(t, e3, host.Resume(first.Marker))
		if !errors.Is(err, host.ErrSubscriptionContinuityLost) {
			t.Fatalf("Resume attempt %d = %v, want ErrSubscriptionContinuityLost", attempt, err)
		}
		var lost *host.SubscriptionContinuityLost
		if !errors.As(err, &lost) {
			t.Fatalf("Resume attempt %d = %T, want *SubscriptionContinuityLost", attempt, err)
		}
		// The ref is the same in both stores; the operation is what differs.
		if lost.Expected != first.Marker.Operation || lost.Observed == lost.Expected {
			t.Fatalf("attempt %d: expected %v observed %v, want the marker's operation %v and another", attempt, lost.Expected, lost.Observed, first.Marker.Operation)
		}
		if wantReplayed := attempt == 2; lost.Replayed != wantReplayed {
			t.Fatalf("attempt %d: replayed = %t, want %t (the first Open recorded the operation in the replacement)", attempt, lost.Replayed, wantReplayed)
		}
	}

	// The failed resumes adopted nothing and deleted nothing: the
	// replacement their Open created is still there, and bootstrapping the
	// identical request replays it. The host then holds a new marker naming
	// the replacement's operation.
	adopted, err := prepare(t, e3, host.Bootstrap(first.Marker.Open.Start, first.Marker.Open.After))
	if err != nil {
		t.Fatal(err)
	}
	if !adopted.Replayed || adopted.Marker.Operation == first.Marker.Operation {
		t.Fatalf("Bootstrap on the replacement = %+v, want a replay of the replacement's own operation", adopted)
	}
}

// Pin of today's behaviour, not a guarantee. After Events.Close the same
// request still replays: uma does not execute it again, so nothing is
// reopened and Resume succeeds. The guarantee covers a lost operation record
// only; a closed subscription is found by the first Poll, which fails.
func TestResumeAfterClose(t *testing.T) {
	store := memory.New(memory.Options{})
	e1 := newEngine(t, store, 1)
	first := mustPrepare(t, e1, host.Bootstrap(model.StartTail, ""))
	c1 := client(t, e1)
	if _, err := c1.Events().Close(t.Context(), model.Lookup[model.SubscriptionRef]{Call: model.Call{Correlation: "close"}, Ref: first.Subscription.Ref},
		model.Command{Call: model.Call{Correlation: "close"}, Operation: "close-1"}); err != nil {
		t.Fatal(err)
	}

	e2 := newEngine(t, store, 2)
	again, err := prepare(t, e2, host.Resume(first.Marker))
	if err != nil {
		t.Fatalf("Resume after Close = %v; the pinned behaviour is a replay, which goes undetected", err)
	}
	if !again.Replayed || again.Marker.Operation != first.Marker.Operation {
		t.Fatalf("Resume after Close = %+v, want a replay of the original operation", again)
	}
	// Why the guarantee does not cover it: nothing was reopened.
	_, err = client(t, e2).Events().Poll(t.Context(), model.Poll{Call: model.Call{Correlation: "poll"}, Subscription: again.Subscription.Ref, Limit: 1})
	if model.CodeOf(err) != model.Conflict {
		t.Fatalf("Poll after Resume of a closed subscription = %v, want Conflict", err)
	}
}

// Hosts compare the checkpoint Prepare returns with a cursor they saved.
func TestPreparedExposesCheckpoint(t *testing.T) {
	store := memory.New(memory.Options{})
	e1 := newEngine(t, store, 1)
	// One committed event, so the tail checkpoint is a real position rather
	// than the empty one of a log never written.
	if _, err := client(t, e1).Contacts().Create(t.Context(), model.CreateContact{
		Command: model.Command{Call: model.Call{Correlation: "seed"}, Operation: "seed-contact"},
		Tenant:  principal.Tenant, DisplayName: "Ada",
	}); err != nil {
		t.Fatal(err)
	}
	first := mustPrepare(t, e1, host.Bootstrap(model.StartTail, ""))
	if first.Subscription.Checkpoint == "" {
		t.Fatal("Bootstrap returned no checkpoint")
	}
	again, err := prepare(t, newEngine(t, store, 2), host.Resume(first.Marker))
	if err != nil {
		t.Fatal(err)
	}
	if again.Subscription.Checkpoint != first.Subscription.Checkpoint {
		t.Fatalf("Resume checkpoint = %q, want Bootstrap's %q", again.Subscription.Checkpoint, first.Subscription.Checkpoint)
	}
}

// The host persists the Marker as JSON. Resume compares the configured
// filter with the marker's by their JSON, so a filter must encode the same
// after a round trip. model.Filter's omitempty drops an empty Kinds slice, so
// nil and empty encode alike.
func TestMarkerSurvivesJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		filter model.Filter
	}{
		{"filter with kinds", model.Filter{Kinds: []string{model.EventMessageReceived, model.EventInteractionReceived}}},
		{"zero filter", model.Filter{}},
		{"empty kinds", model.Filter{Kinds: []string{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := memory.New(memory.Options{})
			first, err := prepareFilter(t, newEngine(t, store, 1), tc.filter, host.Bootstrap(model.StartTail, ""))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(first.Marker)
			if err != nil {
				t.Fatal(err)
			}
			var loaded host.Marker
			if err := json.Unmarshal(raw, &loaded); err != nil {
				t.Fatal(err)
			}
			again, err := prepareFilter(t, newEngine(t, store, 2), tc.filter, host.Resume(loaded))
			if err != nil {
				t.Fatalf("Resume of the round-tripped marker %s: %v", raw, err)
			}
			if !again.Replayed || again.Marker.Operation != first.Marker.Operation {
				t.Fatalf("Resume = %+v, want a replay of %+v", again, first)
			}
		})
	}
}

// Resume continues from the checkpoint, not from the start: after the first
// batch was acknowledged, the resumed subscription polls only what came
// later. The filter is contact.created, so the events are plain contact
// creations through the public client.
func TestResumeContinuesFromCheckpoint(t *testing.T) {
	contactFilter := model.Filter{Kinds: []string{model.EventContactCreated}}
	// Poll leases the batch for the engine's clock: the store must read the
	// same time, or the lease would already have expired.
	store := memory.New(memory.Options{Clock: portstest.NewFixedClock(time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC))})
	e1 := newEngine(t, store, 11)
	c1 := client(t, e1)
	create := func(op, name string) {
		t.Helper()
		if _, err := c1.Contacts().Create(t.Context(), model.CreateContact{
			Command: model.Command{Call: model.Call{Correlation: "create-" + model.CorrelationID(op)}, Operation: model.OperationID(op)},
			Tenant:  principal.Tenant, DisplayName: name,
		}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	poll := func(c uma.Client, p host.Prepared) model.EventBatch {
		t.Helper()
		batch, err := c.Events().Poll(t.Context(), model.Poll{Call: model.Call{Correlation: "poll"}, Subscription: p.Subscription.Ref, Limit: 10, Wait: time.Second})
		if err != nil {
			t.Fatalf("Poll: %v", err)
		}
		return batch
	}
	// onlyCreationOf requires a batch of exactly one contact.created, for the
	// contact displayed as name.
	onlyCreationOf := func(c uma.Client, batch model.EventBatch, name string) {
		t.Helper()
		page, err := c.Contacts().List(t.Context(), model.ListRequest{Call: model.Call{Correlation: "list"}, Page: model.PageRequest{Limit: 10}})
		if err != nil {
			t.Fatal(err)
		}
		var want model.ID
		for _, contact := range page.Items {
			if contact.DisplayName == name {
				want = contact.Ref.ID
			}
		}
		if want == "" {
			t.Fatalf("no contact %q in %+v", name, page.Items)
		}
		if len(batch.Events) != 1 || batch.Events[0].Kind != model.EventContactCreated || batch.Events[0].Resource.ID != want {
			t.Fatalf("batch = %+v, want the single creation of %s (%s)", batch.Events, name, want)
		}
	}

	first, err := prepareFilter(t, e1, contactFilter, host.Bootstrap(model.StartBeginning, ""))
	if err != nil {
		t.Fatal(err)
	}
	create("contact-a", "Ada")
	batchA := poll(c1, first)
	onlyCreationOf(c1, batchA, "Ada")
	if _, err := c1.Events().Ack(t.Context(), model.Acknowledge{
		Command:      model.Command{Call: model.Call{Correlation: "ack"}, Operation: "ack-a"},
		Subscription: first.Subscription.Ref, Token: batchA.AckToken,
	}); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	create("contact-b", "Brian")
	if err := e1.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	e2 := newEngine(t, store, 12)
	c2 := client(t, e2)
	again, err := prepareFilter(t, e2, contactFilter, host.Resume(first.Marker))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !again.Replayed {
		t.Fatalf("Resume = %+v, want a replay", again)
	}
	onlyCreationOf(c2, poll(c2, again), "Brian") // Brian only: Ada was acknowledged before the restart
}
