package microsoft

import "time"

// Microsoft Graph v1.0 wire shapes: only the fields the adapter reads or
// writes. Names follow the documented resource types (message, recipient,
// itemBody, attachment, subscription, changeNotification).

type gEmailAddress struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

type gRecipient struct {
	EmailAddress gEmailAddress `json:"emailAddress"`
}

// recipientsOf lists the addresses of rs.
func recipientsOf(rs []gRecipient) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.EmailAddress.Address)
	}
	return out
}

// graphRecipients are the Graph recipients of bare addresses.
func graphRecipients(list []string) []gRecipient {
	out := make([]gRecipient, 0, len(list))
	for _, a := range list {
		out = append(out, gRecipient{EmailAddress: gEmailAddress{Address: a}})
	}
	return out
}

type gItemBody struct {
	ContentType string `json:"contentType"`
	Content     string `json:"content"`
}

type gHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gExtendedProperty struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// gMessage is a message as read. Pointers distinguish absent from empty
// where the adapter cares.
type gMessage struct {
	ID                     string       `json:"id"`
	InternetMessageID      string       `json:"internetMessageId"`
	ConversationID         string       `json:"conversationId"`
	Subject                string       `json:"subject"`
	From                   *gRecipient  `json:"from"`
	Sender                 *gRecipient  `json:"sender"`
	ToRecipients           []gRecipient `json:"toRecipients"`
	CcRecipients           []gRecipient `json:"ccRecipients"`
	ReplyTo                []gRecipient `json:"replyTo"`
	Body                   *gItemBody   `json:"body"`
	ReceivedDateTime       time.Time    `json:"receivedDateTime"`
	HasAttachments         bool         `json:"hasAttachments"`
	IsDraft                bool         `json:"isDraft"`
	InternetMessageHeaders []gHeader    `json:"internetMessageHeaders"`
	ParentFolderID         string       `json:"parentFolderId"`
	Categories             []string     `json:"categories,omitempty"`
	IsRead                 bool         `json:"isRead"`
	Flag                   *gFlag       `json:"flag,omitempty"`
	Importance             string       `json:"importance,omitempty"`
	LastModifiedDateTime   time.Time    `json:"lastModifiedDateTime,omitzero"`
	// SingleValueExtendedProperties are the extended properties an
	// $expand of a single message names.
	SingleValueExtendedProperties []gExtendedProperty `json:"singleValueExtendedProperties,omitempty"`
}

// gFlag is a message's followupFlag; only the status is read or written.
type gFlag struct {
	FlagStatus string `json:"flagStatus"`
}

// Follow-up flag statuses.
const (
	flagNotFlagged = "notFlagged"
	flagFlagged    = "flagged"
)

// gMailFolder is a mailFolder as read.
type gMailFolder struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// gMove is the body of a message move.
type gMove struct {
	DestinationID string `json:"destinationId"`
}

// gRead is the body of an isRead update.
type gRead struct {
	IsRead bool `json:"isRead"`
}

// gFlagUpdate is the body of a follow-up flag update.
type gFlagUpdate struct {
	Flag gFlag `json:"flag"`
}

// Message importance values; high is the important flag.
const (
	importanceNormal = "normal"
	importanceHigh   = "high"
)

// gImportanceUpdate is the body of an importance update.
type gImportanceUpdate struct {
	Importance string `json:"importance"`
}

// gCategories is the body of a message update that replaces the
// categories: the list is always sent, empty to clear.
type gCategories struct {
	Categories []string `json:"categories"`
}

// gDraftBodyUpdate is the body of a draft update that replaces the body
// only.
type gDraftBodyUpdate struct {
	Body gItemBody `json:"body"`
}

// gDraft is the body of a draft create or update.
type gDraft struct {
	Subject                       string              `json:"subject,omitempty"`
	Body                          gItemBody           `json:"body"`
	ToRecipients                  []gRecipient        `json:"toRecipients"`
	CcRecipients                  []gRecipient        `json:"ccRecipients,omitempty"`
	BccRecipients                 []gRecipient        `json:"bccRecipients,omitempty"`
	SingleValueExtendedProperties []gExtendedProperty `json:"singleValueExtendedProperties"`
}

type gAttachment struct {
	ODataType   string `json:"@odata.type"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	IsInline    bool   `json:"isInline"`
}

// gDeltaItem is one entry of a delta page; Removed is set for deletions
// and moves out of the folder.
type gDeltaItem struct {
	ID      string    `json:"id"`
	Removed *struct{} `json:"@removed"`
	// The fields below are selected on a folder state delta only.
	ParentFolderID       string    `json:"parentFolderId"`
	IsRead               bool      `json:"isRead"`
	IsDraft              bool      `json:"isDraft"`
	Flag                 *gFlag    `json:"flag"`
	Importance           string    `json:"importance"`
	LastModifiedDateTime time.Time `json:"lastModifiedDateTime"`
}

type gDeltaPage struct {
	Value     []gDeltaItem `json:"value"`
	NextLink  string       `json:"@odata.nextLink"`
	DeltaLink string       `json:"@odata.deltaLink"`
}

type gMessagePage struct {
	Value []gMessage `json:"value"`
}

type gAttachmentPage struct {
	Value    []gAttachment `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
}

type gSubscription struct {
	ID                       string    `json:"id,omitempty"`
	ChangeType               string    `json:"changeType,omitempty"`
	NotificationURL          string    `json:"notificationUrl,omitempty"`
	LifecycleNotificationURL string    `json:"lifecycleNotificationUrl,omitempty"`
	Resource                 string    `json:"resource,omitempty"`
	ExpirationDateTime       time.Time `json:"expirationDateTime"`
	ClientState              string    `json:"clientState,omitempty"`
}

type gSubscriptionPage struct {
	Value []gSubscription `json:"value"`
}

// gNotification is one change or lifecycle notification of a
// changeNotificationCollection.
type gNotification struct {
	SubscriptionID string `json:"subscriptionId"`
	ClientState    string `json:"clientState"`
	ChangeType     string `json:"changeType"`
	Resource       string `json:"resource"`
	TenantID       string `json:"tenantId"`
	LifecycleEvent string `json:"lifecycleEvent"`
	ResourceData   *struct {
		ID string `json:"id"`
	} `json:"resourceData"`
}

type gNotificationCollection struct {
	Value []gNotification `json:"value"`
}

// gSendMessage is the message of a sendMail call. Bodies and recipients
// only: the sender is the mailbox the call is made on, unless From names
// one the token may send as.
type gSendMessage struct {
	Subject       string       `json:"subject"`
	Body          gItemBody    `json:"body"`
	From          *gRecipient  `json:"from,omitempty"`
	ToRecipients  []gRecipient `json:"toRecipients"`
	CcRecipients  []gRecipient `json:"ccRecipients,omitempty"`
	BccRecipients []gRecipient `json:"bccRecipients,omitempty"`
	ReplyTo       []gRecipient `json:"replyTo,omitempty"`
}

// gSendMail is the body of POST .../sendMail. SaveToSentItems is always
// sent: Graph's own default is true.
type gSendMail struct {
	Message         gSendMessage `json:"message"`
	SaveToSentItems bool         `json:"saveToSentItems"`
}
