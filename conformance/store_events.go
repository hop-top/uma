package conformance

import (
	"errors"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

var errAbort = errors.New("conformance: abort unit")

var eventCases = []storeCase{
	{"AppendCommitsWithUnit", func(t *testing.T, e *env) {
		evs := []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two"), e.event(t, "conformance.one")}
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, evs) })
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.lost")}); err != nil {
				return err
			}
			return errAbort
		})
		got := e.allEvents(t, model.Filter{}, 2)
		if len(got) != 3 {
			t.Fatalf("events = %d, want 3 committed and none from the rolled-back unit", len(got))
		}
		for i := range evs {
			if got[i].ID != evs[i].ID {
				t.Errorf("event %d = %s, want %s in append order", i, got[i].ID, evs[i].ID)
			}
		}
		ones := e.allEvents(t, model.Filter{Kinds: []string{"conformance.one"}}, 10)
		if len(ones) != 2 {
			t.Errorf("kind filter returned %d events, want 2", len(ones))
		}
	}},
	{"AppendRejectsDuplicatesAndInvalid", func(t *testing.T, e *env) {
		ev := e.event(t, "conformance.one")
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{ev}) })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{ev}) }), model.Conflict)
		bad := e.event(t, "conformance.one")
		bad.Inbox = ""
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{bad}) }), model.Validation)
		e.mustView(t, func(r store.Reader) error {
			_, err := r.Events(e.ctx, model.Filter{Statuses: []string{"open"}}, "", 10)
			wantCode(t, err, model.Validation)
			_, err = r.Events(e.ctx, model.Filter{Labels: &model.LabelSelector{None: []model.LabelRef{{Tenant: tenantA, ID: e.id(t)}}}}, "", 10)
			wantCode(t, err, model.Validation)
			_, err = r.Events(e.ctx, model.Filter{}, "not-a-cursor", 10)
			wantCode(t, err, model.Validation)
			return nil
		})
	}},
	{"EventHeadTracksCommittedLog", func(t *testing.T, e *env) {
		e.mustView(t, func(r store.Reader) error {
			head, err := r.EventHead(e.ctx)
			if err == nil && head != "" {
				t.Errorf("head of an empty log = %q, want the retained beginning", head)
			}
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two")})
		})
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.lost")}); err != nil {
				return err
			}
			return errAbort
		})
		e.mustView(t, func(r store.Reader) error {
			head, err := r.EventHead(e.ctx)
			if err != nil {
				return err
			}
			all, err := r.Events(e.ctx, model.Filter{}, "", 10)
			if err != nil {
				return err
			}
			if len(all.Items) != 2 || !all.Complete || head != all.Next {
				t.Errorf("head = %q, complete page next = %q over %d events", head, all.Next, len(all.Items))
			}
			rest, err := r.Events(e.ctx, model.Filter{}, head, 10)
			if err == nil && (len(rest.Items) != 0 || !rest.Complete || rest.Next != head) {
				t.Errorf("events after head = %d complete=%v next=%q", len(rest.Items), rest.Complete, rest.Next)
			}
			return err
		})
		e.mustTx(t, func(tx store.Tx) error {
			head, err := tx.EventHead(e.ctx)
			if err != nil {
				return err
			}
			if err := tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.three")}); err != nil {
				return err
			}
			after, err := tx.EventHead(e.ctx)
			if err == nil && after == head {
				t.Errorf("head did not move within the unit after an append: %q", after)
			}
			return err
		})
	}},
	{"SubscriptionRoundTrips", func(t *testing.T, e *env) {
		sub := model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}
		rec := store.SubscriptionRecord{
			Subscription: model.Subscription{Ref: sub, Revision: "s1"},
			Principal:    model.Principal{Tenant: tenantA, Actor: "conformance-actor"},
			Consumer:     "bot",
			Filter:       model.Filter{Inboxes: []model.InboxRef{e.inbox}, Kinds: []string{"conformance.one"}},
			PayloadMajor: 1,
		}
		e.mustTx(t, func(tx store.Tx) error { return tx.PutSubscription(e.ctx, rec, "") })
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.PutSubscription(e.ctx, rec, "") }), model.Conflict)
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Subscription(e.ctx, sub)
			if err != nil {
				return err
			}
			if got.PayloadMajor != 1 || got.Consumer != "bot" || got.Principal != rec.Principal ||
				len(got.Filter.Kinds) != 1 || len(got.Filter.Inboxes) != 1 || got.Subscription != rec.Subscription {
				t.Errorf("subscription = %+v, want %+v", got, rec)
			}
			_, err = r.Subscription(e.ctx, model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)})
			wantCode(t, err, model.NotFound)
			return nil
		})
		closedAt := e.fx.Now().Add(time.Minute)
		closed := rec
		closed.Subscription.Revision, closed.ClosedAt = "s2", &closedAt
		e.mustTx(t, func(tx store.Tx) error { return tx.PutSubscription(e.ctx, closed, "s1") })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Subscription(e.ctx, sub)
			if err != nil {
				return err
			}
			if got.ClosedAt == nil || !got.ClosedAt.Equal(closedAt) || got.Subscription.Revision != "s2" {
				t.Errorf("closed subscription = %+v", got)
			}
			return nil
		})
	}},
	{"CheckpointAdvanceIsFenced", func(t *testing.T, e *env) {
		e.mustTx(t, func(tx store.Tx) error {
			return tx.AppendEvents(e.ctx, []model.Event{e.event(t, "conformance.one"), e.event(t, "conformance.two")})
		})
		var p model.Page[model.Event]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.Events(e.ctx, model.Filter{}, "", 10)
			return err
		})
		sub := model.SubscriptionRef{Tenant: tenantA, ID: e.id(t)}
		var l store.Lease
		e.mustTx(t, func(tx store.Tx) error {
			var err error
			if l, err = tx.AcquireLease(e.ctx, store.LeaseRequest{Resource: sub.Ref(), Owner: "consumer", Until: e.fx.Now().Add(time.Minute)}); err != nil {
				return err
			}
			return tx.PutSubscription(e.ctx, store.SubscriptionRecord{
				Subscription: model.Subscription{Ref: sub, Revision: "s1"},
				Principal:    model.Principal{Tenant: tenantA, Actor: "conformance-actor"},
				Consumer:     "bot",
				PendingBatch: &store.BatchRecord{
					Through: p.Next, EventIDs: []model.ID{p.Items[0].ID, p.Items[1].ID},
					TokenDigest: []byte{1}, ExpiresAt: e.fx.Now().Add(time.Minute), Fence: l.Fence,
				},
			}, "")
		})
		adv := store.CheckpointAdvance{Subscription: sub, Prior: "", Next: p.Next, Fence: l.Fence, Revision: "s2"}
		stalePrior := adv
		stalePrior.Prior = p.Next
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.AdvanceCheckpoint(e.ctx, stalePrior) }), model.Conflict)
		staleFence := adv
		staleFence.Fence.Generation++
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.AdvanceCheckpoint(e.ctx, staleFence) }), model.Conflict)
		otherRange := adv
		otherRange.Next = ""
		wantCode(t, e.txErr(func(tx store.Tx) error { return tx.AdvanceCheckpoint(e.ctx, otherRange) }), model.Conflict)
		e.mustTx(t, func(tx store.Tx) error { return tx.AdvanceCheckpoint(e.ctx, adv) })
		e.mustView(t, func(r store.Reader) error {
			got, err := r.Subscription(e.ctx, sub)
			if err != nil {
				return err
			}
			if got.Subscription.Checkpoint != p.Next || got.PendingBatch != nil || got.Subscription.Revision != "s2" {
				t.Errorf("subscription after ack = %+v", got)
			}
			rest, err := r.Events(e.ctx, model.Filter{}, got.Subscription.Checkpoint, 10)
			if err == nil && (len(rest.Items) != 0 || !rest.Complete) {
				t.Errorf("events after checkpoint = %d complete=%v, want none", len(rest.Items), rest.Complete)
			}
			return err
		})
	}},
}

func (e *env) event(t *testing.T, kind string) model.Event {
	t.Helper()
	conv := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	at := time.Unix(1_700_000_000, 0).UTC()
	return model.Event{
		Version: 1, ID: e.id(t), Kind: kind, Tenant: tenantA, Inbox: e.inbox.ID,
		Resource: conv.Ref(), Actor: "conformance-actor", OccurredAt: at, RecordedAt: at,
		Audience: model.Audience{Visibility: model.Internal},
	}
}

// allEvents pages through every committed event matching filter.
func (e *env) allEvents(t *testing.T, filter model.Filter, limit int) []model.Event {
	t.Helper()
	var out []model.Event
	var after model.Cursor
	for range 100 {
		var p model.Page[model.Event]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.Events(e.ctx, filter, after, limit)
			return err
		})
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		after = p.Next
	}
	t.Fatal("event paging did not complete")
	return nil
}
