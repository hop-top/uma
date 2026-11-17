package services_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// denyPolicy allows everything except the decisions it was told to deny:
// an action for an actor, on one resource or (zero Ref) on every one.
type denyPolicy struct {
	mu   sync.Mutex
	deny []policy.Authorization
}

func (p *denyPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.deny {
		if d.Principal.Actor == a.Principal.Actor && d.Action == a.Action && (d.Resource == model.Ref{} || d.Resource == a.Resource) {
			return policy.Decision{}, nil
		}
	}
	return policy.Decision{Allow: true}, nil
}

func (p *denyPolicy) add(actor model.ActorID, action string, res model.Ref) {
	p.mu.Lock()
	p.deny = append(p.deny, policy.Authorization{Principal: model.Principal{Actor: actor}, Action: action, Resource: res})
	p.mu.Unlock()
}

type linkRig struct {
	*rig
	pol   *denyPolicy
	desk  enginetest.Client
	other model.ConversationRef
	item  model.WorkItem
	n     int
}

func newLinkRig(t *testing.T) *linkRig {
	t.Helper()
	pol := &denyPolicy{}
	r := newRig(t, withPolicy(pol))
	l := &linkRig{rig: r, pol: pol, desk: clientAs(t, r, deskActor)}
	other := r.Conv.Clone()
	other.Ref.ID, other.ReplyRoute.ID = "00000005-0000-7000-8000-000000000001", "room-other"
	l.other = other.Ref
	itemConv := r.Conv.Clone()
	itemConv.Ref.ID, itemConv.ReplyRoute.ID = "00000005-0000-7000-8000-000000000002", "room-item"
	l.item = model.WorkItem{
		Ref:      model.WorkItemRef{Conversation: itemConv.Ref, ID: "00000005-0000-7000-8000-000000000003"},
		External: model.ExternalRef{Channel: r.Ch.Ref, Namespace: routeNSpace, Kind: model.RefKindWorkItem, ID: "wi-1"},
		Status:   "open", Binding: model.WorkItemBindingActive, Revision: "w1",
	}
	if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		if err := tx.PutConversation(r.Ctx, other, ""); err != nil {
			return err
		}
		if err := tx.PutConversation(r.Ctx, itemConv, ""); err != nil {
			return err
		}
		return tx.PutWorkItem(r.Ctx, l.item, "")
	}); err != nil {
		t.Fatal(err)
	}
	return l
}

func (l *linkRig) cmd(rev model.Revision) model.Command {
	l.n++
	return model.Command{Call: call(), Operation: model.OperationID(fmt.Sprintf("op-link-%d", l.n)), ExpectedRevision: rev}
}

