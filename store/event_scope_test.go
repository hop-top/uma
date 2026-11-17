package store

import (
	"encoding/json"
	"testing"

	"hop.top/uma/model"
)

// EventConversation reads the documented payload path exactly as
// PostgreSQL's json operators do: exact keys, the last duplicate, strings
// only, and a conversation in the event's own tenant and inbox.
func TestEventConversationPayloadRules(t *testing.T) {
	const tenant model.TenantID = "acme"
	in := model.InboxRef{Tenant: tenant, ID: "00000000-0000-7000-8000-000000000001"}
	conv := model.ConversationRef{Inbox: in, ID: "00000000-0000-7000-8000-000000000002"}
	msg := model.MessageRef{Conversation: conv, ID: "00000000-0000-7000-8000-000000000003"}
	ref := func(c model.ConversationRef) string {
		b, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	other := model.ConversationRef{Inbox: in, ID: "00000000-0000-7000-8000-000000000004"}
	event := func(data string) model.Event {
		return model.Event{
			Tenant: tenant, Inbox: in.ID, Resource: msg.Ref(), Kind: model.EventMessageReceived,
			Payload: model.Extension{Name: model.SchemaMessageReceived, Version: model.PayloadVersion1, Data: json.RawMessage(data)},
		}
	}
	for _, c := range []struct {
		name string
		ev   model.Event
		want model.ID
	}{
		{"message payload", event(`{"message":{"conversation":` + ref(conv) + `}}`), conv.ID},
		{"last duplicate key", event(`{"message":{"conversation":` + ref(other) + `},"message":{"conversation":` + ref(conv) + `}}`), conv.ID},
		{"keys match exactly", event(`{"Message":{"conversation":` + ref(conv) + `}}`), ""},
		{"tenant must be a string", event(`{"message":{"conversation":{"inbox":{"tenant":1,"id":"` + string(in.ID) + `"},"id":"` + string(conv.ID) + `"}}}`), ""},
		{"conversation outside the inbox", event(`{"message":{"conversation":` +
			ref(model.ConversationRef{Inbox: model.InboxRef{Tenant: tenant, ID: other.ID}, ID: conv.ID}) + `}}`), ""},
		{"not an object", event(`{"message":"x"}`), ""},
		{"no data", event(``), ""},
		{"conversation resource", model.Event{Tenant: tenant, Inbox: in.ID, Resource: conv.Ref(), Kind: "com.example.x"}, conv.ID},
		{"tenant-scoped resource", model.Event{Tenant: tenant, Resource: model.ContactRef{Tenant: tenant, ID: conv.ID}.Ref(),
			Kind: model.EventContactCreated}, ""},
	} {
		if got := EventConversation(c.ev); got != c.want {
			t.Errorf("%s: EventConversation = %q, want %q", c.name, got, c.want)
		}
	}
}
