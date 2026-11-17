package conformance

import (
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// searchCases pin Reader.Messages evaluating the message search fields in
// storage: text tokens, OccurredAt bounds and attachment predicates, with
// the same results and cursors on every backend. Other reads refuse the
// fields with model.Validation rather than returning unfiltered records,
// except Since on Events, which compares Event.RecordedAt.
var searchCases = []storeCase{
	{"SearchFieldsRefusedOutsideMessages", func(t *testing.T, e *env) {
		t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		yes := true
		filters := map[string]model.Filter{
			"text":        {Text: "invoice"},
			"since":       {Since: &t0},
			"before":      {Before: &t0},
			"attachments": {Attachments: &model.AttachmentFilter{Has: &yes}},
		}
		reads := map[string]func(store.Reader, model.Filter) error{
			"channels":      func(r store.Reader, f model.Filter) error { return listErr(r.Channels(e.ctx, listOf(f))) },
			"inboxes":       func(r store.Reader, f model.Filter) error { return listErr(r.Inboxes(e.ctx, listOf(f))) },
			"contacts":      func(r store.Reader, f model.Filter) error { return listErr(r.Contacts(e.ctx, listOf(f))) },
			"identities":    func(r store.Reader, f model.Filter) error { return listErr(r.Identities(e.ctx, listOf(f))) },
			"conversations": func(r store.Reader, f model.Filter) error { return listErr(r.Conversations(e.ctx, listOf(f))) },
			"work items":    func(r store.Reader, f model.Filter) error { return listErr(r.WorkItems(e.ctx, listOf(f))) },
			"tasks":         func(r store.Reader, f model.Filter) error { return listErr(r.Tasks(e.ctx, listOf(f))) },
			"events":        func(r store.Reader, f model.Filter) error { return listErr(r.Events(e.ctx, f, "", 1)) },
		}
		e.mustView(t, func(r store.Reader) error {
			for name, read := range reads {
				for field, f := range filters {
					if name == "events" && field == "since" {
						continue // Events honours Since, on Event.RecordedAt.
					}
					t.Run(name+" "+field, func(t *testing.T) { wantCode(t, read(r, f), model.Validation) })
				}
			}
			return nil
		})
	}},
	{"MessageSearchRejectsInvalidFields", func(t *testing.T, e *env) {
		t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
		no := false
		for name, f := range map[string]model.Filter{
			"no token":        {Text: "!!! ..."},
			"long token":      {Text: strings.Repeat("a", model.MaxSearchTokenBytes+1)},
			"empty window":    {Since: &t0, Before: &t0},
			"empty predicate": {Attachments: &model.AttachmentFilter{}},
			"types without":   {Attachments: &model.AttachmentFilter{Has: &no, MediaTypes: []string{"image/png"}}},
			"bad media type":  {Attachments: &model.AttachmentFilter{MediaTypes: []string{"Image/PNG"}}},
		} {
			t.Run(name, func(t *testing.T) {
				e.mustView(t, func(r store.Reader) error {
					wantCode(t, listErr(r.Messages(e.ctx, listOf(f))), model.Validation)
					return nil
				})
			})
		}
	}},
	{"MessageTextMatch", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		other := e.putConversation(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		mail := e.putSearchMessage(t, conv, at, textParts("Invoice #42 for foo@bar.com")...)
		release := e.putSearchMessage(t, conv, at, textParts("Release v2.1 re-open")...)
		summer := e.putSearchMessage(t, conv, at, textParts("ÉTÉ à Paris")...)
		decomposed := e.putSearchMessage(t, conv, at, textParts("été decomposed")...)
		greek := e.putSearchMessage(t, conv, at, textParts("Σίσυφος")...)
		kelvin := e.putSearchMessage(t, conv, at, textParts("Kelvin scale")...)
		cjk := e.putSearchMessage(t, conv, at, textParts("東京駅 2026")...)
		split := e.putSearchMessage(t, other, at, textParts("alpha", "beta")...)
		long := e.putSearchMessage(t, conv, at, textParts(strings.Repeat("x", model.MaxSearchTokenBytes+1)+" tail")...)
		// Structured parts and metadata are never searched.
		e.putSearchMessage(t, conv, at, model.Part{Kind: model.PartStructured,
			Structured: &model.Extension{Name: "conformance.card", Version: 1, Data: []byte(`{"text":"invoice hidden"}`)}})
		e.mustTx(t, func(tx store.Tx) error {
			m := e.searchMessage(model.MessageRef{Conversation: conv, ID: e.id(t)}, at, textParts("plain")...)
			m.Metadata = []model.Extension{{Name: "conformance.mail", Version: 1, Data: []byte(`{"subject":"invoice hidden"}`)}}
			return tx.PutMessage(e.ctx, m, "")
		})
		for _, c := range []struct {
			text string
			want []model.MessageRef
		}{
			{"invoice", msgs(mail)},
			{"INVOICE 42", msgs(mail)},
			{"foo@bar.com", msgs(mail)},
			{"com bar foo", msgs(mail)},
			{"invoic", nil},
			{"v2", msgs(release)},
			{"v2.1", msgs(release)},
			{"2.1", nil},
			{"re open", msgs(release)},
			{"été", msgs(summer)},
			{"Été PARIS à", msgs(summer)},
			{"ete", nil},
			{"été", msgs(decomposed)},
			{"ΣΊΣΥΦΟΣ", msgs(greek)},
			{"σίσυφοσ", msgs(greek)},
			{"kelvin", msgs(kelvin)},
			{"東京駅", msgs(cjk)},
			{"東京", nil},
			{"2026", msgs(cjk)},
			{"alpha beta", msgs(split)},
			{"alpha gamma", nil},
			{strings.Repeat("x", model.MaxSearchTokenBytes), nil},
			{"tail", msgs(long)},
			{"hidden", nil},
		} {
			t.Run(c.text, func(t *testing.T) { e.wantSearch(t, model.Filter{Text: c.text}, c.want...) })
		}
		t.Run("narrowed by conversation", func(t *testing.T) {
			e.wantSearch(t, model.Filter{Text: "alpha", Conversation: &conv})
			e.wantSearch(t, model.Filter{Text: "alpha", Conversation: &other}, split)
			e.wantSearch(t, model.Filter{Text: "alpha", Inboxes: []model.InboxRef{e.inbox}}, split)
		})
	}},
	{"MessageTextPagesInRecordingOrder", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		var want []model.MessageRef
		for i := range 7 {
			text := "noise"
			if i%2 == 0 {
				text = "Quarterly report"
			}
			ref := e.putSearchMessage(t, conv, at.Add(-time.Duration(i)*time.Hour), textParts(text)...)
			if i%2 == 0 {
				want = append(want, ref)
			}
		}
		e.wantSearch(t, model.Filter{Text: "report quarterly"}, want...)
	}},
	{"MessageDateBounds", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		paris := time.FixedZone("Paris", 2*60*60)
		since := time.Date(2026, 3, 1, 12, 0, 0, 500, time.UTC)
		before := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
		old := time.Date(1969, 12, 31, 23, 59, 59, 999_999_999, time.UTC)
		stamps := map[string]time.Time{
			"pre-epoch":      old,
			"just before":    since.Add(-time.Nanosecond),
			"since":          since,
			"since in Paris": since.In(paris),
			"just after":     since.Add(time.Nanosecond),
			"inside":         since.Add(time.Hour),
			"last inside":    before.Add(-time.Nanosecond),
			"before":         before,
			"after":          before.Add(time.Second),
		}
		names := []string{"pre-epoch", "just before", "since", "since in Paris", "just after", "inside", "last inside", "before", "after"}
		got := map[string]model.MessageRef{}
		for _, n := range names {
			got[n] = e.putSearchMessage(t, conv, stamps[n], textParts("dated")...)
		}
		pick := func(ns ...string) []model.MessageRef {
			out := make([]model.MessageRef, len(ns))
			for i, n := range ns {
				out[i] = got[n]
			}
			return out
		}
		sinceParis, beforeParis := since.In(paris), before.In(paris)
		oldEnd := old.Add(time.Nanosecond)
		e.wantSearch(t, model.Filter{Since: &since}, pick("since", "since in Paris", "just after", "inside", "last inside", "before", "after")...)
		e.wantSearch(t, model.Filter{Before: &before}, pick("pre-epoch", "just before", "since", "since in Paris", "just after", "inside", "last inside")...)
		e.wantSearch(t, model.Filter{Since: &sinceParis, Before: &beforeParis}, pick("since", "since in Paris", "just after", "inside", "last inside")...)
		e.wantSearch(t, model.Filter{Since: &old, Before: &oldEnd}, pick("pre-epoch")...)
		e.wantSearch(t, model.Filter{Since: &since, Before: &before, Text: "dated", Conversation: &conv},
			pick("since", "since in Paris", "just after", "inside", "last inside")...)
	}},
	{"MessageDateBoundsFollowRewrite", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		ref := e.putSearchMessage(t, conv, t0, textParts("moved")...)
		later := t0.Add(48 * time.Hour)
		e.mustTx(t, func(tx store.Tx) error {
			m := e.searchMessage(ref, later, textParts("moved")...)
			m.Revision = "r2"
			return tx.PutMessage(e.ctx, m, "r1")
		})
		end := t0.Add(time.Hour)
		e.wantSearch(t, model.Filter{Since: &t0, Before: &end})
		e.wantSearch(t, model.Filter{Since: &later}, ref)
	}},
}

