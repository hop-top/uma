package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/internal/tombstone"
	"hop.top/uma/store/postgres/internal/db"
)

// txn implements store.Tx on a unit's transaction.
type txn struct{ reader }

// put is the shared compare-and-set write path for revisioned records: read
// the stored revision, compare it with expected, then insert or update. The
// update repeats the comparison in SQL, so a row changed since the read
// cannot be overwritten.
func (h *handle) put(ctx context.Context, next, expected model.Revision,
	current func() (string, error), insert func() error, update func() (int64, error),
) error {
	if err := newRevision(next, expected); err != nil {
		return err
	}
	rev, err := current()
	found := true
	switch {
	case missing(err):
		found = false
	case err != nil:
		return h.db(ctx, err)
	}
	if err := cas(found, model.Revision(rev), expected); err != nil {
		return err
	}
	if !found {
		return h.db(ctx, insert())
	}
	n, err := update()
	if err != nil {
		return h.db(ctx, err)
	}
	if n != 1 {
		return conflict("stale expected revision")
	}
	return nil
}

// putNew is put with an empty expected revision for a record whose ID is
// its only key: insert inserts it unless the ID is held and reports the
// rows it inserted. A held ID is model.Conflict and leaves the unit
// usable, as put's compare. No lookup precedes the insert: a read of the
// ID index would leave a predicate lock on the page new, time-ordered IDs
// share, and concurrent writers would fail one another on it.
func (h *handle) putNew(ctx context.Context, next model.Revision, insert func() (int64, error)) error {
	if err := newRevision(next, ""); err != nil {
		return err
	}
	n, err := insert()
	switch {
	case err != nil:
		return h.db(ctx, err)
	case n == 0:
		return cas(true, "", "")
	}
	return nil
}

// exists runs an existence query; false is not_found.
func (h *handle) exists(ctx context.Context, ok bool, err error) error {
	if err != nil {
		return h.db(ctx, err)
	}
	if !ok {
		return notFound()
	}
	return nil
}

func (h *handle) requireChannel(ctx context.Context, c model.ChannelRef) error {
	ok, err := h.q.ChannelExists(ctx, db.ChannelExistsParams{TenantID: h.tenantID, ID: string(c.ID)})
	return h.exists(ctx, ok, err)
}

func (h *handle) requireInbox(ctx context.Context, in model.InboxRef) error {
	ok, err := h.q.InboxExists(ctx, db.InboxExistsParams{TenantID: h.tenantID, ID: string(in.ID)})
	return h.exists(ctx, ok, err)
}

func (h *handle) requireConversation(ctx context.Context, c model.ConversationRef) error {
	ok, err := h.q.ConversationExists(ctx, db.ConversationExistsParams{
		TenantID: h.tenantID, InboxID: string(c.Inbox.ID), ID: string(c.ID),
	})
	return h.exists(ctx, ok, err)
}

