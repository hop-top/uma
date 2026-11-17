package store

import (
	"bytes"
	"slices"
)

// Clone methods return snapshots sharing no slice backing array, pointer
// target or byte buffer with the receiver. Nil stays nil and empty stays
// empty. Backends use them to copy values in and out of retained state.

// Clone returns a deep copy.
func (p ProtocolState) Clone() ProtocolState {
	p.Value = p.Value.Clone()
	return p
}

// Clone returns a deep copy.
func (e RecipientLogEntry) Clone() RecipientLogEntry {
	e.Value = e.Value.Clone()
	return e
}

// Clone returns a deep copy.
func (r IngressRecord) Clone() IngressRecord {
	r.Input.Envelope = r.Input.Envelope.Clone()
	r.Receipt = r.Receipt.Clone()
	return r
}

// Clone returns a deep copy.
func (r OperationRecord) Clone() OperationRecord {
	r.Operation = r.Operation.Clone()
	r.Fingerprint = bytes.Clone(r.Fingerprint)
	r.Command = r.Command.Clone()
	return r
}

// Clone returns a deep copy.
func (e AttemptEvidence) Clone() AttemptEvidence {
	e.ProviderReferences = slices.Clone(e.ProviderReferences)
	if e.Failure != nil {
		f := *e.Failure
		e.Failure = &f
	}
	if e.ReadAt != nil {
		at := *e.ReadAt
		e.ReadAt = &at
	}
	return e
}

// Clone returns a deep copy.
func (r SubscriptionRecord) Clone() SubscriptionRecord {
	r.Filter = r.Filter.Clone()
	if r.PendingBatch != nil {
		b := r.PendingBatch.Clone()
		r.PendingBatch = &b
	}
	if r.ClosedAt != nil {
		at := *r.ClosedAt
		r.ClosedAt = &at
	}
	return r
}

// Clone returns a deep copy.
func (b BatchRecord) Clone() BatchRecord {
	b.EventIDs = slices.Clone(b.EventIDs)
	b.TokenDigest = bytes.Clone(b.TokenDigest)
	return b
}

// Clone returns a deep copy.
func (r ExportRecord) Clone() ExportRecord {
	r.Export = r.Export.Clone()
	if r.Manifest != nil {
		m := r.Manifest.Clone()
		r.Manifest = &m
	}
	return r
}

// Clone returns a deep copy.
func (m ExportManifest) Clone() ExportManifest {
	m.Contacts = slices.Clone(m.Contacts)
	m.Identities = slices.Clone(m.Identities)
	m.Conversations = slices.Clone(m.Conversations)
	m.Messages = slices.Clone(m.Messages)
	m.Interactions = slices.Clone(m.Interactions)
	m.Attachments = slices.Clone(m.Attachments)
	m.Labels = slices.Clone(m.Labels)
	m.Tombstones = slices.Clone(m.Tombstones)
	return m
}

// Clone returns a deep copy.
func (r AttachmentRecord) Clone() AttachmentRecord {
	if r.Blob != nil {
		k := *r.Blob
		r.Blob = &k
	}
	if r.Provider != nil {
		p := *r.Provider
		r.Provider = &p
	}
	return r
}

// Clone returns a deep copy.
func (c WorkClaim) Clone() WorkClaim {
	c.Kinds = slices.Clone(c.Kinds)
	return c
}

// Clone returns a deep copy.
func (f LinkFilter) Clone() LinkFilter {
	if f.Identity != nil {
		i := *f.Identity
		f.Identity = &i
	}
	if f.Contact != nil {
		c := *f.Contact
		f.Contact = &c
	}
	return f
}

// Clone returns a deep copy.
func (c MessageClaim) Clone() MessageClaim {
	c.Message = c.Message.Clone()
	return c
}

// Clone returns a deep copy.
func (c ConversationClaim) Clone() ConversationClaim {
	c.Conversation = c.Conversation.Clone()
	return c
}
