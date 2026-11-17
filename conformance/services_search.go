package conformance

import (
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// serviceSearchCases run message search through Messages.List (the
// MessageSearch group): storage narrows, admission stays per message, media
// types need a readable matching attachment, tombstones never match, and
// pages over hidden matches return every visible match once, in recording
// order. The cases of the other files cover matching rules, labels and
// paging.
var serviceSearchCases = []struct {
	name string
	run  func(*testing.T, *serviceEnv)
}{
	{"AdmissionPerMessage", searchAdmission},
	{"MediaTypeNeedsAttachmentRead", searchMediaTypeGate},
	{"TombstonesNeverMatch", searchTombstones},
	{"PagingOverHiddenMatches", searchPaging},
	{"LabelNeedsConversationRead", searchLabelNeedsConversationRead},
	{"TextRules", searchTextRules},
	{"DateBounds", searchDateBounds},
	{"AttachmentPredicates", searchAttachmentPredicates},
	{"InternalNotesAndRedaction", searchNotesAndRedaction},
	{"LabelSelectorTruthTable", searchLabelSelectors},
	{"LabelChangeVisibleNextPage", searchLabelChangeBetweenPages},
	{"PagingUnderInserts", searchPagingUnderInserts},
	{"PagingUnderConcurrentInserts", searchPagingUnderConcurrentInserts},
	{"CursorBoundToFilter", searchCursorBinding},
	{"ScanBoundOverHiddenMatches", searchScanBound},
}

var searchAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// searchValue builds a message of the suite conversation with text,
// occurring at at, holding atts as attachment parts, without storing it.
func (env *serviceEnv) searchValue(vis model.Visibility, at time.Time, text string, atts ...model.AttachmentRef) model.Message {
	parts := []model.Part{{Kind: model.PartText, Text: text}}
	for _, a := range atts {
		parts = append(parts, model.Part{Kind: model.PartAttachment, Attachment: &a})
	}
	return model.Message{
		Ref: model.MessageRef{Conversation: env.conv.Ref, ID: env.id()}, Audience: model.Audience{Visibility: vis},
		Content: model.Content{Parts: parts}, OccurredAt: at, RecordedAt: env.clock.Now(), Revision: "m1",
	}
}

// searchMessage stores a searchValue message.
func (env *serviceEnv) searchMessage(vis model.Visibility, at time.Time, text string, atts ...model.AttachmentRef) model.Message {
	env.t.Helper()
	m := env.searchValue(vis, at, text, atts...)
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, m, "") })
	return m
}

// searchAttachment stores a public attachment record of mediaType.
func (env *serviceEnv) searchAttachment(mediaType string) model.AttachmentRef {
	env.t.Helper()
	ref := model.AttachmentRef{Inbox: env.inbox, ID: env.id()}
	rec := store.AttachmentRecord{
		Attachment: model.Attachment{Ref: ref, Revision: "a1", Metadata: model.AttachmentMetadata{
			Name: "file", MediaType: mediaType, Size: 1, Audience: model.Audience{Visibility: model.Public},
		}},
		State: "stored",
	}
	env.put(func(tx store.Tx) error { return tx.PutAttachment(env.ctx, rec, "") })
	return ref
}

func wantSearch(t *testing.T, what string, got []model.ID, want ...model.Message) {
	t.Helper()
	ids := make([]model.ID, 0, len(want))
	for _, m := range want {
		ids = append(ids, m.Ref.ID)
	}
	if len(got) != len(ids) || !slices.Equal(got, ids) {
		t.Fatalf("%s = %v, want %v", what, got, ids)
	}
}

