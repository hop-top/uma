package memory

import (
	"context"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// tx implements store.Tx on a unit's private state.
type tx struct{ reader }

// write locks the handle for a mutation and marks the unit dirty. On success
// the caller must call h.mu.Unlock.
func (t tx) write(ctx context.Context) (*handle, error) {
	if err := t.h.enter(ctx); err != nil {
		return nil, err
	}
	t.h.st.dirty = true
	return t.h, nil
}

// put is the shared compare-and-set write path for revisioned records.
func put[K comparable, V any](h *handle, tb *table[K, V], key K, v V, revOf func(V) model.Revision, expected model.Revision) error {
	rev := revOf(v)
	if err := newRevision(rev, expected); err != nil {
		return err
	}
	cur, found := tb.get(key)
	if err := cas(found, revOf(cur.v), expected); err != nil {
		return err
	}
	seq := cur.seq
	if !found {
		seq = h.st.next()
	}
	tb.set(key, entry[V]{seq: seq, v: v})
	return nil
}

// checkMessageLinks requires a message's task and parent, when set, to be
// in its conversation.
func checkMessageLinks(v model.Message) error {
	if v.Task != nil && !v.Task.In(v.Ref.Conversation) {
		return invalid("task: must belong to the message's conversation")
	}
	if v.InReplyTo != nil && !v.InReplyTo.In(v.Ref.Conversation) {
		return invalid("in_reply_to: must belong to the message's conversation")
	}
	return nil
}

func (h *handle) requireInbox(in model.InboxRef) error {
	if _, ok := h.st.inboxes.get(in); !ok {
		return notFound()
	}
	return nil
}

func (h *handle) requireConversation(c model.ConversationRef) error {
	if _, ok := h.st.conversations.get(c); !ok {
		return notFound()
	}
	return nil
}

func (h *handle) requireChannel(c model.ChannelRef) error {
	if _, ok := h.st.channels.get(c); !ok {
		return notFound()
	}
	return nil
}

func (t tx) PutChannel(ctx context.Context, v model.Channel, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref), keyFree(&h.st.channels, v.Ref, v.Key, func(c model.Channel) (model.ChannelRef, string) { return c.Ref, c.Key })); err != nil {
		return err
	}
	return put(h, &h.st.channels, v.Ref, v.Clone(), channelRev, expected)
}

func (t tx) PutInbox(ctx context.Context, v model.Inbox, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref, v.Channel), h.requireChannel(v.Channel),
		keyFree(&h.st.inboxes, v.Ref, v.Key, func(in model.Inbox) (model.InboxRef, string) { return in.Ref, in.Key })); err != nil {
		return err
	}
	return put(h, &h.st.inboxes, v.Ref, v.Clone(), inboxRev, expected)
}

// keyFree refuses a non-empty key another record of t holds.
func keyFree[K comparable, V any](t *table[K, V], ref K, key string, of func(V) (K, string)) error {
	if key == "" {
		return nil
	}
	held := t.sorted(func(v V) bool {
		r, k := of(v)
		return k == key && r != ref
	})
	if len(held) > 0 {
		return conflict("key already names another record")
	}
	return nil
}

func (t tx) PutContact(ctx context.Context, v model.Contact, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := h.scoped(v.Ref); err != nil {
		return err
	}
	return put(h, &h.st.contacts, v.Ref, v.Clone(), contactRev, expected)
}

func (h *handle) checkConversation(v model.Conversation) error {
	if err := firstErr(h.scoped(v.Ref), h.requireInbox(v.Ref.Inbox), h.checkLabels(v)); err != nil {
		return err
	}
	if v.Owner != nil {
		if err := h.scoped(v.Owner); err != nil {
			return err
		}
	}
	if !v.ReplyRoute.IsZero() {
		if err := h.scoped(v.ReplyRoute); err != nil {
			return err
		}
	}
	return nil
}

func (t tx) PutConversation(ctx context.Context, v model.Conversation, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := h.checkConversation(v); err != nil {
		return err
	}
	return put(h, &h.st.conversations, v.Ref, v.Clone(), conversationRev, expected)
}

func (t tx) PutMessage(ctx context.Context, v model.Message, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref), h.requireConversation(v.Ref.Conversation), checkMessageLinks(v)); err != nil {
		return err
	}
	return put(h, &h.st.messages, v.Ref, v.Clone(), messageRev, expected)
}

