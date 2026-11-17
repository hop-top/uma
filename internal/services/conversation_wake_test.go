package services_test

import (
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func (w *workflowRig) snoozeFor(t *testing.T, d time.Duration) time.Time {
	t.Helper()
	until := w.Clock.Now().Add(d)
	if _, err := w.desk.Conversations().Snooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref, Until: &until}); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
	return until
}

func (w *workflowRig) wake(t *testing.T) operations.WakeReport {
	t.Helper()
	rep, err := operations.WakeSnoozes(w.Ctx, w.E, tenantA)
	if err != nil {
		t.Fatalf("WakeSnoozes: %v", err)
	}
	return rep
}

func (w *workflowRig) woken(t *testing.T) []model.Event {
	t.Helper()
	var out []model.Event
	for _, ev := range w.events(t) {
		if ev.Kind == model.EventConversationSnoozeEnded {
			out = append(out, ev)
		}
	}
	return out
}

func (w *workflowRig) ended(t *testing.T, want bool) {
	t.Helper()
	f := model.Filter{SnoozeEnded: &want}
	pg, err := w.desk.Conversations().List(w.Ctx, listReq(f, 10, ""))
	if err != nil {
		t.Fatal(err)
	}
	has := false
	for _, c := range pg.Items {
		has = has || c.Ref == w.Conv.Ref
	}
	if !has {
		t.Fatalf("List with snooze_ended=%v does not hold the conversation: %+v", want, pg.Items)
	}
}

// A snooze still in effect is left alone: no write, no event.
func TestWakeSweepLeavesSnoozeInEffect(t *testing.T) {
	w := newWorkflowRig(t)
	w.snoozeFor(t, time.Hour)
	rev := w.stored(t).Revision
	w.Clock.Advance(time.Minute)
	if rep := w.wake(t); rep.Woken != 0 {
		t.Fatalf("report = %+v, want nothing", rep)
	}
	if got := w.stored(t); got.Snooze == nil || got.Revision != rev || len(w.woken(t)) != 0 {
		t.Fatalf("sweep touched a snooze in effect: %+v, %d woken", got, len(w.woken(t)))
	}
	w.ended(t, false)
}

// A snooze whose Until passed is cleared with one conversation.snooze_ended event
// (cause until) in the same unit; a second sweep does nothing.
func TestWakeSweepOnUntil(t *testing.T) {
	w := newWorkflowRig(t)
	until := w.snoozeFor(t, time.Hour)
	before := w.stored(t)
	w.Clock.Advance(2 * time.Hour)
	w.ended(t, true)
	if rep := w.wake(t); rep.Woken != 1 {
		t.Fatalf("report = %+v, want 1 woken", rep)
	}
	got := w.stored(t)
	if got.Snooze != nil || got.Revision == before.Revision {
		t.Fatalf("stored %+v, want the snooze cleared at a new revision", got)
	}
	evs := w.woken(t)
	if len(evs) != 1 {
		t.Fatalf("%d woken events, want 1", len(evs))
	}
	ev := evs[0]
	p := payloadOf[model.ConversationSnoozeEndedPayload](t, ev)
	if ev.Resource != w.Conv.Ref.Ref() || ev.Inbox != w.Conv.Ref.Inbox.ID || ev.Revision != got.Revision || ev.Payload.Name != model.SchemaConversationSnoozeEnded ||
		p.Conversation != w.Conv.Ref || p.Cause != model.WakeCauseUntil || !p.Until.Equal(until) || p.PreviousRevision != before.Revision || p.Trigger != model.TriggerSystem {
		t.Fatalf("event %+v payload %+v", ev, p)
	}
	w.ended(t, false)
	if rep := w.wake(t); rep.Woken != 0 || len(w.woken(t)) != 1 || w.stored(t).Revision != got.Revision {
		t.Fatalf("second sweep woke again: %+v", rep)
	}
	if _, err := w.desk.Conversations().Unsnooze(w.Ctx, model.ConversationSnooze{Command: w.cmd(""), Conversation: w.Conv.Ref}); err != nil || w.stored(t).Revision != got.Revision {
		t.Fatalf("Unsnooze after the wake: %v", err)
	}
}

// A live inbound message recorded after the snooze was set wakes it before
// Until (cause message); an imported one does not.
func TestWakeSweepOnInboundMessage(t *testing.T) {
	w := newWorkflowRig(t)
	w.snoozeFor(t, 24*time.Hour)
	w.Clock.Advance(time.Minute)
	imported := model.Message{
		Ref: model.MessageRef{Conversation: w.Conv.Ref, ID: "00000003-0000-7000-8000-0000000000a1"}, Audience: model.Audience{Visibility: model.Public},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "history"}}},
		OccurredAt: w.Clock.Now().UTC(), RecordedAt: w.Clock.Now().UTC(), Revision: "m1", ImportedBy: deskActor,
	}
	if err := w.E.Store().Transact(w.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(w.Ctx, imported, "") }); err != nil {
		t.Fatal(err)
	}
	if rep := w.wake(t); rep.Woken != 0 {
		t.Fatalf("an imported message woke the snooze: %+v", rep)
	}
	w.Clock.Advance(time.Second)
	w.inbound(t, "00000003-0000-7000-8000-0000000000a2")
	w.ended(t, true)
	if rep := w.wake(t); rep.Woken != 1 {
		t.Fatalf("report = %+v, want 1 woken", rep)
	}
	evs := w.woken(t)
	if len(evs) != 1 || payloadOf[model.ConversationSnoozeEndedPayload](t, evs[0]).Cause != model.WakeCauseMessage {
		t.Fatalf("events %+v, want one with cause message", evs)
	}
	if w.stored(t).Snooze != nil {
		t.Fatal("snooze not cleared")
	}
}

// Sweeps racing for one ended snooze wake it once: the loser emits nothing.
func TestWakeSweepsRaceOneEvent(t *testing.T) {
	w := newWorkflowRig(t)
	w.snoozeFor(t, time.Hour)
	w.Clock.Advance(2 * time.Hour)
	const n = 8
	reps := make([]operations.WakeReport, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { reps[i], errs[i] = operations.WakeSnoozes(w.Ctx, w.E, tenantA) })
	}
	wg.Wait()
	total := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("sweeper %d: %v", i, errs[i])
		}
		total += reps[i].Woken
	}
	if total != 1 || len(w.woken(t)) != 1 {
		t.Fatalf("woken %d (%+v), events %d, want exactly one", total, reps, len(w.woken(t)))
	}
}

// conversation.snooze_ended reaches a reader of workflow state and no one else.
func TestWokenEventGate(t *testing.T) {
	w := newWorkflowRig(t)
	w.snoozeFor(t, time.Hour)
	w.Clock.Advance(2 * time.Hour)
	w.wake(t)
	count := func(actor model.ActorID) int {
		pg, err := clientAs(t, w.rig, actor).Events().List(w.Ctx, model.ListEvents{Call: call(), Start: model.StartBeginning, Limit: 100})
		if model.IsCode(err, model.Unauthorized) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, ev := range pg.Items {
			if ev.Kind == model.EventConversationSnoozeEnded {
				n++
			}
		}
		return n
	}
	if got := count(viewerActor); got != 1 {
		t.Fatalf("viewer lists %d woken events, want 1", got)
	}
	if got := count(externalActor); got != 0 {
		t.Fatalf("external participant lists %d woken events, want 0", got)
	}
}
