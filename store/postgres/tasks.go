package postgres

import (
	"context"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store/postgres/internal/db"
)

// taskRef is a task's External as nullable column arguments, all nil for
// a zero reference.
type taskRef struct {
	channel, namespace, kind, id, parent *string
}

func taskRefArgs(r model.ExternalRef) taskRef {
	if r.IsZero() {
		return taskRef{}
	}
	return taskRef{optID(r.Channel.ID), &r.Namespace, &r.Kind, &r.ID, &r.ParentID}
}

// ownerArgs returns a conversation owner as nullable column arguments.
func ownerArgs(owner *model.IdentityRef) (inbox, id *string) {
	if owner == nil {
		return nil, nil
	}
	return optID(owner.Inbox.ID), optID(owner.ID)
}

// statusOrder splits a task's StatusAt into the exact Unix seconds and
// nanoseconds the status order compares.
func statusOrder(v model.Task) (int64, int32) {
	return v.StatusAt.Unix(), int32(v.StatusAt.Nanosecond()) // #nosec G115 -- Nanosecond is below 1e9
}

func (t txn) PutTask(ctx context.Context, v model.Task, expected model.Revision) error {
	h := t.h
	return h.call(ctx, func() error {
		if err := h.scoped(v.Ref); err != nil {
			return err
		}
		if !v.External.IsZero() {
			if err := h.scoped(v.External); err != nil {
				return err
			}
		}
		conv := v.Ref.Conversation
		owner, err := h.conversationOwner(ctx, conv)
		if err != nil {
			return err
		}
		inbox, cid, id := string(conv.Inbox.ID), string(conv.ID), string(v.Ref.ID)
		ext := taskRefArgs(v.External)
		if ext.channel != nil {
			// The unique index decides between concurrent units; this
			// check keeps the unit usable when the conflict is visible.
			held, err := h.q.GetTaskByReference(ctx, db.GetTaskByReferenceParams{
				TenantID: h.tenantID, InboxID: inbox, ExternalChannelID: ext.channel, ExternalNamespace: ext.namespace,
				ExternalKind: ext.kind, ExternalID: ext.id, ExternalParentID: ext.parent,
			})
			switch {
			case err == nil && held.ID != id:
				return conflict("task reference held by another task")
			case err != nil && !missing(err):
				return h.db(ctx, err)
			}
		}
		c := v.Clone()
		body, err := encode(&c)
		if err != nil {
			return err
		}
		unix, nanos := statusOrder(v)
		ownerInbox, ownerID := ownerArgs(owner)
		return h.put(ctx, v.Revision, expected,
			func() (string, error) {
				row, err := h.q.GetTask(ctx, db.GetTaskParams{TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id})
				return row.Revision, err
			},
			func() error {
				return h.q.InsertTask(ctx, db.InsertTaskParams{
					TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id, State: v.State,
					ExternalChannelID: ext.channel, ExternalNamespace: ext.namespace, ExternalKind: ext.kind,
					ExternalID: ext.id, ExternalParentID: ext.parent, StatusUnix: unix, StatusNanos: nanos,
					OwnerInboxID: ownerInbox, OwnerID: ownerID, Body: body, Revision: string(v.Revision),
				})
			},
			func() (int64, error) {
				return h.q.UpdateTask(ctx, db.UpdateTaskParams{
					TenantID: h.tenantID, InboxID: inbox, ConversationID: cid, ID: id, State: v.State,
					ExternalChannelID: ext.channel, ExternalNamespace: ext.namespace, ExternalKind: ext.kind,
					ExternalID: ext.id, ExternalParentID: ext.parent, StatusUnix: unix, StatusNanos: nanos,
					OwnerInboxID: ownerInbox, OwnerID: ownerID, Body: body, Revision: string(v.Revision), Expected: string(expected),
				})
			})
	})
}

// conversationOwner returns the Owner of an existing conversation.
func (h *handle) conversationOwner(ctx context.Context, c model.ConversationRef) (*model.IdentityRef, error) {
	row, err := h.q.GetConversation(ctx, db.GetConversationParams{TenantID: h.tenantID, InboxID: string(c.Inbox.ID), ID: string(c.ID)})
	if err != nil {
		return nil, h.lookupErr(ctx, err)
	}
	v, err := decode[model.Conversation](row.Body)
	if err != nil {
		return nil, err
	}
	return v.Owner, nil
}

// syncTaskOwner copies a conversation's Owner onto its tasks.
func (h *handle) syncTaskOwner(ctx context.Context, v model.Conversation) error {
	inbox, id := ownerArgs(v.Owner)
	return h.db(ctx, h.q.SetTaskOwner(ctx, db.SetTaskOwnerParams{
		TenantID: h.tenantID, InboxID: string(v.Ref.Inbox.ID), ConversationID: string(v.Ref.ID), OwnerInboxID: inbox, OwnerID: id,
	}))
}

