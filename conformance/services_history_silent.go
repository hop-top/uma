package conformance

import (
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Imported history is silent for desk state: a message Runtime.Import
// records is marked Message.ImportedBy, never wakes a snooze and never
// makes a conversation unread for anyone. Only messages arriving live
// through ingress do.

// historySilentSnooze: a conversation snoozed before an import stays
// snoozed after it, in Get and under Filter.Snoozed, until a live inbound
// message arrives.
func historySilentSnooze(t *testing.T, env *historyEnv) {
	env.snooze(t, 24*time.Hour)
	env.clock.Advance(time.Second)
	env.importWindow(t, "pm-1", "pm-2")
	env.wantSnoozed(t, true)
	env.clock.Advance(time.Second)
	env.live(t, "pm-after")
	env.wantSnoozed(t, false)
}

// historySilentSnoozeExpiry: a snooze an import left in effect still ends
// at Until.
func historySilentSnoozeExpiry(t *testing.T, env *historyEnv) {
	if env.fx.Advance == nil {
		t.Skip("fixture cannot advance the store clock")
	}
	env.snooze(t, time.Hour)
	env.clock.Advance(time.Second)
	env.importWindow(t, "pm-1")
	env.wantSnoozed(t, true)
	env.fx.Advance(time.Hour)
	env.clock.Advance(time.Hour)
	env.wantSnoozed(t, false)
}

// historySilentAwake: an import into a conversation without a snooze
// leaves it as it was: listed as not snoozed, its imported messages
// marked with the requester and its live message not.
func historySilentAwake(t *testing.T, env *historyEnv) {
	env.clock.Advance(time.Second)
	env.importWindow(t, "pm-1", "pm-2")
	env.wantSnoozed(t, false)
	err := env.st.View(env.ctx, serviceTenant, func(r store.Reader) error {
		p, err := r.Messages(env.ctx, model.ListRequest{Filter: model.Filter{Conversation: &env.conv.Ref}, Page: model.PageRequest{Limit: 100}})
		if err != nil {
			return err
		}
		if len(p.Items) != 3 {
			t.Fatalf("conversation holds %d messages, want the live one and 2 imported", len(p.Items))
		}
		for i, m := range p.Items {
			want := historyStaff
			if i == 0 {
				want = ""
			}
			if m.ImportedBy != want {
				t.Fatalf("message %d ImportedBy = %q, want %q", i, m.ImportedBy, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// historySilentRead: a conversation read before an import stays read
// after it; a live message recorded after the marker makes it unread.
func historySilentRead(t *testing.T, env *historyEnv) {
	env.clock.Advance(time.Second)
	env.markRead(t, true)
	env.wantUnread(t, false)
	env.clock.Advance(time.Second)
	env.importWindow(t, "pm-1", "pm-2")
	env.wantUnread(t, false)
	env.clock.Advance(time.Second)
	env.live(t, "pm-after")
	env.wantUnread(t, true)
}

// historySilentBeforeMarker: messages imported before the marker are read
// with everything else, and stay so.
func historySilentBeforeMarker(t *testing.T, env *historyEnv) {
	env.clock.Advance(time.Second)
	env.importWindow(t, "pm-1")
	env.wantUnread(t, true) // the live message, not yet read
	env.clock.Advance(time.Second)
	env.markRead(t, true)
	env.wantUnread(t, false)
}

// historySilentImportedOnly: a conversation a channel import opened holds
// only imported messages, so it is unread for no one, marker or not.
func historySilentImportedOnly(t *testing.T, env *historyEnv) {
	env.clock.Advance(time.Second)
	env.hold(env.message("room-2", "pm-new-room", env.start.Add(-3*time.Hour)))
	rep, err := env.importNow(env.importReq(func(r *uma.ImportRequest) { r.Conversation, r.Channel = nil, &env.channel }))
	if err != nil || rep.Imported != 1 {
		t.Fatalf("channel import = %+v, %v; want 1 imported", rep, err)
	}
	pg, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Inboxes: []model.InboxRef{env.inbox}}, 10, ""))
	if err != nil || len(pg.Items) != 2 {
		t.Fatalf("List = %+v, %v; want room-1 and room-2", pg.Items, err)
	}
	unread := env.unread(t)
	if len(unread) != 1 || unread[0] != env.conv.Ref {
		t.Fatalf("unread = %v, want only room-1 (its live message)", unread)
	}
}

// historySilentMarkUnread: MarkUnread without a message marks the newest
// live inbound message unread, whatever was imported after it.
func historySilentMarkUnread(t *testing.T, env *historyEnv) {
	env.clock.Advance(time.Second)
	env.markRead(t, true)
	env.clock.Advance(time.Second)
	env.importWindow(t, "pm-1", "pm-2")
	env.wantUnread(t, false)
	env.markRead(t, false)
	env.wantUnread(t, true)
}

// snooze snoozes the env's conversation for d as the staff client.
func (env *historyEnv) snooze(t *testing.T, d time.Duration) {
	t.Helper()
	until := env.clock.Now().Add(d)
	cmd := model.Command{Call: model.Call{Correlation: "corr"}, Operation: model.OperationID("op-snooze")}
	if _, err := env.client.Conversations().Snooze(env.ctx, model.ConversationSnooze{Command: cmd, Conversation: env.conv.Ref, Until: &until}); err != nil {
		t.Fatalf("Snooze: %v", err)
	}
}

// importWindow holds a message of room-1 per provider message ID, all
// older than the live one, and imports them into the env's conversation.
func (env *historyEnv) importWindow(t *testing.T, ids ...string) {
	t.Helper()
	at := env.start.Add(-3 * time.Hour)
	for i, id := range ids {
		env.hold(env.message("room-1", id, at.Add(time.Duration(i)*time.Minute)))
	}
	rep, err := env.importNow(env.importReq(nil))
	if err != nil || rep.Imported != len(ids) {
		t.Fatalf("Import = %+v, %v; want %d imported", rep, err, len(ids))
	}
}

// live delivers a new message of room-1 through the webhook sink, as
// ingress records it at the engine clock.
func (env *historyEnv) live(t *testing.T, msgID string) {
	t.Helper()
	m := env.message("room-1", msgID, env.clock.Now().UTC())
	if _, err := env.sink.Accept(env.ctx, model.Ingress{Call: model.Call{Correlation: "corr-live"}, Envelope: m}); err != nil {
		t.Fatalf("live Accept: %v", err)
	}
}

// wantSnoozed requires the conversation's snooze to be in effect or not,
// both in Get and under Filter.Snoozed.
func (env *historyEnv) wantSnoozed(t *testing.T, in bool) {
	t.Helper()
	got, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "corr"}, Ref: env.conv.Ref})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if (got.Snooze != nil) != in {
		t.Fatalf("Get shows snooze %+v, want in effect %v", got.Snooze, in)
	}
	yes := true
	pg, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Inboxes: []model.InboxRef{env.inbox}, Snoozed: &yes}, 10, ""))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if listed := len(pg.Items) == 1 && pg.Items[0].Ref == env.conv.Ref; listed != in || len(pg.Items) > 1 {
		t.Fatalf("List snoozed = %+v, want the conversation listed %v", pg.Items, in)
	}
}