// textParts returns one text part per text.
func textParts(texts ...string) []model.Part {
	out := make([]model.Part, len(texts))
	for i, s := range texts {
		out[i] = model.Part{Kind: model.PartText, Text: s}
	}
	return out
}

func msgs(ms ...model.MessageRef) []model.MessageRef { return ms }

// searchMessage is a revision r1 message in ref occurring at at with parts.
func (e *env) searchMessage(ref model.MessageRef, at time.Time, parts ...model.Part) model.Message {
	m := e.message(ref, "r1")
	m.Content = model.Content{Parts: parts}
	m.OccurredAt = at
	return m
}

// putSearchMessage commits a new message in conv occurring at at with parts.
func (e *env) putSearchMessage(t *testing.T, conv model.ConversationRef, at time.Time, parts ...model.Part) model.MessageRef {
	t.Helper()
	ref := model.MessageRef{Conversation: conv, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutMessage(e.ctx, e.searchMessage(ref, at, parts...), "") })
	return ref
}

// wantSearch requires Messages under f to return want in recording order,
// both in one page and paged one message at a time through the cursor.
func (e *env) wantSearch(t *testing.T, f model.Filter, want ...model.MessageRef) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		wantSearchIn(t, e, r, f, want...)
		return nil
	})
}

func wantSearchIn(t *testing.T, e *env, r store.Reader, f model.Filter, want ...model.MessageRef) {
	t.Helper()
	ids := make([]model.ID, len(want))
	for i, m := range want {
		ids[i] = m.ID
	}
	whole, err := r.Messages(e.ctx, model.ListRequest{Filter: f, Page: page(100)})
	if err != nil {
		t.Fatalf("Messages(%s): %v", brief(f), err)
	}
	if got := messageIDs(whole.Items); !whole.Complete || !slices.Equal(got, ids) {
		t.Errorf("Messages(%s) = %v (complete %v), want %v", brief(f), got, whole.Complete, ids)
	}
	var paged []model.ID
	req := model.ListRequest{Filter: f, Page: page(1)}
	for range len(want) + 2 {
		p, err := r.Messages(e.ctx, req)
		if err != nil {
			t.Fatalf("Messages(%s) page after %q: %v", brief(f), req.Page.After, err)
		}
		paged = append(paged, messageIDs(p.Items)...)
		if p.Complete {
			break
		}
		req.Page.After = p.Next
	}
	if !slices.Equal(paged, ids) {
		t.Errorf("Messages(%s) paged = %v, want %v", brief(f), paged, ids)
	}
}

func listOf(f model.Filter) model.ListRequest { return model.ListRequest{Filter: f, Page: page(1)} }

func listErr[T any](_ model.Page[T], err error) error { return err }
