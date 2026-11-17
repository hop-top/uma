// Package workitems holds the work item rules: how a provider observation
// of an issue or ticket binds and updates the work item of its
// conversation, how a WorkItems.Act update applies under the declared
// synchronization authority, and the work_item.updated payload both emit.
//
// A work item binds one provider issue or ticket to one conversation. The
// binding never changes: merges, moves, deletions and loss of access are
// binding states with history, so the conversation keeps everything it
// recorded. Each field has one source of truth. Fields the provider owns
// follow its latest state: no command changes them locally. WorkItems.Act
// may ask the provider to move a provider-owned status (Classify,
// PathProvider); the work item then changes only from the provider's
// answer, applied in source order like an inbound observation. Fields the
// work item declares local are seeded from the provider once and then
// change only by command, so a provider echo of a local change can never
// flip them back. Local changes never reach the provider.
//
// The functions here are pure apart from ID allocation and store reads;
// callers write the result and its event in their own unit.
package workitems

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// maxHistory bounds a work item's binding history; the oldest entries go
// first.
const maxHistory = 64

// Observation is one provider observation of a work item: the reference
// naming it, the provider state reported with it (nil for an envelope that
// reports none, such as a comment), and the source version of that state.
type Observation struct {
	External       model.ExternalRef
	State          *model.WorkItemState
	SourceRevision string
	OccurredAt     time.Time
	// Authoritative applies State whatever its source version: a copy the
	// provider was asked for again after a tie.
	Authoritative bool
}

// Change is a work item write: the record to store under Previous, empty
// for a new record, and the changed fields its event reports. Changes may be
// empty for a write no event reports, such as a new authority declaration.
type Change struct {
	Item     model.WorkItem
	Previous model.Revision
	Changes  []string
	Status   *model.WorkItemStatusChange
}

// Bound returns the work item bound to conv, or nil when none is. More
// than one is model.Conflict.
func Bound(ctx context.Context, r store.Reader, conv model.ConversationRef) (*model.WorkItem, error) {
	req := model.ListRequest{Filter: model.Filter{Conversation: &conv}, Page: model.PageRequest{Limit: 2}}
	var found []model.WorkItem
	for {
		p, err := r.WorkItems(ctx, req)
		if err != nil {
			return nil, err
		}
		for _, w := range p.Items {
			if w.Ref.Conversation == conv {
				found = append(found, w)
			}
		}
		if len(found) > 1 {
			return nil, model.NewError(model.Conflict, "conversation bound to more than one work item")
		}
		if p.Complete || p.Next == "" || p.Next == req.Page.After {
			break
		}
		req.Page.After = p.Next
	}
	if len(found) == 0 {
		return nil, nil
	}
	return &found[0], nil
}

// Import applies obs to the work item of conv. cur is the bound work item,
// nil when none is; a new one is created, inheriting seed (the work item of
// the conversation conv succeeds, if any) and taking every field of the
// observed state, local or not. An existing one takes the observed state
// unless it is stale, keeping its local fields. It reports false when
// nothing is to be written. newID allocates the record ID and revision.
func Import(cur, seed *model.WorkItem, conv model.ConversationRef, obs Observation, newID func() (model.ID, error)) (Change, bool, error) {
	var next model.WorkItem
	var ch Change
	creating := cur == nil
	if creating {
		id, err := newID()
		if err != nil {
			return Change{}, false, err
		}
		next = model.WorkItem{External: obs.External, Binding: model.WorkItemBindingActive}
		if seed != nil {
			next = seed.Clone()
			next.External = obs.External
		}
		next.Ref = model.WorkItemRef{Conversation: conv, ID: id}
		ch.Changes = []string{model.WorkItemFieldCreated}
	} else {
		next = cur.Clone()
		ch.Previous = cur.Revision
	}
	before := next.Clone()
	changed := creating
	if s := obs.State; s != nil && (obs.Authoritative || !stale(next.Source, obs)) {
		apply(&next, *s, creating && seed == nil, obs)
		changed = true
	}
	if !changed {
		return Change{}, false, nil
	}
	if !creating {
		ch.Changes, ch.Status = diff(before, next)
		if len(ch.Changes) == 0 && equalRecord(before, next) {
			return Change{}, false, nil
		}
	}
	rev, err := newID()
	if err != nil {
		return Change{}, false, err
	}
	next.Revision = model.Revision(rev)
	ch.Item = next
	return ch, true, nil
}

