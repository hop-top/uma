package services_test

import (
	"slices"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// owned stores a conversation in in, owned by owner (nil: ownerless).
func (m *merger) owned(in model.InboxRef, owner *model.IdentityRef) model.Conversation {
	m.T.Helper()
	cv := model.Conversation{
		Ref: model.ConversationRef{Inbox: in, ID: m.id()}, Mode: model.ModePerRoom, Status: model.ConversationOpen, Owner: owner,
		Participants: []model.Participant{{Role: "member"}}, CreatedAt: m.Clock.Now(), Revision: "cv1",
	}
	m.put(func(tx store.Tx) error { return tx.PutConversation(m.Ctx, cv, "") })
	return cv
}

// The preview lists the identities on the contact and the readable
// conversations they own, with both revisions, and changes nothing; its
// revisions and identities feed Merge as they are.
func TestPreviewMergeListsWhatWouldMove(t *testing.T) {
	m := newMerger(t)
	ownedByA := m.owned(m.inbox1(), &m.a.Ref)
	ownedByB := m.owned(m.inbox2(), &m.b.Ref)
	m.owned(m.inbox1(), &m.c.Ref) // owned by an identity that stays
	m.owned(m.inbox1(), nil)
	got, err := m.preview()
	if err != nil {
		t.Fatal(err)
	}
	if got.From != m.from.Ref || got.Into != m.into.Ref || got.FromRevision != m.from.Revision || got.IntoRevision != m.into.Revision ||
		!slices.Equal(got.Identities, []model.IdentityRef{m.a.Ref, m.b.Ref}) || got.MoreConversations ||
		!slices.Equal(got.Conversations, []model.ConversationRef{ownedByA.Ref, ownedByB.Ref}) {
		t.Fatalf("preview %+v", got)
	}
	m.untouched()
	if _, err := m.operation2(model.ActionContactMerge, m.from.Ref.Ref(), "any"); err == nil {
		t.Fatal("preview claimed an operation")
	}
	acts := actions(m.pol.seen())
	want := []string{model.ActionContactMerge, model.ActionIdentityRead, model.ActionIdentityRead, model.ActionConversationRead, model.ActionConversationRead}
	if !slices.Equal(acts, want) {
		t.Fatalf("preview decided %v, want %v", acts, want)
	}

	req := model.MergeContacts{
		Command: cmd("op-1", got.FromRevision), From: got.From, Into: got.Into, ExpectedIntoRevision: got.IntoRevision,
		Identities: got.Identities, Reason: mergeReason,
	}
	if _, err := m.merge(req); err != nil {
		t.Fatalf("Merge of the preview: %v", err)
	}
	// The conversations stay where they are, owned by the same identities,
	// which now belong to the contact merged into.
	var cv model.Conversation
	m.view(func(r store.Reader) (err error) { cv, err = r.GetConversation(m.Ctx, ownedByA.Ref); return err })
	if cv.Owner == nil || *cv.Owner != m.a.Ref || cv.Revision != ownedByA.Revision || m.stored(m.a.Ref).Contact != m.into.Ref {
		t.Fatalf("conversation after merge %+v", cv)
	}
}

// A preview shows nothing its principal may not read: it is refused
// when an identity is unreadable, and leaves out unreadable conversations.
func TestPreviewMergeIsAuthorizedPerRecord(t *testing.T) {
	m := newMerger(t)
	hidden := m.owned(m.inbox1(), &m.a.Ref)
	shown := m.owned(m.inbox2(), &m.b.Ref)

	m.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionContactMerge })
	_, err := m.preview()
	requireCode(t, err, model.Unauthorized, "")
	m.pol.set(func(a policy.Authorization) bool {
		return a.Action != model.ActionIdentityRead || a.Resource != m.b.Ref.Ref()
	})
	_, err = m.preview()
	requireCode(t, err, model.Unauthorized, "")

	m.pol.set(func(a policy.Authorization) bool {
		return a.Action != model.ActionConversationRead || a.Resource != hidden.Ref.Ref()
	})
	got, err := m.preview()
	if err != nil || !slices.Equal(got.Conversations, []model.ConversationRef{shown.Ref}) {
		t.Fatalf("preview = %+v, %v", got, err)
	}
	m.untouched()
}

func TestPreviewMergeRefusesMergedAndOutOfScope(t *testing.T) {
	m := newMerger(t)
	foreign := model.PreviewMerge{Call: call(), From: m.from.Ref, Into: model.ContactRef{Tenant: tenantB, ID: m.into.Ref.ID}}
	_, err := m.C.Contacts().PreviewMerge(m.Ctx, foreign)
	requireCode(t, err, model.NotFound, "")
	_, err = m.C.Contacts().PreviewMerge(m.Ctx, model.PreviewMerge{Call: call(), From: m.from.Ref, Into: m.from.Ref})
	requireCode(t, err, model.Validation, "into")
	if len(m.pol.seen()) != 0 {
		t.Fatal("a refused preview was decided")
	}
	if _, err := m.merge(m.req("op-1")); err != nil {
		t.Fatal(err)
	}
	_, err = m.preview()
	requireCode(t, err, model.Conflict, "")
	_, err = m.C.Contacts().PreviewMerge(m.Ctx, model.PreviewMerge{Call: call(), From: m.person().Ref, Into: m.from.Ref})
	requireCode(t, err, model.Conflict, "")
}
