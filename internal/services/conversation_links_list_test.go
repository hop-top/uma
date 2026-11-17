package services_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Filter.Linked keeps the conversations holding a link the caller may
// read; a conversation whose only link points at an unreadable far end is
// left out for that caller.
func TestListByLinked(t *testing.T) {
	l := newLinkRig(t)
	if _, err := l.C.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.Conv.Ref, Link: model.ConversationLink{Conversation: &l.other}}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	refs := func(f model.Filter, desk bool) []model.ConversationRef {
		t.Helper()
		c := l.C
		if desk {
			c = l.desk
		}
		pg, err := c.Conversations().List(l.Ctx, model.ListRequest{Call: call(), Filter: f, Page: model.PageRequest{Limit: 50}})
		if err != nil {
			t.Fatalf("List(%+v): %v", f, err)
		}
		var out []model.ConversationRef
		for _, conv := range pg.Items {
			out = append(out, conv.Ref)
		}
		return out
	}
	if got := refs(model.Filter{Linked: &yes}, false); !slices.Equal(got, []model.ConversationRef{l.Conv.Ref, l.other}) {
		t.Fatalf("linked %v", got)
	}
	if got := refs(model.Filter{Linked: &no}, false); !slices.Equal(got, []model.ConversationRef{l.item.Ref.Conversation}) {
		t.Fatalf("unlinked %v", got)
	}
	// desk cannot read l.other: the near end's only link is hidden, so it
	// is not listed as linked; l.other itself is unreadable.
	l.pol.add(deskActor, model.ActionConversationRead, l.other.Ref())
	if got := refs(model.Filter{Linked: &yes}, true); len(got) != 0 {
		t.Fatalf("desk lists %v as linked through an unreadable far end", got)
	}
}

// Filter.Unread is the caller's own: two desk actors see their own unread
// conversations; naming another actor is refused.
func TestListByUnread(t *testing.T) {
	l := newLinkRig(t)
	put := func(conv model.ConversationRef, id model.ID) {
		t.Helper()
		m := model.Message{
			Ref: model.MessageRef{Conversation: conv, ID: id}, Audience: model.Audience{Visibility: model.Public},
			Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi"}}}, RecordedAt: l.Clock.Now().UTC(), Revision: "m1",
		}
		if err := l.E.Store().Transact(l.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(l.Ctx, m, "") }); err != nil {
			t.Fatal(err)
		}
	}
	put(l.Conv.Ref, "00000008-0000-7000-8000-000000000001")
	put(l.other, "00000008-0000-7000-8000-000000000002")
	l.Clock.Advance(time.Second)
	desk2 := clientAs(t, l.rig, "desk-2")
	if _, err := l.desk.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref}); err != nil {
		t.Fatal(err)
	}
	list := func(f model.Filter, desk bool) ([]model.ConversationRef, error) {
		c := desk2
		if desk {
			c = l.desk
		}
		pg, err := c.Conversations().List(l.Ctx, model.ListRequest{Call: call(), Filter: f, Page: model.PageRequest{Limit: 1}})
		var out []model.ConversationRef
		for err == nil {
			for _, conv := range pg.Items {
				out = append(out, conv.Ref)
			}
			if pg.Complete || pg.Next == "" {
				break
			}
			pg, err = c.Conversations().List(l.Ctx, model.ListRequest{Call: call(), Filter: f, Page: model.PageRequest{Limit: 1, After: pg.Next}})
		}
		return out, err
	}
	mine := model.Filter{Unread: &model.UnreadFilter{}}
	if got, err := list(mine, true); err != nil || !slices.Equal(got, []model.ConversationRef{l.other}) {
		t.Fatalf("desk unread = %v, %v, want only the unmarked conversation", got, err)
	}
	if got, err := list(mine, false); err != nil || !slices.Equal(got, []model.ConversationRef{l.Conv.Ref, l.other}) {
		t.Fatalf("desk-2 unread = %v, %v, want both", got, err)
	}
	named := model.Filter{Unread: &model.UnreadFilter{Actor: deskActor}}
	if got, err := list(named, true); err != nil || !slices.Equal(got, []model.ConversationRef{l.other}) {
		t.Fatalf("desk naming itself = %v, %v", got, err)
	}
	_, err := list(named, false)
	requireCode(t, err, model.Validation, "filter.unread.actor")
}
