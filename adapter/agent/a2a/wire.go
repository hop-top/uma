package a2a

import (
	"encoding/json"
	"time"

	"hop.top/uma/model"
)

// A2A wire values (ProtoJSON, camelCase).
const (
	roleUser  = "ROLE_USER"
	roleAgent = "ROLE_AGENT"
)

// wireStates maps uma task states to A2A TaskState names.
var wireStates = map[string]string{
	model.TaskSubmitted:     "TASK_STATE_SUBMITTED",
	model.TaskWorking:       "TASK_STATE_WORKING",
	model.TaskInputRequired: "TASK_STATE_INPUT_REQUIRED",
	model.TaskAuthRequired:  "TASK_STATE_AUTH_REQUIRED",
	model.TaskCompleted:     "TASK_STATE_COMPLETED",
	model.TaskFailed:        "TASK_STATE_FAILED",
	model.TaskCanceled:      "TASK_STATE_CANCELED",
	model.TaskRejected:      "TASK_STATE_REJECTED",
}

// umaState returns the uma state a wire TaskState names; "" for
// TASK_STATE_UNSPECIFIED, which filters nothing, and ok false for a name
// that is not a TaskState.
func umaState(wire string) (state string, ok bool) {
	if wire == "" || wire == "TASK_STATE_UNSPECIFIED" {
		return "", true
	}
	for s, w := range wireStates {
		if w == wire {
			return s, true
		}
	}
	return "", false
}

// wirePart is an A2A Part. Exactly one of Text, Raw, URL and Data is set.
// Raw is base64, as ProtoJSON encodes bytes.
type wirePart struct {
	Text      *string         `json:"text,omitempty"`
	Raw       *string         `json:"raw,omitempty"`
	URL       *string         `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

// wireMessage is an A2A Message.
type wireMessage struct {
	MessageID        string     `json:"messageId"`
	ContextID        string     `json:"contextId,omitempty"`
	TaskID           string     `json:"taskId,omitempty"`
	Role             string     `json:"role"`
	Parts            []wirePart `json:"parts"`
	ReferenceTaskIDs []string   `json:"referenceTaskIds,omitempty"`
}

type wireStatus struct {
	State     string       `json:"state"`
	Message   *wireMessage `json:"message,omitempty"`
	Timestamp string       `json:"timestamp,omitempty"`
}

type wireArtifact struct {
	ArtifactID  string     `json:"artifactId"`
	Name        string     `json:"name,omitempty"`
	Description string     `json:"description,omitempty"`
	Parts       []wirePart `json:"parts"`
}

type wireTask struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    wireStatus     `json:"status"`
	Artifacts []wireArtifact `json:"artifacts,omitempty"`
	History   []wireMessage  `json:"history,omitempty"`
}

// sendResult is a SendMessageResponse: exactly one of Task and Message.
type sendResult struct {
	Task    *wireTask    `json:"task,omitempty"`
	Message *wireMessage `json:"message,omitempty"`
}

type listResult struct {
	Tasks         []wireTask `json:"tasks"`
	NextPageToken string     `json:"nextPageToken"`
	PageSize      int        `json:"pageSize"`
	TotalSize     int        `json:"totalSize"`
}

// timestamp renders t as the specification's UTC millisecond timestamp.
func timestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// taskView renders a task. status is its status message and history its
// messages, either possibly absent; include selects whether artifacts show.
func taskView(t model.Task, status *model.Message, history []model.Message, artifacts bool) wireTask {
	w := wireTask{
		ID: t.External.ID, ContextID: t.Context.ID,
		Status: wireStatus{State: wireStates[t.State], Timestamp: timestamp(t.StatusAt)},
	}
	if status != nil {
		m := messageView(*status, t)
		w.Status.Message = &m
	}
	if artifacts {
		for _, a := range t.Artifacts {
			w.Artifacts = append(w.Artifacts, wireArtifact{
				ArtifactID: a.ID, Name: a.Name, Description: a.Description, Parts: partsView(a.Content),
			})
		}
	}
	for _, m := range history {
		w.History = append(w.History, messageView(m, t))
	}
	return w
}

// messageView renders a recorded message of task t. A message uma authored
// is the agent's; one a remote agent sent is the user's. Its ID is the uma
// message ID.
func messageView(m model.Message, t model.Task) wireMessage {
	w := wireMessage{
		MessageID: string(m.Ref.ID), ContextID: t.Context.ID, TaskID: t.External.ID,
		Role: roleUser, Parts: partsView(m.Content),
	}
	if m.Origin != nil {
		w.Role = roleAgent
	}
	return w
}

// replyView renders a uma-authored reply that belongs to no task.
func replyView(m model.Message, context string) wireMessage {
	return wireMessage{MessageID: string(m.Ref.ID), ContextID: context, Role: roleAgent, Parts: partsView(m.Content)}
}

// partsView renders content as parts. Text is text; the structured parts
// the adapter itself produces on the way in (SchemaData, SchemaFileURL)
// go back out as data and url parts, any other structured part as a data
// part naming its schema; an attachment is named by its ID, because the
// adapter has no read path to its bytes (see the package documentation).
func partsView(c model.Content) []wirePart {
	out := make([]wirePart, 0, len(c.Parts))
	for _, p := range c.Parts {
		switch {
		case p.Kind == "text":
			text := p.Text
			out = append(out, wirePart{Text: &text})
		case p.Kind == "attachment" && p.Attachment != nil:
			out = append(out, wirePart{Data: mustJSON(map[string]any{"attachment": string(p.Attachment.ID)}), MediaType: "application/vnd.uma.attachment+json"})
		case p.Kind == "structured" && p.Structured != nil:
			out = append(out, structuredView(*p.Structured))
		}
	}
	return out
}

type fileURL struct {
	URL       string `json:"url"`
	MediaType string `json:"media_type,omitempty"`
	Filename  string `json:"filename,omitempty"`
}

func structuredView(e model.Extension) wirePart {
	switch {
	case e.Name == SchemaData && e.Version == 1:
		return wirePart{Data: e.Data}
	case e.Name == SchemaFileURL && e.Version == 1:
		var f fileURL
		if json.Unmarshal(e.Data, &f) == nil && f.URL != "" {
			return wirePart{URL: &f.URL, MediaType: f.MediaType, Filename: f.Filename}
		}
	}
	return wirePart{Data: mustJSON(map[string]any{"schema": e.Name, "version": e.Version, "data": json.RawMessage(e.Data)}),
		MediaType: "application/vnd.uma.structured+json"}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("null")
	}
	return b
}
