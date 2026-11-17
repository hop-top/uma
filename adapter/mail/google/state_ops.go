package google

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// The provider conversation state verbs. A uma conversation is a set of
// Gmail messages, and Gmail holds most state per message but changes it per
// thread: every verb is one users.threads call on the thread of each item,
// which Gmail applies to all of its messages ("This applies to all messages
// in the thread"), never a users.messages call on one message of it.
//
//	archive, unarchive   threads.modify, remove or add INBOX
//	read, unread         threads.modify, remove or add UNREAD
//	flag, unflag         threads.modify, add or remove STARRED or IMPORTANT
//	move                 threads.modify with label IDs (labels shape), or
//	                     threads.trash for the trash role
//	trash, untrash       threads.trash, threads.untrash
//
// All five are account scope: Gmail labels change the mailbox's own view
// and nobody else's. They need gmail.modify. Gmail IDs are stable, so no
// verb reports a reference change.
//
// A threads.modify is idempotent, so a state the thread holds already is
// applied. A call that may have reached Gmail without an answer is
// outcome_unknown, and Reconcile reads the labels of the thread back. A
// conversation whose items fall in several Gmail threads is one call per
// thread, in the order of the items: a failure after the first call leaves
// the conversation partly changed, which is outcome_unknown as well.

// stateCapabilities are the capabilities of the state verbs.
var stateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// stateRoles are the destination roles a move resolves.
var stateRoles = []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}

// stateLimits are the limits a state capability reports: account scope; a
// flag of kind star or important; a move in the labels shape.
func stateLimits(c model.Capability) (model.Extension, bool) {
	l := model.StateLimits{Scope: model.ScopeAccount}
	switch c {
	case model.CapabilityFlag:
		l.Kinds = []string{model.FlagStar, model.FlagImportant}
	case model.CapabilityMove:
		l.Shape, l.Roles = model.MoveShapeLabels, slices.Clone(stateRoles)
	case model.CapabilityArchive, model.CapabilityReadState, model.CapabilityTrash:
	default:
		return model.Extension{}, false
	}
	return l.Extension(), true
}

// Thread calls.
const (
	opModify  = "modify"
	opTrash   = "trash"
	opUntrash = "untrash"
)

// stateChange is what one verb asks of a thread: a threads.modify with
// the label lists, or a trash or untrash.
type stateChange struct {
	op          string
	add, remove []string
}

// shows reports whether every message of a thread carries the change.
func (c stateChange) shows(msgs []gMessage) bool {
	add, remove := c.add, c.remove
	switch c.op {
	case opTrash:
		add = []string{labelTrash}
	case opUntrash:
		remove = []string{labelTrash}
	}
	for _, m := range msgs {
		for _, l := range add {
			if !slices.Contains(m.LabelIDs, l) {
				return false
			}
		}
		for _, l := range remove {
			if slices.Contains(m.LabelIDs, l) {
				return false
			}
		}
	}
	return len(msgs) > 0
}

func labelChange(undo bool, label string) stateChange {
	if undo {
		return stateChange{op: opModify, remove: []string{label}}
	}
	return stateChange{op: opModify, add: []string{label}}
}

func archiveChange(undo bool) stateChange {
	if undo {
		return stateChange{op: opModify, add: []string{labelInbox}}
	}
	return stateChange{op: opModify, remove: []string{labelInbox}}
}

func readChange(read bool) stateChange {
	if read {
		return stateChange{op: opModify, remove: []string{labelUnread}}
	}
	return stateChange{op: opModify, add: []string{labelUnread}}
}

func flagChange(kind string, undo bool) (stateChange, *model.Error) {
	switch kind {
	case model.FlagStar:
		return labelChange(undo, labelStarred), nil
	case model.FlagImportant:
		return labelChange(undo, labelImportant), nil
	}
	return stateChange{}, &model.Error{Code: model.Unsupported, SafeMessage: "gmail flags star and important only", Retry: model.NoRetry}
}

func trashChange(undo bool) stateChange {
	if undo {
		return stateChange{op: opUntrash}
	}
	return stateChange{op: opTrash}
}

