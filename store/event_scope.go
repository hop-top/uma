package store

import (
	"encoding/json"

	"hop.top/uma/model"
)

// EventConversation resolves the conversation event e is about, the one a
// Reader.Events conversation filter selects it by, from the event alone, so
// every backend agrees on it. It is the ID of a conversation in e's inbox,
// or empty when the event has none:
//
//   - An event whose Resource is a conversation is about that
//     conversation, whatever its kind.
//   - An event carrying a builtin version 1 payload (Payload.Name a
//     model.Schema* event schema, Payload.Version model.PayloadVersion1)
//     is about the conversation that payload names: the message's for
//     message.received, message.created and message.updated, the
//     conversation's for conversation.changed and the conversation label,
//     assignment, snooze and link kinds (the end the event is about), the
//     task's for task.updated, the work item's for work_item.updated and
//     the interaction's for interaction.received and interaction.updated.
//   - Any other event has none: delivery.updated (about a tenant-scoped
//     operation), identity.linked, the contact kinds, events about a
//     channel or an inbox, and extension events about anything but a
//     conversation. So does a payload that does not hold the reference at
//     its documented path, or names a conversation that is invalid or lies
//     outside e's inbox.
//
// Payload keys match exactly, and of duplicate keys the last one counts, as
// PostgreSQL's json operators read them. The conversation of a stored event
// never changes: its resource and payload are fixed when it is appended.
func EventConversation(e model.Event) model.ID {
	if e.Inbox == "" {
		return ""
	}
	if e.Resource.Kind == model.KindConversation {
		return e.Resource.ID
	}
	if e.Payload.Version != model.PayloadVersion1 {
		return ""
	}
	path, ok := eventConversationPath(e.Payload.Name)
	if !ok {
		return ""
	}
	conv := jsonAt(e.Payload.Data, path...)
	c := model.ConversationRef{
		Inbox: model.InboxRef{Tenant: model.TenantID(jsonString(jsonAt(conv, "inbox", "tenant"))),
			ID: model.ID(jsonString(jsonAt(conv, "inbox", "id")))},
		ID: model.ID(jsonString(jsonAt(conv, "id"))),
	}
	if c.Validate() != nil || c.Inbox != (model.InboxRef{Tenant: e.Tenant, ID: e.Inbox}) {
		return ""
	}
	return c.ID
}

// eventConversationPath returns the path of the conversation reference in
// a payload of the builtin schema, for each schema that names one.
func eventConversationPath(schema string) ([]string, bool) {
	switch schema {
	case model.SchemaMessageReceived, model.SchemaMessageCreated, model.SchemaMessageUpdated:
		return []string{"message", "conversation"}, true
	case model.SchemaConversationChanged,
		model.SchemaConversationLabelAdded, model.SchemaConversationLabelRemoved,
		model.SchemaConversationAssigned, model.SchemaConversationUnassigned,
		model.SchemaConversationSnoozed, model.SchemaConversationUnsnoozed,
		model.SchemaConversationLinked, model.SchemaConversationUnlinked:
		return []string{"conversation"}, true
	case model.SchemaTaskUpdated:
		return []string{"task", "conversation"}, true
	case model.SchemaWorkItemUpdated:
		return []string{"work_item", "conversation"}, true
	case model.SchemaInteractionReceived, model.SchemaInteractionUpdated:
		return []string{"interaction", "conversation"}, true
	}
	return nil, false
}

// jsonAt returns the value at path in the JSON object raw, or nil when a
// step is missing or not an object.
func jsonAt(raw json.RawMessage, path ...string) json.RawMessage {
	for _, key := range path {
		var obj map[string]json.RawMessage
		if json.Unmarshal(raw, &obj) != nil {
			return nil
		}
		raw = obj[key]
	}
	return raw
}

// jsonString returns the JSON string raw holds, or "" for any other value.
func jsonString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
