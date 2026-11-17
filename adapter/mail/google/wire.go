package google

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Gmail API v1 wire shapes: only the fields the adapter reads or writes.
// Names follow the documented resources (Message, History, Profile,
// WatchResponse, ListMessagesResponse, ListHistoryResponse).

// int64s are int64 values Google's JSON encodes as strings ("format:
// int64"). Numbers are accepted too: the push payload and some examples
// carry them unquoted.
type int64s int64

func (n *int64s) UnmarshalJSON(b []byte) error {
	b = bytes.Trim(b, `"`)
	if len(b) == 0 || string(b) == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseUint(string(b), 10, 63)
	if err != nil {
		return err
	}
	*n = int64s(v)
	return nil
}

func (n int64s) MarshalJSON() ([]byte, error) {
	return []byte(`"` + strconv.FormatInt(int64(n), 10) + `"`), nil
}

func (n int64s) String() string { return strconv.FormatInt(int64(n), 10) }

type gProfile struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    int64s `json:"historyId"`
}

type gHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gPart struct {
	Headers []gHeader `json:"headers"`
}

// gMessage is a Message as read. Raw is set with format=raw, Payload
// headers with format=metadata.
type gMessage struct {
	ID           string   `json:"id"`
	ThreadID     string   `json:"threadId,omitempty"`
	LabelIDs     []string `json:"labelIds,omitempty"`
	HistoryID    int64s   `json:"historyId,omitempty"`
	InternalDate int64s   `json:"internalDate,omitempty"`
	Raw          string   `json:"raw,omitempty"`
	// SizeEstimate is Gmail's estimate of the whole message's size in
	// bytes.
	SizeEstimate int64s `json:"sizeEstimate,omitempty"`
	Payload      *gPart `json:"payload,omitempty"`
}

// gSend is the messages.send request body.
type gSend struct {
	Raw      string `json:"raw"`
	ThreadID string `json:"threadId,omitempty"`
}

// gModify is the messages.modify request body (ModifyMessageRequest).
type gModify struct {
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
}

// gDraftBody is the users.drafts.create and users.drafts.update request
// body (Draft): the message whose raw form and threadId the draft holds.
type gDraftBody struct {
	Message gSend `json:"message"`
}

// gDraft is a Draft as answered.
type gDraft struct {
	ID      string   `json:"id"`
	Message gMessage `json:"message"`
}

// gDraftList is the users.drafts.list answer.
type gDraftList struct {
	Drafts []gDraft `json:"drafts"`
}

type gMessageList struct {
	Messages      []gMessage `json:"messages"`
	NextPageToken string     `json:"nextPageToken"`
}

type gMessageAdded struct {
	Message gMessage `json:"message"`
}

// gLabelChange is a labelsAdded or labelsRemoved entry of a history record:
// the message and the labels that changed.
type gLabelChange struct {
	Message  gMessage `json:"message"`
	LabelIDs []string `json:"labelIds"`
}

type gHistory struct {
	ID              int64s          `json:"id"`
	MessagesAdded   []gMessageAdded `json:"messagesAdded"`
	MessagesDeleted []gMessageAdded `json:"messagesDeleted"`
	LabelsAdded     []gLabelChange  `json:"labelsAdded"`
	LabelsRemoved   []gLabelChange  `json:"labelsRemoved"`
}

// gThread is a Thread: its messages, oldest first, with the fields the
// requested format carries.
type gThread struct {
	ID        string     `json:"id"`
	HistoryID int64s     `json:"historyId,omitempty"`
	Messages  []gMessage `json:"messages,omitempty"`
}

type gHistoryList struct {
	History       []gHistory `json:"history"`
	NextPageToken string     `json:"nextPageToken"`
	HistoryID     int64s     `json:"historyId"`
}

type gWatchRequest struct {
	TopicName           string   `json:"topicName"`
	LabelIDs            []string `json:"labelIds"`
	LabelFilterBehavior string   `json:"labelFilterBehavior"`
}

type gWatchResponse struct {
	HistoryID  int64s `json:"historyId"`
	Expiration int64s `json:"expiration"` // epoch milliseconds
}

// gPush is the Pub/Sub push request body.
type gPush struct {
	Message struct {
		Data      string `json:"data"`
		MessageID string `json:"messageId"`
	} `json:"message"`
	Subscription string `json:"subscription"`
}

// gNotification is the decoded data of a Gmail push message.
type gNotification struct {
	EmailAddress string `json:"emailAddress"`
	HistoryID    int64s `json:"historyId"`
}

// Labels the adapter reads.
const (
	labelInbox = "INBOX"
	labelSent  = "SENT"
	labelDraft = "DRAFT"

	labelUnread    = "UNREAD"
	labelStarred   = "STARRED"
	labelImportant = "IMPORTANT"
	labelTrash     = "TRASH"
	labelSpam      = "SPAM"
)

var _ json.Unmarshaler = (*int64s)(nil)