func (r reader) TaskByReference(ctx context.Context, inbox model.InboxRef, ref model.ExternalRef) (model.Task, error) {
	var out model.Task
	err := r.h.call(ctx, func() error {
		if err := r.h.scoped(inbox, ref); err != nil {
			return err
		}
		ext := taskRefArgs(ref)
		row, err := r.h.q.GetTaskByReference(ctx, db.GetTaskByReferenceParams{
			TenantID: r.h.tenantID, InboxID: string(inbox.ID), ExternalChannelID: ext.channel, ExternalNamespace: ext.namespace,
			ExternalKind: ext.kind, ExternalID: ext.id, ExternalParentID: ext.parent,
		})
		if err != nil {
			return r.h.lookupErr(ctx, err)
		}
		out, err = decode[model.Task](row.Body)
		return err
	})
	return out, err
}

// Tasks pages in status order, most recent first, after the cursor's
// (StatusAt, ID) position.
func (r reader) Tasks(ctx context.Context, req model.ListRequest) (model.Page[model.Task], error) {
	f := req.Filter
	var p model.Page[model.Task]
	err := r.h.call(ctx, func() error {
		limit, err := pageLimit(req.Page.Limit)
		if err != nil {
			return err
		}
		after, paged, err := decodeTaskCursor(req.Page.After)
		if err != nil {
			return err
		}
		if err := r.h.checkFilter(f, filterSupport{inboxes: true, conversation: true, statuses: true, identity: true}); err != nil {
			return err
		}
		inbox, conv := conversationArgs(f)
		ownerInbox, ownerID := ownerArgs(f.Identity)
		rows, err := r.h.q.ListTasks(ctx, db.ListTasksParams{
			TenantID: r.h.tenantID, Paged: paged, AfterUnix: after.unix, AfterNanos: after.nanos, AfterID: after.id,
			Inboxes: inboxIDs(f), ConversationInbox: inbox, ConversationID: conv, Statuses: strs(f.Statuses),
			OwnerInboxID: ownerInbox, OwnerID: ownerID, Lim: limit + 1,
		})
		if err != nil {
			return r.h.db(ctx, err)
		}
		p = model.Page[model.Task]{Complete: len(rows) <= int(limit), Next: req.Page.After}
		rows = rows[:min(int(limit), len(rows))]
		for _, row := range rows {
			v, err := decode[model.Task](row.Body)
			if err != nil {
				return err
			}
			p.Items = append(p.Items, v)
		}
		if n := len(rows); n > 0 {
			p.Next = encodeTaskCursor(taskPos{rows[n-1].StatusUnix, rows[n-1].StatusNanos, rows[n-1].ID})
		}
		return nil
	})
	return p, err
}

func (r reader) Replies(ctx context.Context, parent model.MessageRef, page model.PageRequest) (model.Page[model.Message], error) {
	return listBodies[model.Message](ctx, r.h, page,
		func() error { return r.h.scoped(parent) },
		func(after int64, lim int32) ([]positioned, error) {
			rows, err := r.h.q.ListReplies(ctx, db.ListRepliesParams{
				TenantID: r.h.tenantID, ParentID: optID(parent.ID), ParentInbox: string(parent.Conversation.Inbox.ID),
				ParentConversation: string(parent.Conversation.ID), After: after, Lim: lim,
			})
			return positions(rows, err, func(x db.ListRepliesRow) positioned { return positioned{x.Pos, x.Body} })
		})
}

// messageLinks checks that a message's task and parent are in its
// conversation and returns their IDs as column arguments.
func messageLinks(v model.Message) (task, parent *string, err error) {
	if v.Task != nil {
		if !v.Task.In(v.Ref.Conversation) {
			return nil, nil, invalid("task: must belong to the message's conversation")
		}
		task = optID(v.Task.ID)
	}
	if v.InReplyTo != nil {
		if !v.InReplyTo.In(v.Ref.Conversation) {
			return nil, nil, invalid("in_reply_to: must belong to the message's conversation")
		}
		parent = optID(v.InReplyTo.ID)
	}
	return task, parent, nil
}

// taskPos is a task's place in status order.
type taskPos struct {
	unix  int64
	nanos int32
	id    string
}

// zeroID stands in for the cursor ID of the first page, never compared.
const zeroID = "00000000-0000-0000-0000-000000000000"

// Task cursors are "pg.t.<unix>.<nanos>.<id>", the status position of the
// last task returned.
const taskCursorPrefix = cursorPrefix + "t."

func encodeTaskCursor(p taskPos) model.Cursor {
	return model.Cursor(taskCursorPrefix + strconv.FormatInt(p.unix, 10) + "." + strconv.Itoa(int(p.nanos)) + "." + p.id)
}

// decodeTaskCursor returns the cursor's position and whether it names one;
// the empty cursor is the start.
func decodeTaskCursor(c model.Cursor) (taskPos, bool, error) {
	start := taskPos{id: zeroID}
	if c == "" {
		return start, false, nil
	}
	malformed := invalid("cursor: malformed")
	s, ok := strings.CutPrefix(string(c), taskCursorPrefix)
	if !ok {
		return start, false, malformed
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return start, false, malformed
	}
	unix, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return start, false, malformed
	}
	nanos, err := strconv.ParseInt(parts[1], 10, 32)
	if err != nil || nanos < 0 || nanos >= int64(time.Second) {
		return start, false, malformed
	}
	if model.ID(parts[2]).Validate() != nil {
		return start, false, malformed
	}
	return taskPos{unix, int32(nanos), parts[2]}, true, nil
}
