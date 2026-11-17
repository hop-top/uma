package memory

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// reader implements store.Reader only, so a View handle cannot be asserted
// to a Tx.
type reader struct{ h *handle }

// lookup is the shared Get path: validate and scope the ref, then read.
func lookup[K comparable, V interface {
	model.Scoped
	Validate() error
}, R any](ctx context.Context, h *handle, t *table[K, R], key K, ref V, clone func(R) R) (R, error) {
	var zero R
	if err := h.enter(ctx); err != nil {
		return zero, err
	}
	defer h.mu.Unlock()
	if err := h.scoped(ref); err != nil {
		return zero, err
	}
	e, ok := t.get(key)
	if !ok {
		return zero, notFound()
	}
	return clone(e.v), nil
}

func (r reader) GetChannel(ctx context.Context, ref model.ChannelRef) (model.Channel, error) {
	return lookup(ctx, r.h, &r.h.st.channels, ref, ref, model.Channel.Clone)
}

func (r reader) GetInbox(ctx context.Context, ref model.InboxRef) (model.Inbox, error) {
	return lookup(ctx, r.h, &r.h.st.inboxes, ref, ref, model.Inbox.Clone)
}

func (r reader) GetContact(ctx context.Context, ref model.ContactRef) (model.Contact, error) {
	return lookup(ctx, r.h, &r.h.st.contacts, ref, ref, model.Contact.Clone)
}

func (r reader) GetConversation(ctx context.Context, ref model.ConversationRef) (model.Conversation, error) {
	return lookup(ctx, r.h, &r.h.st.conversations, ref, ref, model.Conversation.Clone)
}

func (r reader) GetMessage(ctx context.Context, ref model.MessageRef) (model.Message, error) {
	return lookup(ctx, r.h, &r.h.st.messages, ref, ref, model.Message.Clone)
}

func (r reader) GetWorkItem(ctx context.Context, ref model.WorkItemRef) (model.WorkItem, error) {
	return lookup(ctx, r.h, &r.h.st.workItems, ref, ref, model.WorkItem.Clone)
}

func (r reader) GetTask(ctx context.Context, ref model.TaskRef) (model.Task, error) {
	return lookup(ctx, r.h, &r.h.st.tasks, ref, ref, model.Task.Clone)
}

func (r reader) GetIdentity(ctx context.Context, ref model.IdentityRef) (model.Identity, error) {
	return lookup(ctx, r.h, &r.h.st.identities, ref, ref, model.Identity.Clone)
}

func (r reader) Protocol(ctx context.Context, ch model.ChannelRef, key string) (store.ProtocolState, error) {
	if err := required("key", key); err != nil {
		return store.ProtocolState{}, err
	}
	return lookup(ctx, r.h, &r.h.st.protocol, protocolKey{ch, key}, ch, store.ProtocolState.Clone)
}

