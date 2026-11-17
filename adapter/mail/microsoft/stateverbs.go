package microsoft

import (
	"context"
	"errors"
	"net/url"
	"slices"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Provider conversation state. Every verb changes the mailbox's own view
// (scope account); the move shape is folder. A conversation is the
// messages of it that uma holds on the channel, so each verb is one Graph
// call per message, named by its immutable ID (every call sends
// Prefer: IdType="ImmutableId", so a message keeps its ID across folder
// moves and no verb reports a reference change):
//
//   - read, unread: PATCH {"isRead": bool};
//   - flag, unflag (kind star): PATCH {"flag": {"flagStatus": "flagged" |
//     "notFlagged"}}; kind important: PATCH {"importance": "high" |
//     "normal"};
//   - archive, trash and move: POST .../move with a destinationId, the
//     well-known folder name archive, deleteditems, inbox or junkemail for
//     a role, or the folder ID; unarchive and untrash move to
//     the configured undo folder (Config.UndoFolder), the inbox by default,
//     since Graph keeps no record of the folder a message came from.

// Well-known folder names Graph accepts in place of a folder ID, by the
// destination role they resolve.
const (
	folderInbox   = "inbox"
	folderArchive = "archive"
	folderTrash   = "deleteditems"
	folderJunk    = "junkemail"
)

// roleFolders maps a destination role to its well-known folder name.
var roleFolders = map[string]string{
	model.RoleInbox: folderInbox, model.RoleArchive: folderArchive,
	model.RoleTrash: folderTrash, model.RoleJunk: folderJunk,
}

// folderRoles maps a well-known folder name to its destination role.
var folderRoles = map[string]string{
	folderInbox: model.RoleInbox, folderArchive: model.RoleArchive,
	folderTrash: model.RoleTrash, folderJunk: model.RoleJunk,
}

// stateCapabilities are the provider state capabilities the adapter
// declares.
var stateCapabilities = []model.Capability{
	model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash,
}

// stateLimits are the limits a state capability reports, empty for another
// capability.
func stateLimits(c model.Capability) model.Extension {
	l := model.StateLimits{Scope: model.ScopeAccount}
	switch c {
	case model.CapabilityMove:
		l.Shape = model.MoveShapeFolder
		l.Roles = []string{model.RoleInbox, model.RoleArchive, model.RoleTrash, model.RoleJunk}
	case model.CapabilityFlag:
		l.Kinds = []string{model.FlagStar, model.FlagImportant}
	case model.CapabilityArchive, model.CapabilityReadState, model.CapabilityTrash:
	default:
		return model.Extension{}
	}
	return l.Extension()
}

// stateStep is one Graph call of a state verb on one message.
type stateStep struct {
	method string
	path   string // below the message
	body   []byte
	// move is set for a move: the call answers the moved message.
	move bool
}

// verb is a state request reduced to what Graph is asked: the call per
// message, and how a message is checked for it afterwards.
type verb struct {
	step func(id string) stateStep
	// holds reports whether a message read back shows the verb applied. dest
	// is the destination folder ID a move needs resolved, empty for other
	// verbs.
	holds func(m gMessage, dest string) bool
	// dest is the well-known name or folder ID a move goes to.
	dest string
}

func (i *instance) moveVerb(dest string) verb {
	return verb{
		dest: dest,
		step: func(id string) stateStep {
			return stateStep{method: "POST", path: "/move", body: encode(gMove{DestinationID: dest}), move: true}
		},
		holds: func(m gMessage, folderID string) bool { return folderID != "" && m.ParentFolderID == folderID },
	}
}

func readVerb(read bool) verb {
	return verb{
		step:  func(string) stateStep { return stateStep{method: "PATCH", body: encode(gRead{IsRead: read})} },
		holds: func(m gMessage, _ string) bool { return m.IsRead == read },
	}
}

// importantVerb sets importance high, or back to normal.
func importantVerb(undo bool) verb {
	level := importanceHigh
	if undo {
		level = importanceNormal
	}
	return verb{
		step: func(string) stateStep {
			return stateStep{method: "PATCH", body: encode(gImportanceUpdate{Importance: level})}
		},
		holds: func(m gMessage, _ string) bool { return (m.Importance == importanceHigh) == !undo },
	}
}

func flagVerb(kind string, undo bool) (verb, *model.Error) {
	switch kind {
	case model.FlagStar:
	case model.FlagImportant:
		return importantVerb(undo), nil
	default:
		return verb{}, &model.Error{Code: model.Unsupported, SafeMessage: "microsoft.mail flags star and important only", Retry: model.NoRetry}
	}
	status := flagFlagged
	if undo {
		status = flagNotFlagged
	}
	return verb{
		step: func(string) stateStep {
			return stateStep{method: "PATCH", body: encode(gFlagUpdate{Flag: gFlag{FlagStatus: status}})}
		},
		holds: func(m gMessage, _ string) bool { return (m.Flag != nil && m.Flag.FlagStatus == flagFlagged) == !undo },
	}, nil
}

// destination resolves a move destination to what Graph is sent: the
// well-known name of a role, else the folder ID.
func destination(d model.MoveDestination) (string, *model.Error) {
	if d.Role != "" {
		name, ok := roleFolders[d.Role]
		if !ok {
			return "", &model.Error{Code: model.Unsupported, SafeMessage: "microsoft.mail does not resolve the " + d.Role + " role", Retry: model.NoRetry}
		}
		return name, nil
	}
	if d.ID == "" {
		return "", &model.Error{Code: model.Validation, SafeMessage: "a move names a destination", Retry: model.NoRetry}
	}
	return d.ID, nil
}

// items checks the references a state request names: provider messages of
// this channel.
func (i *instance) items(refs []model.ExternalRef) ([]string, *model.Error) {
	if len(refs) == 0 {
		return nil, &model.Error{Code: model.Validation, SafeMessage: "a state request names no message", Retry: model.NoRetry}
	}
	ids := make([]string, 0, len(refs))
	for _, r := range refs {
		if r.Channel != i.ch.Ref || r.Namespace != model.ProviderMessageNamespace || r.Kind != model.RefKindMessage || r.ID == "" {
			return nil, &model.Error{Code: model.Validation, SafeMessage: "a state request names provider messages of the channel", Retry: model.NoRetry}
		}
		ids = append(ids, r.ID)
	}
	return ids, nil
}

// Archive moves the conversation's messages to Archive, or back to the
// inbox with Undo.
func (i *instance) Archive(ctx context.Context, r adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.apply(ctx, r.Route, r.Items, i.moveVerb(i.undoable(r.Undo, folderArchive)))
}

// Trash moves the conversation's messages to Deleted Items, or back to the
// inbox with Undo. It never deletes a message permanently.
func (i *instance) Trash(ctx context.Context, r adapter.TrashRequest) (adapter.Outcome, error) {
	return i.apply(ctx, r.Route, r.Items, i.moveVerb(i.undoable(r.Undo, folderTrash)))
}

// Move moves the conversation's messages to the destination folder.
// Label changes are not a folder shape's.
func (i *instance) Move(ctx context.Context, r adapter.MoveRequest) (adapter.Outcome, error) {
	if len(r.Add)+len(r.Remove) > 0 {
		return rejected(model.Unsupported, "microsoft.mail moves between folders; it has no label changes"), nil
	}
	dest, ferr := destination(r.Destination)
	if ferr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: ferr}, nil
	}
	return i.apply(ctx, r.Route, r.Items, i.moveVerb(dest))
}

