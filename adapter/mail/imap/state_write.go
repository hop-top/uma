package imap

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/mail/mime"
	"hop.top/uma/model"
)

// The state verbs change what the account's mailbox holds: \Seen and
// \Flagged on the messages of a conversation, and which mailbox each sits
// in. They run on a session of their own, opened for the call and selected
// read-write; the receiver's session stays an EXAMINE, so retrieval never
// changes a flag and only these verbs write.
//
// Every verb is account-scoped and fans out per message: the request names
// one provider message reference per message of the conversation (see
// state_refs.go) and the adapter changes each message it finds.

func isStateCapability(c model.Capability) bool {
	switch c {
	case model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash:
		return true
	}
	return false
}

// stateStatuses reports the state capabilities with their limits: scope
// account for every verb, the folder move shape with the roles the server
// resolves, and the flag kinds the adapter maps. A verb is unavailable when
// the server cannot do it safely: a mailbox move needs MOVE or UIDPLUS, and
// archive and trash a mailbox with the matching special-use attribute.
func (i *Instance) stateStatuses(ctx context.Context) map[model.Capability]model.CapabilityStatus {
	si, err := i.serverInfo(ctx, nil)
	out := map[model.Capability]model.CapabilityStatus{}
	for _, c := range []model.Capability{model.CapabilityArchive, model.CapabilityMove, model.CapabilityReadState, model.CapabilityFlag, model.CapabilityTrash} {
		st := model.CapabilityStatus{Name: c, Available: true}
		lim := model.StateLimits{Scope: model.ScopeAccount}
		switch c {
		case model.CapabilityFlag:
			lim.Kinds = []string{model.FlagStar}
		case model.CapabilityMove:
			lim.Shape, lim.Roles = model.MoveShapeFolder, si.roleList()
		}
		_, hasArchive := si.roles[model.RoleArchive]
		_, hasTrash := si.roles[model.RoleTrash]
		switch {
		case err != nil:
			st.Available, st.Reason = false, model.Unavailable
		case (c == model.CapabilityMove || c == model.CapabilityArchive || c == model.CapabilityTrash) && !si.canMove(),
			c == model.CapabilityArchive && !hasArchive,
			c == model.CapabilityTrash && !hasTrash:
			st.Available, st.Reason = false, model.Unsupported
		}
		st.Limits = lim.Extension()
		out[c] = st
	}
	return out
}

// Archive moves the conversation to the mailbox with the \Archive attribute
// (\All where there is none), or, undone, back to the retrieved mailbox.
func (i *Instance) Archive(ctx context.Context, req adapter.ArchiveRequest) (adapter.Outcome, error) {
	return i.run(ctx, req.Items, func(r *stateRun, found []located) error {
		dest, err := r.placeOf(model.RoleArchive, req.Undo)
		if err != nil {
			return err
		}
		return r.move(found, dest)
	})
}

// Trash moves the conversation to the mailbox with the \Trash attribute, or,
// undone, back to the retrieved mailbox. It never expunges.
func (i *Instance) Trash(ctx context.Context, req adapter.TrashRequest) (adapter.Outcome, error) {
	return i.run(ctx, req.Items, func(r *stateRun, found []located) error {
		dest, err := r.placeOf(model.RoleTrash, req.Undo)
		if err != nil {
			return err
		}
		return r.move(found, dest)
	})
}

// Move moves the conversation to the mailbox named by the destination: its
// name, or a role resolved from the special-use attributes. Label changes
// are refused: a mailbox holds a message in one folder.
func (i *Instance) Move(ctx context.Context, req adapter.MoveRequest) (adapter.Outcome, error) {
	if len(req.Add)+len(req.Remove) > 0 || req.Destination.IsZero() {
		return rejection(model.Unsupported, "imap.mail: a mailbox move names one destination folder"), nil
	}
	return i.run(ctx, req.Items, func(r *stateRun, found []located) error {
		dest, err := r.destination(req.Destination)
		if err != nil {
			return err
		}
		return r.move(found, dest)
	})
}

// SetReadState sets or clears \Seen on every message of the conversation.
func (i *Instance) SetReadState(ctx context.Context, req adapter.ReadStateRequest) (adapter.Outcome, error) {
	return i.run(ctx, req.Items, func(r *stateRun, found []located) error {
		return r.store(found, imap.FlagSeen, req.Read)
	})
}

