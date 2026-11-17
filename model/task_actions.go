package model

import (
	"bytes"
	"encoding/json"
)

// Builtin Tasks.Act action schemas (TaskAction.Action.Name, version 1).
// Neither carries a role or author: the actor is always the caller.
const (
	// SchemaTaskOpen opens a task for a message that has none: a
	// TaskOpen. The host's explicit choice; a message without a task is
	// a valid message-only exchange.
	SchemaTaskOpen = CoreNamespace + ".task.open"
	// SchemaTaskUpdate reports a task's progress: a TaskUpdate.
	SchemaTaskUpdate = CoreNamespace + ".task.update"
)

// TaskOpen is the version 1 SchemaTaskOpen action. Message is the message
// the task is opened for, in the target conversation; State is the initial
// state, empty meaning TaskSubmitted.
type TaskOpen struct {
	Message MessageRef `json:"message"`
	State   string     `json:"state"`
}

func (o TaskOpen) validate() error {
	var state error
	if o.State != "" && !IsTaskState(o.State) {
		state = invalidField("action.data.state", "must be a declared task state")
	}
	var msg error = invalidField("action.data.message", "required")
	if !o.Message.IsZero() {
		msg = o.Message.validate("action.data.message", false)
	}
	return firstError(msg, state)
}

// TaskUpdate is the version 1 SchemaTaskUpdate action; at least one of
// State, StatusMessage and Artifacts is set. State is the new state, empty
// keeping the current one (a same-state update). StatusMessage is status
// content: it is recorded as exactly one uma-authored message, under
// Audience when set and the conversation's public audience otherwise;
// Audience without StatusMessage is invalid. A state change without
// content records no message. Artifacts are upserted by ID.
type TaskUpdate struct {
	State         string           `json:"state"`
	StatusMessage *Content         `json:"status_message,omitempty"`
	Audience      *Audience        `json:"audience,omitempty"`
	Artifacts     []ArtifactUpdate `json:"artifacts,omitempty"`
}

// ArtifactUpdate upserts one task artifact by ID. Without Append it
// replaces the artifact whole, creating it when new, and Content is
// required. With Append, Content's parts extend the artifact's (Content may
// be empty when the update only completes it) and a set Name or Description
// replaces the stored one. Complete marks the last chunk.
type ArtifactUpdate struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Content     Content `json:"content"`
	Append      bool    `json:"append"`
	Complete    bool    `json:"complete"`
}

func (a ArtifactUpdate) validate(path string, in *InboxRef) error {
	var content error
	if !a.Append || len(a.Content.Parts) > 0 {
		content = a.Content.validate(fieldPath(path, "content"), DefaultContentLimits(), in)
	}
	return firstError(
		checkRaw(fieldPath(path, "id"), a.ID, maxTokenBytes),
		optionalText(fieldPath(path, "name"), a.Name, maxTaskTextBytes),
		optionalText(fieldPath(path, "description"), a.Description, maxTaskTextBytes),
		content,
	)
}

// validate checks u; when in is set, attachment parts must belong to it.
func (u TaskUpdate) validate(in *InboxRef) error {
	if u.State == "" && u.StatusMessage == nil && len(u.Artifacts) == 0 {
		return invalidField("action.data", "sets nothing")
	}
	var state, status, audience error
	if u.State != "" && !IsTaskState(u.State) {
		state = invalidField("action.data.state", "must be a declared task state")
	}
	if u.StatusMessage != nil {
		status = u.StatusMessage.validate("action.data.status_message", DefaultContentLimits(), in)
	}
	if u.Audience != nil {
		if u.StatusMessage == nil {
			audience = invalidField("action.data.audience", "only with a status message")
		} else {
			audience = u.Audience.validate("action.data.audience")
		}
	}
	ids := make([]string, len(u.Artifacts))
	for i, a := range u.Artifacts {
		ids[i] = a.ID
	}
	artifacts := checkArtifactIDs("action.data.artifacts", ids)
	for i, a := range u.Artifacts {
		if artifacts != nil {
			break
		}
		artifacts = a.validate(indexPath("action.data.artifacts", i), in)
	}
	return firstError(state, status, audience, artifacts)
}

// DecodedTaskAction is a decoded Tasks.Act action: exactly one of Open and
// Update is set, matching the action's schema.
type DecodedTaskAction struct {
	Open   *TaskOpen
	Update *TaskUpdate
}

// DecodeTaskAction reads a Tasks.Act action. A name other than
// SchemaTaskOpen and SchemaTaskUpdate is model.Unsupported, another version
// model.SchemaMismatch, and unknown, missing or malformed data
// model.Validation.
func DecodeTaskAction(a Extension) (DecodedTaskAction, error) {
	var d DecodedTaskAction
	var dst any
	switch a.Name {
	case SchemaTaskOpen:
		d.Open = &TaskOpen{}
		dst = d.Open
	case SchemaTaskUpdate:
		d.Update = &TaskUpdate{}
		dst = d.Update
	default:
		return DecodedTaskAction{}, NewError(Unsupported, "action.name: unknown task action")
	}
	if a.Version != 1 {
		return DecodedTaskAction{}, &Error{Code: SchemaMismatch, SafeMessage: "action.version: unsupported task action version", Retry: NoRetry}
	}
	dec := json.NewDecoder(bytes.NewReader(a.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil || dec.More() {
		return DecodedTaskAction{}, invalidField("action.data", "must be a "+a.Name+" action")
	}
	var err error
	if d.Open != nil {
		err = d.Open.validate()
	} else {
		err = d.Update.validate(nil)
	}
	if err != nil {
		return DecodedTaskAction{}, err
	}
	return d, nil
}

// Decode reads r's action with DecodeTaskAction and checks it against the
// target: an opening's message belongs to the target conversation, and an
// update's attachments to the target's inbox.
func (r TaskAction) Decode() (DecodedTaskAction, error) {
	d, err := DecodeTaskAction(r.Action)
	if err != nil {
		return DecodedTaskAction{}, err
	}
	switch {
	case d.Open != nil && !d.Open.Message.In(r.Target.Conversation):
		return DecodedTaskAction{}, invalidField("action.data.message", "must belong to the target conversation")
	case d.Update != nil:
		in := r.Target.Conversation.Inbox
		if err := d.Update.validate(&in); err != nil {
			return DecodedTaskAction{}, err
		}
	}
	return d, nil
}