func (t txn) PutChannel(ctx context.Context, v model.Channel, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(v.Ref); err != nil {
			return err
		}
		holder, err := h.keyHolder(ctx, v.Key, func() (string, error) {
			row, err := h.q.ResolveChannelKey(ctx, db.ResolveChannelKeyParams{TenantID: h.tenantID, Key: v.Key})
			return row.ID, err
		})
		if err != nil {
			return err
		}
		if holder != "" && holder != string(v.Ref.ID) {
			return conflict("key already names another channel")
		}
		cfg := v.Configuration
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetChannel(ctx, db.GetChannelParams{TenantID: h.tenantID, ID: string(v.Ref.ID)})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertChannel(ctx, db.InsertChannelParams{
					TenantID: h.tenantID, ID: string(v.Ref.ID), Key: v.Key, TypeKey: string(v.Type),
					ConfigName: cfg.Name, ConfigVersion: int64(cfg.Version), ConfigData: extData(cfg.Data),
					CredentialRef: v.CredentialRef, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateChannel(ctx, db.UpdateChannelParams{
					TenantID: h.tenantID, ID: string(v.Ref.ID), Key: v.Key, TypeKey: string(v.Type),
					ConfigName: cfg.Name, ConfigVersion: int64(cfg.Version), ConfigData: extData(cfg.Data),
					CredentialRef: v.CredentialRef, Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

func (t txn) PutInbox(ctx context.Context, v model.Inbox, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(v.Ref, v.Channel); err != nil {
			return err
		}
		if err := h.requireChannel(ctx, v.Channel); err != nil {
			return err
		}
		holder, err := h.keyHolder(ctx, v.Key, func() (string, error) {
			row, err := h.q.ResolveInboxKey(ctx, db.ResolveInboxKeyParams{TenantID: h.tenantID, Key: v.Key})
			return row.ID, err
		})
		if err != nil {
			return err
		}
		if holder != "" && holder != string(v.Ref.ID) {
			return conflict("key already names another inbox")
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		dests := strs(c.Destinations)
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetInbox(ctx, db.GetInboxParams{TenantID: h.tenantID, ID: string(v.Ref.ID)})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertInbox(ctx, db.InsertInboxParams{
					TenantID: h.tenantID, ID: string(v.Ref.ID), ChannelID: string(v.Channel.ID), Key: v.Key,
					Destinations: dests, Disabled: c.Disabled, Body: body, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateInbox(ctx, db.UpdateInboxParams{
					TenantID: h.tenantID, ID: string(v.Ref.ID), ChannelID: string(v.Channel.ID), Key: v.Key,
					Destinations: dests, Disabled: c.Disabled, Body: body, Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

func (t txn) PutContact(ctx context.Context, v model.Contact, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(v.Ref); err != nil {
			return err
		}
		body, err := encode(&v)
		if err != nil {
			return err
		}
		insert := func() (int64, error) {
			return h.q.InsertContact(ctx, db.InsertContactParams{
				TenantID: h.tenantID, ID: string(v.Ref.ID), Kind: v.Kind, Body: body, Revision: string(v.Revision),
			})
		}
		if expected == "" {
			// A new contact, such as the provisional contact of a sender
			// claimed in the unit, is inserted with no prior lookup, as a
			// new message.
			return h.putNew(ctx, v.Revision, insert)
		}
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetContact(ctx, db.GetContactParams{TenantID: h.tenantID, ID: string(v.Ref.ID)})
				return row.Revision, err
			},
			nil, // never called: a record missing under a set expected revision fails the compare
			func() (int64, error) {
				return h.q.UpdateContact(ctx, db.UpdateContactParams{
					TenantID: h.tenantID, ID: string(v.Ref.ID), Kind: v.Kind, Body: body,
					Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

// createdOrder splits a conversation's CreatedAt into the exact Unix
// seconds and nanoseconds the candidate order compares.
func createdOrder(v model.Conversation) (int64, int32) {
	return v.CreatedAt.Unix(), int32(v.CreatedAt.Nanosecond()) // #nosec G115 -- Nanosecond is below 1e9
}

// checkConversation validates a conversation's scope and labels and
// requires its inbox and labels.
func (h *handle) checkConversation(ctx context.Context, v model.Conversation) error {
	if err := h.scoped(v.Ref); err != nil {
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
	if err := h.requireInbox(ctx, v.Ref.Inbox); err != nil {
		return err
	}
	return h.checkLabels(ctx, v)
}

func (t txn) PutConversation(ctx context.Context, v model.Conversation, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.checkConversation(ctx, v); err != nil {
			return err
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		ref := v.Ref
		unix, nanos := createdOrder(v)
		wf := workflowColumnsOf(c)
		err = h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetConversation(ctx, db.GetConversationParams{
					TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
				})
				return row.Revision, err
			},
			func() error {
				n, err := h.q.InsertConversation(ctx, db.InsertConversationParams{
					TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
					Mode: v.Mode, Status: v.Status, CreatedUnix: unix, CreatedNanos: nanos, Body: body, Revision: string(v.Revision),
					Assignee: wf.assignee, SnoozeUntilUnix: wf.untilUnix, SnoozeUntilNanos: wf.untilNanos,
					SnoozeAtUnix: wf.atUnix, SnoozeAtNanos: wf.atNanos, LinkCount: wf.links,
				})
				if err == nil && n == 0 {
					// Unreachable: the read found the ID free in the unit's
					// snapshot, and one a concurrent unit committed since
					// fails the insert as a serialization failure.
					return cas(true, "", "")
				}
				return err
			},
			func() (int64, error) {
				n, err := h.q.UpdateConversation(ctx, db.UpdateConversationParams{
					TenantID: h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
					Mode: v.Mode, Status: v.Status, CreatedUnix: unix, CreatedNanos: nanos,
					Body: body, Revision: string(v.Revision), Expected: string(expected),
					Assignee: wf.assignee, SnoozeUntilUnix: wf.untilUnix, SnoozeUntilNanos: wf.untilNanos,
					SnoozeAtUnix: wf.atUnix, SnoozeAtNanos: wf.atNanos, LinkCount: wf.links,
				})
				if err != nil || n != 1 {
					return n, err
				}
				// Only an existing conversation can hold tasks.
				return n, h.syncTaskOwner(ctx, v)
			})
		if err != nil {
			return err
		}
		return h.putConversationLabels(ctx, string(ref.ID), c.Labels)
	})
}

func (t txn) PutMessage(ctx context.Context, v model.Message, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error { return h.putMessage(ctx, v, expected) })
}

// putMessage is PutMessage with h entered.
func (h *handle) putMessage(ctx context.Context, v model.Message, expected model.Revision) error {
	if err := h.checkMessage(ctx, v); err != nil {
		return err
	}
	row, err := h.messageRow(v)
	if err != nil {
		return err
	}
	if expected == "" {
		if err := newRevision(v.Revision, ""); err != nil {
			return err
		}
		inserted, err := h.insertNewMessage(ctx, row)
		if err != nil || inserted {
			return err
		}
		return h.heldMessage(ctx, row.p)
	}
	p := row.p
	err = h.put(ctx, v.Revision, expected,
		func() (string, error) {
			row, err := h.q.GetMessage(ctx, db.GetMessageParams{TenantID: h.tenantID, InboxID: p.InboxID, ConversationID: p.ConversationID, ID: p.ID})
			return row.Revision, err
		},
		nil, // never called: a record missing under a set expected revision fails the compare
		func() (int64, error) {
			return h.q.UpdateMessage(ctx, db.UpdateMessageParams{
				TenantID: h.tenantID, InboxID: p.InboxID, ConversationID: p.ConversationID, ID: p.ID, TaskID: p.TaskID, InReplyToID: p.InReplyToID,
				Body: p.Body, Revision: p.Revision, Expected: string(expected),
				SearchTokens: p.SearchTokens, OccurredUnix: p.OccurredUnix, OccurredNanos: p.OccurredNanos,
				RecordedUnix: p.RecordedUnix, RecordedNanos: p.RecordedNanos,
			})
		})
	if err != nil {
		return err
	}
	if err := h.putMessageRefs(ctx, p.InboxID, p.ID, row.v.External); err != nil {
		return err
	}
	return h.putMessageAttachments(ctx, p.ID, tombstone.Attachments(row.v))
}

// messageRow is a message as its row stores it: v is the stored copy.
type messageRow struct {
	p db.InsertMessageParams
	v model.Message
}

// messageRow encodes a checked message as its row.
func (h *handle) messageRow(v model.Message) (messageRow, error) {
	task, parent, err := messageLinks(v)
	if err != nil {
		return messageRow{}, err
	}
	conv := v.Ref.Conversation
	c := v.Clone()
	body, err := encode(&c)
	if err != nil {
		return messageRow{}, err
	}
	sc := searchColumnsOf(c)
	return messageRow{v: c, p: db.InsertMessageParams{
		TenantID: h.tenantID, InboxID: string(conv.Inbox.ID), ConversationID: string(conv.ID), ID: string(v.Ref.ID),
		TaskID: task, InReplyToID: parent, Body: body, Revision: string(v.Revision),
		SearchTokens: sc.tokens, OccurredUnix: sc.unix, OccurredNanos: sc.nanos,
		RecordedUnix: sc.recUnix, RecordedNanos: sc.recNanos,
	}}, nil
}

// insertNewMessage inserts a new message with its reference and attachment
// rows, as put with an empty expected revision, and reports whether it
// did: nothing is inserted when the tenant holds its ID (heldMessage
// answers then). The insert comes first: a read of the message's key would
// leave a predicate lock on the index page new, time-ordered IDs share,
// and concurrent writers of new messages would fail one another on it.
func (h *handle) insertNewMessage(ctx context.Context, row messageRow) (bool, error) {
	n, err := h.q.InsertMessage(ctx, row.p)
	if err != nil || n == 0 {
		return false, h.db(ctx, err)
	}
	// A new message: no reference or attachment row names it yet, since
	// each references the message.
	if err := h.insertMessageRefs(ctx, row.p.InboxID, row.p.ID, row.v.External); err != nil {
		return false, err
	}
	return true, h.insertMessageAttachments(ctx, row.p.ID, tombstone.Attachments(row.v))
}

// heldMessage answers a new message whose ID the tenant holds: under the
// same reference it is model.Conflict and leaves the unit usable; held by a
// message of another conversation it is the unique violation its insert
// would raise.
func (h *handle) heldMessage(ctx context.Context, p db.InsertMessageParams) error {
	_, err := h.q.GetMessage(ctx, db.GetMessageParams{TenantID: h.tenantID, InboxID: p.InboxID, ConversationID: p.ConversationID, ID: p.ID})
	switch {
	case err == nil:
		return cas(true, "", "")
	case !missing(err):
		return h.db(ctx, err)
	}
	return h.db(ctx, &pgconn.PgError{
		Code: "23505", Message: "duplicate key value violates unique constraint", TableName: "uma_messages", ConstraintName: "uma_messages_pkey",
	})
}

// checkMessage scopes a message and requires its conversation.
func (h *handle) checkMessage(ctx context.Context, v model.Message) error {
	if err := h.scoped(v.Ref); err != nil {
		return err
	}
	if v.Task != nil {
		if err := h.scoped(v.Task); err != nil {
			return err
		}
	}
	return h.requireConversation(ctx, v.Ref.Conversation)
}

// putMessageAttachments replaces the attachment rows of a stored message
// with atts, the lookup index of Reader.MessagesByAttachment. Another
// tenant's attachments are not indexed: no lookup in this tenant names
// them.
func (h *handle) putMessageAttachments(ctx context.Context, id string, atts []model.AttachmentRef) error {
	if err := h.q.DeleteMessageAttachments(ctx, db.DeleteMessageAttachmentsParams{TenantID: h.tenantID, MessageID: id}); err != nil {
		return h.db(ctx, err)
	}
	return h.insertMessageAttachments(ctx, id, atts)
}

// insertMessageAttachments adds the attachment rows of a message that has
// none.
func (h *handle) insertMessageAttachments(ctx context.Context, id string, atts []model.AttachmentRef) error {
	p := db.InsertMessageAttachmentsParams{TenantID: h.tenantID, MessageID: id}
	for _, a := range atts {
		if a.Inbox.Tenant == h.tenant {
			p.InboxIds = append(p.InboxIds, string(a.Inbox.ID))
			p.AttachmentIds = append(p.AttachmentIds, string(a.ID))
		}
	}
	if len(p.AttachmentIds) == 0 {
		return nil
	}
	return h.db(ctx, h.q.InsertMessageAttachments(ctx, p))
}

// putMessageRefs replaces the reference rows of a stored message with refs,
// the lookup index of Reader.MessagesByReference.
func (h *handle) putMessageRefs(ctx context.Context, inbox, id string, refs []model.ExternalRef) error {
	if err := h.q.DeleteMessageRefs(ctx, db.DeleteMessageRefsParams{TenantID: h.tenantID, MessageID: id}); err != nil {
		return h.db(ctx, err)
	}
	return h.insertMessageRefs(ctx, inbox, id, refs)
}

// insertMessageRefs adds the reference rows of a message that has none.
func (h *handle) insertMessageRefs(ctx context.Context, inbox, id string, refs []model.ExternalRef) error {
	if len(refs) == 0 {
		return nil
	}
	p := db.InsertMessageRefsParams{TenantID: h.tenantID, InboxID: inbox, MessageID: id}
	for _, r := range refs {
		if r.Channel.ID.Validate() != nil {
			return invalid("message external: channel ID must be an internal ID")
		}
		p.ChannelTenants = append(p.ChannelTenants, string(r.Channel.Tenant))
		p.ChannelIds = append(p.ChannelIds, string(r.Channel.ID))
		p.Namespaces = append(p.Namespaces, r.Namespace)
		p.Kinds = append(p.Kinds, r.Kind)
		p.ExtIds = append(p.ExtIds, r.ID)
		p.ParentIds = append(p.ParentIds, r.ParentID)
	}
	return h.db(ctx, h.q.InsertMessageRefs(ctx, p))
}

func (t txn) PutWorkItem(ctx context.Context, v model.WorkItem, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(v.Ref); err != nil {
			return err
		}
		conv := v.Ref.Conversation
		if err := h.requireConversation(ctx, conv); err != nil {
			return err
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		inbox, cid, id := string(conv.Inbox.ID), string(conv.ID), string(v.Ref.ID)
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetWorkItem(ctx, db.GetWorkItemParams{TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertWorkItem(ctx, db.InsertWorkItemParams{
					TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id, Status: v.Status,
					Body: body, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateWorkItem(ctx, db.UpdateWorkItemParams{
					TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id, Status: v.Status,
					Body: body, Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

func (t txn) PutStream(ctx context.Context, v model.Stream, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(v.Ref); err != nil {
			return err
		}
		conv := v.Ref.Conversation
		if err := h.requireConversation(ctx, conv); err != nil {
			return err
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		inbox, cid, id := string(conv.Inbox.ID), string(conv.ID), string(v.Ref.ID)
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetStream(ctx, db.GetStreamParams{TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertStream(ctx, db.InsertStreamParams{
					TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id, Body: body, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateStream(ctx, db.UpdateStreamParams{
					TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id, Body: body,
					Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

func (t txn) PutAttachment(ctx context.Context, v store.AttachmentRecord, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		ref := v.Attachment.Ref
		if err := h.scoped(ref); err != nil {
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
		if err := h.requireInbox(ctx, ref.Inbox); err != nil {
			return err
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		rev := v.Attachment.Revision
		inbox, id := string(ref.Inbox.ID), string(ref.ID)
		return h.put(ctx, rev, expected,
			func() (string, error) {
				row, err := h.q.GetAttachment(ctx, db.GetAttachmentParams{TenantID: h.tenantID, InboxID: inbox, ID: id})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertAttachment(ctx, db.InsertAttachmentParams{
					TenantID: h.tenantID, InboxID: inbox, ID: id, Body: body, Revision: string(rev),
				})
			},
			func() (int64, error) {
				return h.q.UpdateAttachment(ctx, db.UpdateAttachmentParams{
					TenantID: h.tenantID, InboxID: inbox, ID: id, Body: body, Revision: string(rev), Expected: string(expected),
				})
			})
	})
}

func (t txn) PutProtocol(ctx context.Context, v store.ProtocolState, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := firstErr(h.scoped(v.Channel), required("key", v.Key)); err != nil {
			return err
		}
		if err := h.requireChannel(ctx, v.Channel); err != nil {
			return err
		}
		val, ch := v.Value, string(v.Channel.ID)
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetProtocol(ctx, db.GetProtocolParams{TenantID: h.tenantID, ChannelID: ch, Key: v.Key})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertProtocol(ctx, db.InsertProtocolParams{
					TenantID: h.tenantID, ChannelID: ch, Key: v.Key, ValueName: val.Name,
					ValueVersion: int64(val.Version), ValueData: extData(val.Data), Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateProtocol(ctx, db.UpdateProtocolParams{
					TenantID: h.tenantID, ChannelID: ch, Key: v.Key, ValueName: val.Name,
					ValueVersion: int64(val.Version), ValueData: extData(val.Data),
					Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

func (t txn) PutSubscription(ctx context.Context, v store.SubscriptionRecord, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		ref := v.Subscription.Ref
		if err := firstErr(
			h.scoped(ref),
			h.sameTenant(v.Principal.Tenant),
			required("consumer", v.Consumer),
			h.checkFilter(v.Filter, filterSupport{inboxes: true, kinds: true}),
		); err != nil {
			return err
		}
		if err := h.checkPosition(ctx, v.Subscription.Checkpoint); err != nil {
			return err
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		rev := v.Subscription.Revision
		return h.put(ctx, rev, expected,
			func() (string, error) {
				row, err := h.q.GetSubscription(ctx, db.GetSubscriptionParams{TenantID: h.tenantID, ID: string(ref.ID)})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertSubscription(ctx, db.InsertSubscriptionParams{
					TenantID: h.tenantID, ID: string(ref.ID), Consumer: v.Consumer, Body: body, Revision: string(rev),
				})
			},
			func() (int64, error) {
				return h.q.UpdateSubscription(ctx, db.UpdateSubscriptionParams{
					TenantID: h.tenantID, ID: string(ref.ID), Consumer: v.Consumer, Body: body,
					Revision: string(rev), Expected: string(expected),
				})
			})
	})
}

// checkPosition requires c to be a valid position in the event log.
func (h *handle) checkPosition(ctx context.Context, c model.Cursor) error {
	pos, err := decodeCursor(c)
	if err != nil {
		return err
	}
	head, err := h.q.EventHead(ctx, h.tenantID)
	if err != nil {
		return h.db(ctx, err)
	}
	if pos > head {
		return invalid("cursor: beyond the end of the log")
	}
	return nil
}