// Flag sets or clears \Flagged, the star, on every message of the
// conversation. No other kind has a flag here.
func (i *Instance) Flag(ctx context.Context, req adapter.FlagRequest) (adapter.Outcome, error) {
	if req.Kind != model.FlagStar {
		return rejection(model.Unsupported, "imap.mail: only the star flag is supported"), nil
	}
	return i.run(ctx, req.Items, func(r *stateRun, found []located) error {
		return r.store(found, imap.FlagFlagged, !req.Undo)
	})
}

// rejection is the final outcome of a request the adapter refuses before any
// provider call.
func rejection(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Rejected, Failure: model.NewError(code, msg)}
}

// located is a message found in the account under the reference the request
// named it by.
type located struct {
	ref model.ExternalRef
	loc loc
}

// stateRun is one state call on its session.
type stateRun struct {
	i    *Instance
	ctx  context.Context
	s    *session
	info serverInfo
	// sent is set once a command that changes the mailbox was sent: from
	// then on a lost connection leaves the outcome unknown. applied counts the
	// messages fully changed, partial the changes begun and not finished.
	sent, partial bool
	applied       int
	changes       []adapter.ReferenceChange
}

// run opens a session, finds the messages the request names and applies do
// to them.
func (i *Instance) run(ctx context.Context, items []model.ExternalRef, do func(*stateRun, []located) error) (adapter.Outcome, error) {
	if i.closed.Load() {
		return adapter.Outcome{}, i.errClosed()
	}
	if i.deps.Credentials == nil {
		return adapter.Outcome{}, i.noCredentials(PurposeIMAP)
	}
	if len(items) == 0 || slices.ContainsFunc(items, func(r model.ExternalRef) bool { return !i.isMessageRef(r) }) {
		return rejection(model.NotFound, "imap.mail: the conversation holds no provider message"), nil
	}
	s, err := i.connect(ctx)
	if err != nil {
		return adapter.Outcome{}, err
	}
	defer s.close()
	info, err := i.serverInfo(ctx, s)
	if err != nil {
		return adapter.Outcome{}, err
	}
	r := &stateRun{i: i, ctx: ctx, s: s, info: info}
	found, err := i.locate(ctx, s, info, items)
	if err != nil {
		return r.outcome(err)
	}
	if len(found) == 0 {
		return rejection(model.NotFound, "imap.mail: no such message in the mailbox"), nil
	}
	if err := do(r, found); err != nil {
		return r.outcome(err)
	}
	if r.applied == 0 {
		return rejection(model.NotFound, "imap.mail: no such message in the mailbox"), nil
	}
	return adapter.Outcome{State: model.ProviderAccepted, ReferenceChanges: r.changes}, nil
}

// refusal is an error that is the final answer to the request: the
// provider's own reply or a name it cannot resolve.
type refusal struct{ err *model.Error }

func (r *refusal) Error() string { return r.err.SafeMessage }

// outcome maps a failure of the call. A reply that refuses the command
// before anything changed is final; a failure once the mailbox may have
// changed, or a reply that refuses part of a fan-out, leaves the outcome
// unknown for Reconcile; a failure before that is the connection's.
func (r *stateRun) outcome(err error) (adapter.Outcome, error) {
	if r.ctx.Err() != nil {
		return adapter.Outcome{}, r.ctx.Err()
	}
	changed := r.applied > 0 || r.partial
	var ref *refusal
	var ie *imap.Error
	switch {
	case errors.As(err, &ref):
		if !changed {
			return adapter.Outcome{State: model.Rejected, Failure: ref.err}, nil
		}
	case errors.As(err, &ie):
		f := replyFailure(ie)
		if !changed {
			if f.Retry != model.NoRetry {
				return adapter.Outcome{State: model.Failed, Failure: f}, nil
			}
			return adapter.Outcome{State: model.Rejected, Failure: f}, nil
		}
	case !r.sent:
		return adapter.Outcome{}, lost(r.ctx, err)
	}
	return adapter.Outcome{State: model.Unknown, ReferenceChanges: r.changes,
		Failure: model.NewError(model.OutcomeUnknown, PurposeIMAP+": the mailbox change may be partly applied")}, nil
}

