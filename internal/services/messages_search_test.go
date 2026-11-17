package services_test

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// searchMsg seeds a message in conv with text, occurring at at, holding
// atts as attachment parts.
func (s *svc) searchMsg(conv model.ConversationRef, vis model.Visibility, at time.Time, text string, atts ...model.AttachmentRef) model.Message {
	s.T.Helper()
	parts := []model.Part{{Kind: model.PartText, Text: text}}
	for _, a := range atts {
		parts = append(parts, model.Part{Kind: model.PartAttachment, Attachment: &a})
	}
	m := model.Message{
		Ref: model.MessageRef{Conversation: conv, ID: s.id()}, Audience: model.Audience{Visibility: vis},
		Content:    model.Content{Parts: parts},
		External:   []model.ExternalRef{{Channel: s.Ch.Ref, Namespace: routeNSpace, Kind: "message", ID: string(s.id())}},
		OccurredAt: at, RecordedAt: s.Clock.Now(), Revision: "m1",
	}
	s.put(func(tx store.Tx) error { return tx.PutMessage(s.Ctx, m, "") })
	return m
}

// searchAtt seeds an attachment record of mediaType in inbox one.
func (s *svc) searchAtt(mediaType string, vis model.Visibility) model.AttachmentRef {
	s.T.Helper()
	ref := model.AttachmentRef{Inbox: s.Conv.Ref.Inbox, ID: s.id()}
	rec := store.AttachmentRecord{
		Attachment: model.Attachment{Ref: ref, Revision: "a1", Metadata: model.AttachmentMetadata{
			Name: "file", MediaType: mediaType, Size: 1, Audience: model.Audience{Visibility: vis},
		}},
		State: "stored",
	}
	s.put(func(tx store.Tx) error { return tx.PutAttachment(s.Ctx, rec, "") })
	return ref
}

func (s *svc) search(f model.Filter, limit int) []model.ID {
	s.T.Helper()
	list := func(r model.ListRequest) (model.Page[model.Message], error) { return s.C.Messages().List(s.Ctx, r) }
	return msgIDs(drain(s.T, list, f, limit))
}

func wantIDs(t *testing.T, what string, got []model.ID, want ...model.Message) {
	t.Helper()
	ids := msgIDs(want)
	if ids == nil {
		ids = []model.ID{}
	}
	if got == nil {
		got = []model.ID{}
	}
	if !slices.Equal(got, ids) {
		t.Fatalf("%s = %v, want %v", what, got, ids)
	}
}

var searchT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// Text matches every query token as a whole token of a text part, in any
// order and case.
func TestSearchFindsTextInVisibleMessage(t *testing.T) {
	s := newSvc(t)
	hit := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "Invoice #42 paid, thanks")
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "hello there")
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoices are due")
	wantIDs(t, "invoice", s.search(model.Filter{Text: "invoice"}, 10), hit)
	wantIDs(t, "PAID INVOICE", s.search(model.Filter{Text: "PAID INVOICE"}, 10), hit)
	wantIDs(t, "invoice unpaid", s.search(model.Filter{Text: "invoice unpaid"}, 10))
}

// An internal note matching the text never reaches a public caller: no
// item, no placeholder, and the page reads exactly as a search matching
// nothing.
func TestSearchSkipsInternalNoteForPublicCaller(t *testing.T) {
	s := newSvc(t)
	pub := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoice sent")
	note := s.searchMsg(s.Conv.Ref, model.Internal, searchT0, "invoice secret margin")
	s.pol.set(publicOnly)
	wantIDs(t, "member invoice", s.search(model.Filter{Text: "invoice"}, 1), pub)
	hidden, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Text: "secret"}, 10, ""))
	if err != nil {
		t.Fatal(err)
	}
	none, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Text: "nowhere"}, 10, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden.Items) != 0 || !hidden.Complete || hidden.Next != "" || hidden.Complete != none.Complete || len(none.Items) != 0 {
		t.Fatalf("note search page = %+v, empty search page = %+v; want both empty and complete", hidden, none)
	}
	s.pol.set(nil)
	wantIDs(t, "staff invoice", s.search(model.Filter{Text: "invoice"}, 1), pub, note)
}

