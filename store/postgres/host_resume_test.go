package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/companions/bot/host"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

// TestHostPrepareResumeAcrossRestart runs the bot host's Resume against a
// durable store. A restarted process finds the operation its marker records
// and replays it; a database that never held the Open reports the loss.
func TestHostPrepareResumeAcrossRestart(t *testing.T) {
	ctx := t.Context()
	principal := model.Principal{Tenant: "acme", Actor: "helper"}
	filter := model.Filter{Kinds: []string{model.EventMessageReceived}}
	clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())

	// Each engine gets its own ID seed, as production IDs never repeat: a
	// repeat Resume on a database must not see an operation ref equal to
	// the marker's.
	prepare := func(st store.Store, seed uint32, in host.Intent) (host.Prepared, error) {
		t.Helper()
		e, err := uma.New(uma.Config{
			Store: st,
			Authorizer: policy.AuthorizeFunc(func(context.Context, policy.Authorization) (policy.Decision, error) {
				return policy.Decision{Allow: true}, nil
			}),
			Clock: clock,
			IDs:   portstest.NewSequenceIDs(seed),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.Close(context.WithoutCancel(ctx)) })
		c, err := e.For(principal)
		if err != nil {
			t.Fatal(err)
		}
		return host.Prepare(ctx, host.PrepareConfig{Client: c, Principal: principal, Consumer: "support-helper", Filter: filter, Intent: in})
	}

	st1, err := postgres.Open(ctx, postgres.NewTestDatabase(t, true), postgres.Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	first, err := prepare(st1, 0x71, host.Bootstrap(model.StartBeginning, ""))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if first.Replayed {
		t.Fatal("Bootstrap replayed")
	}

	// The process restarts: a new store handle on a new connection pool, a
	// new engine, the same database.
	st2 := postgres.RestartTestStore(t, st1)
	again, err := prepare(st2, 0x72, host.Resume(first.Marker))
	if err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	if !again.Replayed || again.Marker.Operation != first.Marker.Operation || again.Subscription.Ref != first.Subscription.Ref {
		t.Fatalf("Resume = %+v, want a replay of %+v", again, first)
	}

	// A database that never held the Open: the same derived ref, no record of
	// the operation.
	empty, err := postgres.Open(ctx, postgres.NewTestDatabase(t, true), postgres.Options{Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	for attempt, seed := range []uint32{0x73, 0x74} {
		_, err = prepare(empty, seed, host.Resume(first.Marker))
		var lost *host.SubscriptionContinuityLost
		if !errors.Is(err, host.ErrSubscriptionContinuityLost) || !errors.As(err, &lost) || lost.Expected != first.Marker.Operation {
			t.Fatalf("Resume attempt %d on a fresh database = %v, want continuity lost for %v", attempt+1, err, first.Marker.Operation)
		}
		// The first Open recorded a new operation, so the repeat replays, but
		// of that operation, not the marker's.
		if lost.Replayed != (attempt == 1) || lost.Observed == lost.Expected {
			t.Fatalf("attempt %d: %+v, want replayed only on the repeat and another operation", attempt+1, lost)
		}
	}
}