// searchAdmission: a member never receives an internal note, whatever it
// matches, and a search only the note matches reads exactly as a search
// matching nothing: empty, complete, no Next.
func searchAdmission(t *testing.T, env *serviceEnv) {
	pub := env.searchMessage(model.Public, searchAt, "Invoice sent")
	note := env.searchMessage(model.Internal, searchAt, "invoice secret margin")
	wantSearch(t, "member invoice", env.messages(model.Filter{Text: "INVOICE"}, 1), pub)
	page := func(text string) model.Page[model.Message] {
		t.Helper()
		p, err := env.client.Messages().List(env.ctx, serviceList(model.Filter{Text: text}, 10, ""))
		if err != nil {
			t.Fatalf("Messages.List: %v", err)
		}
		return p
	}
	hidden, none := page("secret"), page("nowhere")
	if len(hidden.Items) != 0 || !hidden.Complete || hidden.Next != "" || len(none.Items) != 0 || !none.Complete || none.Next != "" {
		t.Fatalf("hidden-only page %+v, no-match page %+v; want both empty, complete, without Next", hidden, none)
	}
	env.policy.setStaff(true)
	wantSearch(t, "staff invoice", env.messages(model.Filter{Text: "invoice"}, 1), pub, note)
}

// searchMediaTypeGate: a media type is confirmed only through a matching
// attachment the caller may read; a readable attachment of another type
// confirms nothing, and Has needs no attachment read.
func searchMediaTypeGate(t *testing.T, env *serviceEnv) {
	readable := env.searchAttachment("image/png")
	denied := env.searchAttachment("Image/PNG; name=x")
	pdf := env.searchAttachment("application/pdf")
	open := env.searchMessage(model.Public, searchAt, "a", readable)
	shut := env.searchMessage(model.Public, searchAt, "b", denied)
	mixed := env.searchMessage(model.Public, searchAt, "c", pdf, denied)
	png := model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/*"}}}
	wantSearch(t, "before denial", env.messages(png, 1), open, shut, mixed)
	env.policy.denyOn(model.ActionAttachmentRead, denied.Ref())
	wantSearch(t, "image/*", env.messages(png, 1), open)
	wantSearch(t, "application/pdf", env.messages(model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"application/pdf"}}}, 1), mixed)
	yes := true
	wantSearch(t, "has", env.messages(model.Filter{Attachments: &model.AttachmentFilter{Has: &yes}}, 1), open, shut, mixed)
}

// searchTombstones: redacted and provider-deleted messages match no text,
// no Has and no media type.
func searchTombstones(t *testing.T, env *serviceEnv) {
	env.policy.setStaff(true)
	gone := env.searchMessage(model.Public, searchAt, "invoice", env.searchAttachment("image/png"))
	kept := env.searchMessage(model.Public, searchAt, "invoice", env.searchAttachment("image/png"))
	deleted := env.searchMessage(model.Public, searchAt, "invoice", env.searchAttachment("image/png"))
	if _, err := env.client.Privacy().Redact(env.ctx, model.Redact{Command: env.cmd(gone.Revision), Target: gone.Ref.Ref(), Message: &gone.Ref}); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	del := deleted.Clone()
	del.Deleted, del.Content, del.Revision = true, model.Content{}, "m2"
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, del, deleted.Revision) })
	yes := true
	for name, f := range map[string]model.Filter{
		"text":  {Text: "invoice"},
		"has":   {Attachments: &model.AttachmentFilter{Has: &yes}},
		"media": {Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}},
	} {
		wantSearch(t, name, env.messages(f, 1), kept)
	}
}

// searchPaging: visible matches interleaved with hidden matches and
// non-matches come once each, in recording order, at every page size,
// and narrow further by a half-open date window.
func searchPaging(t *testing.T, env *serviceEnv) {
	var all, window []model.Message
	for i := range 24 {
		at := searchAt.Add(time.Duration(i) * time.Minute)
		switch i % 3 {
		case 0:
			m := env.searchMessage(model.Public, at, "refund request")
			all = append(all, m)
			if i >= 6 && i < 18 {
				window = append(window, m)
			}
		case 1:
			env.searchMessage(model.Internal, at, "refund request")
		default:
			env.searchMessage(model.Public, at, "thanks")
		}
	}
	since, before := searchAt.Add(6*time.Minute), searchAt.Add(18*time.Minute)
	for _, limit := range []int{1, 3, 50} {
		wantSearch(t, "refund", env.messages(model.Filter{Text: "refund"}, limit), all...)
		wantSearch(t, "refund window", env.messages(model.Filter{Text: "refund", Since: &since, Before: &before}, limit), window...)
	}
}