func (l *linkRig) conv(t *testing.T, ref model.ConversationRef) model.Conversation {
	t.Helper()
	var c model.Conversation
	if err := l.E.Store().View(l.Ctx, tenantA, func(r store.Reader) (err error) {
		c, err = r.GetConversation(l.Ctx, ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func (l *linkRig) linkEvents(t *testing.T) []model.Event {
	t.Helper()
	var out []model.Event
	if err := l.E.Store().View(l.Ctx, tenantA, func(r store.Reader) error {
		pg, err := r.Events(l.Ctx, model.Filter{}, "", 100)
		for _, ev := range pg.Items {
			if ev.Kind == model.EventConversationLinked || ev.Kind == model.EventConversationUnlinked {
				out = append(out, ev)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// A link between two conversations is held by both, written in one unit,
// with one linked event per end naming its far end; linking again changes
// nothing; unlinking removes it from both.
func TestLinkConversationsBothEnds(t *testing.T) {
	l := newLinkRig(t)
	toOther := model.ConversationLink{Conversation: &l.other}
	c := l.cmd(l.Conv.Revision)
	if _, err := l.desk.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: c, Conversation: l.Conv.Ref, Link: toOther}); err != nil {
		t.Fatalf("Link: %v", err)
	}
	a, b := l.conv(t, l.Conv.Ref), l.conv(t, l.other)
	if len(a.Links) != 1 || a.Links[0].Conversation == nil || *a.Links[0].Conversation != l.other || a.Revision == l.Conv.Revision {
		t.Fatalf("near end %+v", a)
	}
	if len(b.Links) != 1 || b.Links[0].Conversation == nil || *b.Links[0].Conversation != l.Conv.Ref || b.Revision == "cv1" {
		t.Fatalf("far end %+v", b)
	}
	evs := l.linkEvents(t)
	if len(evs) != 2 || evs[0].Kind != model.EventConversationLinked || evs[0].Resource != l.Conv.Ref.Ref() || evs[1].Resource != l.other.Ref() {
		t.Fatalf("events %+v, want one linked event per end", evs)
	}
	if p := payloadOf[model.ConversationLinkPayload](t, evs[1]); p.Link.Conversation == nil || *p.Link.Conversation != l.Conv.Ref {
		t.Fatalf("far-end payload %+v", p)
	}
	replay, err := l.desk.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: c, Conversation: l.Conv.Ref, Link: toOther})
	if err != nil || !replay.Replayed {
		t.Fatalf("retry = %+v, %v", replay, err)
	}
	if _, err := l.desk.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.other, Link: model.ConversationLink{Conversation: &l.Conv.Ref}}); err != nil {
		t.Fatalf("Link from the far end: %v", err)
	}
	if got := l.conv(t, l.Conv.Ref); got.Revision != a.Revision || len(l.linkEvents(t)) != 2 {
		t.Fatal("linking an existing link changed something")
	}
	if _, err := l.desk.Conversations().Unlink(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.other, Link: model.ConversationLink{Conversation: &l.Conv.Ref}}); err != nil {
		t.Fatalf("Unlink: %v", err)
	}
	if a, b := l.conv(t, l.Conv.Ref), l.conv(t, l.other); len(a.Links) != 0 || len(b.Links) != 0 {
		t.Fatalf("links after Unlink: %+v / %+v", a.Links, b.Links)
	}
	if evs := l.linkEvents(t); len(evs) != 4 || evs[2].Kind != model.EventConversationUnlinked || evs[3].Kind != model.EventConversationUnlinked {
		t.Fatalf("events %+v, want two unlinked", evs)
	}
}

// Linking needs conversation.link on both conversation ends, or
// work_item.read on a work item end; a link to a work item is held by the
// conversation alone. A stale expected revision conflicts.
func TestLinkRequiresBothEnds(t *testing.T) {
	l := newLinkRig(t)
	l.pol.add(deskActor, model.ActionConversationLink, l.other.Ref())
	_, err := l.desk.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.Conv.Ref, Link: model.ConversationLink{Conversation: &l.other}})
	requireCode(t, err, model.Unauthorized, "")
	l.pol.add(deskActor, model.ActionWorkItemRead, l.item.Ref.Ref())
	toItem := model.ConversationLink{WorkItem: &l.item.Ref}
	_, err = l.desk.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.Conv.Ref, Link: toItem})
	requireCode(t, err, model.Unauthorized, "")
	if c := l.conv(t, l.Conv.Ref); len(c.Links) != 0 || len(l.linkEvents(t)) != 0 {
		t.Fatalf("refused links changed %+v", c)
	}
	_, err = l.C.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd("stale"), Conversation: l.Conv.Ref, Link: toItem})
	requireCode(t, err, model.Conflict, "")
	if _, err := l.C.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(l.Conv.Revision), Conversation: l.Conv.Ref, Link: toItem}); err != nil {
		t.Fatalf("Link work item: %v", err)
	}
	if c := l.conv(t, l.Conv.Ref); len(c.Links) != 1 || c.Links[0].WorkItem == nil || *c.Links[0].WorkItem != l.item.Ref {
		t.Fatalf("links %+v", c.Links)
	}
	if c := l.conv(t, l.item.Ref.Conversation); len(c.Links) != 0 {
		t.Fatalf("the work item's conversation holds %+v", c.Links)
	}
	if evs := l.linkEvents(t); len(evs) != 1 {
		t.Fatalf("events %+v, want one for the one conversation end", evs)
	}
	missing := model.WorkItemRef{Conversation: l.Conv.Ref, ID: "00000005-0000-7000-8000-0000000000ff"}
	_, err = l.C.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.Conv.Ref, Link: model.ConversationLink{WorkItem: &missing}})
	requireCode(t, err, model.NotFound, "")
}

// A reader sees only the links whose far end it may read, and receives a
// link event only when it may read the far end it names.
func TestLinkFarEndHiddenWhenUnreadable(t *testing.T) {
	l := newLinkRig(t)
	for _, link := range []model.ConversationLink{{Conversation: &l.other}, {WorkItem: &l.item.Ref}} {
		if _, err := l.C.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.Conv.Ref, Link: link}); err != nil {
			t.Fatal(err)
		}
	}
	l.pol.add(deskActor, model.ActionConversationRead, l.other.Ref())
	got, err := l.desk.Conversations().Get(l.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: l.Conv.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Links) != 1 || got.Links[0].WorkItem == nil {
		t.Fatalf("links %+v, want only the readable work item", got.Links)
	}
	pg, err := l.desk.Events().List(l.Ctx, model.ListEvents{Call: call(), Start: model.StartBeginning, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range pg.Items {
		if ev.Kind != model.EventConversationLinked {
			continue
		}
		p := payloadOf[model.ConversationLinkPayload](t, ev)
		if p.Link.Conversation != nil && *p.Link.Conversation == l.other {
			t.Fatalf("event naming an unreadable far end delivered: %+v", ev)
		}
	}
	if full, err := l.C.Conversations().Get(l.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: l.Conv.Ref}); err != nil || len(full.Links) != 2 {
		t.Fatalf("unrestricted reader sees %+v, %v", full.Links, err)
	}
}

