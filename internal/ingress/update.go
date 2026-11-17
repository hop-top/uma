package ingress

import (
	"encoding/json"
	"time"

	"hop.top/uma/model"
)

// sourceMetadataName is the core metadata entry recording which provider
// version of a message is applied: its source revision and source time.
// Later updates compare against it so a stale or reordered edit never
// overwrites newer content.
const sourceMetadataName = model.CoreNamespace + ".ingress.source"

type sourceVersion struct {
	SourceRevision string    `json:"source_revision,omitempty"`
	OccurredAt     time.Time `json:"occurred_at,omitzero"`
}

func sourceMetadata(env model.Envelope) model.Extension {
	data, _ := json.Marshal(sourceVersion{SourceRevision: env.SourceRevision, OccurredAt: env.OccurredAt.UTC()})
	return model.Extension{Name: sourceMetadataName, Version: 1, Data: data}
}

// appliedSource returns the source version recorded on m, if any.
func appliedSource(m model.Message) (sourceVersion, bool) {
	for _, x := range m.Metadata {
		if x.Name == sourceMetadataName && x.Version == 1 {
			var v sourceVersion
			if json.Unmarshal(x.Data, &v) == nil {
				return v, true
			}
		}
	}
	return sourceVersion{}, false
}

// staleUpdate reports whether an update at t is older than, or as old as, the
// version already applied. An update or applied version without a source
// time cannot be ordered and is applied in arrival order.
func staleUpdate(m model.Message, t time.Time) bool {
	v, ok := appliedSource(m)
	return ok && !t.IsZero() && !v.OccurredAt.IsZero() && !t.After(v.OccurredAt)
}

// applyUpdate applies an edit, deletion or reaction to the known message
// the envelope targets. Updates join the target's conversation and never
// select, create or reopen one, whatever its status. An unknown target is
// model.NotFound: nothing is recorded and the adapter keeps the update for
// redelivery once its message arrives.
func (u *ingressUnit) applyUpdate() error {
	// The marked echo of uma's own edit, delete or reaction is evidence for
	// that operation, whose settle applies it; ingress applies nothing.
	if echo, err := u.mutationEcho(); echo || err != nil {
		return err
	}
	target, err := u.updateTarget()
	if err != nil {
		return err
	}
	sender, err := u.resolveSender()
	if err != nil {
		return err
	}
	switch u.env.Kind {
	case model.EnvelopeReactionAdded, model.EnvelopeReactionRemoved:
		return u.applyReaction(target, sender)
	case model.EnvelopeMessageDeleted:
		if target.Deleted || target.Redacted {
			return nil
		}
		// A provider deletion tombstones the message; privacy redaction
		// (Redacted) is a separate, local operation.
		target.Deleted = true
		target.Content = model.Content{}
		target.Reactions, target.ReactionsComplete = nil, false
		return u.rewrite(target, changeDeleted)
	}
	if target.Deleted || target.Redacted {
		return nil
	}
	narrowed := narrows(target.Audience, u.env.Audience)
	if narrowed {
		target.Audience = u.env.Audience
	}
	if len(u.env.Content.Parts) == 0 || (!u.authoritative && staleUpdate(target, u.env.OccurredAt)) {
		// Keep the newer content, or the content when the edit carries
		// none; the ingress record retains the input. A narrowing applies
		// regardless of order. An edit tying with the applied version
		// over other content is refetched.
		if u.tiedEdit(target) {
			if err := u.refetch(model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID), messageResource(target.Ref)); err != nil {
				return err
			}
		}
		if narrowed {
			return u.store(target, changeAudience, false, nil)
		}
		return nil
	}
	if err := u.checkAttachments(u.env.Content, target.Audience); err != nil {
		return err
	}
	if sameContent(target.Content, u.env.Content) {
		// Nothing to apply: an unmarked echo of uma's own edit, or a
		// redelivery. A narrowing in the same envelope still applies.
		if narrowed {
			return u.store(target, changeAudience, false, nil)
		}
		return nil
	}
	target.Content = u.env.Content
	target.LastEdit = u.editRecord(sender)
	return u.rewrite(target, changeEdited)
}

// updateTarget is the stored message an update envelope names, by its
// provider message ID. This is the one place an update finds its target.
func (u *ingressUnit) updateTarget() (model.Message, error) {
	msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID), 2)
	switch {
	case err != nil:
		return model.Message{}, err
	case len(msgs) == 0:
		return model.Message{}, model.NewError(model.NotFound, "target message not found")
	case len(msgs) > 1:
		return model.Message{}, model.NewError(model.Conflict, "provider message ID stored more than once")
	}
	return msgs[0], nil
}

// narrows reports whether next narrows stored: a public audience made
// internal or restricted. Internal and restricted are not ordered against
// each other, and nothing widens.
func narrows(stored, next model.Audience) bool {
	return stored.Visibility == model.Public && (next.Visibility == model.Internal || next.Visibility == model.Restricted)
}

// editRecord is the editor record of an inbound content edit: the
// envelope's sender (none for a system actor), the sink's principal and
// the envelope's time.
func (u *ingressUnit) editRecord(sender *model.Identity) *model.Edit {
	e := &model.Edit{Actor: u.s.principal.Actor, At: u.env.OccurredAt.UTC()}
	if sender != nil {
		by := sender.Ref
		e.By = &by
	}
	return e
}

// rewrite stores a changed message under a new revision with the applied
// source version and emits its message.updated event.
func (u *ingressUnit) rewrite(m model.Message, change string) error {
	return u.store(m, change, true, nil)
}

// store writes a changed message under a new revision, recording the
// envelope as its applied source version when source is set, and emits
// its message.updated event, carrying reaction when the change is one.
func (u *ingressUnit) store(m model.Message, change string, source bool, reaction *model.PayloadReaction) error {
	prev, m, err := u.put(m, source)
	if err != nil {
		return err
	}
	return u.messageUpdated(m, change, prev, reaction)
}

// put writes a changed message under a new revision, recording the
// envelope as its applied source version when source is set. It returns
// the revision the message changed from and the message as stored.
func (u *ingressUnit) put(m model.Message, source bool) (model.Revision, model.Message, error) {
	prev := m.Revision
	rev, err := u.newRevision()
	if err != nil {
		return "", m, err
	}
	m.Revision = rev
	if source {
		m.Metadata = u.metadata(m.Metadata)
	}
	return prev, m, u.tx.PutMessage(u.ctx, m, prev)
}
