package postgres

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres/internal/db"
)

// reader implements store.Reader only, so a View handle cannot be asserted
// to a Tx.
type reader struct{ h *handle }

// call runs fn with h entered for one store call.
func (h *handle) call(ctx context.Context, fn func() error) error {
	if err := h.enter(ctx, false); err != nil {
		return err
	}
	defer h.mu.Unlock()
	return fn()
}

// claimCall runs fn with h entered for one ClaimWork call.
func (h *handle) claimCall(ctx context.Context, fn func() error) error {
	if err := h.enter(ctx, true); err != nil {
		return err
	}
	defer h.mu.Unlock()
	return fn()
}

// missing reports whether err is a lookup that found no row.
func missing(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// lookupErr maps a single-row lookup error: no row is not_found and leaves
// the unit usable; anything else is a database failure.
func (h *handle) lookupErr(ctx context.Context, err error) error {
	if missing(err) {
		return notFound()
	}
	return h.db(ctx, err)
}

// getBody is the shared Get path for records stored as a JSON body: validate
// and scope the ref, fetch, decode.
func getBody[T any, R interface {
	model.Scoped
	Validate() error
}](ctx context.Context, h *handle, ref R, fetch func() ([]byte, error)) (T, error) {
	var out T
	err := h.call(ctx, func() error {
		if err := h.scoped(ref); err != nil {
			return err
		}
		b, err := fetch()
		if err != nil {
			return h.lookupErr(ctx, err)
		}
		out, err = decode[T](b)
		return err
	})
	return out, err
}

func (r reader) GetChannel(ctx context.Context, ref model.ChannelRef) (model.Channel, error) {
	var out model.Channel
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(ref); err != nil {
			return err
		}
		row, err := r.h.q.GetChannel(ctx, db.GetChannelParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out = channelOf(ref, row.Key, row.TypeKey, row.ConfigName, row.ConfigVersion, row.ConfigData, row.CredentialRef, row.Revision)
		return nil
	})
	return out, err
}

func channelOf(ref model.ChannelRef, key, typ, cfgName string, cfgVersion int64, cfgData []byte, cred, rev string) model.Channel {
	return model.Channel{
		Ref:  ref,
		Key:  key,
		Type: model.TypeKey(typ),
		Configuration: model.Extension{
			Name:    cfgName,
			Version: uint32(cfgVersion), // #nosec G115 -- column CHECK bounds it to uint32
			Data:    nonEmpty(cfgData),
		},
		CredentialRef: cred,
		Revision:      model.Revision(rev),
	}
}

// nonEmpty returns nil for an empty value read from a nullable column.
func nonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

func (r reader) GetInbox(ctx context.Context, ref model.InboxRef) (model.Inbox, error) {
	return getBody[model.Inbox](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetInbox(ctx, db.GetInboxParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		return row.Body, err
	})
}

func (r reader) GetContact(ctx context.Context, ref model.ContactRef) (model.Contact, error) {
	return getBody[model.Contact](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetContact(ctx, db.GetContactParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		return row.Body, err
	})
}

