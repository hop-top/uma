package conformance

import (
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// searchRefused: Messages.List refuses f as model.Validation.
func searchRefused(t *testing.T, env *serviceEnv, name string, f model.Filter) {
	t.Helper()
	_, err := env.client.Messages().List(env.ctx, serviceList(f, 10, ""))
	if model.CodeOf(err) != model.Validation {
		t.Fatalf("%s: error = %v, want %s", name, err, model.Validation)
	}
}

// searchTextRules: text matches whole, case-folded tokens of text parts, all
// of them in any order; punctuation separates tokens; structured parts and
// metadata are not searched; an edit moves the match with the content; and
// text the tokenizer or the limits cannot use is refused.
func searchTextRules(t *testing.T, env *serviceEnv) {
	mail := env.searchMessage(model.Public, searchAt, "Invoice #42 for foo@bar.com")
	release := env.searchMessage(model.Public, searchAt, "Release v2.1 re-open")
	summer := env.searchMessage(model.Public, searchAt, "ÉTÉ à Paris")
	structured := env.searchValue(model.Public, searchAt, "plain")
	structured.Content.Parts = append(structured.Content.Parts, model.Part{Kind: model.PartStructured,
		Structured: &model.Extension{Name: "conformance.card", Version: 1, Data: []byte(`{"text":"card hidden"}`)}})
	structured.Metadata = []model.Extension{{Name: "conformance.mail", Version: 1, Data: []byte(`{"subject":"subject hidden"}`)}}
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, structured, "") })
	for _, tc := range []struct {
		text string
		want []model.Message
	}{
		{"invoice", []model.Message{mail}},
		{"INVOICE 42", []model.Message{mail}},
		{"com bar foo", []model.Message{mail}},
		{"foo@bar.com", []model.Message{mail}},
		{"invoic", nil},
		{"re open", []model.Message{release}},
		{"v2.1", []model.Message{release}},
		{"2.1", nil},
		{"été PARIS", []model.Message{summer}},
		{"ete", nil},
		{"invoice release", nil},
		{"plain", []model.Message{structured}},
		{"card hidden subject", nil},
	} {
		wantSearch(t, "text "+tc.text, env.messages(model.Filter{Text: tc.text}, 1), tc.want...)
	}

	edited := mail.Clone()
	edited.Content.Parts = []model.Part{{Kind: model.PartText, Text: "Credit note"}}
	edited.Revision = "m2"
	env.put(func(tx store.Tx) error { return tx.PutMessage(env.ctx, edited, mail.Revision) })
	wantSearch(t, "old text after edit", env.messages(model.Filter{Text: "invoice"}, 1))
	wantSearch(t, "new text after edit", env.messages(model.Filter{Text: "credit"}, 1), mail)

	for name, text := range map[string]string{ // #nosec G101 -- search inputs, not credentials
		"no token":    "!!! ...",
		"long token":  strings.Repeat("a", model.MaxSearchTokenBytes+1),
		"long text":   strings.Repeat("a ", model.MaxSearchTextBytes/2+1),
		"many tokens": "a b c d e f g h i j k l m n o p q",
	} {
		searchRefused(t, env, name, model.Filter{Text: text})
	}
	// Sixteen distinct tokens is the limit, repeats do not count.
	wantSearch(t, "token limit", env.messages(model.Filter{Text: "a b c d e f g h i j k l m n o p a"}, 1))
}

// searchDateBounds: Since and Before bound OccurredAt as the half-open
// range [Since, Before), exact to the nanosecond and whatever the time's
// location; an empty or inverted window is refused.
func searchDateBounds(t *testing.T, env *serviceEnv) {
	before := env.searchMessage(model.Public, searchAt.Add(-time.Nanosecond), "refund")
	at := env.searchMessage(model.Public, searchAt, "refund")
	after := env.searchMessage(model.Public, searchAt.Add(time.Nanosecond), "refund")
	other := env.searchMessage(model.Public, searchAt, "thanks")
	next := searchAt.Add(time.Nanosecond)
	zone := time.FixedZone("plus2", 2*60*60)
	local := searchAt.In(zone)
	for _, tc := range []struct {
		name string
		f    model.Filter
		want []model.Message
	}{
		{"since inclusive", model.Filter{Text: "refund", Since: &searchAt}, []model.Message{at, after}},
		{"before exclusive", model.Filter{Text: "refund", Before: &searchAt}, []model.Message{before}},
		{"one nanosecond window", model.Filter{Text: "refund", Since: &searchAt, Before: &next}, []model.Message{at}},
		{"adjacent windows", model.Filter{Text: "refund", Since: &next}, []model.Message{after}},
		{"location is irrelevant", model.Filter{Text: "refund", Since: &local, Before: &next}, []model.Message{at}},
		{"dates alone", model.Filter{Since: &searchAt, Before: &next}, []model.Message{at, other}},
	} {
		for _, limit := range []int{1, 50} {
			wantSearch(t, tc.name, env.messages(tc.f, limit), tc.want...)
		}
	}
	searchRefused(t, env, "empty window", model.Filter{Since: &searchAt, Before: &searchAt})
	searchRefused(t, env, "inverted window", model.Filter{Since: &next, Before: &searchAt})
}