// Growing a conversation past MaxConversationLinks conflicts.
func TestLinkCap(t *testing.T) {
	l := newLinkRig(t)
	cur := l.conv(t, l.Conv.Ref)
	next := cur.Clone()
	for i := range model.MaxConversationLinks {
		ref := model.ConversationRef{Inbox: l.Conv.Ref.Inbox, ID: model.ID(fmt.Sprintf("00000006-0000-7000-8000-%012d", i))}
		next.Links = append(next.Links, model.ConversationLink{Conversation: &ref})
	}
	next.Revision = "cv-full"
	if err := l.E.Store().Transact(l.Ctx, tenantA, func(tx store.Tx) error { return tx.PutConversation(l.Ctx, next, cur.Revision) }); err != nil {
		t.Fatal(err)
	}
	_, err := l.C.Conversations().Link(l.Ctx, model.ConversationLinkChange{Command: l.cmd(""), Conversation: l.Conv.Ref, Link: model.ConversationLink{WorkItem: &l.item.Ref}})
	requireCode(t, err, model.Conflict, "")
}

// Two desk actors keep independent read markers; marking moves no
// conversation revision and emits nothing; a participant without
// conversation.mark_read cannot mark.
func TestReadStatePerActor(t *testing.T) {
	l := newLinkRig(t)
	other := clientAs(t, l.rig, "desk-2")
	m1 := model.Message{
		Ref: model.MessageRef{Conversation: l.Conv.Ref, ID: "00000007-0000-7000-8000-000000000001"}, Audience: model.Audience{Visibility: model.Public},
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hi"}}}, RecordedAt: l.Clock.Now().UTC(), Revision: "m1",
	}
	if err := l.E.Store().Transact(l.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(l.Ctx, m1, "") }); err != nil {
		t.Fatal(err)
	}
	l.Clock.Advance(time.Minute)
	before := l.conv(t, l.Conv.Ref).Revision
	marker, err := l.desk.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref})
	if err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if marker.Actor != deskActor || !marker.ReadThrough.Equal(l.Clock.Now()) || marker.Revision == "" {
		t.Fatalf("marker %+v", marker)
	}
	again, err := l.desk.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref})
	if err != nil || again.Revision != marker.Revision {
		t.Fatalf("repeat = %+v, %v, want the stored marker", again, err)
	}
	mine, err := l.desk.Conversations().ReadMarker(l.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: l.Conv.Ref})
	if err != nil || mine != marker {
		t.Fatalf("ReadMarker = %+v, %v", mine, err)
	}
	theirs, err := other.Conversations().ReadMarker(l.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: l.Conv.Ref})
	if err != nil || theirs.Revision != "" || !theirs.ReadThrough.IsZero() || theirs.Actor != "desk-2" {
		t.Fatalf("other actor's marker %+v, %v, want none", theirs, err)
	}
	unread, err := l.desk.Conversations().MarkUnread(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref, Message: &m1.Ref})
	if err != nil || !unread.ReadThrough.Before(m1.RecordedAt) {
		t.Fatalf("MarkUnread = %+v, %v, want the marker before m1", unread, err)
	}
	if got := l.conv(t, l.Conv.Ref).Revision; got != before {
		t.Fatalf("conversation revision %q after marking, want %q", got, before)
	}
	var evs int
	if err := l.E.Store().View(l.Ctx, tenantA, func(r store.Reader) error {
		pg, err := r.Events(l.Ctx, model.Filter{}, "", 100)
		evs = len(pg.Items)
		return err
	}); err != nil || evs != 0 {
		t.Fatalf("%d events after marking, %v; want none", evs, err)
	}
	l.pol.add(externalActor, model.ActionConversationMarkRead, model.Ref{})
	l.pol.add(externalActor, model.ActionConversationReadWorkflow, model.Ref{})
	ext := clientAs(t, l.rig, externalActor)
	_, err = ext.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref})
	requireCode(t, err, model.Unauthorized, "")
	_, err = ext.Conversations().ReadMarker(l.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: l.Conv.Ref})
	requireCode(t, err, model.Unauthorized, "")
	_, err = l.desk.Conversations().MarkRead(l.Ctx, model.ReadMark{Call: call(), Conversation: l.Conv.Ref, Message: &model.MessageRef{Conversation: l.Conv.Ref, ID: "00000007-0000-7000-8000-0000000000ff"}})
	requireCode(t, err, model.NotFound, "")
}