// moveChange maps a move to label changes: a destination label ID is added;
// a role is resolved (inbox adds INBOX, archive removes it, junk adds SPAM
// and removes INBOX, trash is a threads.trash); or the request's own Add
// and Remove lists are applied.
func moveChange(dest model.MoveDestination, add, remove []string) (stateChange, *model.Error) {
	refuse := func(code model.ErrorCode, msg string) (stateChange, *model.Error) {
		return stateChange{}, &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
	}
	switch {
	case dest.Role != "":
		switch dest.Role {
		case model.RoleInbox:
			return stateChange{op: opModify, add: []string{labelInbox}}, nil
		case model.RoleArchive:
			return stateChange{op: opModify, remove: []string{labelInbox}}, nil
		case model.RoleJunk:
			return stateChange{op: opModify, add: []string{labelSpam}, remove: []string{labelInbox}}, nil
		case model.RoleTrash:
			return stateChange{op: opTrash}, nil
		}
		return refuse(model.Unsupported, "gmail resolves the inbox, archive, trash and junk roles only")
	case dest.ID != "":
		add = []string{dest.ID}
	}
	switch {
	case len(add)+len(remove) == 0:
		return refuse(model.Validation, "a move names a destination or labels to add or remove")
	case len(add) > maxLabelsPerList || len(remove) > maxLabelsPerList:
		return refuse(model.Validation, "at most 100 labels can be added, and 100 removed, per move")
	}
	for _, id := range slices.Concat(add, remove) {
		if id == "" {
			return refuse(model.Validation, "a label ID is empty")
		}
	}
	return stateChange{op: opModify, add: slices.Clone(add), remove: slices.Clone(remove)}, nil
}

// Archive archives the conversation's threads, or unarchives them.
func (i *instance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.change(ctx, req.Effect, req.Items, archiveChange(req.Undo), nil)
}

// Move changes the labels of the conversation's threads.
func (i *instance) Move(ctx context.Context, req adapter.MoveRequest) (adapter.Outcome, error) {
	c, refusal := moveChange(req.Destination, req.Add, req.Remove)
	return i.change(ctx, req.Effect, req.Items, c, refusal)
}

// SetReadState marks the conversation's threads read or unread.
func (i *instance) SetReadState(ctx context.Context, req adapter.ReadStateRequest) (adapter.Outcome, error) {
	return i.change(ctx, req.Effect, req.Items, readChange(req.Read), nil)
}

// Flag stars or marks important the conversation's threads, or clears it.
func (i *instance) Flag(ctx context.Context, req adapter.FlagRequest) (adapter.Outcome, error) {
	c, refusal := flagChange(req.Kind, req.Undo)
	return i.change(ctx, req.Effect, req.Items, c, refusal)
}

// Trash moves the conversation's threads to the trash, or restores them.
func (i *instance) Trash(ctx context.Context, req adapter.TrashRequest) (adapter.Outcome, error) {
	return i.change(ctx, req.Effect, req.Items, trashChange(req.Undo), nil)
}