// replyFailure is the failure a NO or BAD reply stands for: a missing
// mailbox, a refused permission or a temporary condition; anything else is
// permanent.
func replyFailure(ie *imap.Error) *model.Error {
	switch ie.Code {
	case imap.ResponseCodeTryCreate, imap.ResponseCodeNonExistent:
		return model.NewError(model.NotFound, PurposeIMAP+": no such mailbox")
	case imap.ResponseCodeNoPerm, imap.ResponseCodeAuthorizationFailed:
		return model.NewError(model.Unauthorized, PurposeIMAP+": the account may not change this mailbox")
	case imap.ResponseCodeUnavailable, imap.ResponseCodeServerBug, imap.ResponseCodeOverQuota:
		return model.NewError(model.Transient, PurposeIMAP+": the server cannot change the mailbox now")
	}
	return model.NewError(model.Permanent, PurposeIMAP+": the server refused the change")
}

// placeOf is the mailbox a verb that has an undo moves to: the mailbox of
// role, or the retrieved mailbox when undone.
func (r *stateRun) placeOf(role string, undo bool) (string, error) {
	if undo {
		return r.i.cfg.mailbox(), nil
	}
	return r.destination(model.MoveDestination{Role: role})
}

// destination resolves a move destination to a mailbox name.
func (r *stateRun) destination(d model.MoveDestination) (string, error) {
	if d.Role != "" {
		if box, ok := r.info.roles[d.Role]; ok {
			return box, nil
		}
		return "", &refusal{model.NewError(model.Unsupported, "imap.mail: no mailbox with the special use of role "+d.Role)}
	}
	if !mailboxName(d.ID, false) {
		return "", &refusal{model.NewError(model.Validation, "imap.mail: the destination is not a mailbox name")}
	}
	return d.ID, nil
}

// byMailbox groups found by mailbox, in the order of first appearance.
func byMailbox(found []located) [][]located {
	var groups [][]located
	for _, f := range found {
		if k := slices.IndexFunc(groups, func(g []located) bool { return g[0].loc.mailbox == f.loc.mailbox }); k >= 0 {
			groups[k] = append(groups[k], f)
		} else {
			groups = append(groups, []located{f})
		}
	}
	return groups
}

// open selects mailbox read-write. It reports false for a mailbox that is
// gone or whose UIDVALIDITY is no longer the one the messages were found
// under: their UIDs are void.
func (r *stateRun) open(box string, uidValidity uint32) (bool, error) {
	var sel *imap.SelectData
	err := r.s.within(func() error {
		var err error
		sel, err = r.s.c.Select(box, nil).Wait()
		return err
	})
	var ie *imap.Error
	switch {
	case errors.As(err, &ie) && r.ctx.Err() == nil:
		return false, nil
	case err != nil:
		return false, err
	}
	return sel.UIDValidity == uidValidity, nil
}

// store sets or clears flag on every found message.
func (r *stateRun) store(found []located, flag imap.Flag, on bool) error {
	op := imap.StoreFlagsDel
	if on {
		op = imap.StoreFlagsAdd
	}
	for _, g := range byMailbox(found) {
		ok, err := r.open(g[0].loc.mailbox, g[0].loc.uidValidity)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		var set imap.UIDSet
		for _, f := range g {
			set.AddNum(imap.UID(f.loc.uid))
		}
		r.sent = true
		if err := r.s.within(func() error {
			return r.s.c.Store(set, &imap.StoreFlags{Op: op, Silent: true, Flags: []imap.Flag{flag}}, nil).Close()
		}); err != nil {
			return err
		}
		r.applied += len(g)
	}
	return nil
}

// move moves every found message to dest, one message at a time so each
// COPYUID names one pair, and records the reference each copy gets. A
// message already in dest is as it should be: applied, no new reference.
func (r *stateRun) move(found []located, dest string) error {
	var unresolved []located
	for _, g := range byMailbox(found) {
		if g[0].loc.mailbox == dest {
			r.applied += len(g)
			continue
		}
		ok, err := r.open(g[0].loc.mailbox, g[0].loc.uidValidity)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for _, f := range g {
			uv, uid, err := r.moveOne(f.loc.uid, dest)
			if err != nil {
				return err
			}
			r.applied++
			if uv == 0 || uid == 0 {
				unresolved = append(unresolved, f)
				continue
			}
			r.changed(f, loc{mailbox: dest, uidValidity: uv, uid: uid, messageID: f.loc.messageID})
		}
	}
	return r.resolve(unresolved, dest)
}

