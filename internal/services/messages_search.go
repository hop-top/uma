package services

import (
	"context"
	"slices"
	"strings"

	"hop.top/uma/internal/attachments"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Message search admission.
//
// The store narrows a message list by the search fields (store.Reader
// Messages); the service never trusts that narrowing alone. searchWithin
// re-checks what the message record carries: Text against its text-part
// tokens (none once redacted or deleted), Since and Before on OccurredAt,
// Has on its attachment parts. Labels live on the conversation, so
// labelsHeld matches the selector again against its current record and
// requires conversation.read on it. Media types live on attachment records,
// so mediaReadable matches them again against the current records, and
// admits a message only through a matching attachment the principal may
// read (attachments.ReadableThrough): a list never confirms the media type
// of an attachment its caller could not open.

// searching reports whether f sets a message search field.
func searching(f model.Filter) bool {
	return f.Text != "" || f.Since != nil || f.Before != nil || f.Attachments != nil || f.Labels != nil
}

// searchWithin reports whether m satisfies f's message search fields that
// the message record carries.
func searchWithin(f model.Filter, m model.Message) bool {
	if f.Text != "" {
		var tokens []string
		if !m.Redacted && !m.Deleted {
			tokens = m.Content.SearchTokens()
		}
		for _, q := range model.SearchTokens(f.Text) {
			if !slices.Contains(tokens, q) {
				return false
			}
		}
	}
	if f.Since != nil && m.OccurredAt.Before(*f.Since) {
		return false
	}
	if f.Before != nil && !m.OccurredAt.Before(*f.Before) {
		return false
	}
	if a := f.Attachments; a != nil && a.Has != nil && *a.Has != (len(heldAttachments(m)) > 0) {
		return false
	}
	return true
}

// heldAttachments returns the distinct attachments m's parts name in m's
// tenant, the attachments search judges.
func heldAttachments(m model.Message) []model.AttachmentRef {
	var out []model.AttachmentRef
	for _, p := range m.Content.Parts {
		if a := p.Attachment; a != nil && a.Inbox.Tenant == m.Ref.Conversation.Inbox.Tenant && !slices.Contains(out, *a) {
			out = append(out, *a)
		}
	}
	return out
}

// labelsHeld admits m for a filter with a label selector when the principal
// may read m's conversation (conversation.read) and the conversation
// currently holds labels the selector accepts; otherwise m is skipped
// without trace (model.NotFound or model.Unauthorized). Without a selector
// it admits m unchecked. A principal who may read a message but not its
// conversation therefore learns nothing of the conversation's labels: its
// label-filtered list reads as one that matched nothing.
func labelsHeld(ctx context.Context, c *engine.Client, sel *model.LabelSelector, m model.Message) error {
	if sel == nil {
		return nil
	}
	var conv model.Conversation
	if err := view(ctx, c, func(r store.Reader) (err error) {
		conv, err = r.GetConversation(ctx, m.Ref.Conversation)
		return err
	}); err != nil {
		return err
	}
	if err := c.Authorize(ctx, engine.ConversationRead(conv), conv.Ref.Ref()); err != nil {
		return err
	}
	if !hasLabels(model.Filter{Labels: sel}, conv.Labels) {
		return engine.ErrNotFound()
	}
	return nil
}

// mediaReadable admits m for a filter with media types when one of its
// attachments matches them in its current record and is readable through
// m; otherwise m is model.Unauthorized, skipped without trace. Without
// media types it admits m unchecked.
func mediaReadable(ctx context.Context, c *engine.Client, a *model.AttachmentFilter, m model.Message) error {
	if a == nil || len(a.MediaTypes) == 0 {
		return nil
	}
	for _, ref := range heldAttachments(m) {
		var rec store.AttachmentRecord
		err := view(ctx, c, func(r store.Reader) (err error) {
			rec, err = r.Attachment(ctx, ref)
			return err
		})
		switch {
		case model.IsCode(err, model.NotFound):
			continue
		case err != nil:
			return err
		}
		if rec.Attachment.Ref != ref || !matchMediaType(rec, a.MediaTypes) {
			continue
		}
		ok, err := attachments.ReadableThrough(ctx, c, rec, m)
		if err != nil || ok {
			return err
		}
	}
	return model.NewError(model.Unauthorized, "denied by policy")
}

// matchMediaType reports whether rec's media type matches one validated
// pattern, under the store's rule (store.Reader.Messages): parameters
// stripped, ASCII spaces and tabs trimmed, ASCII letters lowercased; a
// redacted record has none and never matches.
func matchMediaType(rec store.AttachmentRecord, patterns []string) bool {
	if rec.State == store.AttachmentRedacted {
		return false
	}
	mt, _, _ := strings.Cut(rec.Attachment.Metadata.MediaType, ";")
	mt = asciiLower(strings.Trim(mt, " \t"))
	if mt == "" {
		return false
	}
	return slices.ContainsFunc(patterns, func(p string) bool {
		if typ, ok := strings.CutSuffix(p, "/*"); ok {
			return strings.HasPrefix(mt, typ+"/")
		}
		return mt == p
	})
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
