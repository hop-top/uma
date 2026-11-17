package conformance

import (
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// retrying submits req until the engine stops reporting that the unit lost
// a serialization race to a concurrent one. Resubmitting the same operation
// ID is how a caller retries: the response is claimed once however often
// the same request arrives.
func (env *interactionEnv) retrying(req model.Respond) (model.Acceptance, error) {
	var a model.Acceptance
	var err error
	for range 500 {
		if a, err = env.client.Interactions().Respond(env.ctx, req); !model.IsCode(err, model.Transient) {
			return a, err
		}
	}
	return a, err
}

func (env *interactionEnv) ephemeralFor(in model.Interaction) model.Ephemeral {
	return model.Ephemeral{
		Command: env.cmd(), Conversation: in.Ref.Conversation, Recipient: in.Actor.Identity,
		Content: textContent("only you"), ExpiresAt: env.now().Add(30 * time.Minute), Interaction: &in.Ref,
	}
}

var interactionWinnerCases = []interactionCase{
	{"InitialResponseHasOneWinner", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		first := env.respond(env.request(in, model.ResponseModeMessage))
		_, err := env.client.Interactions().Respond(env.ctx, env.request(in, model.ResponseModeEphemeral))
		wantCode(t, err, model.Conflict)
		got := env.stored(in.Ref)
		if got.Response.Initial == nil || *got.Response.Initial != first.Operation.Ref || got.Response.FollowUps != 0 {
			t.Fatalf("response after the loser = %+v, want only the first claim", got.Response)
		}
		if n := len(env.events(model.EventInteractionUpdated)); n != 1 {
			t.Fatalf("interaction.updated events = %d, want only the winner's", n)
		}
		if n := len(env.messages(s)); n != 1 {
			t.Fatalf("messages = %d, want only the winner's", n)
		}
	}},
	{"ConcurrentInitialResponsesHaveOneWinner", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		for round := range 3 {
			in := env.seed(s)
			const workers = 8
			reqs := make([]model.Respond, workers)
			for i := range reqs {
				reqs[i] = env.request(in, model.ResponseModeEphemeral)
			}
			accepted := make([]*model.Acceptance, workers)
			var wg sync.WaitGroup
			for i := range workers {
				wg.Go(func() {
					a, err := env.retrying(reqs[i])
					switch {
					case err == nil:
						accepted[i] = &a
					case !model.IsCode(err, model.Conflict):
						t.Errorf("round %d: response %d: %v, want accepted or conflict", round, i, err)
					}
				})
			}
			wg.Wait()
			var winners []model.OperationRef
			for _, a := range accepted {
				if a != nil {
					winners = append(winners, a.Operation.Ref)
				}
			}
			got := env.stored(in.Ref)
			if len(winners) != 1 || got.Response.Initial == nil || *got.Response.Initial != winners[0] || got.Response.FollowUps != 0 {
				t.Fatalf("round %d: %d accepted %v, stored response %+v; want exactly one winner, stored", round, len(winners), winners, got.Response)
			}
		}
	}},
	{"FollowUpsNeverFailTheInitialResponse", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		follow := env.request(in, model.ResponseModeMessage)
		follow.FollowUp = true
		before := env.respond(follow)
		initial := env.respond(env.request(in, model.ResponseModeMessage))
		after := follow
		after.Command = env.cmd()
		last := env.respond(after)
		for _, a := range []model.Acceptance{before, initial, last} {
			if op := env.deliver(a.Operation.Ref); op.State != model.ProviderAccepted {
				t.Fatalf("response settled %+v", op)
			}
		}
		var followed int
		for _, c := range env.provider.all() {
			if c.Method == "respond" && c.FollowUp {
				followed++
			}
		}
		if calls := env.provider.all(); len(calls) != 3 || followed != 2 {
			t.Fatalf("provider calls = %+v, want the initial response and two follow-ups marked as such", calls)
		}
		got := env.stored(in.Ref)
		if got.Response.Initial == nil || *got.Response.Initial != initial.Operation.Ref || got.Response.FollowUps != 2 {
			t.Fatalf("response = %+v, want the initial and two follow-ups", got.Response)
		}
		if before.Operation.Ref == initial.Operation.Ref {
			t.Fatal("a follow-up and the initial response shared an operation")
		}
		changes := map[string]int{}
		for _, ev := range env.events(model.EventInteractionUpdated) {
			changes[decodeUpdate(t, ev).Change]++
		}
		if changes[model.InteractionChangeResponded] != 1 || changes[model.InteractionChangeFollowUp] != 2 {
			t.Fatalf("interaction.updated changes = %v, want one responded and two follow_up", changes)
		}
	}},
	{"FollowUpsRacingInitialResponsesNeverConflict", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		for round := range 3 {
			in := env.seed(s)
			const initials, followUps = 3, 5
			var wg sync.WaitGroup
			var mu sync.Mutex
			var winners []model.OperationRef
			followed := 0
			for i := range initials + followUps {
				req := env.request(in, model.ResponseModeEphemeral)
				req.FollowUp = i >= initials
				wg.Go(func() {
					a, err := env.retrying(req)
					mu.Lock()
					defer mu.Unlock()
					switch {
					case err == nil && req.FollowUp:
						followed++
					case err == nil:
						winners = append(winners, a.Operation.Ref)
					case req.FollowUp:
						t.Errorf("round %d: follow-up: %v, want it accepted whatever raced it", round, err)
					case !model.IsCode(err, model.Conflict):
						t.Errorf("round %d: initial response: %v, want accepted or conflict", round, err)
					}
				})
			}
			wg.Wait()
			got := env.stored(in.Ref)
			if len(winners) != 1 || got.Response.Initial == nil || *got.Response.Initial != winners[0] || int(got.Response.FollowUps) != followUps || followed != followUps {
				t.Fatalf("round %d: winners %v, %d follow-ups accepted, stored %+v; want one winner and all %d follow-ups counted",
					round, winners, followed, got.Response, followUps)
			}
		}
	}},
	{"ConcurrentSubmissionsOfOneRespondAreOneResponse", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		req := env.request(in, model.ResponseModeEphemeral)
		const workers = 8
		var created, replayed atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Go(func() {
				a, err := env.retrying(req)
				switch {
				case err != nil:
					t.Errorf("submission of one request: %v, want accepted or replayed, never a conflict", err)
				case a.Replayed:
					replayed.Add(1)
				default:
					created.Add(1)
				}
			})
		}
		wg.Wait()
		got := env.stored(in.Ref)
		if created.Load() != 1 || replayed.Load() != workers-1 || got.Response.Initial == nil || got.Response.FollowUps != 0 {
			t.Fatalf("%d created and %d replayed of %d, stored %+v; want one response and every other submission its replay",
				created.Load(), replayed.Load(), workers, got.Response)
		}
	}},
	{"UnsupportedWithoutTheCapability", func(t *testing.T, env *interactionEnv) {
		none, only := env.sites[noRespondType], env.sites[respondOnlyType]
		plain, withRespond := env.seed(none), env.seed(only)
		_, err := env.client.Interactions().Respond(env.ctx, env.request(plain, model.ResponseModeMessage))
		wantCode(t, err, model.Unsupported)
		follow := env.request(withRespond, model.ResponseModeMessage)
		follow.FollowUp = true
		_, err = env.client.Interactions().Respond(env.ctx, follow)
		wantCode(t, err, model.Unsupported)
		_, err = env.client.Interactions().Ephemeral(env.ctx, env.ephemeralFor(withRespond))
		wantCode(t, err, model.Unsupported)
		_, err = env.client.Interactions().Ephemeral(env.ctx, env.ephemeralFor(plain))
		wantCode(t, err, model.Unsupported)
		for _, in := range []model.Interaction{plain, withRespond} {
			if got := env.stored(in.Ref); got.Response.Initial != nil || got.Response.FollowUps != 0 || got.Revision != in.Revision {
				t.Fatalf("interaction after a refusal = %+v, want it untouched", got)
			}
		}
		if calls := env.provider.all(); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none: nothing falls back to another send", calls)
		}
		// The initial response is still the adapter's to give.
		a := env.respond(env.request(withRespond, model.ResponseModeMessage))
		if op := env.deliver(a.Operation.Ref); op.State != model.ProviderAccepted {
			t.Fatalf("respond-only adapter settled %+v", op)
		}
	}},
	{"ExpiredWindowIsDeadlineExpired", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		late, queued := env.seed(s), env.seed(s)
		a := env.respond(env.request(queued, model.ResponseModeMessage))
		if !env.advance(2 * time.Hour) {
			t.Skip("the store cannot move its clock")
		}
		// At submit: the claiming unit refuses a response past the window.
		_, err := env.client.Interactions().Respond(env.ctx, env.request(late, model.ResponseModeMessage))
		wantCode(t, err, model.DeadlineExpired)
		if got := env.stored(late.Ref); got.Response.Initial != nil || got.Revision != late.Revision {
			t.Fatalf("interaction after an expired respond = %+v, want it untouched", got)
		}
		// At the attempt: the queued response fails explicitly, never reaching the provider.
		op := env.deliver(a.Operation.Ref)
		if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.DeadlineExpired {
			t.Fatalf("queued response settled %+v, want failed with deadline_expired", op)
		}
		if calls := env.provider.all(); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none past the window", calls)
		}
	}},
	{"ReplayedRespondReusesTheResult", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		req := env.request(in, model.ResponseModeMessage)
		first := env.respond(req)
		env.deliver(first.Operation.Ref)
		again, err := env.client.Interactions().Respond(env.ctx, req)
		if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("retry = %+v, %v; want a replay of %v", again, err, first.Operation.Ref)
		}
		if env.advance(2 * time.Hour) {
			if again, err = env.client.Interactions().Respond(env.ctx, req); err != nil || !again.Replayed {
				t.Fatalf("retry after the window = %+v, %v; want the stored result", again, err)
			}
		}
		changed := req
		changed.Content = textContent("different")
		_, err = env.client.Interactions().Respond(env.ctx, changed)
		wantCode(t, err, model.Conflict)
		got := env.stored(in.Ref)
		if got.Response.Initial == nil || *got.Response.Initial != first.Operation.Ref || got.Response.FollowUps != 0 {
			t.Fatalf("response = %+v, want the one claim", got.Response)
		}
		if n, calls := len(env.messages(s)), len(env.provider.all()); n != 1 || calls != 1 {
			t.Fatalf("%d messages and %d provider calls, want one of each", n, calls)
		}
		if n := len(env.events(model.EventInteractionUpdated)); n != 1 {
			t.Fatalf("interaction.updated events = %d, want 1", n)
		}
	}},
	{"EphemeralNamingAnInteractionIsAFollowUp", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		in := env.seed(s)
		a, err := env.client.Interactions().Ephemeral(env.ctx, env.ephemeralFor(in))
		if err != nil {
			t.Fatalf("Ephemeral: %v", err)
		}
		env.queued = append(env.queued, a.Operation.Ref)
		got := env.stored(in.Ref)
		if got.Response.Initial != nil || got.Response.FollowUps != 1 || got.Revision == in.Revision {
			t.Fatalf("response after the ephemeral = %+v, want one follow-up and no initial", got.Response)
		}
		if op := env.deliver(a.Operation.Ref); op.State != model.ProviderAccepted {
			t.Fatalf("ephemeral settled %+v", op)
		}
		calls := env.provider.all()
		if len(calls) != 1 || calls[0].Method != "ephemeral" || calls[0].Interaction == nil || calls[0].Interaction.Token != interactionToken ||
			calls[0].Recipient.ID != s.actor.SourceID {
			t.Fatalf("provider calls = %+v, want one ephemeral to the actor carrying the interaction and its token", calls)
		}
		if n := len(env.messages(s)); n != 0 {
			t.Fatalf("ephemeral stored %d messages", n)
		}
		env.noToken(a.Operation.Ref)
		// The initial response is still open.
		env.respond(env.request(in, model.ResponseModeMessage))
	}},
	{"RedactedInteractionRefusesAndFailsQueuedResponses", func(t *testing.T, env *interactionEnv) {
		s := env.sites[interactiveType]
		queued, fresh := env.seed(s), env.seed(s)
		a := env.respond(env.request(queued, model.ResponseModeMessage))
		redact := func(in model.Interaction) {
			env.put(func(tx store.Tx) error {
				_, err := tx.Redact(env.ctx, store.Redaction{Interaction: &in.Ref, Expected: in.Revision, Revision: model.Revision("redacted-" + string(in.Ref.ID))})
				return err
			})
		}
		redact(env.stored(queued.Ref))
		op := env.deliver(a.Operation.Ref)
		if op.State != model.Failed || op.Failure == nil || op.Failure.Code != model.Permanent {
			t.Fatalf("queued response after redaction settled %+v, want a final permanent failure", op)
		}
		if calls := env.provider.all(); len(calls) != 0 {
			t.Fatalf("provider calls = %+v, want none after redaction", calls)
		}
		redact(fresh)
		_, err := env.client.Interactions().Respond(env.ctx, env.request(env.stored(fresh.Ref), model.ResponseModeMessage))
		wantCode(t, err, model.Conflict)
		if got := env.stored(fresh.Ref); !got.Redacted || got.Response.Initial != nil {
			t.Fatalf("redacted interaction = %+v, want it unclaimed", got)
		}
		if slices.ContainsFunc(env.events(model.EventInteractionUpdated), func(ev model.Event) bool { return ev.Resource == fresh.Ref.Ref() }) {
			t.Fatal("a response event was recorded for a redacted interaction")
		}
	}},
}
