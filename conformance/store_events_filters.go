package conformance

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// eventFilterCases pin the Conversation and Since filters of Reader.Events:
// the conversation each event shape is about, as store.EventConversation
// resolves it; Since on RecordedAt, which does not follow the log, exact to
// the nanosecond and continued across units; and both composing with the
// other fields, with paging and with a unit's own appends.
var eventFilterCases = []storeCase{
	{"ConversationOfEachShape", eventConversationShapes},
	{"SinceOnRecordedAt", eventSinceRecordedAt},
	{"SinceComposesWithCursor", eventSinceCursor},
	{"FiltersSeeTheUnitsAppends", eventFiltersInUnit},
}

// eventAbout returns an event of kind in e's inbox about res, carrying the
// builtin version 1 payload schema with data, or none for an empty schema.
func (e *env) eventAbout(t *testing.T, kind string, res model.Ref, schema string, data any) model.Event {
	t.Helper()
	ev := e.event(t, kind)
	ev.Resource, ev.Inbox = res, res.Inbox
	if schema != "" {
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		ev.Payload = model.Extension{Name: schema, Version: model.PayloadVersion1, Data: raw}
	}
	return ev
}

// readEventIDs pages Events with filter from after at page size limit and
// returns the IDs read and the final Next, failing unless the last page is
// complete and every incomplete page advances.
func (e *env) readEventIDs(t *testing.T, filter model.Filter, after model.Cursor, limit int) ([]model.ID, model.Cursor) {
	t.Helper()
	var ids []model.ID
	for range 100 {
		var p model.Page[model.Event]
		e.mustView(t, func(r store.Reader) error {
			var err error
			p, err = r.Events(e.ctx, filter, after, limit)
			return err
		})
		for _, ev := range p.Items {
			ids = append(ids, ev.ID)
		}
		if p.Complete {
			return ids, p.Next
		}
		if p.Next == after {
			t.Fatalf("incomplete page did not advance from %q", after)
		}
		after = p.Next
	}
	t.Fatal("event paging did not complete")
	return nil, ""
}

func (e *env) eventHeadNow(t *testing.T) model.Cursor {
	t.Helper()
	var head model.Cursor
	e.mustView(t, func(r store.Reader) error {
		var err error
		head, err = r.EventHead(e.ctx)
		return err
	})
	return head
}