func (r reader) GetConversation(ctx context.Context, ref model.ConversationRef) (model.Conversation, error) {
	return getBody[model.Conversation](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetConversation(ctx, db.GetConversationParams{
			TenantID: r.h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) GetMessage(ctx context.Context, ref model.MessageRef) (model.Message, error) {
	return getBody[model.Message](ctx, r.h, ref, func() ([]byte, error) {
		c := ref.Conversation
		row, err := r.h.q.GetMessage(ctx, db.GetMessageParams{
			TenantID: r.h.tenantID, InboxID: string(c.Inbox.ID), ConversationID: string(c.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) GetWorkItem(ctx context.Context, ref model.WorkItemRef) (model.WorkItem, error) {
	return getBody[model.WorkItem](ctx, r.h, ref, func() ([]byte, error) {
		c := ref.Conversation
		row, err := r.h.q.GetWorkItem(ctx, db.GetWorkItemParams{
			TenantID: r.h.tenantID, InboxID: string(c.Inbox.ID), ConversationID: string(c.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) GetTask(ctx context.Context, ref model.TaskRef) (model.Task, error) {
	return getBody[model.Task](ctx, r.h, ref, func() ([]byte, error) {
		c := ref.Conversation
		row, err := r.h.q.GetTask(ctx, db.GetTaskParams{
			TenantID: r.h.tenantID, InboxID: string(c.Inbox.ID), ConversationID: string(c.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) Stream(ctx context.Context, ref model.StreamRef) (model.Stream, error) {
	return getBody[model.Stream](ctx, r.h, ref, func() ([]byte, error) {
		c := ref.Conversation
		row, err := r.h.q.GetStream(ctx, db.GetStreamParams{
			TenantID: r.h.tenantID, InboxID: string(c.Inbox.ID), ConversationID: string(c.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) Attachment(ctx context.Context, ref model.AttachmentRef) (store.AttachmentRecord, error) {
	return getBody[store.AttachmentRecord](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetAttachment(ctx, db.GetAttachmentParams{
			TenantID: r.h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) Subscription(ctx context.Context, ref model.SubscriptionRef) (store.SubscriptionRecord, error) {
	return getBody[store.SubscriptionRecord](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetSubscription(ctx, db.GetSubscriptionParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		return row.Body, err
	})
}

func (r reader) Operation(ctx context.Context, ref model.OperationRef) (store.OperationRecord, error) {
	var out store.OperationRecord
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(ref); err != nil {
			return err
		}
		row, err := r.h.q.GetOperation(ctx, db.GetOperationParams{TenantID: r.h.tenantID, ID: string(ref.ID)})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out, err = decodeOperation(row.Body, row.WrittenAt)
		return err
	})
	return out, err
}

func (r reader) Identity(ctx context.Context, key model.IdentityKey) (model.Identity, error) {
	return getBody[model.Identity](ctx, r.h, key, func() ([]byte, error) {
		row, err := r.h.q.GetIdentityByKey(ctx, db.GetIdentityByKeyParams{
			TenantID: r.h.tenantID, InboxID: string(key.Inbox.ID), Namespace: key.Namespace, SourceID: key.SourceID,
		})
		return row.Body, err
	})
}

func (r reader) GetIdentity(ctx context.Context, ref model.IdentityRef) (model.Identity, error) {
	return getBody[model.Identity](ctx, r.h, ref, func() ([]byte, error) {
		row, err := r.h.q.GetIdentity(ctx, db.GetIdentityParams{
			TenantID: r.h.tenantID, InboxID: string(ref.Inbox.ID), ID: string(ref.ID),
		})
		return row.Body, err
	})
}

func (r reader) Protocol(ctx context.Context, ch model.ChannelRef, key string) (store.ProtocolState, error) {
	if err := required("key", key); err != nil {
		return store.ProtocolState{}, err
	}
	var out store.ProtocolState
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(ch); err != nil {
			return err
		}
		row, err := r.h.q.GetProtocol(ctx, db.GetProtocolParams{TenantID: r.h.tenantID, ChannelID: string(ch.ID), Key: key})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out = store.ProtocolState{
			Channel: ch, Key: key,
			Value: model.Extension{
				Name:    row.ValueName,
				Version: uint32(row.ValueVersion), // #nosec G115 -- column CHECK bounds it to uint32
				Data:    nonEmpty(row.ValueData),
			},
			Revision: model.Revision(row.Revision),
		}
		return nil
	})
	return out, err
}

func (r reader) Ingress(ctx context.Context, id model.ID) (store.IngressRecord, error) {
	var out store.IngressRecord
	err := r.h.call(ctx, func() error {
		if err := id.Validate(); err != nil {
			return err
		}
		b, err := r.h.q.GetIngress(ctx, db.GetIngressParams{TenantID: r.h.tenantID, ID: string(id)})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out, err = decode[store.IngressRecord](b)
		return err
	})
	return out, err
}

// positioned is a fetched row: its list position and stored body.
type positioned struct {
	pos  int64
	body []byte
}

// listBodies is the shared list path: validate the page and filter, fetch
// one row beyond the limit to learn completeness, decode.
func listBodies[V any](ctx context.Context, h *handle, req model.PageRequest, check func() error,
	fetch func(after int64, lim int32) ([]positioned, error),
) (model.Page[V], error) {
	var p model.Page[V]
	err := h.call(ctx, func() error {
		limit, err := pageLimit(req.Limit)
		if err != nil {
			return err
		}
		after, err := decodeCursor(req.After)
		if err != nil {
			return err
		}
		if err := check(); err != nil {
			return err
		}
		rows, err := fetch(after, limit+1)
		if err != nil {
			return h.db(ctx, err)
		}
		p, err = pageOf[V](rows, int(limit), req.After)
		return err
	})
	return p, err
}

// pageOf decodes up to limit rows; more rows than limit means more exist.
func pageOf[V any](rows []positioned, limit int, after model.Cursor) (model.Page[V], error) {
	p := model.Page[V]{Complete: len(rows) <= limit, Next: after}
	rows = rows[:min(limit, len(rows))]
	for _, row := range rows {
		v, err := decode[V](row.body)
		if err != nil {
			return model.Page[V]{}, err
		}
		p.Items = append(p.Items, v)
	}
	if len(rows) > 0 {
		p.Next = encodeCursor(rows[len(rows)-1].pos)
	}
	return p, nil
}

func (r reader) Channels(ctx context.Context, req model.ListRequest) (model.Page[model.Channel], error) {
	var p model.Page[model.Channel]
	err := r.h.call(ctx, func() error {
		limit, err := pageLimit(req.Page.Limit)
		if err != nil {
			return err
		}
		after, err := decodeCursor(req.Page.After)
		if err != nil {
			return err
		}
		if err := r.h.checkFilter(req.Filter, filterSupport{}); err != nil {
			return err
		}
		rows, err := r.h.q.ListChannels(ctx, db.ListChannelsParams{TenantID: r.h.tenantID, After: after, Lim: limit + 1})
		if err != nil {
			return r.h.db(ctx, err)
		}
		p = model.Page[model.Channel]{Complete: len(rows) <= int(limit), Next: req.Page.After}
		rows = rows[:min(int(limit), len(rows))]
		for _, row := range rows {
			ref := model.ChannelRef{Tenant: r.h.tenant, ID: model.ID(row.ID)}
			p.Items = append(p.Items, channelOf(ref, row.Key, row.TypeKey, row.ConfigName, row.ConfigVersion, row.ConfigData, row.CredentialRef, row.Revision))
		}
		if len(rows) > 0 {
			p.Next = encodeCursor(rows[len(rows)-1].Pos)
		}
		return nil
	})
	return p, err
}

func positions[T any](rows []T, err error, of func(T) positioned) ([]positioned, error) {
	if err != nil {
		return nil, err
	}
	out := make([]positioned, len(rows))
	for i, row := range rows {
		out[i] = of(row)
	}
	return out, nil
}

func (r reader) Inboxes(ctx context.Context, req model.ListRequest) (model.Page[model.Inbox], error) {
	f := req.Filter
	return listBodies[model.Inbox](ctx, r.h, req.Page,
		func() error { return r.h.checkFilter(f, filterSupport{inboxes: true}) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListInboxes(ctx, db.ListInboxesParams{TenantID: r.h.tenantID, After: after, Inboxes: inboxIDs(f), Lim: lim})
			return positions(rows, err, func(x db.ListInboxesRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (r reader) Contacts(ctx context.Context, req model.ListRequest) (model.Page[model.Contact], error) {
	f := req.Filter
	return listBodies[model.Contact](ctx, r.h, req.Page,
		func() error { return r.h.checkFilter(f, filterSupport{kinds: true}) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListContacts(ctx, db.ListContactsParams{TenantID: r.h.tenantID, After: after, Kinds: strs(f.Kinds), Lim: lim})
			return positions(rows, err, func(x db.ListContactsRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (r reader) Identities(ctx context.Context, req model.ListRequest) (model.Page[model.Identity], error) {
	f := req.Filter
	return listBodies[model.Identity](ctx, r.h, req.Page,
		func() error { return r.h.checkFilter(f, filterSupport{inboxes: true, contact: true}) },
		func(after int64, lim int32) ([]positioned, error) {
			var contact *string
			if f.Contact != nil {
				contact = optID(f.Contact.ID)
			}
			rows, err := r.h.q.ListIdentities(ctx, db.ListIdentitiesParams{
				TenantID: r.h.tenantID, After: after, Inboxes: inboxIDs(f), ContactID: contact, Lim: lim,
			})
			return positions(rows, err, func(x db.ListIdentitiesRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (r reader) Conversations(ctx context.Context, req model.ListRequest) (model.Page[model.Conversation], error) {
	f := req.Filter
	now := r.h.now()
	return listBodies[model.Conversation](ctx, r.h, req.Page,
		func() error { return r.h.checkFilter(f, conversationReads) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListConversations(ctx, r.h.listConversationsParams(f, after, lim, now))
			return positions(rows, err, func(x db.ListConversationsRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (r reader) Messages(ctx context.Context, req model.ListRequest) (model.Page[model.Message], error) {
	f := req.Filter
	return listBodies[model.Message](ctx, r.h, req.Page,
		func() error { return r.h.checkFilter(f, messageSearch) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListMessages(ctx, r.h.listMessagesParams(f, after, lim))
			return positions(rows, err, func(x db.ListMessagesRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

// InboundMessages pages the conversation's live inbound messages newest
// first through the partial index uma_messages_inbound, so the read costs
// the page and not the conversation's history.
func (r reader) InboundMessages(ctx context.Context, conv model.ConversationRef, page model.PageRequest) (model.Page[model.Message], error) {
	return listBodies[model.Message](ctx, r.h, page,
		func() error { return r.h.scoped(conv) },
		func(after int64, lim int32) ([]positioned, error) {
			before := after
			if page.After == "" {
				before = math.MaxInt64
			}
			rows, err := r.h.q.ListInboundMessages(ctx, db.ListInboundMessagesParams{
				TenantID: r.h.tenantID, InboxID: string(conv.Inbox.ID), ConversationID: string(conv.ID), Before: before, Lim: lim,
			})
			return positions(rows, err, func(x db.ListInboundMessagesRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

// MessagesByAttachment pages the messages holding ref through the
// attachment index, in recording order.
func (r reader) MessagesByAttachment(ctx context.Context, ref model.AttachmentRef, page model.PageRequest) (model.Page[model.Message], error) {
	return listBodies[model.Message](ctx, r.h, page,
		func() error { return r.h.scoped(ref) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListMessagesByAttachment(ctx, db.ListMessagesByAttachmentParams{
				TenantID: r.h.tenantID, AttachmentID: string(ref.ID), InboxID: string(ref.Inbox.ID), After: after, Lim: lim,
			})
			return positions(rows, err, func(x db.ListMessagesByAttachmentRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (r reader) WorkItems(ctx context.Context, req model.ListRequest) (model.Page[model.WorkItem], error) {
	f := req.Filter
	return listBodies[model.WorkItem](ctx, r.h, req.Page,
		func() error {
			return r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, statuses: true})
		},
		func(after int64, lim int32) ([]positioned, error) {
			inbox, conv := conversationArgs(f)
			rows, err := r.h.q.ListWorkItems(ctx, db.ListWorkItemsParams{
				TenantID: r.h.tenantID, After: after, Inboxes: inboxIDs(f), ConversationInbox: inbox, ConversationID: conv,
				Statuses: strs(f.Statuses), Lim: lim,
			})
			return positions(rows, err, func(x db.ListWorkItemsRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

func (r reader) Attempts(ctx context.Context, op model.OperationRef, pr model.PageRequest) (model.Page[store.AttemptEvidence], error) {
	return listBodies[store.AttemptEvidence](ctx, r.h, pr,
		func() error {
			if err := r.h.scoped(op); err != nil {
				return err
			}
			_, err := r.h.q.GetOperation(ctx, db.GetOperationParams{TenantID: r.h.tenantID, ID: string(op.ID)})
			if err != nil {
				return r.h.lookupErr(ctx, err)
			}
			return nil
		},
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListAttempts(ctx, db.ListAttemptsParams{
				TenantID: r.h.tenantID, OperationID: string(op.ID), After: after, Lim: lim,
			})
			return positions(rows, err, func(x db.ListAttemptsRow) positioned { return positioned{x.Seq, x.Body} })
		})
}

func (r reader) IdentityLinks(ctx context.Context, f store.LinkFilter, pr model.PageRequest) (model.Page[store.LinkRecord], error) {
	return listBodies[store.LinkRecord](ctx, r.h, pr,
		func() error {
			if f.Identity == nil && f.Contact == nil {
				return invalid("filter: identity or contact required")
			}
			if f.Identity != nil {
				if err := r.h.scoped(f.Identity); err != nil {
					return err
				}
			}
			if f.Contact != nil {
				if err := r.h.scoped(f.Contact); err != nil {
					return err
				}
			}
			return nil
		},
		func(after int64, lim int32) ([]positioned, error) {
			arg := db.ListLinksParams{TenantID: r.h.tenantID, After: after, Lim: lim}
			if f.Identity != nil {
				arg.IdentityInbox, arg.IdentityID = optID(f.Identity.Inbox.ID), optID(f.Identity.ID)
			}
			if f.Contact != nil {
				arg.ContactID = optID(f.Contact.ID)
			}
			rows, err := r.h.q.ListLinks(ctx, arg)
			return positions(rows, err, func(x db.ListLinksRow) positioned { return positioned{x.Seq, x.Body} })
		})
}

// Events pages the committed log in position order. The page is bounded by
// the log head read in the same snapshot, so a complete page's Next is the
// head: the position after the last scanned event.
func (r reader) Events(ctx context.Context, f model.Filter, after model.Cursor, limit int) (model.Page[model.Event], error) {
	var p model.Page[model.Event]
	err := r.h.call(ctx, func() error {
		lim, err := pageLimit(limit)
		if err != nil {
			return err
		}
		pos, err := decodeCursor(after)
		if err != nil {
			return err
		}
		if err := r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, kinds: true, since: true}); err != nil {
			return err
		}
		if err := r.h.placeOwn(ctx); err != nil {
			return err
		}
		head, err := r.h.q.EventHead(ctx, r.h.tenantID)
		if err != nil {
			return r.h.db(ctx, err)
		}
		if pos > head {
			return invalid("cursor: beyond the end of the log")
		}
		base, err := r.h.q.EventBase(ctx, r.h.tenantID)
		if err != nil {
			return r.h.db(ctx, err)
		}
		if pos < base {
			return errCursorExpired()
		}
		var sinceUnix *int64
		var sinceNanos *int32
		if f.Since != nil {
			u, n := instant(*f.Since)
			sinceUnix, sinceNanos = &u, &n
			// No event before the first position whose running RecordedAt
			// maximum reaches Since can match: start the scan there.
			first, err := r.h.q.EventSinceStart(ctx, db.EventSinceStartParams{TenantID: r.h.tenantID, SinceUnix: u, SinceNanos: n})
			if err != nil {
				return r.h.db(ctx, err)
			}
			start := head
			if len(first) == 1 {
				start = first[0] - 1
			}
			pos = max(pos, start)
		}
		got, err := r.events(ctx, f, pos, head, sinceUnix, sinceNanos, lim+1)
		if err != nil {
			return err
		}
		if p, err = pageOf[model.Event](got, int(lim), encodeCursor(pos)); err != nil {
			return err
		}
		if p.Complete {
			p.Next = encodeCursor(head)
		}
		return nil
	})
	return p, err
}

// events reads at most lim events after position after through head that
// f selects: through uma_events_conversation for a conversation, else in
// position order over the log.
func (r reader) events(ctx context.Context, f model.Filter, after, head int64, sinceUnix *int64, sinceNanos *int32, lim int32) ([]positioned, error) {
	kinds := strs(f.Kinds)
	if c := f.Conversation; c != nil {
		if len(f.Inboxes) > 0 && !slices.ContainsFunc(f.Inboxes, func(in model.InboxRef) bool { return in.ID == c.Inbox.ID }) {
			return nil, nil
		}
		rows, err := r.h.q.ListConversationEvents(ctx, db.ListConversationEventsParams{
			TenantID: r.h.tenantID, InboxID: optID(c.Inbox.ID), ConversationID: optID(c.ID), After: after, Head: head,
			Kinds: kinds, SinceUnix: sinceUnix, SinceNanos: sinceNanos, Lim: lim,
		})
		if err != nil {
			return nil, r.h.db(ctx, err)
		}
		return positions(rows, nil, func(x db.ListConversationEventsRow) positioned { return positioned{x.Position, x.Body} })
	}
	rows, err := r.h.q.ListEvents(ctx, db.ListEventsParams{
		TenantID: r.h.tenantID, After: after, Head: head, Inboxes: inboxIDs(f), Kinds: kinds,
		SinceUnix: sinceUnix, SinceNanos: sinceNanos, Lim: lim,
	})
	if err != nil {
		return nil, r.h.db(ctx, err)
	}
	return positions(rows, nil, func(x db.ListEventsRow) positioned { return positioned{x.Position, x.Body} })
}

// EventHead reads the log head in the unit's snapshot, past the unit's own
// appends.
func (r reader) EventHead(ctx context.Context) (model.Cursor, error) {
	var c model.Cursor
	err := r.h.call(ctx, func() error {
		if err := r.h.placeOwn(ctx); err != nil {
			return err
		}
		head, err := r.h.q.EventHead(ctx, r.h.tenantID)
		if err != nil {
			return r.h.db(ctx, err)
		}
		c = encodeCursor(head)
		return nil
	})
	return c, err
}

func (r reader) Candidates(ctx context.Context, key store.SelectionKey, limit int) ([]model.Conversation, error) {
	var out []model.Conversation
	err := r.h.call(ctx, func() error {
		if err := firstErr(r.h.scoped(key.Inbox), required("key", key.Key)); err != nil {
			return err
		}
		if limit <= 0 {
			return invalid("limit: must be positive")
		}
		// The members that are not closed are fetched one row beyond the
		// limit, so a larger set is detected rather than truncated; the
		// newest closed member joins them.
		limit = min(limit, math.MaxInt32-1)
		lim := int32(limit) // #nosec G115 -- bounded above
		inbox := string(key.Inbox.ID)
		rows, err := r.h.q.Candidates(ctx, db.CandidatesParams{
			TenantID: r.h.tenantID, InboxID: inbox, Mode: key.Mode, SelectionKey: key.Key,
			Closed: model.ConversationClosed, Lim: lim + 1,
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		newest, err := r.h.q.NewestClosedCandidate(ctx, db.NewestClosedCandidateParams{
			TenantID: r.h.tenantID, InboxID: inbox, Mode: key.Mode, SelectionKey: key.Key,
			Closed: model.ConversationClosed,
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		rows = append(rows, newest...)
		if len(rows) > limit {
			return conflict("candidate set exceeds limit")
		}
		out = make([]model.Conversation, 0, len(rows))
		for _, b := range rows {
			c, err := decode[model.Conversation](b)
			if err != nil {
				return err
			}
			out = append(out, c)
		}
		slices.SortFunc(out, func(a, b model.Conversation) int {
			if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(string(a.Ref.ID), string(b.Ref.ID))
		})
		return nil
	})
	return out, err
}

// MessagesByReference reads the inbox's messages holding ref through the
// reference index, one row beyond limit so a larger set is detected rather
// than truncated.
func (r reader) MessagesByReference(ctx context.Context, in model.InboxRef, ref model.ExternalRef, limit int) ([]model.Message, error) {
	var out []model.Message
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(in, ref); err != nil {
			return err
		}
		if limit <= 0 {
			return invalid("limit: must be positive")
		}
		limit = min(limit, math.MaxInt32-1)
		lim := int32(limit) // #nosec G115 -- bounded above
		rows, err := r.h.q.MessagesByReference(ctx, db.MessagesByReferenceParams{
			TenantID: r.h.tenantID, InboxID: string(in.ID), ExtID: ref.ID,
			ChannelTenant: string(ref.Channel.Tenant), ChannelID: string(ref.Channel.ID),
			Namespace: ref.Namespace, Kind: ref.Kind, ParentID: ref.ParentID, Lim: lim + 1,
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		if len(rows) > limit {
			return conflict("matching message set exceeds limit")
		}
		out = make([]model.Message, 0, len(rows))
		for _, b := range rows {
			m, err := decode[model.Message](b)
			if err != nil {
				return err
			}
			out = append(out, m)
		}
		slices.SortFunc(out, func(a, b model.Message) int {
			if c := a.RecordedAt.Compare(b.RecordedAt); c != 0 {
				return c
			}
			return strings.Compare(string(a.Ref.ID), string(b.Ref.ID))
		})
		return nil
	})
	return out, err
}

func (r reader) ResolveDestination(ctx context.Context, ch model.ChannelRef, dest string) (model.Inbox, error) {
	var out model.Inbox
	err := r.h.call(ctx, func() error {
		if err := firstErr(r.h.scoped(ch), required("destination", dest)); err != nil {
			return err
		}
		rows, err := r.h.q.ResolveDestination(ctx, db.ResolveDestinationParams{
			TenantID: r.h.tenantID, ChannelID: string(ch.ID), Destination: dest,
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		switch len(rows) {
		case 0:
			return notFound()
		case 1:
			out, err = decode[model.Inbox](rows[0])
			return err
		}
		return conflict("destination resolves to more than one inbox")
	})
	return out, err
}

func (r reader) ResolveChannelKey(ctx context.Context, key string) (model.Channel, error) {
	var out model.Channel
	err := r.h.call(ctx, func() error {
		if err := required("key", key); err != nil {
			return err
		}
		row, err := r.h.q.ResolveChannelKey(ctx, db.ResolveChannelKeyParams{TenantID: r.h.tenantID, Key: key})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		ref := model.ChannelRef{Tenant: r.h.tenant, ID: model.ID(row.ID)}
		out = channelOf(ref, row.Key, row.TypeKey, row.ConfigName, row.ConfigVersion, row.ConfigData, row.CredentialRef, row.Revision)
		return nil
	})
	return out, err
}

func (r reader) ResolveInboxKey(ctx context.Context, key string) (model.Inbox, error) {
	var out model.Inbox
	err := r.h.call(ctx, func() error {
		if err := required("key", key); err != nil {
			return err
		}
		row, err := r.h.q.ResolveInboxKey(ctx, db.ResolveInboxKeyParams{TenantID: r.h.tenantID, Key: key})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out, err = decode[model.Inbox](row.Body)
		return err
	})
	return out, err
}

// keyHolder reports the ID of the record holding key through resolve, or
// "" when none does. An empty key is held by no record.
func (h *handle) keyHolder(ctx context.Context, key string, resolve func() (string, error)) (string, error) {
	if key == "" {
		return "", nil
	}
	id, err := resolve()
	if missing(err) {
		return "", nil
	}
	if err != nil {
		return "", h.db(ctx, err)
	}
	return id, nil
}

func (r reader) Lease(ctx context.Context, res model.Ref) (store.Lease, error) {
	var out store.Lease
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(res); err != nil {
			return err
		}
		l, live, err := r.h.liveLease(ctx, res)
		switch {
		case err != nil:
			return err
		case !live:
			return notFound()
		}
		out = l
		return nil
	})
	return out, err
}