func (r *stateRun) changed(f located, now loc) {
	r.changes = append(r.changes, adapter.ReferenceChange{Old: f.ref, New: r.i.locationRef(now)})
}

// resolve finds the messages a move gave no COPYUID for, by their Message-ID
// in dest, and records their new references. A message it cannot find keeps
// its old reference.
func (r *stateRun) resolve(moved []located, dest string) error {
	if len(moved) == 0 {
		return nil
	}
	got, err := r.inMailbox(dest, moved)
	if err != nil {
		return err
	}
	for _, f := range moved {
		if l, ok := got[f.loc.messageID]; ok {
			r.changed(f, l)
		}
	}
	return nil
}

// moveOne moves the message uid of the selected mailbox to dest and returns
// the UIDVALIDITY and UID the copy has there, zero when the server says
// nothing (no COPYUID).
//
// With MOVE (RFC 6851) one command moves it. Without, it is COPY, then, once
// the server confirmed the copy, \Deleted and UID EXPUNGE of that UID alone
// (RFC 4315): the steps never overlap, so a refused COPY deletes nothing.
// That is why the client library's own fallback, which sends all three at
// once, is not used.
func (r *stateRun) moveOne(uid uint32, dest string) (uidValidity, newUID uint32, err error) {
	set := imap.UIDSetNum(imap.UID(uid))
	r.sent = true
	if r.info.move {
		var data *imapclient.MoveData
		if err := r.s.within(func() error {
			var err error
			data, err = r.s.c.Move(set, dest).Wait()
			return err
		}); err != nil {
			return 0, 0, err
		}
		return data.UIDValidity, firstUID(data.DestUIDs), nil
	}
	if !r.info.uidplus {
		return 0, 0, &refusal{model.NewError(model.Unsupported, "imap.mail: the server has neither MOVE nor UIDPLUS")}
	}
	var cd *imap.CopyData
	if err := r.s.within(func() error {
		var err error
		cd, err = r.s.c.Copy(set, dest).Wait()
		return err
	}); err != nil {
		return 0, 0, err
	}
	r.partial = true
	if err := r.s.within(func() error {
		return r.s.c.Store(set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
	}); err != nil {
		return 0, 0, err
	}
	if err := r.s.within(func() error { return r.s.c.UIDExpunge(set).Close() }); err != nil {
		return 0, 0, err
	}
	r.partial = false
	return cd.UIDValidity, firstUID(cd.DestUIDs), nil
}

// firstUID is the first UID of a COPYUID destination set, zero when there
// is none.
func firstUID(ns imap.NumSet) uint32 {
	if u, ok := ns.(imap.UIDSet); ok {
		if nums, ok := u.Nums(); ok && len(nums) > 0 {
			return uint32(nums[0])
		}
	}
	return 0
}

// inMailbox finds, in mailbox, the messages with the Message-IDs of moved.
func (r *stateRun) inMailbox(box string, moved []located) (map[string]loc, error) {
	var sel *imap.SelectData
	err := r.s.within(func() error {
		var err error
		sel, err = r.s.c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	})
	var ie *imap.Error
	switch {
	case errors.As(err, &ie) && r.ctx.Err() == nil:
		return nil, nil
	case err != nil:
		return nil, lost(r.ctx, err)
	}
	out := map[string]loc{}
	for _, f := range moved {
		uids, err := r.s.uidsByMessageID(f.loc.messageID)
		if err != nil {
			return nil, lost(r.ctx, err)
		}
		if len(uids) > 0 {
			out[f.loc.messageID] = loc{mailbox: box, uidValidity: sel.UIDValidity, uid: uint32(uids[len(uids)-1]), messageID: f.loc.messageID}
		}
	}
	return out, nil
}

// locate finds the messages the references name. A location is read where it
// says, while its UIDVALIDITY holds. A Message-ID is looked for in the
// mailboxes the adapter keeps mail in or follows: the retrieved mailbox, the
// sent-copies mailbox, the watched mailboxes and the mailboxes with the
// special use of archive, trash and junk, and nowhere else; a message a
// move gave a location is named by it.
func (i *Instance) locate(ctx context.Context, s *session, info serverInfo, refs []model.ExternalRef) ([]located, error) {
	type byLoc struct {
		ref model.ExternalRef
		loc loc
	}
	type byID struct {
		ref model.ExternalRef
		id  string
	}
	var order []string
	locs, ids := map[string][]byLoc{}, map[string][]byID{}
	visit := func(box string) {
		if !slices.Contains(order, box) {
			order = append(order, box)
		}
	}
	for _, ref := range refs {
		if l, ok := parseLocation(ref.ID); ok {
			visit(l.mailbox)
			locs[l.mailbox] = append(locs[l.mailbox], byLoc{ref, l})
			continue
		}
		for _, box := range i.followed(info) {
			visit(box)
			ids[box] = append(ids[box], byID{ref, ref.ID})
		}
	}
	var out []located
	seen := map[[2]any]bool{}
	add := func(l located) {
		k := [2]any{l.loc.mailbox, l.loc.uid}
		if !seen[k] {
			seen[k] = true
			out = append(out, l)
		}
	}
	for _, box := range order {
		var sel *imap.SelectData
		err := s.within(func() error {
			var err error
			sel, err = s.c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait()
			return err
		})
		var ie *imap.Error
		switch {
		case errors.As(err, &ie) && ctx.Err() == nil:
			continue // no such mailbox
		case err != nil:
			return nil, lost(ctx, err)
		}
		for _, w := range locs[box] {
			if sel.UIDValidity != w.loc.uidValidity {
				continue
			}
			var data *imap.SearchData
			if err := s.within(func() error {
				var err error
				data, err = s.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(imap.UID(w.loc.uid))}}, nil).Wait()
				return err
			}); err != nil {
				return nil, lost(ctx, err)
			}
			if slices.Contains(data.AllUIDs(), imap.UID(w.loc.uid)) {
				add(located(w))
			}
		}
		for _, w := range ids[box] {
			uids, err := s.uidsByMessageID(w.id)
			if err != nil {
				return nil, lost(ctx, err)
			}
			for _, u := range uids {
				add(located{ref: w.ref, loc: loc{mailbox: box, uidValidity: sel.UIDValidity, uid: uint32(u), messageID: w.id}})
			}
		}
	}
	return out, nil
}