func requireIDList(t *testing.T, what string, got []model.ID, want ...model.ID) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// eventConversationShapes appends one event per shape and lists each
// conversation: an event is in the conversation its resource is, else the
// one its builtin payload names in its own inbox; every other event is in
// none and never matches.
func eventConversationShapes(t *testing.T, e *env) {
	other := model.InboxRef{Tenant: tenantA, ID: e.id(t)}
	cA := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	cB := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	cO := model.ConversationRef{Inbox: other, ID: e.id(t)}
	msg := func(c model.ConversationRef) model.MessageRef { return model.MessageRef{Conversation: c, ID: e.id(t)} }
	mA, mO := msg(cA), msg(cO)
	task := model.TaskRef{Conversation: cA, ID: e.id(t)}
	work := model.WorkItemRef{Conversation: cA, ID: e.id(t)}
	ix := model.InteractionRef{Conversation: cA, ID: e.id(t)}
	contact := model.ContactRef{Tenant: tenantA, ID: e.id(t)}
	op := model.OperationRef{Tenant: tenantA, ID: e.id(t)}

	var inA, inB, inO []model.ID
	var evs []model.Event
	add := func(into *[]model.ID, ev model.Event) {
		evs = append(evs, ev)
		if into != nil {
			*into = append(*into, ev.ID)
		}
	}
	add(&inA, e.eventAbout(t, "com.example.conformance.pinned", cA.Ref(), "", nil))
	add(&inA, e.eventAbout(t, model.EventMessageReceived, mA.Ref(), model.SchemaMessageReceived,
		model.MessageReceivedPayload{Message: mA, Trigger: model.TriggerProvider}))
	add(&inB, e.eventAbout(t, model.EventConversationChanged, cB.Ref(), model.SchemaConversationChanged,
		model.ConversationChangedPayload{Conversation: cB, Changes: []string{"status"}}))
	add(&inA, e.eventAbout(t, model.EventTaskUpdated, task.Ref(), model.SchemaTaskUpdated,
		model.TaskUpdatedPayload{Task: task, Changes: []string{model.TaskFieldCreated}}))
	add(&inA, e.eventAbout(t, model.EventWorkItemUpdated, work.Ref(), model.SchemaWorkItemUpdated,
		model.WorkItemUpdatedPayload{WorkItem: work}))
	add(&inA, e.eventAbout(t, model.EventInteractionReceived, ix.Ref(), model.SchemaInteractionReceived,
		model.InteractionReceivedPayload{Interaction: ix}))
	add(&inA, e.eventAbout(t, model.EventMessageUpdated, mA.Ref(), model.SchemaMessageUpdated,
		model.MessageUpdatedPayload{Message: mA, Change: "edited"}))
	add(&inO, e.eventAbout(t, model.EventMessageCreated, mO.Ref(), model.SchemaMessageCreated,
		model.MessageCreatedPayload{Message: mO}))
	// None of these is about a conversation.
	foreignPayload := e.eventAbout(t, model.EventMessageReceived, mA.Ref(), model.SchemaMessageReceived,
		model.MessageReceivedPayload{Message: mO})
	add(nil, foreignPayload)
	v2 := e.eventAbout(t, model.EventMessageReceived, mA.Ref(), model.SchemaMessageReceived,
		model.MessageReceivedPayload{Message: mA})
	v2.Payload.Version = 2
	add(nil, v2)
	add(nil, e.eventAbout(t, "com.example.conformance.noted", mA.Ref(), "com.example.conformance.noted",
		model.MessageReceivedPayload{Message: mA}))
	add(nil, e.eventAbout(t, model.EventDeliveryUpdated, op.Ref(), model.SchemaDeliveryUpdated,
		model.DeliveryUpdatedPayload{Operation: op, Result: new(mA.Ref())}))
	add(nil, e.eventAbout(t, model.EventContactCreated, contact.Ref(), model.SchemaContactCreated,
		model.ContactCreatedPayload{Contact: contact}))
	add(nil, e.eventAbout(t, "com.example.conformance.inbox", e.inbox.Ref(), "", nil))
	malformed := e.eventAbout(t, model.EventTaskUpdated, task.Ref(), model.SchemaTaskUpdated, map[string]any{"task": "x"})
	add(nil, malformed)

	for _, ev := range evs {
		e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{ev}) })
	}
	head := e.eventHeadNow(t)
	for _, c := range []struct {
		name   string
		filter model.Filter
		want   []model.ID
	}{
		{"conversation A", model.Filter{Conversation: &cA}, inA},
		{"conversation B", model.Filter{Conversation: &cB}, inB},
		{"conversation in another inbox", model.Filter{Conversation: &cO}, inO},
		{"conversation and its inbox", model.Filter{Conversation: &cA, Inboxes: []model.InboxRef{other, e.inbox}}, inA},
		{"conversation outside the inboxes", model.Filter{Conversation: &cA, Inboxes: []model.InboxRef{other}}, nil},
		{"conversation and kind", model.Filter{Conversation: &cA, Kinds: []string{model.EventMessageReceived, model.EventMessageUpdated}},
			[]model.ID{inA[1], inA[5]}},
		{"conversation without events", model.Filter{Conversation: &model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}}, nil},
	} {
		for _, limit := range []int{100, 1} {
			got, next := e.readEventIDs(t, c.filter, "", limit)
			requireIDList(t, c.name, got, c.want...)
			if next != head {
				t.Fatalf("%s at page size %d ended at %q, want the head %q", c.name, limit, next, head)
			}
		}
	}
	for _, ev := range evs {
		if got, want := store.EventConversation(ev), conversationOf(ev.ID, cA, cB, cO, inA, inB, inO); got != want {
			t.Fatalf("store.EventConversation(%s) = %q, want %q", ev.Kind, got, want)
		}
	}
}

// conversationOf returns the conversation the shape test placed id in.
func conversationOf(id model.ID, cA, cB, cO model.ConversationRef, inA, inB, inO []model.ID) model.ID {
	switch {
	case slices.Contains(inA, id):
		return cA.ID
	case slices.Contains(inB, id):
		return cB.ID
	case slices.Contains(inO, id):
		return cO.ID
	}
	return ""
}

// recordedAt is the RecordedAt of the since tests: second s plus n
// nanoseconds.
func (e *env) recordedAt(s, n int) time.Time { return e.at(s).Add(time.Duration(n)) }

// appendRecorded appends, in one unit, one event per RecordedAt and returns
// their IDs.
func (e *env) appendRecorded(t *testing.T, at ...time.Time) []model.ID {
	t.Helper()
	evs := make([]model.Event, len(at))
	ids := make([]model.ID, len(at))
	for i, a := range at {
		evs[i] = e.event(t, "conformance.recorded")
		evs[i].RecordedAt = a
		ids[i] = evs[i].ID
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, evs) })
	return ids
}

