package imap

import (
	"context"
	"errors"
	"slices"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// reconcileState answers what became of a state change whose answer was
// lost, by reading the mailbox.
//
// A flag change is idempotent: the messages are found and their flags read;
// all as asked is applied, else the attempt did not apply and is safe to
// repeat. A move is not: repeating one whose answer was lost finds nothing
// at the source. So a move is looked up in its destination, and a message
// found there is applied, with the reference its copy has; one found at its
// source is not applied; one found in neither is reported not found; a mix
// leaves the outcome unknown. This is derived from what the commands do,
// not observed: a message that was in the destination before the attempt
// reads as applied.
func (i *Instance) reconcileState(ctx context.Context, req adapter.ReconcileRequest) (adapter.Outcome, error) {
	if i.deps.Credentials == nil {
		return adapter.Outcome{}, i.noCredentials(PurposeIMAP)
	}
	if len(req.Items) == 0 || slices.ContainsFunc(req.Items, func(r model.ExternalRef) bool { return !i.isMessageRef(r) }) {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "imap.mail: the conversation holds no provider message")}, nil
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
	found, err := i.locate(ctx, s, info, req.Items)
	if err != nil {
		return adapter.Outcome{}, err
	}
	var out adapter.Outcome
	switch req.Capability {
	case model.CapabilityReadState:
		out, err = r.reconcileFlag(found, imap.FlagSeen, req.Read)
	case model.CapabilityFlag:
		if req.FlagKind != model.FlagStar {
			return rejection(model.Unsupported, "imap.mail: only the star flag is supported"), nil
		}
		out, err = r.reconcileFlag(found, imap.FlagFlagged, !req.Undo)
	default:
		var dest string
		if dest, err = r.reconcileDestination(req); err != nil {
			break
		}
		out, err = r.reconcileMove(req.Items, found, dest)
	}
	var ref *refusal
	if errors.As(err, &ref) {
		return adapter.Outcome{State: model.Rejected, Failure: ref.err}, nil
	}
	return out, err
}

// reconcileDestination is the mailbox the attempt moved to.
func (r *stateRun) reconcileDestination(req adapter.ReconcileRequest) (string, error) {
	switch req.Capability {
	case model.CapabilityArchive:
		return r.placeOf(model.RoleArchive, req.Undo)
	case model.CapabilityTrash:
		return r.placeOf(model.RoleTrash, req.Undo)
	}
	return r.destination(req.Destination)
}

// notApplied is the answer for an attempt the mailbox shows was not applied.
func notApplied(code model.ErrorCode, msg string) adapter.Outcome {
	return adapter.Outcome{State: model.Failed, Failure: model.NewError(code, msg)}
}

// reconcileFlag reads the flags of the found messages.
func (r *stateRun) reconcileFlag(found []located, flag imap.Flag, want bool) (adapter.Outcome, error) {
	if len(found) == 0 {
		return notApplied(model.NotFound, "imap.mail: no such message in the mailbox"), nil
	}
	for _, g := range byMailbox(found) {
		ok, err := r.examine(g[0].loc.mailbox, g[0].loc.uidValidity)
		if err != nil {
			return adapter.Outcome{}, err
		}
		if !ok {
			continue
		}
		var set imap.UIDSet
		for _, f := range g {
			set.AddNum(imap.UID(f.loc.uid))
		}
		var msgs []*fetchedFlags
		if err := r.s.within(func() error {
			var err error
			msgs, err = collectFlags(r.s, set)
			return err
		}); err != nil {
			return adapter.Outcome{}, lost(r.ctx, err)
		}
		for _, m := range msgs {
			if slices.Contains(m.flags, flag) != want {
				return notApplied(model.Transient, "imap.mail: the mailbox does not show the change"), nil
			}
		}
	}
	return adapter.Outcome{State: model.ProviderAccepted}, nil
}

// examine opens mailbox read-only; false when it is gone or its
// UIDVALIDITY is not the one the messages were found under.
func (r *stateRun) examine(box string, uidValidity uint32) (bool, error) {
	var sel *imap.SelectData
	err := r.s.within(func() error {
		var err error
		sel, err = r.s.c.Select(box, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	})
	var ie *imap.Error
	switch {
	case errors.As(err, &ie) && r.ctx.Err() == nil:
		return false, nil
	case err != nil:
		return false, lost(r.ctx, err)
	}
	return sel.UIDValidity == uidValidity, nil
}

// reconcileMove looks the messages of a move up in dest.
func (r *stateRun) reconcileMove(items []model.ExternalRef, atSource []located, dest string) (adapter.Outcome, error) {
	var asked []located
	for _, ref := range items {
		id := ref.ID
		if l, ok := parseLocation(id); ok {
			id = l.messageID
		}
		asked = append(asked, located{ref: ref, loc: loc{messageID: id}})
	}
	there, err := r.inMailbox(dest, asked)
	if err != nil {
		return adapter.Outcome{}, err
	}
	var changes []adapter.ReferenceChange
	inDest := 0
	for _, a := range asked {
		l, ok := there[a.loc.messageID]
		if !ok {
			continue
		}
		inDest++
		if cur, isLoc := parseLocation(a.ref.ID); isLoc && cur.mailbox == dest && cur.uidValidity == l.uidValidity && cur.uid == l.uid {
			continue // named by where it is
		}
		changes = append(changes, adapter.ReferenceChange{Old: a.ref, New: r.i.locationRef(l)})
	}
	switch {
	case inDest == len(asked):
		return adapter.Outcome{State: model.ProviderAccepted, ReferenceChanges: changes}, nil
	case inDest == 0 && len(atSource) > 0:
		return notApplied(model.Transient, "imap.mail: the messages are still at the source"), nil
	case inDest == 0:
		return notApplied(model.NotFound, "imap.mail: no such message in the mailbox"), nil
	}
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, PurposeIMAP+": the move is partly applied")}, nil
}

// fetchedFlags is the flags of one message.
type fetchedFlags struct {
	uid   uint32
	flags []imap.Flag
}

// collectFlags fetches the flags of the UIDs of the selected mailbox.
func collectFlags(s *session, set imap.UIDSet) ([]*fetchedFlags, error) {
	return collectFlagsOpts(s, set, &imap.FetchOptions{UID: true, Flags: true})
}
