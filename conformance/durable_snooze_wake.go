package conformance

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// Snooze wake cases. A snooze that ended, by Until passing or by a live
// inbound message after it was set, is cleared by a sweep that judges it by
// the store's own clock, with one conversation.snooze_ended event per conversation
// however many sweeps race, and a snooze still in effect is left alone.
// cross is the restart: the next process sweeps a store reopened on what
// was committed.
var snoozeWakeCases = []durableCase{
	{"SnoozeWakeSurvivesRestart", snoozeWakeSurvivesRestart},
	{"SnoozeWakeConcurrentSweepsOneEvent", snoozeWakeConcurrentSweeps},
	{"SnoozeWakeJudgesAgainInsideTheUnit", snoozeWakeJudgesAgain},
}

func (e *env) wakeSweep(t *testing.T, eng *engine.Engine) operations.WakeReport {
	t.Helper()
	rep, err := operations.WakeSnoozes(e.ctx, eng, tenantA)
	if err != nil {
		t.Fatalf("WakeSnoozes: %v", err)
	}
	return rep
}

// wokenEvents returns the conversation.snooze_ended events of the store, in log order.
func (e *env) wokenEvents(t *testing.T) []model.Event {
	t.Helper()
	var out []model.Event
	for _, ev := range e.allEvents(t, model.Filter{}, 1000) {
		if ev.Kind == model.EventConversationSnoozeEnded {
			out = append(out, ev)
		}
	}
	return out
}

func snoozeWakeSurvivesRestart(t *testing.T, e *env, cross func(*testing.T)) {
	set := e.at(100)
	byUntil := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(time.Hour).UTC(), At: set, By: "desk-1"})
	byMessage := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(48 * time.Hour).UTC(), At: set, By: "desk-1"})
	inEffect := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(48 * time.Hour).UTC(), At: set, By: "desk-1"})
	e.putWorkflowMessage(t, byMessage, set.Add(time.Second), nil)
	e.advance(t, 2*time.Hour)
	yes := true
	cross(t)
	e.wantConversations(t, model.Filter{SnoozeEnded: &yes}, byUntil, byMessage)
	if rep := e.wakeSweep(t, e.sweepEngine(t, nil)); rep.Woken != 2 {
		t.Fatalf("report = %+v, want 2 woken", rep)
	}
	e.wantConversations(t, model.Filter{SnoozeEnded: &yes})
	evs := e.wokenEvents(t)
	if len(evs) != 2 {
		t.Fatalf("%d woken events, want 2", len(evs))
	}
	causes := map[model.ID]string{}
	for _, ev := range evs {
		if ev.Inbox != e.inbox.ID || ev.Resource.Kind != model.KindConversation || ev.Payload.Name != model.SchemaConversationSnoozeEnded {
			t.Fatalf("envelope %+v", ev)
		}
		causes[ev.Resource.ID] = wakeCause(t, ev)
	}
	if causes[byUntil.ID] != model.WakeCauseUntil || causes[byMessage.ID] != model.WakeCauseMessage {
		t.Fatalf("causes %v", causes)
	}
	var kept model.Conversation
	e.mustView(t, func(r store.Reader) error {
		var err error
		kept, err = r.GetConversation(e.ctx, inEffect)
		return err
	})
	if kept.Snooze == nil {
		t.Fatal("a snooze still in effect was cleared")
	}
	// The next process finds the transitions committed: idempotent.
	cross(t)
	if rep := e.wakeSweep(t, e.sweepEngine(t, nil)); rep.Woken != 0 {
		t.Fatalf("second sweep = %+v, want nothing", rep)
	}
	if n := len(e.wokenEvents(t)); n != 2 {
		t.Fatalf("%d woken events after the second sweep, want 2", n)
	}
}

func snoozeWakeConcurrentSweeps(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(time.Hour).UTC(), At: e.at(100), By: "desk-1"})
	e.advance(t, 2*time.Hour)
	cross(t)
	const sweepers = 6
	meet := newRendezvous(sweepers)
	reps := make([]operations.WakeReport, sweepers)
	errs := make([]error, sweepers)
	engines := make([]*engine.Engine, sweepers)
	for i := range engines {
		engines[i] = e.sweepEngine(t, &rendezvousIDs{IDs: e.ids, meet: meet})
	}
	var wg sync.WaitGroup
	for i := range sweepers {
		wg.Go(func() { reps[i], errs[i] = operations.WakeSnoozes(e.ctx, engines[i], tenantA) })
	}
	wg.Wait()
	total := 0
	for i := range reps {
		if errs[i] != nil {
			t.Fatalf("sweeper %d: %v", i, errs[i])
		}
		total += reps[i].Woken
	}
	evs := e.wokenEvents(t)
	if total != 1 || len(evs) != 1 || evs[0].Resource.ID != conv.ID {
		t.Fatalf("woken %d (%+v), events %d, want exactly one", total, reps, len(evs))
	}
}

func wakeCause(t *testing.T, ev model.Event) string {
	t.Helper()
	var p model.ConversationSnoozeEndedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p.Cause
}

// afterScanIDs runs fn once, when the sweep first asks for an ID: after its
// scan and before its first unit.
type afterScanIDs struct {
	ports.IDs
	once sync.Once
	fn   func()
}

func (a *afterScanIDs) New(ctx context.Context) (model.ID, error) {
	a.once.Do(a.fn)
	return a.IDs.New(ctx)
}

// A snooze renewed between the sweep's scan and its unit is in effect again:
// the unit judges it again and neither clears it nor emits an event.
func snoozeWakeJudgesAgain(t *testing.T, e *env, cross func(*testing.T)) {
	conv := e.putWorkflowConversation(t, nil, &model.Snooze{Until: e.fx.Now().Add(time.Hour).UTC(), At: e.at(100), By: "desk-1"})
	e.advance(t, 2*time.Hour)
	cross(t)
	renewed := model.Snooze{Until: e.fx.Now().Add(24 * time.Hour).UTC(), At: e.fx.Now().UTC(), By: "desk-1"}
	eng := e.sweepEngine(t, &afterScanIDs{IDs: e.ids, fn: func() {
		e.mustTx(t, func(tx store.Tx) error {
			c, err := tx.GetConversation(e.ctx, conv)
			if err != nil {
				return err
			}
			rev := c.Revision
			c.Snooze, c.Revision = &renewed, "renewed"
			return tx.PutConversation(e.ctx, c, rev)
		})
	}})
	if rep := e.wakeSweep(t, eng); rep.Woken != 0 {
		t.Fatalf("report = %+v, want nothing", rep)
	}
	var got model.Conversation
	e.mustView(t, func(r store.Reader) (err error) {
		got, err = r.GetConversation(e.ctx, conv)
		return err
	})
	if got.Snooze == nil || got.Revision != "renewed" || len(e.wokenEvents(t)) != 0 {
		t.Fatalf("stored %+v, %d woken events; want the renewed snooze untouched", got, len(e.wokenEvents(t)))
	}
}
