package memory

import (
	"context"
	"slices"

	"hop.top/uma/model"
)

// InboundMessages selects, among the messages of conv, the page-limit newest
// inbound ones strictly older than the cursor. The memory store keeps no
// secondary index, so it visits every message once, keeps only the page
// being built and clones only what it returns; a durable backend indexes
// the conversation's inbound messages.
func (r reader) InboundMessages(ctx context.Context, conv model.ConversationRef, page model.PageRequest) (model.Page[model.Message], error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Page[model.Message]{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(conv); err != nil {
		return model.Page[model.Message]{}, err
	}
	limit, err := pageLimit(page.Limit)
	if err != nil {
		return model.Page[model.Message]{}, err
	}
	before, err := decodeCursor(page.After)
	if err != nil {
		return model.Page[model.Message]{}, err
	}
	if page.After == "" {
		before = ^uint64(0)
	}

	// top holds the newest limit+1 candidates, newest first; the extra one
	// only tells whether the page is the last.
	top := make([]entry[model.Message], 0, min(limit, 64)+1)
	for _, e := range r.h.st.messages.m {
		if e.seq >= before || len(top) > limit && e.seq < top[limit].seq {
			continue
		}
		if m := &e.v; m.Ref.Conversation != conv || m.Origin != nil || m.EchoOf != nil || m.Redacted || m.Deleted {
			continue
		}
		at, _ := slices.BinarySearchFunc(top, e.seq, func(x entry[model.Message], seq uint64) int {
			return compareSeq(seq, x.seq) // descending
		})
		top = slices.Insert(top, at, e)
		if len(top) > limit+1 {
			top = top[:limit+1]
		}
	}
	p := model.Page[model.Message]{Complete: len(top) <= limit, Next: page.After}
	top = top[:min(limit, len(top))]
	for _, e := range top {
		p.Items = append(p.Items, e.v.Clone())
	}
	if len(top) > 0 {
		p.Next = encodeCursor(top[len(top)-1].seq)
	}
	return p, nil
}