// searchAttachmentPredicates: Has judges the message's attachment parts;
// MediaTypes the attachment records, parameters stripped and case folded,
// by exact type or type/* wildcard. A record that is redacted or missing
// never matches a media type, and invalid predicates are refused.
func searchAttachmentPredicates(t *testing.T, env *serviceEnv) {
	png := env.searchAttachment("Image/PNG; name=a")
	pdf := env.searchAttachment("application/pdf")
	gone := env.searchAttachment("image/png")
	env.put(func(tx store.Tx) error {
		return tx.PutAttachment(env.ctx, store.AttachmentRecord{
			Attachment: model.Attachment{Ref: gone, Revision: "a2"}, State: store.AttachmentRedacted,
		}, "a1")
	})
	missing := model.AttachmentRef{Inbox: env.inbox, ID: env.id()}
	withPNG := env.searchMessage(model.Public, searchAt, "a", png)
	withPDF := env.searchMessage(model.Public, searchAt, "b", pdf)
	withBoth := env.searchMessage(model.Public, searchAt, "c", png, pdf)
	redacted := env.searchMessage(model.Public, searchAt, "d", gone)
	dangling := env.searchMessage(model.Public, searchAt, "e", missing)
	plain := env.searchMessage(model.Public, searchAt, "f")
	yes, no := true, false
	types := func(mt ...string) *model.AttachmentFilter { return &model.AttachmentFilter{MediaTypes: mt} }
	for _, tc := range []struct {
		name string
		a    *model.AttachmentFilter
		want []model.Message
	}{
		{"has", &model.AttachmentFilter{Has: &yes}, []model.Message{withPNG, withPDF, withBoth, redacted, dangling}},
		{"has none", &model.AttachmentFilter{Has: &no}, []model.Message{plain}},
		{"exact type", types("image/png"), []model.Message{withPNG, withBoth}},
		{"wildcard", types("image/*"), []model.Message{withPNG, withBoth}},
		{"either type", types("application/pdf", "image/png"), []model.Message{withPNG, withPDF, withBoth}},
		{"other subtype", types("image/jpeg"), nil},
		{"has and type", &model.AttachmentFilter{Has: &yes, MediaTypes: []string{"application/pdf"}}, []model.Message{withPDF, withBoth}},
	} {
		for _, limit := range []int{1, 50} {
			wantSearch(t, tc.name, env.messages(model.Filter{Attachments: tc.a}, limit), tc.want...)
		}
	}
	wantSearch(t, "type and text", env.messages(model.Filter{Text: "c", Attachments: types("image/*")}, 1), withBoth)
	searchRefused(t, env, "empty predicate", model.Filter{Attachments: &model.AttachmentFilter{}})
	searchRefused(t, env, "types without attachments", model.Filter{Attachments: &model.AttachmentFilter{Has: &no, MediaTypes: []string{"image/png"}}})
	searchRefused(t, env, "uppercase pattern", model.Filter{Attachments: types("Image/PNG")})
	searchRefused(t, env, "too many types", model.Filter{Attachments: types(manyMediaTypes()...)})
}

func manyMediaTypes() []string {
	out := make([]string, 0, model.MaxSearchMediaTypes+1)
	for i := range model.MaxSearchMediaTypes + 1 {
		out = append(out, "image/t"+strings.Repeat("x", i+1))
	}
	return out
}

// searchNotesAndRedaction: internal notes are indexed but reach only a
// caller who may read them, with their text, attachments and labels alike;
// a member's search over a note reads as a search matching nothing; a
// redacted note matches nothing for anyone afterwards.
func searchNotesAndRedaction(t *testing.T, env *serviceEnv) {
	vip := env.searchLabelled("vip")
	png := env.searchAttachment("image/png")
	public := env.searchMessage(model.Public, searchAt, "escalation raised")
	note := env.searchMessage(model.Internal, searchAt, "escalation approved", png)
	yes := true
	filters := map[string]model.Filter{
		"text":  {Text: "approved"},
		"has":   {Attachments: &model.AttachmentFilter{Has: &yes}},
		"media": {Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}},
		"label": {Labels: &model.LabelSelector{Any: []model.LabelRef{vip}}, Text: "approved"},
	}
	for name, f := range filters {
		if p := env.searchPage(f, 10, ""); len(p.Items) != 0 || !p.Complete || p.Next != "" {
			t.Fatalf("member %s page = %+v; want empty, complete, without Next", name, p)
		}
	}
	wantSearch(t, "member", env.messages(model.Filter{Text: "escalation"}, 1), public)
	env.policy.setStaff(true)
	for name, f := range filters {
		wantSearch(t, "staff "+name, env.messages(f, 1), note)
	}
	if _, err := env.client.Privacy().Redact(env.ctx, model.Redact{Command: env.cmd(note.Revision), Target: note.Ref.Ref(), Message: &note.Ref}); err != nil {
		t.Fatalf("Redact: %v", err)
	}
	for name, f := range filters {
		wantSearch(t, "staff after redaction "+name, env.messages(f, 1))
	}
	wantSearch(t, "staff public", env.messages(model.Filter{Text: "escalation"}, 1), public)
}