// stale reports whether obs is not newer than the applied source version:
// the same non-empty source revision, or a source time not after the
// applied one. Without source times observations apply in arrival order.
func stale(applied model.WorkItemSource, obs Observation) bool {
	if obs.SourceRevision != "" && obs.SourceRevision == applied.Revision {
		return true
	}
	return !obs.OccurredAt.IsZero() && !applied.OccurredAt.IsZero() && !obs.OccurredAt.After(applied.OccurredAt)
}

// Tied reports whether obs ties with cur's applied source version (the
// same non-empty source revision, or the same source time) while its
// state differs from cur's: a coarse provider revision covering two
// changes, which arrival order cannot settle.
func Tied(cur model.WorkItem, obs Observation) bool {
	if obs.State == nil || obs.Authoritative {
		return false
	}
	a := cur.Source
	same := (obs.SourceRevision != "" && obs.SourceRevision == a.Revision) ||
		(!obs.OccurredAt.IsZero() && obs.OccurredAt.Equal(a.OccurredAt))
	if !same {
		return false
	}
	next := cur.Clone()
	apply(&next, *obs.State, false, obs)
	changes, _ := diff(cur, next)
	return len(changes) > 0
}

// apply sets the provider's state on w. Fields declared local keep their
// value unless seeding, when the provider's value is the starting point.
// RawStatus is always the provider's: it is what the provider says.
func apply(w *model.WorkItem, s model.WorkItemState, seeding bool, obs Observation) {
	local := func(field string) bool { return !seeding && slices.Contains(s.LocalFields, field) }
	if !local(model.WorkItemFieldStatus) {
		w.Status = s.Status
	}
	w.RawStatus = s.RawStatus
	if !local(model.WorkItemFieldSubject) {
		w.Subject = s.Subject
	}
	if !local(model.WorkItemFieldAssignees) {
		w.Assignees = slices.Clone(s.Assignees)
	}
	if !local(model.WorkItemFieldFields) {
		w.Fields = cloneFields(s.Fields)
	}
	w.URL = s.URL
	w.Requester = s.Requester
	binding := s.Binding
	if binding == "" {
		binding = model.WorkItemBindingActive
	}
	if binding != w.Binding || !sameRef(s.Successor, w.Successor) {
		from := w.Binding
		if from == "" {
			from = model.WorkItemBindingActive
		}
		w.History = append(w.History, model.WorkItemBindingChange{
			From: from, To: binding, Successor: clonePtr(s.Successor),
			OccurredAt: obs.OccurredAt.UTC(), SourceRevision: obs.SourceRevision,
		})
		if n := len(w.History); n > maxHistory {
			w.History = slices.Clone(w.History[n-maxHistory:])
		}
		w.Binding, w.Successor = binding, clonePtr(s.Successor)
	}
	w.LocalFields = canonicalLocal(s.LocalFields)
	w.Source = model.WorkItemSource{Revision: obs.SourceRevision, OccurredAt: obs.OccurredAt.UTC()}
}

// Lose moves an active work item to binding, deleted or inaccessible, as
// an outbound refusal at showed, recording the transition. It reports false
// when the work item is already bound otherwise or binding is another
// state. The caller sets the new revision.
func Lose(cur model.WorkItem, binding string, at time.Time) (Change, bool) {
	if binding != model.WorkItemBindingDeleted && binding != model.WorkItemBindingInaccessible {
		return Change{}, false
	}
	if b := cur.Binding; b != "" && b != model.WorkItemBindingActive {
		return Change{}, false
	}
	next := cur.Clone()
	next.Binding, next.Successor = binding, nil
	next.History = append(next.History, model.WorkItemBindingChange{
		From: model.WorkItemBindingActive, To: binding, OccurredAt: at.UTC(),
	})
	if n := len(next.History); n > maxHistory {
		next.History = slices.Clone(next.History[n-maxHistory:])
	}
	changes, status := diff(cur, next)
	return Change{Item: next, Previous: cur.Revision, Changes: changes, Status: status}, true
}

