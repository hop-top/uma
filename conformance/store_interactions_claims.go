package conformance

import (
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// interactionClaimCases cover RecordInteractionResponse: the single initial
// response, follow-up counting, revisions, and the claim under contention.
var interactionClaimCases = []storeCase{
	{"InitialResponseIsClaimedOnce", func(t *testing.T, e *env) {
		rec := e.storedInteraction(t, "tok")
		ref := rec.Interaction.Ref
		first, second := e.operationRef(t), e.operationRef(t)

		var got model.Interaction
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			got, err = tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: first, Revision: "i2"})
			return err
		})
		want := rec.Interaction.Clone()
		want.Response.Initial, want.Revision = &first, "i2"
		if !reflect.DeepEqual(got, want) {
			t.Errorf("claim returned %+v, want %+v", got, want)
		}
		e.wantInteraction(t, want)
		e.wantToken(t, ref, store.ResponseToken{Token: "tok", ExpiresAt: rec.TokenExpiresAt})

		// A second initial claim loses, whichever operation makes it and
		// whatever revision it brings, and changes nothing.
		for _, c := range []struct {
			name string
			op   model.OperationRef
		}{{"another operation", second}, {"the winning operation", first}} {
			op := c.op
			err := e.txErr(func(tx store.Tx) error {
				_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: op, Revision: "i3"})
				wantCode(t, err, model.Conflict, "second initial claim by ", c.name)
				return nil
			})
			if err != nil {
				t.Fatalf("Transact: %v", err)
			}
			e.wantInteraction(t, want)
		}
	}},
	{"FollowUpsCountAndNeverFailOnTheInitialClaim", func(t *testing.T, e *env) {
		rec := e.storedInteraction(t, "tok")
		ref := rec.Interaction.Ref
		initial := e.operationRef(t)
		claim := func(follow bool, op model.OperationRef, rev model.Revision) model.Interaction {
			var got model.Interaction
			e.mustTx(t, func(tx store.Tx) error {
				var err error
				got, err = tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: op, FollowUp: follow, Revision: rev})
				return err
			})
			return got
		}
		// A follow-up first bumps the revision; the initial claim that
		// follows does not compare it.
		got := claim(true, e.operationRef(t), "i2")
		if got.Response.FollowUps != 1 || got.Response.Initial != nil || got.Revision != "i2" {
			t.Fatalf("after a follow-up: response %+v revision %q, want 1 follow-up, no initial, i2", got.Response, got.Revision)
		}
		got = claim(false, initial, "i3")
		if got.Response.FollowUps != 1 || got.Response.Initial == nil || *got.Response.Initial != initial || got.Revision != "i3" {
			t.Fatalf("after the initial claim: response %+v revision %q, want 1 follow-up, the initial, i3", got.Response, got.Revision)
		}
		got = claim(true, e.operationRef(t), "i4")
		if got.Response.FollowUps != 2 || *got.Response.Initial != initial {
			t.Fatalf("after a second follow-up: response %+v, want 2 follow-ups and the same initial", got.Response)
		}
		e.wantInteraction(t, got)
	}},
	{"ResponseClaimChecksTheClaim", func(t *testing.T, e *env) {
		rec := e.storedInteraction(t, "tok")
		ref := rec.Interaction.Ref
		missing := model.InteractionRef{Conversation: ref.Conversation, ID: e.id(t)}
		op := e.operationRef(t)
		foreignOp := model.OperationRef{Tenant: tenantB, ID: e.id(t)}
		for name, c := range map[string]struct {
			claim store.ResponseClaim
			code  model.ErrorCode
		}{
			"unknown":       {store.ResponseClaim{Interaction: missing, Operation: op, Revision: "i2"}, model.NotFound},
			"no operation":  {store.ResponseClaim{Interaction: ref, Revision: "i2"}, model.Validation},
			"foreign op":    {store.ResponseClaim{Interaction: ref, Operation: foreignOp, Revision: "i2"}, model.NotFound},
			"no revision":   {store.ResponseClaim{Interaction: ref, Operation: op}, model.Validation},
			"same revision": {store.ResponseClaim{Interaction: ref, Operation: op, Revision: "i1"}, model.Validation},
			"no id":         {store.ResponseClaim{Interaction: model.InteractionRef{Conversation: ref.Conversation}, Operation: op, Revision: "i2"}, model.Validation},
		} {
			err := e.txErr(func(tx store.Tx) error {
				_, err := tx.RecordInteractionResponse(e.ctx, c.claim)
				if !model.IsCode(err, c.code) {
					t.Errorf("%s: RecordInteractionResponse = %v, want %s", name, err, c.code)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%s: Transact: %v", name, err)
			}
		}
		e.wantInteraction(t, rec.Interaction)
		wantCode(t, e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: foreignOp, Revision: "i2"})
			return err
		}), model.NotFound)
	}},
	{"RolledBackResponseClaimLeavesTheInteractionOpen", func(t *testing.T, e *env) {
		rec := e.storedInteraction(t, "tok")
		ref := rec.Interaction.Ref
		err := e.txErr(func(tx store.Tx) error {
			if _, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: e.operationRef(t), Revision: "i2"}); err != nil {
				return err
			}
			return errRollback
		})
		if err == nil {
			t.Fatal("Transact reported success for a rolled back unit")
		}
		e.wantInteraction(t, rec.Interaction)
		// The initial response is still open to the next claimant.
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{Interaction: ref, Operation: e.operationRef(t), Revision: "i2"})
			return err
		})
	}},
	{"ConcurrentInitialResponseClaimsHaveOneWinner", func(t *testing.T, e *env) {
		rec := e.storedInteraction(t, "tok")
		ref := rec.Interaction.Ref
		const workers = 8
		ops := make([]model.OperationRef, workers)
		for i := range ops {
			ops[i] = e.operationRef(t)
		}
		var won, lost atomic.Int64
		winner := make([]bool, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Go(func() {
				err := e.retryTx(func(tx store.Tx) error {
					_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{
						Interaction: ref, Operation: ops[i], Revision: model.Revision("i-" + string(ops[i].ID)),
					})
					return err
				})
				switch {
				case err == nil:
					won.Add(1)
					winner[i] = true
				case model.IsCode(err, model.Conflict):
					lost.Add(1)
				default:
					t.Errorf("claim %d: %v", i, err)
				}
			})
		}
		wg.Wait()
		if won.Load() != 1 || lost.Load() != workers-1 {
			t.Fatalf("%d claims won and %d lost of %d, want exactly one winner", won.Load(), lost.Load(), workers)
		}
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Interaction(e.ctx, ref)
			if err != nil {
				return err
			}
			if got.Response.Initial == nil || got.Response.FollowUps != 0 {
				t.Fatalf("response = %+v, want the winner's initial claim and no follow-ups", got.Response)
			}
			for i, op := range ops {
				if winner[i] != (*got.Response.Initial == op) {
					t.Errorf("claim %d won=%v but the stored initial is %v", i, winner[i], got.Response.Initial)
				}
			}
			return nil
		})
	}},
	{"FollowUpsRacingInitialClaimsNeverConflict", func(t *testing.T, e *env) {
		// A follow-up and an initial claim commute: whichever way they
		// interleave, no follow-up is a conflict, a lost initial claim is
		// a conflict only because another initial claim holds the
		// interaction, and every follow-up is counted. A unit that loses a
		// serialization race is rerun, as the engine does.
		for round := range 3 {
			rec := e.storedInteraction(t, "tok")
			ref := rec.Interaction.Ref
			const initials, followUps = 4, 8
			type claim struct {
				op       model.OperationRef
				followUp bool
				err      error
			}
			claims := make([]claim, initials+followUps)
			var wg sync.WaitGroup
			for i := range claims {
				claims[i] = claim{op: e.operationRef(t), followUp: i >= initials}
				wg.Go(func() {
					claims[i].err = e.retryTx(func(tx store.Tx) error {
						_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{
							Interaction: ref, Operation: claims[i].op, FollowUp: claims[i].followUp,
							Revision: model.Revision("r-" + string(claims[i].op.ID)),
						})
						return err
					})
				})
			}
			wg.Wait()
			var winner *model.OperationRef
			for _, c := range claims {
				switch {
				case c.err == nil && !c.followUp:
					if winner != nil {
						t.Fatalf("round %d: two initial claims won", round)
					}
					winner = &c.op
				case c.err == nil:
				case c.followUp:
					t.Fatalf("round %d: a follow-up failed: %v", round, c.err)
				case !model.IsCode(c.err, model.Conflict):
					t.Fatalf("round %d: a lost initial claim = %v, want conflict", round, c.err)
				}
			}
			e.mustView(t, func(r store.Reader) error {
				got, err := r.Interaction(e.ctx, ref)
				if err != nil {
					return err
				}
				if winner == nil || got.Response.Initial == nil || *got.Response.Initial != *winner || got.Response.FollowUps != followUps {
					t.Fatalf("round %d: response = %+v, want the winner %v and %d follow-ups", round, got.Response, winner, followUps)
				}
				return nil
			})
		}
	}},
	{"ConcurrentFollowUpsAreAllCounted", func(t *testing.T, e *env) {
		rec := e.storedInteraction(t, "tok")
		ref := rec.Interaction.Ref
		const workers = 8
		var wg sync.WaitGroup
		for range workers {
			op := e.operationRef(t)
			wg.Go(func() {
				err := e.retryTx(func(tx store.Tx) error {
					_, err := tx.RecordInteractionResponse(e.ctx, store.ResponseClaim{
						Interaction: ref, Operation: op, FollowUp: true, Revision: model.Revision("f-" + string(op.ID)),
					})
					return err
				})
				if err != nil {
					t.Errorf("follow-up: %v", err)
				}
			})
		}
		wg.Wait()
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Interaction(e.ctx, ref)
			if err == nil && (got.Response.FollowUps != workers || got.Response.Initial != nil) {
				t.Errorf("response = %+v, want %d follow-ups and no initial", got.Response, workers)
			}
			return err
		})
	}},
}

// storedInteraction stores an interaction in a fresh conversation.
func (e *env) storedInteraction(t *testing.T, token string) store.InteractionRecord {
	t.Helper()
	rec := e.interactionRecord(t, e.putConversation(t), token)
	e.mustTx(t, func(tx store.Tx) error { return tx.PutInteraction(e.ctx, rec) })
	return rec
}

func (e *env) operationRef(t *testing.T) model.OperationRef {
	t.Helper()
	return model.OperationRef{Tenant: tenantA, ID: e.id(t)}
}