// Since and Before bound OccurredAt as [Since, Before), to the
// nanosecond.
func TestSearchDateBounds(t *testing.T) {
	s := newSvc(t)
	t1 := searchT0.Add(time.Hour)
	early := s.searchMsg(s.Conv.Ref, model.Public, searchT0.Add(-time.Nanosecond), "x")
	first := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "x")
	last := s.searchMsg(s.Conv.Ref, model.Public, t1.Add(-time.Nanosecond), "x")
	after := s.searchMsg(s.Conv.Ref, model.Public, t1, "x")
	wantIDs(t, "window", s.search(model.Filter{Since: &searchT0, Before: &t1}, 10), first, last)
	wantIDs(t, "since", s.search(model.Filter{Since: &t1}, 10), after)
	paris := searchT0.In(time.FixedZone("CEST", 2*3600))
	wantIDs(t, "before (other zone)", s.search(model.Filter{Before: &paris, Text: "x"}, 10), early)
}

// Has selects messages holding at least one attachment part, or none.
func TestSearchHasAttachment(t *testing.T) {
	s := newSvc(t)
	att := s.searchAtt("application/pdf", model.Public)
	with := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "report", att)
	without := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "report")
	yes, no := true, false
	wantIDs(t, "has", s.search(model.Filter{Attachments: &model.AttachmentFilter{Has: &yes}}, 10), with)
	wantIDs(t, "has not", s.search(model.Filter{Attachments: &model.AttachmentFilter{Has: &no}}, 10), without)
}

// Search narrows together with the inbox and conversation filters.
func TestSearchCombinesWithInboxFilter(t *testing.T) {
	s := newSvc(t)
	other := s.conversation(s.inbox2.Ref, model.ConversationOpen)
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "refund please")
	two := s.searchMsg(other.Ref, model.Public, searchT0, "refund please")
	s.searchMsg(other.Ref, model.Public, searchT0, "thanks")
	in2 := s.inbox2.Ref
	wantIDs(t, "inbox two refund", s.search(model.Filter{Inboxes: []model.InboxRef{in2}, Text: "refund"}, 10), two)
	conv := other.Ref
	wantIDs(t, "conversation refund", s.search(model.Filter{Conversation: &conv, Text: "refund"}, 10), two)
}

// A redacted or provider-deleted message keeps no searchable text and no
// attachment, so it never matches.
func TestSearchRedactedMessageNotReturned(t *testing.T) {
	s := newSvc(t)
	png := func() model.AttachmentRef { return s.searchAtt("image/png", model.Public) }
	gone := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoice", png())
	kept := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoice", png())
	deleted := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoice", png())
	s.put(func(tx store.Tx) error {
		_, err := tx.Redact(s.Ctx, store.Redaction{Message: &gone.Ref, Expected: "m1", Revision: "m2"})
		return err
	})
	del := deleted.Clone()
	del.Deleted, del.Content, del.Revision = true, model.Content{}, "m2"
	s.put(func(tx store.Tx) error { return tx.PutMessage(s.Ctx, del, "m1") })
	yes := true
	for name, f := range map[string]model.Filter{
		"text":  {Text: "invoice"},
		"has":   {Attachments: &model.AttachmentFilter{Has: &yes}},
		"media": {Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}},
	} {
		wantIDs(t, name, s.search(f, 10), kept)
	}
}

