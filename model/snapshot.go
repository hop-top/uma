package model

import (
	"bytes"
	"maps"
	"slices"
)

// Clone methods return snapshots that share no slice backing array, map,
// pointer target or byte buffer with the receiver. Nil stays nil and empty
// stays empty. The engine clones values it returns or retains, so callers
// never alias engine state.

func cloneEach[T any](s []T, f func(T) T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	for i, v := range s {
		out[i] = f(v)
	}
	return out
}

func clonePtr[T any](p *T, f func(T) T) *T {
	if p == nil {
		return nil
	}
	v := f(*p)
	return &v
}

func same[T any](v T) T { return v }

// Clone returns a deep copy.
func (e Extension) Clone() Extension {
	e.Data = bytes.Clone(e.Data)
	return e
}

// Clone returns a deep copy.
func (p Part) Clone() Part {
	p.Attachment = clonePtr(p.Attachment, same)
	p.Structured = clonePtr(p.Structured, Extension.Clone)
	return p
}

// Clone returns a deep copy.
func (c Content) Clone() Content {
	c.Parts = cloneEach(c.Parts, Part.Clone)
	return c
}

// Clone returns a deep copy.
func (e Export) Clone() Export {
	e.Filter = e.Filter.Clone()
	return e
}

// Clone returns a deep copy.
func (l ExportLine) Clone() ExportLine {
	l.Data = slices.Clone(l.Data)
	return l
}

// Clone returns a deep copy.
func (f Filter) Clone() Filter {
	f.Inboxes = slices.Clone(f.Inboxes)
	f.Conversation = clonePtr(f.Conversation, same)
	f.Contact = clonePtr(f.Contact, same)
	f.Kinds = slices.Clone(f.Kinds)
	f.Statuses = slices.Clone(f.Statuses)
	f.Identity = clonePtr(f.Identity, same)
	f.Labels = clonePtr(f.Labels, LabelSelector.Clone)
	f.Task = clonePtr(f.Task, same)
	f.Since = clonePtr(f.Since, same)
	f.Before = clonePtr(f.Before, same)
	f.Attachments = clonePtr(f.Attachments, AttachmentFilter.Clone)
	f.Assignee = clonePtr(f.Assignee, same)
	f.Snoozed = clonePtr(f.Snoozed, same)
	f.Linked = clonePtr(f.Linked, same)
	f.Unread = clonePtr(f.Unread, same)
	f.Actor = clonePtr(f.Actor, same)
	return f
}

// Clone returns a deep copy.
func (r ListRequest) Clone() ListRequest {
	r.Filter = r.Filter.Clone()
	return r
}

// Clone returns a deep copy.
func (s Selection) Clone() Selection {
	s.LockToSingle = clonePtr(s.LockToSingle, same)
	return s
}

// Clone returns a deep copy.
func (i Identity) Clone() Identity {
	i.Attributes = slices.Clone(i.Attributes)
	return i
}

// Clone returns a deep copy.
func (m Message) Clone() Message {
	m.Content = m.Content.Clone()
	m.External = slices.Clone(m.External)
	m.Task = clonePtr(m.Task, same)
	m.Origin = clonePtr(m.Origin, same)
	m.Metadata = cloneEach(m.Metadata, Extension.Clone)
	m.EchoOf = clonePtr(m.EchoOf, same)
	m.Reactions = cloneEach(m.Reactions, MessageReaction.Clone)
	m.LastEdit = clonePtr(m.LastEdit, Edit.Clone)
	m.InReplyTo = clonePtr(m.InReplyTo, same)
	m.InResponseTo = clonePtr(m.InResponseTo, same)
	return m
}

// Clone returns a deep copy.
func (c Conversation) Clone() Conversation {
	c.Owner = clonePtr(c.Owner, same)
	c.Participants = slices.Clone(c.Participants)
	c.External = slices.Clone(c.External)
	c.Labels = slices.Clone(c.Labels)
	c.Assignee = clonePtr(c.Assignee, same)
	c.Snooze = clonePtr(c.Snooze, same)
	c.Links = cloneEach(c.Links, ConversationLink.Clone)
	c.ProviderState = cloneEach(c.ProviderState, ProviderConversationState.Clone)
	return c
}

// Clone returns a deep copy.
func (i Inbox) Clone() Inbox {
	i.Destinations = slices.Clone(i.Destinations)
	i.Selection = i.Selection.Clone()
	return i
}

// Clone returns a deep copy.
func (c Channel) Clone() Channel {
	c.Configuration = c.Configuration.Clone()
	return c
}

// Clone returns a deep copy.
func (w WorkItem) Clone() WorkItem {
	w.Assignees = slices.Clone(w.Assignees)
	w.Fields = cloneEach(w.Fields, Extension.Clone)
	w.Successor = clonePtr(w.Successor, same)
	w.History = cloneEach(w.History, WorkItemBindingChange.Clone)
	w.LocalFields = slices.Clone(w.LocalFields)
	return w
}

