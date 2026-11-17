package memory

import (
	"slices"

	"hop.top/uma/model"
	"hop.top/uma/store/internal/search"
)

// messageSearch is the support Messages declares: the message search
// fields are evaluated here, under the shared rules of
// store/internal/search.
var messageSearch = filterSupport{inboxes: true, conversation: true, labels: true, task: true, text: true, since: true, before: true, attachments: true}

// searchMatch returns the predicate of f's message search fields. Each
// call reads the unit's state, so the unit sees its own writes; media
// types come from the attachment records at read time.
func (h *handle) searchMatch(f model.Filter) func(model.Message) bool {
	var query, exact, types []string
	if f.Text != "" {
		query = model.SearchTokens(f.Text)
	}
	if f.Attachments != nil {
		exact, types = search.Patterns(f.Attachments.MediaTypes)
	}
	return func(m model.Message) bool {
		if f.Labels != nil {
			// The conversation's labels now: nothing is copied onto messages.
			conv, ok := h.st.conversations.get(m.Ref.Conversation)
			var held []model.LabelRef
			if ok {
				held = conv.v.Labels
			}
			if !matchLabels(f, held) {
				return false
			}
		}
		if len(query) > 0 {
			tokens := search.Tokens(m)
			for _, q := range query {
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
		a := f.Attachments
		if a == nil {
			return true
		}
		held := search.Attachments(h.tenant, m)
		if a.Has != nil && *a.Has != (len(held) > 0) {
			return false
		}
		return len(a.MediaTypes) == 0 || slices.ContainsFunc(held, func(ref model.AttachmentRef) bool {
			rec, ok := h.st.attachments.get(ref)
			return ok && search.MatchMediaType(search.MediaType(rec.v), exact, types)
		})
	}
}