// A media type is confirmed only through an attachment the caller may
// read: ActionAttachmentRead under the attachment's own audience and
// under the holding message's audience, on an attachment that matches.
func TestSearchMediaTypeRequiresAttachmentRead(t *testing.T) {
	s := newSvc(t)
	readable := s.searchAtt("image/png", model.Public)
	denied := s.searchAtt("Image/PNG; name=x", model.Public)
	floor := s.searchAtt("image/png", model.Restricted)
	pdf := s.searchAtt("application/pdf", model.Public)
	open := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "a", readable)
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "b", denied)
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "c", floor)
	// A readable attachment of another type confirms nothing.
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "d", pdf, denied)
	// Readable under its own audience but not under the holder's.
	internal := s.searchMsg(s.Conv.Ref, model.Internal, searchT0, "e", readable)
	s.pol.set(func(a policy.Authorization) bool {
		if a.Action != model.ActionAttachmentRead {
			return true
		}
		if a.Resource == denied.Ref() {
			return false
		}
		return a.Audience.Visibility == model.Public
	})
	png := model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}}
	wantIDs(t, "image/png", s.search(png, 1), open)
	s.pol.set(nil)
	all := s.search(png, 1)
	if len(all) != 5 || !slices.Contains(all, internal.Ref.ID) {
		t.Fatalf("staff image/png = %v, want all five holders", all)
	}
	// The gate runs only for media types: Has confirms nothing the
	// message's own parts do not show.
	s.pol.set(func(a policy.Authorization) bool { return a.Action != model.ActionAttachmentRead })
	yes := true
	if got := s.search(model.Filter{Attachments: &model.AttachmentFilter{Has: &yes}}, 10); len(got) != 5 {
		t.Fatalf("has under attachment denial = %v, want five", got)
	}
	wantIDs(t, "image/png under attachment denial", s.search(png, 10))
}

// The service re-checks what the message record carries, whatever the
// store returned: text, dates and Has narrow even when a backend ignores
// them, and media types are matched against the attachment records again.
func TestSearchRechecksStoreResults(t *testing.T) {
	s := newSvc(t)
	png := s.searchAtt("image/png", model.Public)
	pdf := s.searchAtt("application/pdf", model.Public)
	hit := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "Invoice paid", png)
	early := s.searchMsg(s.Conv.Ref, model.Public, searchT0.Add(-time.Hour), "invoice paid", pdf)
	s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoices paid", png)
	bare := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "Invoice paid")
	other := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "Invoice paid", pdf)
	late := s.searchMsg(s.Conv.Ref, model.Public, searchT0.Add(time.Hour), "invoice paid", png)
	gone := s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoice paid", png)
	gone.Redacted, gone.Revision = true, "m2"
	s.put(func(tx store.Tx) error { return tx.PutMessage(s.Ctx, gone, "m1") })
	s.Store.Set(looseStore{Store: s.Store.Inner()})
	yes, no := true, false
	before := searchT0.Add(time.Hour)
	wantIDs(t, "text", s.search(model.Filter{Text: "paid invoice"}, 10), hit, early, bare, other, late)
	wantIDs(t, "dates", s.search(model.Filter{Since: &searchT0, Before: &before, Text: "invoice"}, 10), hit, bare, other)
	wantIDs(t, "has", s.search(model.Filter{Attachments: &model.AttachmentFilter{Has: &no}}, 10), bare)
	wantIDs(t, "media", s.search(model.Filter{
		Text: "invoice", Attachments: &model.AttachmentFilter{Has: &yes, MediaTypes: []string{"image/*"}},
	}, 10), hit, late)
}

// Hidden and non-matching messages never shape the pages a caller sees
// beyond their pace: matches come in recording order, across short and
// empty pages, each exactly once.
func TestSearchPagesOverHiddenMatches(t *testing.T) {
	s := newSvc(t)
	var want []model.Message
	for i := range 30 {
		switch i % 3 {
		case 0:
			want = append(want, s.searchMsg(s.Conv.Ref, model.Public, searchT0, "invoice"))
		case 1:
			s.searchMsg(s.Conv.Ref, model.Internal, searchT0, "invoice")
		default:
			s.searchMsg(s.Conv.Ref, model.Public, searchT0, "other")
		}
	}
	s.pol.set(publicOnly)
	for _, limit := range []int{1, 3, 7, 50} {
		wantIDs(t, "paged search", s.search(model.Filter{Text: "invoice"}, limit), want...)
	}
}
