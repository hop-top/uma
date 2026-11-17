package teams

import (
	"encoding/json"
	"time"
)

// activity is the subset of a Bot Framework activity the adapter reads
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference#activity-object)
// with the Teams channelData fields
// (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/subscribe-to-conversation-events).
type activity struct {
	Type         string          `json:"type"`
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Timestamp    string          `json:"timestamp"`
	ServiceURL   string          `json:"serviceUrl"`
	ChannelID    string          `json:"channelId"`
	From         account         `json:"from"`
	Recipient    account         `json:"recipient"`
	Conversation conversation    `json:"conversation"`
	Text         string          `json:"text"`
	ReplyToID    string          `json:"replyToId"`
	Attachments  []attachment    `json:"attachments"`
	Entities     []entity        `json:"entities"`
	ChannelData  channelData     `json:"channelData"`
	Value        json.RawMessage `json:"value"`
	// ReactionsAdded and ReactionsRemoved are a messageReaction's.
	ReactionsAdded   []reaction `json:"reactionsAdded"`
	ReactionsRemoved []reaction `json:"reactionsRemoved"`
}

type account struct {
	ID          string `json:"id"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
	Role        string `json:"role,omitempty"`
}

type conversation struct {
	ID               string `json:"id"`
	ConversationType string `json:"conversationType"`
	TenantID         string `json:"tenantId"`
	IsGroup          bool   `json:"isGroup"`
}

type attachment struct {
	ContentType string          `json:"contentType"`
	ContentURL  string          `json:"contentUrl,omitempty"`
	Content     json.RawMessage `json:"content,omitempty"`
	Name        string          `json:"name,omitempty"`
}

type entity struct {
	Type      string   `json:"type"`
	Mentioned *account `json:"mentioned,omitempty"`
	Text      string   `json:"text,omitempty"`
}

type channelData struct {
	EventType string `json:"eventType"`
	Tenant    *struct {
		ID string `json:"id"`
	} `json:"tenant"`
	Team *struct {
		ID string `json:"id"`
	} `json:"team"`
	Channel *struct {
		ID string `json:"id"`
	} `json:"channel"`
}

type reaction struct {
	Type string `json:"type"`
}

// tenant is the tenant the activity names: channelData.tenant.id, else
// conversation.tenantId.
func (a activity) tenant() string {
	if a.ChannelData.Tenant != nil && a.ChannelData.Tenant.ID != "" {
		return a.ChannelData.Tenant.ID
	}
	return a.Conversation.TenantID
}

// at is the activity's timestamp; the zero time when it has none.
func (a activity) at() time.Time {
	t, err := time.Parse(time.RFC3339Nano, a.Timestamp)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// channel reports whether the activity is in a Teams channel, where
// conversations divide into threads.
func (a activity) channel() bool { return a.Conversation.ConversationType == "channel" }
