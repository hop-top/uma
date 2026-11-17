package operations

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// SnoozeInEffect reports whether conv's snooze hides it at now: its newest
// live inbound message that was not imported (NewestLiveInbound) is read
// only when it holds a snooze.
func SnoozeInEffect(ctx context.Context, r store.Reader, conv model.Conversation, now time.Time) (bool, error) {
	if conv.Snooze == nil {
		return false, nil
	}
	m, err := NewestLiveInbound(ctx, r, conv.Ref, &conv.Snooze.At)
	if err != nil {
		return false, err
	}
	var newest *time.Time
	if m != nil {
		newest = &m.RecordedAt
	}
	return conv.Snooze.InEffect(now, newest), nil
}

// inboundPage is the page size NewestLiveInbound reads with past the
// first message.
const inboundPage = 50

// NewestLiveInbound returns conv's newest live inbound message
// (Reader.InboundMessages) that was not imported from the provider's
// history (Message.ImportedBy), or nil when it has none. Imported messages
// are recorded when the import ran, so they come first and are passed
// over; with after set the read stops at the first message recorded at or
// before it, and nil then means none was recorded after it. The first
// read takes one message, the common case without imports.
func NewestLiveInbound(ctx context.Context, r store.Reader, conv model.ConversationRef, after *time.Time) (*model.Message, error) {
	page := model.PageRequest{Limit: 1}
	for {
		pg, err := r.InboundMessages(ctx, conv, page)
		if err != nil {
			return nil, err
		}
		for i := range pg.Items {
			m := &pg.Items[i]
			switch {
			case after != nil && !m.RecordedAt.After(*after):
				return nil, nil
			case m.ImportedBy == "":
				return m, nil
			}
		}
		if pg.Complete || pg.Next == "" || len(pg.Items) == 0 {
			return nil, nil
		}
		page = model.PageRequest{Limit: inboundPage, After: pg.Next}
	}
}

// WakeReport counts what one WakeSnoozes sweep wrote.
type WakeReport struct {
	// Woken is the conversations whose ended snooze this sweep cleared.
	Woken int
}

// wakePage is the page size WakeSnoozes lists candidates with.
const wakePage = 100

// WakeSnoozes sweeps the tenant's conversations once: one whose snooze has
// ended, by Until passing or by a live inbound message that was not
// imported recorded after it was set (the rule model.Snooze.InEffect and
// Filter.SnoozeEnded share), has the snooze cleared and one
// conversation.snooze_ended event appended in the same unit.
//
// "Now" is the engine clock, the one a read judges a snooze by, so a
// conversation the sweep leaves is one every reader already shows as
// snoozed. Each conversation is its own unit and a compare-and-swap on the
// revision the unit read: sweeps racing in one process or many wake a
// conversation once, and the loser skips it without an event. A sweep is
// idempotent: a woken conversation holds no snooze. A conversation another
// unit changed since the scan is judged again by the unit, or left to the
// next sweep.
func WakeSnoozes(ctx context.Context, e *engine.Engine, tenant model.TenantID) (WakeReport, error) {
	var rep WakeReport
	if e.IsClosed() {
		return rep, engine.ErrClosed()
	}
	done, err := e.Begin()
	if err != nil {
		return rep, err
	}
	defer done()
	refs, err := endedSnoozes(ctx, e, tenant)
	if model.IsCode(err, model.Validation) {
		// A store that refuses Filter.SnoozeEnded gets no wake events;
		// the snooze rule readers apply is unaffected.
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	for _, ref := range refs {
		evID, err := NewID(ctx, e)
		if err != nil {
			return rep, err
		}
		rev, err := NewID(ctx, e)
		if err != nil {
			return rep, err
		}
		var woke bool
		err = Transact(ctx, e, tenant, func(tx store.Tx) error {
			var err error
			woke, err = wakeOne(ctx, e, tx, ref, evID, model.Revision(rev))
			return err
		})
		switch {
		case model.IsCode(err, model.Conflict), model.IsCode(err, model.NotFound):
			continue // lost to another unit, or gone; the next sweep sees the result
		case err != nil:
			return rep, err
		case woke:
			rep.Woken++
		}
	}
	return rep, nil
}

// endedSnoozes lists the conversations whose stored snooze has ended.
func endedSnoozes(ctx context.Context, e *engine.Engine, tenant model.TenantID) ([]model.ConversationRef, error) {
	var refs []model.ConversationRef
	yes := true
	req := model.ListRequest{Filter: model.Filter{SnoozeEnded: &yes}, Page: model.PageRequest{Limit: wakePage}}
	err := e.Store().View(ctx, tenant, func(r store.Reader) error {
		for {
			p, err := r.Conversations(ctx, req)
			if err != nil {
				return err
			}
			for _, c := range p.Items {
				refs = append(refs, c.Ref)
			}
			if p.Complete || p.Next == "" || p.Next == req.Page.After {
				return nil
			}
			req.Page.After = p.Next
		}
	})
	return refs, err
}

// wakeOne re-reads ref in tx and, when its snooze has still ended, clears
// it under rev and appends the conversation.snooze_ended event evID.
func wakeOne(ctx context.Context, e *engine.Engine, tx store.Tx, ref model.ConversationRef, evID model.ID, rev model.Revision) (bool, error) {
	cur, err := tx.GetConversation(ctx, ref)
	if err != nil {
		return false, err
	}
	now := e.Clock().Now().UTC()
	if cur.Snooze == nil {
		return false, nil
	}
	in, err := SnoozeInEffect(ctx, tx, cur, now)
	if err != nil || in {
		return false, err
	}
	cause := model.WakeCauseUntil
	if cur.Snooze.Until.After(now) {
		cause = model.WakeCauseMessage
	}
	data, err := json.Marshal(model.ConversationSnoozeEndedPayload{
		Conversation: cur.Ref, Cause: cause, Until: cur.Snooze.Until.UTC(),
		Trigger: model.TriggerSystem, PreviousRevision: cur.Revision,
	})
	if err != nil {
		return false, model.NewError(model.Permanent, "wake event cannot be encoded")
	}
	next := cur.Clone()
	next.Snooze, next.Revision = nil, rev
	if err := tx.PutConversation(ctx, next, cur.Revision); err != nil {
		return false, err
	}
	res := cur.Ref.Ref()
	ev := model.Event{
		Version: model.EventEnvelopeVersion, ID: evID, Kind: model.EventConversationSnoozeEnded,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		OccurredAt: now, RecordedAt: now, Audience: model.Audience{Visibility: model.Internal}, Revision: rev,
		Payload: model.Extension{Name: model.SchemaConversationSnoozeEnded, Version: model.PayloadVersion1, Data: data},
	}
	return true, tx.AppendEvents(ctx, []model.Event{ev})
}