// Clone returns a deep copy.
func (c WorkItemBindingChange) Clone() WorkItemBindingChange {
	c.Successor = clonePtr(c.Successor, same)
	return c
}

// Clone returns a deep copy.
func (t Task) Clone() Task {
	t.StatusMessage = clonePtr(t.StatusMessage, same)
	t.Artifacts = cloneEach(t.Artifacts, Artifact.Clone)
	return t
}

// Clone returns a deep copy.
func (a Artifact) Clone() Artifact {
	a.Content = a.Content.Clone()
	return a
}

// Clone returns a deep copy.
func (r IngressReceipt) Clone() IngressReceipt {
	r.Conversation = clonePtr(r.Conversation, same)
	r.Context = clonePtr(r.Context, same)
	r.Message = clonePtr(r.Message, same)
	r.Task = clonePtr(r.Task, same)
	r.Operation = clonePtr(r.Operation, same)
	return r
}

// Clone returns a deep copy.
func (o Operation) Clone() Operation {
	o.Result = clonePtr(o.Result, same)
	o.ResultMessage = clonePtr(o.ResultMessage, same)
	o.ResultStream = clonePtr(o.ResultStream, same)
	o.ResultTask = clonePtr(o.ResultTask, same)
	o.ProviderReferences = slices.Clone(o.ProviderReferences)
	o.ReadConfirmedAt = clonePtr(o.ReadConfirmedAt, same)
	o.Failure = clonePtr(o.Failure, same)
	o.Progress = clonePtr(o.Progress, same)
	o.Pending = clonePtr(o.Pending, PendingMutation.Clone)
	o.PendingState = clonePtr(o.PendingState, PendingState.Clone)
	o.NextAttemptAt = clonePtr(o.NextAttemptAt, same)
	o.Key = clonePtr(o.Key, same)
	o.Held = clonePtr(o.Held, HeldBlock.Clone)
	return o
}

// Clone returns a deep copy.
func (a Acceptance) Clone() Acceptance {
	a.Operation = a.Operation.Clone()
	return a
}

// Clone returns a deep copy.
func (r SubscriptionResult) Clone() SubscriptionResult {
	r.Acceptance = r.Acceptance.Clone()
	return r
}

// Clone returns a deep copy.
func (c CapabilityStatus) Clone() CapabilityStatus {
	c.Limits = c.Limits.Clone()
	return c
}

// Clone returns a deep copy.
func (e Event) Clone() Event {
	e.Payload = e.Payload.Clone()
	return e
}

// Clone returns a deep copy.
func (b EventBatch) Clone() EventBatch {
	b.Events = cloneEach(b.Events, Event.Clone)
	return b
}

// Clone returns a deep copy.
func (s Stream) Clone() Stream {
	s.LastOperation = clonePtr(s.LastOperation, same)
	s.ProviderReferences = slices.Clone(s.ProviderReferences)
	return s
}

// Clone returns a deep copy.
func (e Evidence) Clone() Evidence {
	e.Facts = e.Facts.Clone()
	return e
}

// Clone returns a deep copy.
func (r LinkRequest) Clone() LinkRequest {
	r.Target = clonePtr(r.Target, same)
	r.Evidence = r.Evidence.Clone()
	return r
}

// Clone returns a deep copy.
func (l IdentityLink) Clone() IdentityLink {
	l.Evidence = l.Evidence.Clone()
	l.Anchor = clonePtr(l.Anchor, same)
	return l
}

// Clone returns a deep copy.
func (e Envelope) Clone() Envelope {
	e.References = slices.Clone(e.References)
	e.SenderAttributes = slices.Clone(e.SenderAttributes)
	e.Content = e.Content.Clone()
	e.Payload = e.Payload.Clone()
	e.Interaction = clonePtr(e.Interaction, InteractionInput.Clone)
	e.WorkItem = clonePtr(e.WorkItem, WorkItemState.Clone)
	e.Delivery = clonePtr(e.Delivery, DeliveryReport.Clone)
	e.Task = clonePtr(e.Task, same)
	e.Event = clonePtr(e.Event, Extension.Clone)
	e.State = clonePtr(e.State, ProviderStateReport.Clone)
	return e
}

// Clone returns a deep copy.
func (d DeliveryReport) Clone() DeliveryReport {
	if d.Failure != nil {
		f := *d.Failure
		d.Failure = &f
	}
	d.ReadAt = clonePtr(d.ReadAt, same)
	return d
}

// Clone returns a deep copy.
func (d InteractionDecision) Clone() InteractionDecision {
	d.FieldErrors = maps.Clone(d.FieldErrors)
	d.Options = d.Options.Clone()
	return d
}

// Clone copies Items, applying item to each element; a nil item copies
// elements as values, which suffices for element types without slices, maps
// or pointers (refs, Contact, Attachment).
func (p Page[T]) Clone(item func(T) T) Page[T] {
	if item == nil {
		item = same[T]
	}
	p.Items = cloneEach(p.Items, item)
	return p
}