// SetReadState marks the conversation's messages read or unread.
func (i *instance) SetReadState(ctx context.Context, r adapter.ReadStateRequest) (adapter.Outcome, error) {
	return i.apply(ctx, r.Route, r.Items, readVerb(r.Read))
}

// Flag sets or clears the Outlook follow-up flag, the star.
func (i *instance) Flag(ctx context.Context, r adapter.FlagRequest) (adapter.Outcome, error) {
	v, ferr := flagVerb(r.Kind, r.Undo)
	if ferr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: ferr}, nil
	}
	return i.apply(ctx, r.Route, r.Items, v)
}

// undoable is dest, or the configured undo folder (the inbox by default)
// for an undo: Graph keeps no record of the folder a message was in before.
func (i *instance) undoable(undo bool, dest string) string {
	if undo {
		if i.cfg.UndoFolder != "" {
			return i.cfg.UndoFolder
		}
		return folderInbox
	}
	return dest
}

// apply runs the verb on each message in turn. A lost answer, or any 5xx
// but 503, leaves the call possibly applied: the outcome is unknown and
// Reconcile reads the messages back (the verbs are idempotent, a move is
// not, so every verb reconciles by state). A failure after some messages
// were changed is unknown too: the change is partial. A refusal before any
// message changed is final. Graph keeps message IDs across the move, so no
// reference changes; if Graph ever answered a move with another ID, the
// change is reported.
func (i *instance) apply(ctx context.Context, route model.ExternalRef, refs []model.ExternalRef, v verb) (adapter.Outcome, error) {
	if err := i.checkRoute(route); err != nil {
		return adapter.Outcome{}, err
	}
	ids, verr := i.items(refs)
	if verr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: verr}, nil
	}
	s := i.c.session()
	var changes []adapter.ReferenceChange
	for n, id := range ids {
		st := v.step(id)
		resp, err := s.do(ctx, request{
			method: st.method, url: i.messageURL(id, st.path), body: st.body, contentType: "application/json",
			prefer: []string{preferImmutableID},
		})
		if err != nil {
			if errors.Is(err, errTransport) || sendAmbiguous(resp.status) || n > 0 {
				return adapter.Outcome{}, model.NewError(model.OutcomeUnknown, "graph state change answer lost or partial")
			}
			return rejectedOr(err)
		}
		if st.move {
			var m gMessage
			if decode(resp.body, &m) == nil && m.ID != "" && m.ID != id {
				changes = append(changes, adapter.ReferenceChange{Old: refs[n], New: model.ProviderMessageRef(i.ch.Ref, m.ID)})
			}
		}
	}
	return adapter.Outcome{State: model.ProviderAccepted, ReferenceChanges: changes}, nil
}