// followed lists the mailboxes a Message-ID reference is looked up in.
func (i *Instance) followed(info serverInfo) []string {
	out := []string{i.cfg.mailbox()}
	add := func(box string) {
		if box != "" && !slices.Contains(out, box) {
			out = append(out, box)
		}
	}
	add(i.cfg.Sent.Mailbox)
	for _, role := range []string{model.RoleArchive, model.RoleTrash, model.RoleJunk} {
		add(info.roles[role])
	}
	for _, box := range i.cfg.WatchMailboxes {
		add(box)
	}
	return out
}

// uidsByMessageID returns the UIDs, ascending, of the messages of the
// selected mailbox whose Message-ID is exactly id. SEARCH HEADER matches
// substrings, so each hit's header is checked.
func (s *session) uidsByMessageID(id string) ([]imap.UID, error) {
	var out []imap.UID
	err := s.within(func() error {
		data, err := s.c.UIDSearch(&imap.SearchCriteria{
			Header: []imap.SearchCriteriaHeaderField{{Key: "Message-ID", Value: id}},
		}, nil).Wait()
		if err != nil {
			return err
		}
		uids := data.AllUIDs()
		if len(uids) == 0 {
			return nil
		}
		section := &imap.FetchItemBodySection{Peek: true, Specifier: imap.PartSpecifierHeader, HeaderFields: []string{"Message-ID"}}
		msgs, err := s.c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if hdr, err := mime.Parse(bytes.NewReader(m.FindBodySection(section)), mime.Limits{}); err == nil {
				if ids := model.ParseMsgIDs(hdr.Header.MessageID); len(ids) > 0 && ids[0] == id {
					out = append(out, m.UID)
				}
			}
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}