// Update applies a WorkItems.Act update to cur on the local path. Classify
// refuses it first. An update Classify routes to the provider never
// applies locally: the provider's answer changes the work item. Here it is
// model.Conflict, since the caller routed it as local against another
// state of cur. A closed local status is terminal (model.Conflict). An
// update that changes nothing reports no changes. The caller sets the new
// revision.
func Update(cur model.WorkItem, u model.WorkItemUpdate) (Change, error) {
	path, err := Classify(cur, u)
	if err != nil {
		return Change{}, err
	}
	if path != PathLocal {
		return Change{}, model.NewError(model.Conflict, "work item status is provider-owned now; submit the action again")
	}
	next := cur.Clone()
	if s := u.Status; s != nil {
		if cur.Status == model.WorkItemClosed && *s != model.WorkItemClosed {
			return Change{}, model.NewError(model.Conflict, "a closed work item is terminal")
		}
		next.Status = *s
	}
	if s := u.Subject; s != nil {
		next.Subject = *s
	}
	if a := u.Assignees; a != nil {
		next.Assignees = slices.Clone(*a)
	}
	if f := u.Fields; f != nil {
		next.Fields = cloneFields(*f)
	}
	changes, status := diff(cur, next)
	return Change{Item: next, Previous: cur.Revision, Changes: changes, Status: status}, nil
}

// diff returns the work_item.updated changes between a and b.
func diff(a, b model.WorkItem) ([]string, *model.WorkItemStatusChange) {
	var out []string
	var status *model.WorkItemStatusChange
	if a.Status != b.Status || a.RawStatus != b.RawStatus {
		out = append(out, model.WorkItemFieldStatus)
		status = &model.WorkItemStatusChange{From: a.Status, To: b.Status, RawFrom: a.RawStatus, RawTo: b.RawStatus}
	}
	if a.Subject != b.Subject {
		out = append(out, model.WorkItemFieldSubject)
	}
	if a.URL != b.URL {
		out = append(out, model.WorkItemFieldURL)
	}
	if a.Requester != b.Requester {
		out = append(out, model.WorkItemFieldRequester)
	}
	if !slices.Equal(a.Assignees, b.Assignees) {
		out = append(out, model.WorkItemFieldAssignees)
	}
	if !equalFields(a.Fields, b.Fields) {
		out = append(out, model.WorkItemFieldFields)
	}
	if a.External != b.External || a.Binding != b.Binding || !sameRef(a.Successor, b.Successor) {
		out = append(out, model.WorkItemFieldExternal)
	}
	return out, status
}

// equalRecord reports whether a write would change nothing stored besides
// the revision: same fields, authority and source version.
func equalRecord(a, b model.WorkItem) bool {
	a.Revision, b.Revision = "", ""
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}

func equalFields(a, b []model.Extension) bool {
	return slices.EqualFunc(a, b, func(x, y model.Extension) bool {
		return x.Name == y.Name && x.Version == y.Version && string(x.Data) == string(y.Data)
	})
}

func cloneFields(xs []model.Extension) []model.Extension {
	if xs == nil {
		return nil
	}
	out := make([]model.Extension, len(xs))
	for i, x := range xs {
		out[i] = x.Clone()
	}
	return out
}

// canonicalLocal orders declared local fields as model.WorkItemLocalFields
// does, so equal declarations store equally.
func canonicalLocal(fs []string) []string {
	var out []string
	for _, f := range model.WorkItemLocalFields() {
		if slices.Contains(fs, f) {
			out = append(out, f)
		}
	}
	return out
}

func sameRef(a, b *model.ExternalRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func clonePtr(r *model.ExternalRef) *model.ExternalRef {
	if r == nil {
		return nil
	}
	v := *r
	return &v
}

// Payload returns the work_item.updated payload of c: provider-triggered
// with the observation's source revision, or command-triggered with the
// originating operation.
func Payload(c Change, trigger, sourceRevision string, op *model.OperationRef) model.WorkItemUpdatedPayload {
	return model.WorkItemUpdatedPayload{
		WorkItem: c.Item.Ref, Changes: slices.Clone(c.Changes), Trigger: trigger, Status: c.Status,
		PreviousRevision: c.Previous, SourceRevision: sourceRevision, Operation: op,
	}
}
