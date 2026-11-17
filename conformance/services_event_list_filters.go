package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// appendRecordedOf commits one public event of kind about conv, in inbox,
// recorded at at, and returns it.
func (env *serviceEnv) appendRecordedOf(inbox model.InboxRef, conv model.Conversation, kind string, at time.Time) model.Event {
	env.t.Helper()
	ev := model.Event{
		Version: model.EventEnvelopeVersion, ID: env.id(), Kind: kind,
		Tenant: serviceTenant, Inbox: inbox.ID, Resource: conv.Ref.Ref(), Actor: "conformance-actor",
		OccurredAt: at, RecordedAt: at, Audience: model.Audience{Visibility: model.Public},
	}
	env.put(func(tx store.Tx) error { return tx.AppendEvents(env.ctx, []model.Event{ev}) })
	return ev
}

// listAll pages Events.List with filter from start (and after, for a
// checkpoint) one call of limit at a time until complete, and returns the
// events and the final Next.
func (env *serviceEnv) listAll(filter model.Filter, start string, after model.Cursor, limit int) ([]model.Event, model.Cursor) {
	env.t.Helper()
	var out []model.Event
	for range 100 {
		req := eventListReq(start, after, limit)
		req.Filter = filter
		p := env.listEvents(env.client, req)
		out = append(out, p.Items...)
		if p.Complete {
			return out, p.Next
		}
		if start == model.StartCheckpoint && p.Next == after {
			env.t.Fatalf("incomplete page did not advance from %q", after)
		}
		start, after = model.StartCheckpoint, p.Next
	}
	env.t.Fatal("event list paging did not complete")
	return nil, ""
}

// eventListConversationSince lists by conversation and by Since through
// Events.List. Conversation keeps the events about one conversation and
// drops those about another or about none; Since compares RecordedAt,
// which goes back and forth along the log, from the beginning, from a
// checkpoint and from the tail, and pages resume at each Next without
// repeat or gap. Subscriptions refuse both fields.
func eventListConversationSince(t *testing.T, env *serviceEnv) {
	tail := env.tailPosition()
	conv2 := env.conversation(env.inbox, model.ConversationOpen)
	t0 := env.clock.Now().UTC().Add(time.Hour)
	const kind = model.EventMessageReceived
	var (
		a = env.appendRecordedOf(env.inbox, env.conv, kind, t0.Add(5*time.Second))
		b = env.appendRecordedOf(env.inbox, conv2, kind, t0.Add(1*time.Second))
		c = env.appendRecordedOf(env.inbox, env.conv, model.EventConversationChanged, t0.Add(9*time.Second))
		d = env.appendRecordedOf(env.inbox, conv2, kind, t0.Add(3*time.Second))
		f = env.appendRecordedOf(env.inbox, env.conv, kind, t0.Add(7*time.Second))
	)
	head := env.eventHead()
	since := t0.Add(4 * time.Second)
	late := t0.Add(time.Minute)
	for _, cs := range []struct {
		name   string
		filter model.Filter
		want   []model.Event
	}{
		{"conversation", model.Filter{Conversation: &env.conv.Ref}, []model.Event{a, c, f}},
		{"other conversation", model.Filter{Conversation: &conv2.Ref}, []model.Event{b, d}},
		{"conversation and kind", model.Filter{Conversation: &env.conv.Ref, Kinds: []string{kind}}, []model.Event{a, f}},
		{"conversation outside the inbox filter", model.Filter{Conversation: &env.conv.Ref, Inboxes: []model.InboxRef{env.inbox2}}, nil},
		{"since", model.Filter{Since: &since}, []model.Event{a, c, f}},
		{"since and conversation", model.Filter{Since: &since, Conversation: &conv2.Ref}, nil},
		{"since after every event", model.Filter{Since: &late}, nil},
	} {
		for _, limit := range []int{0, 1} {
			got, next := env.listAll(cs.filter, model.StartCheckpoint, tail, limit)
			requireEventIDs(t, cs.name+" from the checkpoint", got, cs.want)
			if next != head {
				t.Fatalf("%s at limit %d ended at %q, want the head %q", cs.name, limit, next, head)
			}
		}
	}
	// Since from the beginning skips the events recorded before it,
	// those of newEventListEnv included.
	got, next := env.listAll(model.Filter{Since: &since}, model.StartBeginning, "", 1)
	requireEventIDs(t, "since from the beginning", got, []model.Event{a, c, f})
	if next != head {
		t.Fatalf("since from the beginning ended at %q, want the head %q", next, head)
	}
	tailReq := eventListReq(model.StartTail, "", 0)
	tailReq.Filter = model.Filter{Since: &since}
	if p := env.listEvents(env.client, tailReq); len(p.Items) != 0 || !p.Complete || p.Next != head {
		t.Fatalf("since from the tail = %+v, want an empty, complete page at the head %q", p, head)
	}
	// A page that starts at Since's first position still reports its own
	// Next, from which the list resumes.
	first := eventListReq(model.StartCheckpoint, tail, 1)
	first.Filter = model.Filter{Since: &since}
	p := env.listEvents(env.client, first)
	requireEventIDs(t, "first page with since", p.Items, []model.Event{a})
	rest, _ := env.listAll(model.Filter{Since: &since}, model.StartCheckpoint, p.Next, 1)
	requireEventIDs(t, "resumed with since", rest, []model.Event{c, f})

	for _, fl := range []model.Filter{{Conversation: &env.conv.Ref}, {Since: &since}} {
		_, err := env.client.Events().Open(env.ctx, model.OpenSubscription{
			Command: env.cmd(""), Consumer: "filtered", Start: model.StartTail, Filter: fl, PayloadMajor: model.PayloadVersion1,
		})
		serviceCode(t, err, model.Validation)
	}
}
