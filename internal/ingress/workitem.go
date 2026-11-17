package ingress

import (
	"slices"

	"hop.top/uma/internal/workitems"
	"hop.top/uma/model"
)

// Work items. In a per-work-item inbox every new message and every
// work_item envelope binds the work item of its conversation: the first
// one, whatever it is, creates the binding (so a comment observed before
// its issue binds the same work item the issue later fills in), and each
// reported provider state updates it under the rules of package workitems.

// checkWorkItem validates the work item state an envelope carries: only
// message and work_item envelopes carry one, a work_item envelope always
// does and carries no content, and every reference in it belongs to the
// bound channel.
func (s *Sink) checkWorkItem(env model.Envelope) error {
	w := env.WorkItem
	switch env.Kind {
	case model.EnvelopeWorkItem:
		if w == nil {
			return model.NewError(model.Validation, "envelope.work_item: required by a work_item envelope")
		}
		if len(env.Content.Parts) > 0 {
			return model.NewError(model.Validation, "envelope.content: a work_item envelope carries no content")
		}
	case model.EnvelopeMessage, model.EnvelopeAutomatedMessage:
		if w == nil {
			return nil
		}
	default:
		if w != nil {
			return model.NewError(model.Validation, "envelope.work_item: only message and work_item envelopes carry one")
		}
		return nil
	}
	if err := w.Validate(); err != nil {
		return err
	}
	if !slices.ContainsFunc(env.References, isWorkItemRef) {
		return model.NewError(model.Validation, "envelope.references: work item state requires a work_item reference")
	}
	refs := slices.Clone(w.Assignees)
	if !w.Requester.IsZero() {
		refs = append(refs, w.Requester)
	}
	if w.Successor != nil {
		refs = append(refs, *w.Successor)
	}
	for _, r := range refs {
		if err := s.ownRef("envelope.work_item", r); err != nil {
			return err
		}
	}
	return nil
}

func isWorkItemRef(r model.ExternalRef) bool { return r.Kind == model.RefKindWorkItem }

// requireWorkItemMode rejects work item state outside a per-work-item inbox.
func (u *ingressUnit) requireWorkItemMode() error {
	if u.env.WorkItem != nil && u.inbox.Selection.Mode != model.ModePerWorkItem {
		return model.NewError(model.Validation, "envelope.work_item: accepted only by a per-work-item inbox")
	}
	return nil
}

// applyWorkItem records a provider-side work item change. It updates the
// newest conversation bound to the work item, whatever its status, and
// creates one only when none is: a metadata change never opens a successor
// conversation or reopens one.
func (u *ingressUnit) applyWorkItem() error {
	if u.inbox.Selection.Mode != model.ModePerWorkItem {
		return model.NewError(model.Validation, "envelope.kind: work_item requires a per-work-item inbox")
	}
	sc, err := u.scope(nil)
	if err != nil {
		return err
	}
	cands, err := u.tx.Candidates(u.ctx, sc.group, candidateLimit)
	if err != nil {
		return err
	}
	if len(cands) > 0 {
		latest, err := newest(cands)
		if err != nil {
			return err
		}
		return u.bindWorkItem(latest, false)
	}
	conv, created, err := u.selectConversation(nil)
	if err != nil {
		return err
	}
	return u.bindWorkItem(conv, created)
}

// bindWorkItem applies the envelope's observation to the work item of conv
// in a per-work-item inbox and emits work_item.updated for changed fields.
// A conversation created as the successor of a closed one inherits its
// predecessor's work item.
func (u *ingressUnit) bindWorkItem(conv model.Conversation, created bool) error {
	if u.inbox.Selection.Mode != model.ModePerWorkItem {
		return nil
	}
	i := slices.IndexFunc(u.env.References, isWorkItemRef)
	if i < 0 {
		return model.NewError(model.Validation, "envelope.references: per_work_item selection requires a work_item reference")
	}
	cur, err := workitems.Bound(u.ctx, u.tx, conv.Ref)
	if err != nil {
		return err
	}
	var seed *model.WorkItem
	if cur == nil && created {
		if seed, err = u.predecessorItem(conv.Ref); err != nil {
			return err
		}
	}
	obs := workitems.Observation{
		External: u.env.References[i], State: u.env.WorkItem,
		SourceRevision: u.env.SourceRevision, OccurredAt: u.env.OccurredAt, Authoritative: u.authoritative,
	}
	if u.tiedWorkItem(cur, obs) {
		if err := u.refetch(u.env.References[i], cur.Ref.Ref()); err != nil {
			return err
		}
	}
	ch, ok, err := workitems.Import(cur, seed, conv.Ref, obs, u.newID)
	if err != nil || !ok {
		return err
	}
	if err := u.tx.PutWorkItem(u.ctx, ch.Item, ch.Previous); err != nil {
		return err
	}
	if len(ch.Changes) == 0 {
		return nil
	}
	return u.event(model.EventWorkItemUpdated, model.SchemaWorkItemUpdated, ch.Item.Ref.Ref(), ch.Item.Revision,
		workitems.Payload(ch, model.TriggerProvider, u.env.SourceRevision, nil))
}

// predecessorItem returns the work item of the newest other conversation
// of the latest selection's candidate group, if any.
func (u *ingressUnit) predecessorItem(conv model.ConversationRef) (*model.WorkItem, error) {
	var prior []model.Conversation
	for _, c := range u.cands {
		if c.Ref != conv {
			prior = append(prior, c)
		}
	}
	if len(prior) == 0 {
		return nil, nil
	}
	p, err := newest(prior)
	if err != nil {
		return nil, err
	}
	return workitems.Bound(u.ctx, u.tx, p.Ref)
}