// eventSinceRecordedAt appends events whose RecordedAt goes back and forth
// along the log, over four units: Since keeps exactly those recorded at or
// after it, in log order, at every page size, to the nanosecond.
func eventSinceRecordedAt(t *testing.T, e *env) {
	u1 := e.appendRecorded(t, e.recordedAt(5, 0), e.recordedAt(1, 0))
	u2 := e.appendRecorded(t, e.recordedAt(9, 0), e.recordedAt(3, 0))
	u3 := e.appendRecorded(t, e.recordedAt(9, 0), e.recordedAt(7, 0), e.recordedAt(4, 500))
	// A unit wholly below the maximum so far continues it.
	u4 := e.appendRecorded(t, e.recordedAt(8, 0))
	head := e.eventHeadNow(t)
	at5, at1, at9, at3, at9b, at7, at4, at8 := u1[0], u1[1], u2[0], u2[1], u3[0], u3[1], u3[2], u4[0]
	for _, c := range []struct {
		name  string
		since time.Time
		want  []model.ID
	}{
		{"before every event", e.recordedAt(0, 0), []model.ID{at5, at1, at9, at3, at9b, at7, at4, at8}},
		{"between", e.recordedAt(4, 0), []model.ID{at5, at9, at9b, at7, at4, at8}},
		{"past an earlier maximum", e.recordedAt(6, 0), []model.ID{at9, at9b, at7, at8}},
		{"the maximum itself", e.recordedAt(9, 0), []model.ID{at9, at9b}},
		{"a nanosecond before", e.recordedAt(4, 499), []model.ID{at5, at9, at9b, at7, at4, at8}},
		{"the exact nanosecond", e.recordedAt(4, 500), []model.ID{at5, at9, at9b, at7, at4, at8}},
		{"a nanosecond after", e.recordedAt(4, 501), []model.ID{at5, at9, at9b, at7, at8}},
		{"after every event", e.recordedAt(10, 0), nil},
	} {
		since := c.since
		for _, limit := range []int{100, 2, 1} {
			got, next := e.readEventIDs(t, model.Filter{Since: &since}, "", limit)
			requireIDList(t, c.name, got, c.want...)
			if next != head {
				t.Fatalf("%s at page size %d ended at %q, want the head %q", c.name, limit, next, head)
			}
		}
	}
}

// eventSinceCursor lists with Since from a cursor before and after the
// first position Since can select: the read starts at the later of the
// two, a page that starts past the cursor still reports its own Next, and
// resuming at each Next neither repeats nor skips an event.
func eventSinceCursor(t *testing.T, e *env) {
	e.appendRecorded(t, e.recordedAt(1, 0), e.recordedAt(2, 0))
	mid := e.eventHeadNow(t)
	late := e.appendRecorded(t, e.recordedAt(8, 0), e.recordedAt(3, 0), e.recordedAt(9, 0))
	head := e.eventHeadNow(t)
	since := e.recordedAt(5, 0)
	f := model.Filter{Since: &since}

	var p model.Page[model.Event]
	e.mustView(t, func(r store.Reader) error {
		var err error
		p, err = r.Events(e.ctx, f, "", 1)
		return err
	})
	if len(p.Items) != 1 || p.Items[0].ID != late[0] || p.Complete || p.Next == "" || p.Next == mid {
		t.Fatalf("first page with since = %+v, want late[0] alone, incomplete, past the early events", p)
	}
	rest, next := e.readEventIDs(t, f, p.Next, 1)
	requireIDList(t, "resumed from the jumped page", rest, late[2])
	if next != head {
		t.Fatalf("resumed pages ended at %q, want the head %q", next, head)
	}
	got, _ := e.readEventIDs(t, f, mid, 100)
	requireIDList(t, "from a cursor before the since start", got, late[0], late[2])
	// A cursor past the since start wins: the 8 is behind it.
	var afterFirst model.Cursor
	e.mustView(t, func(r store.Reader) error {
		pg, err := r.Events(e.ctx, model.Filter{}, mid, 1)
		afterFirst = pg.Next
		return err
	})
	got, _ = e.readEventIDs(t, f, afterFirst, 100)
	requireIDList(t, "from a cursor past the since start", got, late[2])
	// Since with the conversation and kind filters.
	conv := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	ev := e.eventAbout(t, "conformance.recorded", conv.Ref(), "", nil)
	ev.RecordedAt = e.recordedAt(6, 0)
	old := e.eventAbout(t, "conformance.recorded", conv.Ref(), "", nil)
	old.RecordedAt = e.recordedAt(4, 0)
	e.mustTx(t, func(tx store.Tx) error { return tx.AppendEvents(e.ctx, []model.Event{old, ev}) })
	got, _ = e.readEventIDs(t, model.Filter{Since: &since, Conversation: &conv, Kinds: []string{"conformance.recorded"}}, "", 1)
	requireIDList(t, "since, conversation and kind", got, ev.ID)
}

// eventFiltersInUnit reads the filters inside the unit that appends: its
// own events are selected before it commits.
func eventFiltersInUnit(t *testing.T, e *env) {
	e.appendRecorded(t, e.recordedAt(9, 0))
	conv := model.ConversationRef{Inbox: e.inbox, ID: e.id(t)}
	ev := e.eventAbout(t, "conformance.recorded", conv.Ref(), "", nil)
	ev.RecordedAt = e.recordedAt(2, 0)
	since := e.recordedAt(1, 0)
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.AppendEvents(e.ctx, []model.Event{ev}); err != nil {
			return err
		}
		p, err := tx.Events(e.ctx, model.Filter{Since: &since, Conversation: &conv}, "", 10)
		if err != nil {
			return err
		}
		if len(p.Items) != 1 || p.Items[0].ID != ev.ID || !p.Complete {
			t.Errorf("unit's own event through since and conversation = %+v, want it alone", p)
		}
		return nil
	})
}