// markRead marks the env's conversation read (or unread) for the staff
// client, without naming a message.
func (env *historyEnv) markRead(t *testing.T, read bool) {
	t.Helper()
	req := model.ReadMark{Call: model.Call{Correlation: "corr"}, Conversation: env.conv.Ref}
	var err error
	if read {
		_, err = env.client.Conversations().MarkRead(env.ctx, req)
	} else {
		_, err = env.client.Conversations().MarkUnread(env.ctx, req)
	}
	if err != nil {
		t.Fatalf("MarkRead(%v): %v", read, err)
	}
}

// unread lists the inbox's conversations unread for the staff client.
func (env *historyEnv) unread(t *testing.T) []model.ConversationRef {
	t.Helper()
	pg, err := env.client.Conversations().List(env.ctx, serviceList(model.Filter{Inboxes: []model.InboxRef{env.inbox}, Unread: &model.UnreadFilter{}}, 10, ""))
	if err != nil {
		t.Fatalf("List unread: %v", err)
	}
	var out []model.ConversationRef
	for _, c := range pg.Items {
		out = append(out, c.Ref)
	}
	return out
}

// wantUnread requires the env's conversation to be unread for the staff
// client or not.
func (env *historyEnv) wantUnread(t *testing.T, unread bool) {
	t.Helper()
	got := env.unread(t)
	if listed := len(got) == 1 && got[0] == env.conv.Ref; listed != unread || len(got) > 1 {
		t.Fatalf("unread = %v, want the conversation unread %v", got, unread)
	}
}