// reconcileState reads each message back and compares it with what the
// attempt asked: the verbs are checked by the state they leave, a move by
// the destination folder, so a move found there is applied.
func (i *instance) reconcileState(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	var v verb
	switch req.Capability {
	case model.CapabilityArchive:
		v = i.moveVerb(i.undoable(req.Undo, folderArchive))
	case model.CapabilityTrash:
		v = i.moveVerb(i.undoable(req.Undo, folderTrash))
	case model.CapabilityMove:
		dest, ferr := destination(req.Destination)
		if ferr != nil {
			return adapter.Outcome{State: model.Rejected, Failure: ferr}, nil
		}
		v = i.moveVerb(dest)
	case model.CapabilityReadState:
		v = readVerb(req.Read)
	default:
		var ferr *model.Error
		if v, ferr = flagVerb(req.FlagKind, req.Undo); ferr != nil {
			return adapter.Outcome{State: model.Rejected, Failure: ferr}, nil
		}
	}
	ids, verr := i.items(req.Items)
	if verr != nil {
		return adapter.Outcome{State: model.Rejected, Failure: verr}, nil
	}
	s := i.c.session()
	var folderID string
	if v.dest != "" {
		f, err := i.folder(ctx, s, v.dest)
		if model.IsCode(err, model.NotFound) {
			return rejected(model.NotFound, "destination folder not found"), nil
		}
		if err != nil {
			return adapter.Outcome{}, err
		}
		folderID = f.ID
	}
	for _, id := range ids {
		m, err := i.stateOf(ctx, s, id)
		if model.IsCode(err, model.NotFound) {
			return rejected(model.NotFound, "message not found"), nil
		}
		if err != nil {
			return adapter.Outcome{}, err
		}
		if !v.holds(m, folderID) {
			return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.Transient, "graph shows the state change not applied")}, nil
		}
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// stateSelect is the $select of a message state read.
const stateSelect = "id,parentFolderId,isRead,isDraft,flag,importance,lastModifiedDateTime"

// stateOf reads the state of message id.
func (i *instance) stateOf(ctx context.Context, s *session, id string) (gMessage, error) {
	q := url.Values{"$select": {stateSelect}}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/messages/"+url.PathEscape(id), q), prefer: []string{preferImmutableID}})
	if err != nil {
		return gMessage{}, transient(err)
	}
	var m gMessage
	if err := decode(resp.body, &m); err != nil {
		return gMessage{}, err
	}
	return m, nil
}

// folder reads a mail folder by well-known name or ID.
func (i *instance) folder(ctx context.Context, s *session, nameOrID string) (gMailFolder, error) {
	q := url.Values{"$select": {"id,displayName"}}
	resp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/mailFolders/"+url.PathEscape(nameOrID), q), prefer: []string{preferImmutableID}})
	if err != nil {
		return gMailFolder{}, transient(err)
	}
	var f gMailFolder
	if err := decode(resp.body, &f); err != nil {
		return gMailFolder{}, err
	}
	if f.ID == "" {
		return gMailFolder{}, model.NewError(model.NotFound, "folder has no ID")
	}
	return f, nil
}

// wellKnownFolders lists the well-known folders the adapter resolves, for
// the role of a folder ID.
func wellKnownFolders() []string {
	out := make([]string, 0, len(folderRoles))
	for n := range folderRoles {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

var (
	_ adapter.Archiver        = (*instance)(nil)
	_ adapter.Mover           = (*instance)(nil)
	_ adapter.ReadStateSetter = (*instance)(nil)
	_ adapter.Flagger         = (*instance)(nil)
	_ adapter.Trasher         = (*instance)(nil)
)