// change runs c on every thread of the items. A refusal found while
// building the change is a final rejected outcome, made before any call.
func (i *instance) change(ctx context.Context, eff adapter.Effect, items []model.ExternalRef, c stateChange, refusal *model.Error) (adapter.Outcome, error) {
	if err := i.checkRoute(eff.Route); err != nil {
		return adapter.Outcome{}, err
	}
	if refusal != nil {
		return rejected(refusal.Code, refusal.SafeMessage), nil
	}
	s := i.c.session()
	threads, out, err := i.threadsOf(ctx, s, eff.Route, items)
	if threads == nil {
		return out, err
	}
	for n, id := range threads {
		if status, err := i.callThread(ctx, s, id, c); err != nil {
			return stateFailure(status, err, n > 0)
		}
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// callThread makes the thread call for c on thread id and returns the HTTP
// status of the answer, zero without one.
func (i *instance) callThread(ctx context.Context, s *session, id string, c stateChange) (int, error) {
	var body []byte
	if c.op == opModify {
		body = encode(gModify{AddLabelIDs: c.add, RemoveLabelIDs: c.remove})
	}
	resp, err := s.do(ctx, request{method: "POST", url: i.c.api("/threads/"+url.PathEscape(id)+"/"+c.op, nil), body: body})
	return resp.status, err
}

// stateFailure maps a failed thread call. A call whose answer may be lost
// is outcome_unknown, as for a send, and so is any failure after an earlier
// thread of the conversation was changed: the change cannot be repeated or
// dropped blindly, and Reconcile reads the threads back. A failure before
// the request was written is transient while nothing was changed.
func stateFailure(status int, err error, partial bool) (adapter.Outcome, error) {
	unknown := model.NewError(model.OutcomeUnknown, "gmail thread change answer lost")
	switch {
	case errors.Is(err, errUnsent) && !isContext(err):
		if partial {
			return adapter.Outcome{}, unknown
		}
		return adapter.Outcome{}, model.NewError(model.Transient, "gmail unreachable; change not attempted")
	case errors.Is(err, errTransport), status >= 500, status == http.StatusRequestTimeout:
		return adapter.Outcome{}, unknown
	case partial:
		return adapter.Outcome{}, unknown
	}
	return rejectedOr(err)
}

// threadsOf resolves the Gmail threads of the items, in the order of the
// items and each once. The items are the conversation's provider messages
// on this channel; with none the route's parent message stands in. A message
// Gmail no longer holds is left out. It returns no threads, and the outcome
// to answer with, when none can be found.
func (i *instance) threadsOf(ctx context.Context, s *session, route model.ExternalRef, items []model.ExternalRef) ([]string, adapter.Outcome, error) {
	var ids []string
	for _, it := range items {
		if it.Channel == i.ch.Ref && it.Namespace == model.ProviderMessageNamespace && it.Kind == model.RefKindMessage && it.ID != "" && !slices.Contains(ids, it.ID) {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 && route.ParentID != "" {
		ids = []string{route.ParentID}
	}
	if len(ids) == 0 {
		return nil, rejected(model.Validation, "no provider message to change: the conversation holds none on this channel"), nil
	}
	var threads []string
	for _, id := range ids {
		resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/messages/"+url.PathEscape(id), url.Values{"format": {"minimal"}})})
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			out, err := rejectedOr(err)
			return nil, out, err
		}
		m, err := decodeMessage(resp.body, id)
		if err == nil && m.ThreadID == "" {
			err = errUnexpectedAnswer()
		}
		if err != nil {
			return nil, adapter.Outcome{}, err
		}
		if !slices.Contains(threads, m.ThreadID) {
			threads = append(threads, m.ThreadID)
		}
	}
	if len(threads) == 0 {
		return nil, rejected(model.NotFound, "gmail holds none of the conversation's messages"), nil
	}
	return threads, adapter.Outcome{}, nil
}

// getThread reads thread id; format is minimal (label IDs) or metadata
// (with the headers named). A thread Gmail no longer holds is NotFound.
func (i *instance) getThread(ctx context.Context, s *session, id, format string, headers ...string) (gThread, error) {
	q := url.Values{"format": {format}}
	if len(headers) > 0 {
		q["metadataHeaders"] = headers
	}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/threads/"+url.PathEscape(id), q)})
	if err != nil {
		return gThread{}, transient(err)
	}
	var t gThread
	if err := decodeAnswer(resp.body, &t); err != nil {
		return gThread{}, err
	}
	if t.ID != id {
		return gThread{}, errUnexpectedAnswer()
	}
	return t, nil
}

// reconcileState reads the labels of every message of the conversation's
// threads back (users.threads.get, format=minimal): the change is applied
// when all of them show it. A conversation Gmail no longer holds is a final
// rejected not_found; a thread that does not show the change is failed with
// a transient failure, which allows a new attempt, since every verb is
// idempotent.
func (i *instance) reconcileState(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	var (
		c       stateChange
		refusal *model.Error
	)
	switch req.Capability {
	case model.CapabilityArchive:
		c = archiveChange(req.Undo)
	case model.CapabilityReadState:
		c = readChange(req.Read)
	case model.CapabilityTrash:
		c = trashChange(req.Undo)
	case model.CapabilityFlag:
		c, refusal = flagChange(req.FlagKind, req.Undo)
	case model.CapabilityMove:
		c, refusal = moveChange(req.Destination, req.AddLabels, req.RemoveLabels)
	}
	if refusal != nil {
		return rejected(refusal.Code, refusal.SafeMessage), nil
	}
	s := i.c.session()
	threads, out, err := i.threadsOf(ctx, s, req.Route, req.Items)
	if threads == nil {
		return out, err
	}
	for _, id := range threads {
		t, err := i.getThread(ctx, s, id, "minimal")
		switch {
		case model.IsCode(err, model.NotFound):
			return rejected(model.NotFound, "gmail no longer holds the conversation"), nil
		case err != nil:
			return adapter.Outcome{}, err
		case !c.shows(t.Messages):
			return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "gmail does not hold the change; a new attempt is safe")}, nil
		}
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

var (
	_ adapter.Archiver        = (*instance)(nil)
	_ adapter.Mover           = (*instance)(nil)
	_ adapter.ReadStateSetter = (*instance)(nil)
	_ adapter.Flagger         = (*instance)(nil)
	_ adapter.Trasher         = (*instance)(nil)
)
