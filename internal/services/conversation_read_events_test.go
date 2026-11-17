package services_test

import (
	"encoding/json"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

func listedKinds(t *testing.T, l *linkRig, c enginetest.Client, kinds ...string) []model.Event {
	t.Helper()
	pg, err := c.Events().List(l.Ctx, model.ListEvents{Call: call(), Start: model.StartBeginning, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []model.Event
	for _, ev := range pg.Items {
		for _, k := range kinds {
			if ev.Kind == k {
				out = append(out, ev)
			}
		}
	}
	return out
}

// Each actual move of a read marker emits one conversation.read_marked or
// conversation.unread_marked event, for the marker's own actor only; a call that
// moves nothing emits nothing; the conversation revision never moves and
// the payload carries no message content.
func TestReadMarkerEvents(t *testing.T) {
	l := newLinkRig(t)
	other := clientAs(t, l.rig, "desk-2")
	m1 := model.Message{
		Ref: model.MessageRef{Conversation: l.Conv.Ref, ID: "00000007-0000-7000-8000-000000000001"}, Audience: model.Audience{Visibility: model.Public},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "secret body"}}}, RecordedAt: l.Clock.Now().UTC(), Revision: "m1",
	}
	if err := l.E.Store().Transact(l.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(l.Ctx, m1, "") }); err != nil {
		t.Fatal(err)
	}
	l.Clock.Advance(time.Minute)
	before := l.conv(t, l.Conv.Ref).Revision
	kinds := []string{model.EventConversationReadMarked, model.EventConversationUnreadMarked}

	read, err := l.desk.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.desk.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref}); err != nil {
		t.Fatal(err)
	}
	evs := listedKinds(t, l, l.desk, kinds...)
	if len(evs) != 1 || evs[0].Kind != model.EventConversationReadMarked {
		t.Fatalf("after mark read twice: %+v, want one conversation.read_marked", evs)
	}
	ev := evs[0]
	if ev.Inbox != l.Conv.Ref.Inbox.ID || ev.Resource != l.Conv.Ref.Ref() || ev.Actor != deskActor || ev.Payload.Name != model.SchemaConversationReadMarked {
		t.Fatalf("envelope %+v", ev)
	}
	var p model.ReadMarkerPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Conversation != l.Conv.Ref || p.Actor != deskActor || p.Revision != read.Revision {
		t.Fatalf("payload %+v, want conversation %v actor %s revision %s", p, l.Conv.Ref, deskActor, read.Revision)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload.Data, &raw); err != nil || len(raw) != 3 {
		t.Fatalf("payload keys %v, %v, want conversation, actor, revision only", raw, err)
	}

	unread, err := l.desk.Conversations().MarkUnread(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref, Message: &m1.Ref})
	if err != nil {
		t.Fatal(err)
	}
	evs = listedKinds(t, l, l.desk, kinds...)
	if len(evs) != 2 || evs[1].Kind != model.EventConversationUnreadMarked {
		t.Fatalf("after mark unread: %+v, want read then unread", evs)
	}
	if err := json.Unmarshal(evs[1].Payload.Data, &p); err != nil || p.Revision != unread.Revision {
		t.Fatalf("unread payload %+v, %v, want revision %s", p, err, unread.Revision)
	}
	if got := l.conv(t, l.Conv.Ref).Revision; got != before {
		t.Fatalf("conversation revision %q, want %q", got, before)
	}

	// Another desk actor, who may read the conversation and its workflow,
	// never receives them; neither does one that may not read workflow.
	if evs := listedKinds(t, l, other, kinds...); len(evs) != 0 {
		t.Fatalf("other actor lists %+v, want none", evs)
	}
	l.pol.add(deskActor, model.ActionConversationReadWorkflow, model.Ref{})
	if evs := listedKinds(t, l, l.desk, kinds...); len(evs) != 0 {
		t.Fatalf("actor without read_workflow lists %+v, want none", evs)
	}
}
