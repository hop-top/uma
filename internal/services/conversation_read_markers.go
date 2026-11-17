package services

import (
	"context"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// MarkRead moves the caller's read marker so that req.Message, or every
// message recorded until now when unset, is read; see markRead.
func (s Conversations) MarkRead(ctx context.Context, req model.ReadMark) (model.ReadMarker, error) {
	return markRead(ctx, s.c, req, true)
}

// MarkUnread moves the caller's read marker so that req.Message, or the
// newest live inbound message not imported when unset, and everything
// after it is unread; see markRead.
func (s Conversations) MarkUnread(ctx context.Context, req model.ReadMark) (model.ReadMarker, error) {
	return markRead(ctx, s.c, req, false)
}

// ReadMarker returns the caller's read marker on a conversation, or one
// with a zero ReadThrough and no revision when it has none. Authorized as
// model.ActionConversationReadWorkflow on the conversation: no one reads
// another actor's marker.
func (s Conversations) ReadMarker(ctx context.Context, req model.Lookup[model.ConversationRef]) (model.ReadMarker, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.ReadMarker{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.ReadMarker{}, err
	}
	if req.Ref.Inbox.Tenant != c.Principal().Tenant {
		return model.ReadMarker{}, engine.ErrNotFound()
	}
	var conv model.Conversation
	var marker model.ReadMarker
	if err := view(ctx, c, func(r store.Reader) error {
		if err := loadConversation(ctx, r, req.Ref, &conv); err != nil {
			return err
		}
		var err error
		marker, err = currentMarker(ctx, r, conv.Ref, c.Principal().Actor)
		return err
	}); err != nil {
		return model.ReadMarker{}, err
	}
	res := conv.Ref.Ref()
	if err := c.Authorize(ctx, policy.Authorization{Action: model.ActionConversationReadWorkflow, Resource: res, Revision: conv.Revision}, res); err != nil {
		return model.ReadMarker{}, err
	}
	return marker, nil
}

// markRead sets the caller's read marker on a conversation, authorized as
// model.ActionConversationMarkRead on it (and model.ActionMessageRead on
// req.Message when set). Read (read true): ReadThrough is req.Message's
// recording time, or the engine clock's now. Unread: ReadThrough is just
// before req.Message's recording time, or before the newest live inbound
// message's that was not imported (the zero time when there is none):
// imported messages never count as unread, so marking before one would
// leave the conversation read. The marker is the actor's
// own record: the conversation and its revision never move, and nothing
// is emitted. Setting the ReadThrough the marker holds changes nothing, so
// both calls are idempotent; a lost answer is recovered by repeating.
func markRead(ctx context.Context, c *engine.Client, req model.ReadMark, read bool) (model.ReadMarker, error) {
	e := c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.ReadMarker{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.ReadMarker{}, err
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.ReadMarker{}, engine.ErrNotFound()
	}
	var conv model.Conversation
	var msg *model.Message
	if err := view(ctx, c, func(r store.Reader) error {
		if err := loadConversation(ctx, r, req.Conversation, &conv); err != nil {
			return err
		}
		switch {
		case req.Message != nil:
			m, err := r.GetMessage(ctx, *req.Message)
			if err := exactRef(err, m.Ref == *req.Message); err != nil {
				return err
			}
			msg = &m
		case !read:
			m, err := newestLiveInbound(ctx, r, conv.Ref, nil)
			if err != nil {
				return err
			}
			msg = m
		}
		return nil
	}); err != nil {
		return model.ReadMarker{}, err
	}
	res := conv.Ref.Ref()
	if err := c.Authorize(ctx, policy.Authorization{Action: model.ActionConversationMarkRead, Resource: res, Revision: conv.Revision}, res); err != nil {
		return model.ReadMarker{}, err
	}
	if req.Message != nil {
		if err := c.Authorize(ctx, engine.MessageRead(*msg), msg.Ref.Ref()); err != nil {
			return model.ReadMarker{}, err
		}
	}
	now := e.Clock().Now().UTC()
	var through time.Time
	switch {
	case read && msg != nil:
		through = msg.RecordedAt
	case read:
		through = now
	case msg != nil:
		through = msg.RecordedAt.Add(-time.Nanosecond)
	}
	rev, err := operations.NewID(ctx, e)
	if err != nil {
		return model.ReadMarker{}, err
	}
	actor := c.Principal().Actor
	var out model.ReadMarker
	err = operations.Transact(ctx, e, c.Principal().Tenant, func(tx store.Tx) error {
		if err := loadConversation(ctx, tx, conv.Ref, nil); err != nil {
			return err
		}
		cur, err := currentMarker(ctx, tx, conv.Ref, actor)
		if err != nil {
			return err
		}
		if cur.Revision != "" && cur.ReadThrough.Equal(through) {
			out = cur
			return nil
		}
		out = model.ReadMarker{Conversation: conv.Ref, Actor: actor, ReadThrough: through, UpdatedAt: now, Revision: model.Revision(rev)}
		return tx.PutReadMarker(ctx, out, cur.Revision)
	})
	if model.IsCode(err, model.NotFound) {
		return model.ReadMarker{}, engine.ErrNotFound()
	}
	return out, err
}

// currentMarker returns actor's marker on conv, or an empty one (no
// revision) when it has none.
func currentMarker(ctx context.Context, r store.Reader, conv model.ConversationRef, actor model.ActorID) (model.ReadMarker, error) {
	m, err := r.ReadMarker(ctx, conv, actor)
	if model.IsCode(err, model.NotFound) {
		return model.ReadMarker{Conversation: conv, Actor: actor}, nil
	}
	return m, err
}