func (r reader) Operation(ctx context.Context, ref model.OperationRef) (store.OperationRecord, error) {
	if err := r.h.enter(ctx); err != nil {
		return store.OperationRecord{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(ref); err != nil {
		return store.OperationRecord{}, err
	}
	e, ok := r.h.st.operations.get(ref)
	if !ok {
		return store.OperationRecord{}, notFound()
	}
	return shownOperation(e), nil
}

func (r reader) OperationByKey(ctx context.Context, k store.OperationKey) (store.OperationRecord, error) {
	if err := r.h.enter(ctx); err != nil {
		return store.OperationRecord{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.checkOperationKey(k); err != nil {
		return store.OperationRecord{}, err
	}
	ref, ok := r.h.st.operationKeys.get(k)
	if !ok {
		return store.OperationRecord{}, notFound()
	}
	e, ok := r.h.st.operations.get(ref.v)
	if !ok {
		return store.OperationRecord{}, notFound()
	}
	return shownOperation(e), nil
}

func (r reader) Stream(ctx context.Context, ref model.StreamRef) (model.Stream, error) {
	return lookup(ctx, r.h, &r.h.st.streams, ref, ref, model.Stream.Clone)
}

func (r reader) Identity(ctx context.Context, key model.IdentityKey) (model.Identity, error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Identity{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(key); err != nil {
		return model.Identity{}, err
	}
	ref, ok := r.h.st.identityKeys.get(key)
	if !ok {
		return model.Identity{}, notFound()
	}
	e, ok := r.h.st.identities.get(ref.v)
	if !ok {
		return model.Identity{}, notFound()
	}
	return e.v.Clone(), nil
}

func (r reader) Subscription(ctx context.Context, ref model.SubscriptionRef) (store.SubscriptionRecord, error) {
	return lookup(ctx, r.h, &r.h.st.subscriptions, ref, ref, store.SubscriptionRecord.Clone)
}

func (r reader) Export(ctx context.Context, ref model.ExportRef) (store.ExportRecord, error) {
	return lookup(ctx, r.h, &r.h.st.exports, ref, ref, store.ExportRecord.Clone)
}

func (r reader) Attachment(ctx context.Context, ref model.AttachmentRef) (store.AttachmentRecord, error) {
	return lookup(ctx, r.h, &r.h.st.attachments, ref, ref, store.AttachmentRecord.Clone)
}

func (r reader) Ingress(ctx context.Context, id model.ID) (store.IngressRecord, error) {
	if err := r.h.enter(ctx); err != nil {
		return store.IngressRecord{}, err
	}
	defer r.h.mu.Unlock()
	if err := id.Validate(); err != nil {
		return store.IngressRecord{}, err
	}
	e, ok := r.h.st.ingress.get(id)
	if !ok {
		return store.IngressRecord{}, notFound()
	}
	return e.v.Clone(), nil
}

// listTable is the shared list path over a table in insertion order.
func listTable[K comparable, V any](ctx context.Context, h *handle, t *table[K, V], req model.ListRequest,
	ok filterSupport, match func(model.Filter, V) bool, clone func(V) V,
) (model.Page[V], error) {
	if err := h.enter(ctx); err != nil {
		return model.Page[V]{}, err
	}
	defer h.mu.Unlock()
	limit, err := pageLimit(req.Page.Limit)
	if err != nil {
		return model.Page[V]{}, err
	}
	after, err := decodeCursor(req.Page.After)
	if err != nil {
		return model.Page[V]{}, err
	}
	if err := h.checkFilter(req.Filter, ok); err != nil {
		return model.Page[V]{}, err
	}
	rows := t.sorted(func(v V) bool { return match(req.Filter, v) })
	start, _ := slices.BinarySearchFunc(rows, after+1, func(e entry[V], seq uint64) int {
		return compareSeq(e.seq, seq)
	})
	rows = rows[start:]
	p := model.Page[V]{Complete: len(rows) <= limit, Next: req.Page.After}
	rows = rows[:min(limit, len(rows))]
	for _, e := range rows {
		p.Items = append(p.Items, clone(e.v))
	}
	if len(rows) > 0 {
		p.Next = encodeCursor(rows[len(rows)-1].seq)
	}
	return p, nil
}

func compareSeq(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func same[T any](v T) T { return v }

func inInboxes(f model.Filter, in model.InboxRef) bool { return matchAny(f.Inboxes, in) }

func inConversation(f model.Filter, c model.ConversationRef) bool {
	return f.Conversation == nil || *f.Conversation == c
}

func (r reader) Channels(ctx context.Context, req model.ListRequest) (model.Page[model.Channel], error) {
	return listTable(ctx, r.h, &r.h.st.channels, req, filterSupport{},
		func(model.Filter, model.Channel) bool { return true }, model.Channel.Clone)
}

func (r reader) Inboxes(ctx context.Context, req model.ListRequest) (model.Page[model.Inbox], error) {
	return listTable(ctx, r.h, &r.h.st.inboxes, req, filterSupport{inboxes: true},
		func(f model.Filter, v model.Inbox) bool { return inInboxes(f, v.Ref) }, model.Inbox.Clone)
}

func (r reader) Contacts(ctx context.Context, req model.ListRequest) (model.Page[model.Contact], error) {
	return listTable(ctx, r.h, &r.h.st.contacts, req, filterSupport{kinds: true},
		func(f model.Filter, v model.Contact) bool { return matchAny(f.Kinds, v.Kind) }, model.Contact.Clone)
}

func (r reader) Identities(ctx context.Context, req model.ListRequest) (model.Page[model.Identity], error) {
	return listTable(ctx, r.h, &r.h.st.identities, req, filterSupport{inboxes: true, contact: true},
		func(f model.Filter, v model.Identity) bool {
			return inInboxes(f, v.Ref.Inbox) && (f.Contact == nil || *f.Contact == v.Contact)
		}, model.Identity.Clone)
}

func (r reader) Conversations(ctx context.Context, req model.ListRequest) (model.Page[model.Conversation], error) {
	workflow := r.h.workflowMatch(r.h.now())
	return listTable(ctx, r.h, &r.h.st.conversations, req, conversationReads,
		func(f model.Filter, v model.Conversation) bool {
			return inInboxes(f, v.Ref.Inbox) && matchAny(f.Statuses, v.Status) && matchLabels(f, v.Labels) &&
				workflow(f, v)
		}, model.Conversation.Clone)
}

func (r reader) Messages(ctx context.Context, req model.ListRequest) (model.Page[model.Message], error) {
	match := r.h.searchMatch(req.Filter)
	return listTable(ctx, r.h, &r.h.st.messages, req, messageSearch,
		func(f model.Filter, v model.Message) bool {
			return inInboxes(f, v.Ref.Conversation.Inbox) && inConversation(f, v.Ref.Conversation) &&
				(f.Task == nil || v.Task != nil && *v.Task == *f.Task) && match(v)
		}, model.Message.Clone)
}

func (r reader) WorkItems(ctx context.Context, req model.ListRequest) (model.Page[model.WorkItem], error) {
	return listTable(ctx, r.h, &r.h.st.workItems, req, filterSupport{inboxes: true, conversation: true, statuses: true},
		func(f model.Filter, v model.WorkItem) bool {
			return inInboxes(f, v.Ref.Conversation.Inbox) && inConversation(f, v.Ref.Conversation) &&
				matchAny(f.Statuses, v.Status)
		}, model.WorkItem.Clone)
}

// listLog is the shared paging path over an append-only sequence whose
// first base entries were removed; the cursor counts entries scanned, the
// removed ones included, and must not precede base.
func listLog[V any](items []V, base uint64, after model.Cursor, limit int, keep func(int, V) bool, clone func(V) V) (model.Page[V], error) {
	limit, err := pageLimit(limit)
	if err != nil {
		return model.Page[V]{}, err
	}
	pos, err := decodeCursor(after)
	if err != nil {
		return model.Page[V]{}, err
	}
	if pos > base+uint64(len(items)) {
		return model.Page[V]{}, invalid("cursor: beyond the end of the log")
	}
	p := model.Page[V]{Complete: true}
	i := int(pos - base) // #nosec G115 -- bounded by len(items) above
	for ; i < len(items); i++ {
		if !keep(i, items[i]) {
			continue
		}
		if len(p.Items) == limit {
			p.Complete = false
			break
		}
		p.Items = append(p.Items, clone(items[i]))
	}
	p.Next = encodeCursor(base + uint64(i)) // #nosec G115 -- i is non-negative
	return p, nil
}

func (r reader) Attempts(ctx context.Context, op model.OperationRef, pr model.PageRequest) (model.Page[store.AttemptEvidence], error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Page[store.AttemptEvidence]{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(op); err != nil {
		return model.Page[store.AttemptEvidence]{}, err
	}
	if _, ok := r.h.st.operations.get(op); !ok {
		return model.Page[store.AttemptEvidence]{}, notFound()
	}
	e, _ := r.h.st.attempts.get(op)
	return listAttempts(e.v, pr)
}

// listAttempts pages one operation's evidence; the cursor is the position
// of the last entry returned, so it stays valid when Purge removes entries.
func listAttempts(items []attempt, pr model.PageRequest) (model.Page[store.AttemptEvidence], error) {
	limit, err := pageLimit(pr.Limit)
	if err != nil {
		return model.Page[store.AttemptEvidence]{}, err
	}
	after, err := decodeCursor(pr.After)
	if err != nil {
		return model.Page[store.AttemptEvidence]{}, err
	}
	p := model.Page[store.AttemptEvidence]{Complete: true, Next: pr.After}
	for _, a := range items {
		if a.pos <= after {
			continue
		}
		if len(p.Items) == limit {
			p.Complete = false
			break
		}
		p.Items = append(p.Items, a.ev.Clone())
		p.Next = encodeCursor(a.pos)
	}
	return p, nil
}

func (r reader) Events(ctx context.Context, f model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Page[model.Event]{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, kinds: true, since: true}); err != nil {
		return model.Page[model.Event]{}, err
	}
	inboxes := make([]model.ID, len(f.Inboxes))
	for i, in := range f.Inboxes {
		inboxes[i] = in.ID
	}
	base := r.h.st.eventBase
	pos, err := decodeCursor(after)
	if err == nil && pos < base {
		return model.Page[model.Event]{}, errCursorExpired()
	}
	meta := r.h.st.eventMeta.items
	start := after
	if f.Since != nil && err == nil && pos <= r.h.st.eventHead() {
		start = encodeCursor(max(pos, base+uint64(sinceStart(meta, *f.Since)))) // #nosec G115 -- an index, never negative
	}
	return listLog(r.h.st.events.items, base, start, limit, func(i int, e model.Event) bool {
		return matchAny(inboxes, eventInbox(e)) && matchAny(f.Kinds, e.Kind) &&
			(f.Conversation == nil || e.Inbox == f.Conversation.Inbox.ID && meta[i].conversation == f.Conversation.ID) &&
			(f.Since == nil || !e.RecordedAt.Before(*f.Since))
	}, model.Event.Clone)
}

// sinceStart returns the index of the first retained event whose
// recordedMax reaches since: no event before it was recorded at or after
// since. It is len(meta) when none does.
func sinceStart(meta []eventMeta, since time.Time) int {
	return sort.Search(len(meta), func(i int) bool { return !meta[i].recordedMax.Before(since) })
}

// eventInbox is the inbox an Inboxes filter matches e on: the inbox it is
// scoped to, or the inbox itself for an event about an inbox resource.
func eventInbox(e model.Event) model.ID {
	if e.Resource.Kind == model.KindInbox {
		return e.Resource.ID
	}
	return e.Inbox
}

func (r reader) EventHead(ctx context.Context) (model.Cursor, error) {
	if err := r.h.enter(ctx); err != nil {
		return "", err
	}
	defer r.h.mu.Unlock()
	return encodeCursor(r.h.st.eventHead()), nil
}

func (r reader) IdentityLinks(ctx context.Context, f store.LinkFilter, pr model.PageRequest) (model.Page[store.LinkRecord], error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Page[store.LinkRecord]{}, err
	}
	defer r.h.mu.Unlock()
	if f.Identity == nil && f.Contact == nil {
		return model.Page[store.LinkRecord]{}, invalid("filter: identity or contact required")
	}
	if f.Identity != nil {
		if err := r.h.scoped(f.Identity); err != nil {
			return model.Page[store.LinkRecord]{}, err
		}
	}
	if f.Contact != nil {
		if err := r.h.scoped(f.Contact); err != nil {
			return model.Page[store.LinkRecord]{}, err
		}
	}
	return listLog(r.h.st.links.items, 0, pr.After, pr.Limit, func(_ int, l store.LinkRecord) bool {
		return (f.Identity == nil || l.Identity == *f.Identity) &&
			(f.Contact == nil || l.From == *f.Contact || l.To == *f.Contact)
	}, store.LinkRecord.Clone)
}

func (r reader) Candidates(ctx context.Context, key store.SelectionKey, limit int) ([]model.Conversation, error) {
	if err := r.h.enter(ctx); err != nil {
		return nil, err
	}
	defer r.h.mu.Unlock()
	if err := firstErr(r.h.scoped(key.Inbox), required("key", key.Key)); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, invalid("limit: must be positive")
	}
	g, _ := r.h.st.convGroups.get(key)
	var out []model.Conversation
	var newestClosed *model.Conversation
	for _, ref := range g.v {
		e, ok := r.h.st.conversations.get(ref)
		switch {
		case !ok:
		case e.v.Status != model.ConversationClosed:
			out = append(out, e.v)
		case newestClosed == nil || compareCandidates(e.v, *newestClosed) > 0:
			newestClosed = &e.v
		}
	}
	if newestClosed != nil {
		out = append(out, *newestClosed)
	}
	if len(out) > limit {
		return nil, conflict("candidate set exceeds limit")
	}
	for i := range out {
		out[i] = out[i].Clone()
	}
	slices.SortFunc(out, compareCandidates)
	return out, nil
}

// compareCandidates orders conversations by CreatedAt, then ID.
func compareCandidates(a, b model.Conversation) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(string(a.Ref.ID), string(b.Ref.ID))
}

func (r reader) ResolveDestination(ctx context.Context, ch model.ChannelRef, dest string) (model.Inbox, error) {
	if err := r.h.enter(ctx); err != nil {
		return model.Inbox{}, err
	}
	defer r.h.mu.Unlock()
	if err := firstErr(r.h.scoped(ch), required("destination", dest)); err != nil {
		return model.Inbox{}, err
	}
	matches := r.h.st.inboxes.sorted(func(in model.Inbox) bool {
		return in.Channel == ch && slices.Contains(in.Destinations, dest)
	})
	switch len(matches) {
	case 0:
		return model.Inbox{}, notFound()
	case 1:
		return matches[0].v.Clone(), nil
	}
	return model.Inbox{}, conflict("destination resolves to more than one inbox")
}

func (r reader) ResolveChannelKey(ctx context.Context, key string) (model.Channel, error) {
	return resolveKey(ctx, r.h, &r.h.st.channels, key, func(v model.Channel) string { return v.Key }, model.Channel.Clone)
}

func (r reader) ResolveInboxKey(ctx context.Context, key string) (model.Inbox, error) {
	return resolveKey(ctx, r.h, &r.h.st.inboxes, key, func(v model.Inbox) string { return v.Key }, model.Inbox.Clone)
}

// resolveKey returns the one record of t whose key is key.
func resolveKey[K comparable, V any](ctx context.Context, h *handle, t *table[K, V], key string, keyOf func(V) string, clone func(V) V) (V, error) {
	var zero V
	if err := h.enter(ctx); err != nil {
		return zero, err
	}
	defer h.mu.Unlock()
	if err := required("key", key); err != nil {
		return zero, err
	}
	matches := t.sorted(func(v V) bool { return keyOf(v) == key })
	if len(matches) == 0 {
		return zero, notFound()
	}
	return clone(matches[0].v), nil
}

func (r reader) Lease(ctx context.Context, res model.Ref) (store.Lease, error) {
	if err := r.h.enter(ctx); err != nil {
		return store.Lease{}, err
	}
	defer r.h.mu.Unlock()
	if err := r.h.scoped(res); err != nil {
		return store.Lease{}, err
	}
	l, live := r.h.liveLease(res)
	if !live {
		return store.Lease{}, notFound()
	}
	return l, nil
}
