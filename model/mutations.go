package model

import "time"

// Edit records who last changed a message's content, kept apart from its
// Author, which never changes. An outbound edit applied after provider
// success sets Actor to the operation's caller and Operation to it, with By
// nil. An inbound provider edit, including a re-reported provider message,
// sets By to the resolved sender (nil for a system actor), Actor to the
// ingress sink's principal and At to the envelope's OccurredAt. Audience
// narrowing, deletion and reactions leave it unchanged.
type Edit struct {
	// Actor is the uma principal that applied the edit: the command's
	// caller outbound, the ingress sink's principal inbound. Required.
	Actor ActorID `json:"actor"`
	// By is the provider identity that made an inbound edit; nil for
	// outbound edits and for a system actor.
	By *IdentityRef `json:"by,omitempty"`
	// At is when the edit happened: the settle time outbound, the
	// envelope's OccurredAt inbound (zero when the provider gave none).
	At time.Time `json:"at"`
	// Operation is the edit operation of an outbound edit; nil inbound.
	Operation *OperationRef `json:"operation,omitempty"`
}

// Validate checks the actor token and the optional references.
func (e Edit) Validate() error { return e.validate("") }

func (e Edit) validate(path string) error {
	var by, op error
	if e.By != nil {
		by = e.By.validate(fieldPath(path, "by"), false)
	}
	if e.Operation != nil {
		op = e.Operation.validate(fieldPath(path, "operation"), false)
	}
	return firstError(checkToken(fieldPath(path, "actor"), string(e.Actor)), by, op)
}

// Clone returns a deep copy.
func (e Edit) Clone() Edit {
	e.By = clonePtr(e.By, same)
	e.Operation = clonePtr(e.Operation, same)
	return e
}

// PendingMutation is the not-yet-applied change of an accepted edit,
// delete or react operation, with the base it was accepted against. It is
// separate from canonical content: a message changes only once the
// provider applied the effect or an authoritative inbound copy confirmed
// it, and Messages.Get and Messages.List never carry pending content.
//
// Operations.Get derives it from the operation's stored intent while the
// operation is unsettled (any state but provider_accepted, delivered,
// failed, rejected and committed) and the caller may read the target
// message; it is never stored, never emitted in an event and absent from
// an Acceptance. Exactly one of Content, Delete and Reaction is set.
type PendingMutation struct {
	// Operation is the mutation operation.
	Operation OperationRef `json:"operation"`
	// BaseRevision is the target's revision when the operation was
	// accepted.
	BaseRevision Revision `json:"base_revision"`
	// BaseLastEdit is the target's LastEdit when the operation was
	// accepted; nil when the target had never been edited.
	BaseLastEdit *Edit `json:"base_last_edit,omitempty"`
	// Content is the replacement content of an edit.
	Content *Content `json:"content,omitempty"`
	// Delete marks a delete.
	Delete bool `json:"delete"`
	// Reaction is the reaction a react operation adds or removes.
	Reaction *PendingReaction `json:"reaction,omitempty"`
}

// Validate checks the operation, the base and that exactly one change is
// set and valid.
func (p PendingMutation) Validate() error {
	const path = "pending"
	set := 0
	for _, b := range []bool{p.Content != nil, p.Delete, p.Reaction != nil} {
		if b {
			set++
		}
	}
	var change, base, content, reaction error
	if set != 1 {
		change = invalidField(path, "needs exactly one of content, delete and reaction")
	}
	if p.BaseLastEdit != nil {
		base = p.BaseLastEdit.validate(fieldPath(path, "base_last_edit"))
	}
	if p.Content != nil {
		content = p.Content.validate(fieldPath(path, "content"), DefaultContentLimits(), nil)
	}
	if p.Reaction != nil {
		reaction = p.Reaction.validate(fieldPath(path, "reaction"))
	}
	var rev error
	if p.BaseRevision == "" {
		rev = invalidField(fieldPath(path, "base_revision"), "required")
	}
	return firstError(change, p.Operation.validate(fieldPath(path, "operation"), false), rev, base, content, reaction)
}

// Clone returns a deep copy.
func (p PendingMutation) Clone() PendingMutation {
	p.BaseLastEdit = clonePtr(p.BaseLastEdit, Edit.Clone)
	p.Content = clonePtr(p.Content, Content.Clone)
	p.Reaction = clonePtr(p.Reaction, same)
	return p
}

// PendingReaction is the reaction of a pending react operation: Emoji is
// added, or removed when Remove is set.
type PendingReaction struct {
	// Emoji follows ValidateEmoji.
	Emoji string `json:"emoji"`
	// Remove marks a removal.
	Remove bool `json:"remove"`
}

// Validate applies the emoji rule.
func (r PendingReaction) Validate() error { return r.validate("") }

func (r PendingReaction) validate(path string) error {
	return checkEmoji(fieldPath(path, "emoji"), r.Emoji)
}
