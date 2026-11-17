package conformance

import (
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// searchAttachmentCases pin the attachment predicates of message search
// and that redacted, deleted and rewritten messages match by their current
// content only.
var searchAttachmentCases = []storeCase{
	{"MessageAttachmentPredicates", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		png := e.putSearchAttachment(t, "image/png")
		plain := e.putSearchAttachment(t, "\t Text/Plain ; charset=utf-8")
		pdf := e.putSearchAttachment(t, "application/pdf")
		bare := e.putSearchAttachment(t, "text")
		near := e.putSearchAttachment(t, "texts/plain")
		gone := e.putSearchAttachment(t, "image/png")
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Attachment: &gone, Expected: "a1", Revision: "a2"})
			return err
		})
		unknown := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)} // no record

		none := e.putSearchMessage(t, conv, at, textParts("no attachment")...)
		withPNG := e.putSearchMessage(t, conv, at, attachmentParts(png)...)
		withPlain := e.putSearchMessage(t, conv, at, append(textParts("notes"), attachmentParts(plain)...)...)
		withBoth := e.putSearchMessage(t, conv, at, attachmentParts(pdf, png)...)
		withBare := e.putSearchMessage(t, conv, at, attachmentParts(bare, near)...)
		withGone := e.putSearchMessage(t, conv, at, attachmentParts(gone)...)
		withUnknown := e.putSearchMessage(t, conv, at, attachmentParts(unknown)...)
		// Another tenant's attachment is never held here, as the holder
		// index never names it.
		foreign := e.putSearchMessage(t, conv, at, attachmentParts(model.AttachmentRef{
			Inbox: model.InboxRef{Tenant: tenantB, ID: e.inbox.ID}, ID: png.ID})...)

		yes, no := true, false
		for name, c := range map[string]struct {
			f    model.AttachmentFilter
			want []model.MessageRef
		}{
			"has":             {model.AttachmentFilter{Has: &yes}, msgs(withPNG, withPlain, withBoth, withBare, withGone, withUnknown)},
			"has none":        {model.AttachmentFilter{Has: &no}, msgs(none, foreign)},
			"exact":           {model.AttachmentFilter{MediaTypes: []string{"image/png"}}, msgs(withPNG, withBoth)},
			"params stripped": {model.AttachmentFilter{MediaTypes: []string{"text/plain"}}, msgs(withPlain)},
			"wildcard":        {model.AttachmentFilter{MediaTypes: []string{"text/*"}}, msgs(withPlain)},
			"any of":          {model.AttachmentFilter{Has: &yes, MediaTypes: []string{"application/pdf", "image/*"}}, msgs(withPNG, withBoth)},
			"unmatched":       {model.AttachmentFilter{MediaTypes: []string{"video/*", "image/gif"}}, nil},
		} {
			t.Run(name, func(t *testing.T) {
				f := c.f
				e.wantSearch(t, model.Filter{Attachments: &f}, c.want...)
			})
		}
		t.Run("with text", func(t *testing.T) {
			e.wantSearch(t, model.Filter{Text: "notes", Attachments: &model.AttachmentFilter{Has: &yes}}, withPlain)
			e.wantSearch(t, model.Filter{Text: "attachment", Attachments: &model.AttachmentFilter{Has: &no}}, none)
		})
		// Redacting a record after the message was written stops it
		// matching: media types are read from the record at query time.
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Attachment: &png, Expected: "a1", Revision: "a2"})
			return err
		})
		e.wantSearch(t, model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}})
		e.wantSearch(t, model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/*", "application/pdf"}}}, withBoth)
	}},
	{"RedactedMessageNeverMatches", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		png := e.putSearchAttachment(t, "image/png")
		kept := e.putSearchMessage(t, conv, at, append(textParts("secret invoice"), attachmentParts(png)...)...)
		redacted := e.putSearchMessage(t, conv, at, append(textParts("secret invoice"), attachmentParts(png)...)...)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.Redact(e.ctx, store.Redaction{Message: &redacted, Expected: "r1", Revision: "r2"})
			return err
		})
		yes, no := true, false
		e.wantSearch(t, model.Filter{Text: "secret"}, kept)
		e.wantSearch(t, model.Filter{Attachments: &model.AttachmentFilter{Has: &yes}}, kept)
		e.wantSearch(t, model.Filter{Attachments: &model.AttachmentFilter{Has: &no}}, redacted)
		e.wantSearch(t, model.Filter{Attachments: &model.AttachmentFilter{MediaTypes: []string{"image/png"}}})
	}},
	{"DeletedMessageNeverMatchesText", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		ref := e.putSearchMessage(t, conv, at, textParts("removed by the provider")...)
		e.mustTx(t, func(tx store.Tx) error {
			m := e.searchMessage(ref, at, textParts("removed by the provider")...)
			m.Deleted, m.Revision = true, "r2"
			return tx.PutMessage(e.ctx, m, "r1")
		})
		e.wantSearch(t, model.Filter{Text: "removed"})
	}},
	{"MessageSearchFollowsRewrite", func(t *testing.T, e *env) {
		conv := e.putConversation(t)
		at := time.Unix(1_700_000_000, 0).UTC()
		png := e.putSearchAttachment(t, "image/png")
		ref := e.putSearchMessage(t, conv, at, append(textParts("first draft"), attachmentParts(png)...)...)
		edit := e.searchMessage(ref, at, textParts("second version")...)
		edit.Revision = "r2"
		yes := true
		hasPNG := model.Filter{Attachments: &model.AttachmentFilter{Has: &yes, MediaTypes: []string{"image/png"}}}
		// A rolled-back rewrite leaves the stored tokens.
		_ = e.txErr(func(tx store.Tx) error {
			if err := tx.PutMessage(e.ctx, edit, "r1"); err != nil {
				return err
			}
			return errAbort
		})
		e.wantSearch(t, model.Filter{Text: "draft"}, ref)
		e.wantSearch(t, hasPNG, ref)
		e.mustTx(t, func(tx store.Tx) error {
			if err := tx.PutMessage(e.ctx, edit, "r1"); err != nil {
				return err
			}
			// The unit reads its own rewrite.
			wantSearchIn(t, e, tx, model.Filter{Text: "draft"})
			wantSearchIn(t, e, tx, model.Filter{Text: "second version"}, ref)
			wantSearchIn(t, e, tx, hasPNG)
			return nil
		})
		e.wantSearch(t, model.Filter{Text: "draft"})
		e.wantSearch(t, model.Filter{Text: "version"}, ref)
		e.wantSearch(t, hasPNG)
	}},
}

// attachmentParts returns one attachment part per ref.
func attachmentParts(atts ...model.AttachmentRef) []model.Part {
	out := make([]model.Part, len(atts))
	for i := range atts {
		out[i] = model.Part{Kind: model.PartAttachment, Attachment: &atts[i]}
	}
	return out
}

// putSearchAttachment commits a revision a1 attachment record in the
// fixture inbox with mediaType.
func (e *env) putSearchAttachment(t *testing.T, mediaType string) model.AttachmentRef {
	t.Helper()
	ref := model.AttachmentRef{Inbox: e.inbox, ID: e.id(t)}
	rec := store.AttachmentRecord{
		Attachment: model.Attachment{Ref: ref, Revision: "a1", Metadata: model.AttachmentMetadata{
			Name: "file", MediaType: mediaType, Size: 1, Audience: model.Audience{Visibility: model.Public},
		}},
		State: "prepared",
	}
	e.mustTx(t, func(tx store.Tx) error { return tx.PutAttachment(e.ctx, rec, "") })
	return ref
}
