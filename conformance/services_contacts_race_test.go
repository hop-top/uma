package conformance

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// TestRacingCasesResendTransientUnits runs each race case on a memory
// store whose racing units lose to a concurrent unit more often than the
// engine reruns one, as a store under contention can make them: the racing
// calls report model.Transient. Each case resends them and still sees its
// invariants. A case's units of a kind before its race (skip) commit.
func TestRacingCasesResendTransientUnits(t *testing.T) {
	link, unlink := model.ActionIdentityLink, model.ActionIdentityUnlink
	for _, c := range []struct {
		name string
		run  func(*testing.T, *contactsEnv)
		skip map[string]int32
	}{
		{"ConcurrentLinksOneWins", contactsConcurrentLinks, map[string]int32{link: 0}},
		{"ConcurrentSameOperationReplays", contactsConcurrentSameOperation, map[string]int32{link: 0, unlink: 0}},
		{"ConcurrentClaimAndLinkNoOrphanContact", contactsConcurrentClaimAndLink, map[string]int32{link: 0}},
		// The anchor's move off and back, then the link that records it.
		{"AnchorRelinkedBeforeCommit", contactsAnchorRelinkedBeforeCommit, map[string]int32{link: 3}},
		// The purge the refused Link runs into. Its racing Links are not
		// made to lose: one refused as not found would leave the losses to
		// the next round's setup.
		{"LinkRacesOrphanPurge", contactsLinkRacesOrphanPurge, map[string]int32{purgeContacts: 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var st *losingUnits
			factory := func(*testing.T) StoreFixture {
				clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0).UTC())
				st = &losingUnits{Store: memory.New(memory.Options{Clock: clock}), kinds: map[string]*losses{}}
				for k, skip := range c.skip {
					l := &losses{}
					l.skip.Store(skip)
					l.left.Store(unitLosses)
					st.kinds[k] = l
				}
				return StoreFixture{Store: st, Now: clock.Now, Advance: func(d time.Duration) { clock.Advance(d) }}
			}
			c.run(t, newContactsEnv(t, factory))
			for k, l := range st.kinds {
				if n := l.left.Load(); n > 0 {
					t.Fatalf("%d of %d injected %s losses unused: no racing unit ran into them", n, unitLosses, k)
				}
			}
		})
	}
}

// unitLosses exceeds the reruns the engine gives one unit, so the first
// racing call of a kind loses every one of them.
const unitLosses = 8

// purgeContacts is the kind of a unit purging the contacts family.
const purgeContacts = "purge:" + store.FamilyContacts

// losses counts down the units of one kind that commit (skip), then those
// that lose (left).
type losses struct{ skip, left atomic.Int32 }

// losingUnits fails with model.Transient, after the unit ran, so the store
// rolls it back as it would a unit lost to a concurrent commit, the units
// of each kind in kinds its losses say: a unit's kinds are those of the
// operations it claimed and the families it purged.
type losingUnits struct {
	store.Store
	kinds map[string]*losses
}

func (s *losingUnits) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		spy := &unitSpy{Tx: tx}
		if err := fn(spy); err != nil {
			return err
		}
		for _, k := range spy.kinds {
			l, ok := s.kinds[k]
			if ok && l.skip.Add(-1) < 0 && l.left.Add(-1) >= 0 {
				return model.NewError(model.Transient, "concurrent unit committed; retry the unit")
			}
		}
		return nil
	})
}

// unitSpy notes the kinds of the operations its unit claimed and of the
// families it purged.
type unitSpy struct {
	store.Tx
	kinds []string
}

func (t *unitSpy) ClaimOperation(ctx context.Context, rec store.OperationRecord) (store.OperationRecord, bool, error) {
	t.kinds = append(t.kinds, rec.Key.Kind)
	return t.Tx.ClaimOperation(ctx, rec)
}

func (t *unitSpy) Purge(ctx context.Context, req store.PurgeRequest) (store.PurgeResult, error) {
	t.kinds = append(t.kinds, "purge:"+req.Family)
	return t.Tx.Purge(ctx, req)
}
