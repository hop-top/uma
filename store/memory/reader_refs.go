package memory

import (
	"context"
	"slices"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/store/internal/tombstone"
)

// MessagesByReference scans the inbox's messages for ref. The memory store
// keeps no secondary index; a durable backend indexes the reference.
func (r reader) MessagesByReference(ctx context.Context, in model.InboxRef, ref model.ExternalRef, limit int) ([]model.Message, error) {
	if err := r.h.enter(ctx); err != nil {
		return nil, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(in, ref); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, invalid("limit: must be positive")
	}
	rows := r.h.st.messages.sorted(func(m model.Message) bool {
		return m.Ref.Conversation.Inbox == in && slices.Contains(m.External, ref)
	})
	if len(rows) > limit {
		return nil, conflict("matching message set exceeds limit")
	}
	out := make([]model.Message, len(rows))
	for i, e := range rows {
		out[i] = e.v.Clone()
	}
	slices.SortStableFunc(out, func(a, b model.Message) int {
		if c := a.RecordedAt.Compare(b.RecordedAt); c != 0 {
			return c
		}
		return strings.Compare(string(a.Ref.ID), string(b.Ref.ID))
	})
	return out, nil
}

// MessagesByAttachment pages the messages whose parts name ref, scanning
// them in recording order like Messages.
func (r reader) MessagesByAttachment(ctx context.Context, ref model.AttachmentRef, page model.PageRequest) (model.Page[model.Message], error) {
	if err := r.h.scoped(ref); err != nil {
		return model.Page[model.Message]{}, err
	}
	return listTable(ctx, r.h, &r.h.st.messages, model.ListRequest{Page: page}, filterSupport{},
		func(_ model.Filter, m model.Message) bool { return slices.Contains(tombstone.Attachments(m), ref) },
		model.Message.Clone)
}

// StreamsOf scans the conversation's streams for those not terminal. The
// memory store keeps no secondary index.
func (r reader) StreamsOf(ctx context.Context, conv model.ConversationRef) ([]model.Stream, error) {
	if err := r.h.enter(ctx); err != nil {
		return nil, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(conv); err != nil {
		return nil, err
	}
	rows := r.h.st.streams.sorted(func(s model.Stream) bool {
		return s.Ref.Conversation == conv && nonTerminalStream(s.State)
	})
	out := make([]model.Stream, len(rows))
	for i, e := range rows {
		out[i] = e.v.Clone()
	}
	slices.SortFunc(out, func(a, b model.Stream) int { return strings.Compare(string(a.Ref.ID), string(b.Ref.ID)) })
	return out, nil
}

// nonTerminalStream reports whether a stream in state s may still change
// its provider message: opening, open or ending.
func nonTerminalStream(s string) bool {
	return s == model.StreamOpening || s == model.StreamOpen || s == model.StreamEnding
}