func (t tx) PutWorkItem(ctx context.Context, v model.WorkItem, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref), h.requireConversation(v.Ref.Conversation)); err != nil {
		return err
	}
	if _, found := h.st.workItems.get(v.Ref); !found {
		bound := h.st.workItems.sorted(func(w model.WorkItem) bool { return w.Ref.Conversation == v.Ref.Conversation })
		if len(bound) > 0 {
			return conflict("conversation already holds a work item")
		}
	}
	return put(h, &h.st.workItems, v.Ref, v.Clone(), workItemRev, expected)
}

func (t tx) PutStream(ctx context.Context, v model.Stream, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Ref), h.requireConversation(v.Ref.Conversation)); err != nil {
		return err
	}
	return put(h, &h.st.streams, v.Ref, v.Clone(), streamRev, expected)
}

func (t tx) PutAttachment(ctx context.Context, v store.AttachmentRecord, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	ref := v.Attachment.Ref
	if err := firstErr(h.scoped(ref), h.requireInbox(ref.Inbox)); err != nil {
		return err
	}
	if v.Blob != nil {
		if err := h.sameTenant(v.Blob.Tenant); err != nil {
			return err
		}
	}
	if v.Provider != nil {
		if err := h.scoped(v.Provider); err != nil {
			return err
		}
	}
	if !v.StagingOperation.IsZero() {
		if err := h.scoped(v.StagingOperation); err != nil {
			return err
		}
	}
	return put(h, &h.st.attachments, ref, v.Clone(), attachmentRev, expected)
}

func (t tx) PutExport(ctx context.Context, v store.ExportRecord, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	ref := v.Export.Ref
	if err := firstErr(h.scoped(ref), h.checkKey(v.Blob)); err != nil {
		return err
	}
	if !v.Operation.IsZero() {
		if err := h.scoped(v.Operation); err != nil {
			return err
		}
	}
	rev := func(r store.ExportRecord) model.Revision { return r.Export.Revision }
	cur, found := h.st.exports.get(ref)
	if err := firstErr(newRevision(v.Export.Revision, expected), cas(found, rev(cur.v), expected)); err != nil {
		return err
	}
	seq, at := cur.seq, cur.at
	if !found {
		seq, at = h.st.next(), h.now()
	}
	h.st.exports.set(ref, entry[store.ExportRecord]{seq: seq, at: at, v: v.Clone()})
	return nil
}

func (t tx) PutProtocol(ctx context.Context, v store.ProtocolState, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if err := firstErr(h.scoped(v.Channel), required("key", v.Key), h.requireChannel(v.Channel)); err != nil {
		return err
	}
	return put(h, &h.st.protocol, protocolKey{v.Channel, v.Key}, v.Clone(), protocolRev, expected)
}

func (t tx) PutSubscription(ctx context.Context, v store.SubscriptionRecord, expected model.Revision) error {
	h, err := t.write(ctx)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	ref := v.Subscription.Ref
	if err := firstErr(
		h.scoped(ref),
		h.sameTenant(v.Principal.Tenant),
		required("consumer", v.Consumer),
		h.checkFilter(v.Filter, filterSupport{inboxes: true, kinds: true}),
		h.checkPosition(v.Subscription.Checkpoint),
	); err != nil {
		return err
	}
	return put(h, &h.st.subscriptions, ref, v.Clone(), subscriptionRev, expected)
}

// checkPosition requires c to be a valid position in the event log.
func (h *handle) checkPosition(c model.Cursor) error {
	pos, err := decodeCursor(c)
	if err != nil {
		return err
	}
	if pos > h.st.eventHead() {
		return invalid("cursor: beyond the end of the log")
	}
	return nil
}

// Revision accessors for put.
func channelRev(v model.Channel) model.Revision                 { return v.Revision }
func inboxRev(v model.Inbox) model.Revision                     { return v.Revision }
func contactRev(v model.Contact) model.Revision                 { return v.Revision }
func conversationRev(v model.Conversation) model.Revision       { return v.Revision }
func messageRev(v model.Message) model.Revision                 { return v.Revision }
func workItemRev(v model.WorkItem) model.Revision               { return v.Revision }
func taskRev(v model.Task) model.Revision                       { return v.Revision }
func streamRev(v model.Stream) model.Revision                   { return v.Revision }
func protocolRev(v store.ProtocolState) model.Revision          { return v.Revision }
func attachmentRev(v store.AttachmentRecord) model.Revision     { return v.Attachment.Revision }
func subscriptionRev(v store.SubscriptionRecord) model.Revision { return v.Subscription.Revision }
