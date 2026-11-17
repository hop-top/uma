package zendesk

import (
	"bytes"
	"encoding/json"
	"errors"
)

// zid is a Zendesk ID. The REST API encodes IDs as JSON numbers, webhook
// events as strings; both decode to the decimal text, kept byte-exact. A
// JSON null is the empty ID.
type zid string

func (z *zid) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")):
		*z = ""
		return nil
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*z = zid(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return errors.New("zendesk: id must be a number or a string")
	}
	*z = zid(n.String())
	return nil
}

// ticket is the subset of a Zendesk ticket the adapter reads, from the
// Tickets API or a webhook event's detail object.
type ticket struct {
	ID          zid    `json:"id"`
	Subject     string `json:"subject"`
	Status      string `json:"status"`
	RequesterID zid    `json:"requester_id"`
	AssigneeID  zid    `json:"assignee_id"`
	BrandID     zid    `json:"brand_id"`
	UpdatedAt   string `json:"updated_at"`
}

// comment is a ticket comment as an incremental export's comment child
// event, a webhook comment event or an audit's Comment event carries it.
type comment struct {
	ID        zid     `json:"id"`
	Type      string  `json:"type"`
	EventType string  `json:"event_type"`
	Body      string  `json:"body"`
	Public    *bool   `json:"public"`
	IsPublic  *bool   `json:"is_public"`
	AuthorID  zid     `json:"author_id"`
	Author    *author `json:"author"`
	AuditID   zid     `json:"audit_id"`
	CreatedAt string  `json:"created_at"`
	Via       *via    `json:"via"`
}

type author struct {
	ID      zid    `json:"id"`
	IsStaff bool   `json:"is_staff"`
	Name    string `json:"name"`
}

type via struct {
	Channel string `json:"channel"`
}

// public reports whether the comment is explicitly public. A comment whose
// visibility is absent is not.
func (c comment) public() bool {
	if c.Public != nil {
		return *c.Public
	}
	return c.IsPublic != nil && *c.IsPublic
}

func (c comment) authorID() zid {
	if c.Author != nil && c.Author.ID != "" {
		return c.Author.ID
	}
	return c.AuthorID
}

func (c comment) isComment() bool {
	return c.Type == "Comment" || c.EventType == "Comment"
}

// audit is a ticket audit: its custom metadata and events.
type audit struct {
	ID        zid       `json:"id"`
	TicketID  zid       `json:"ticket_id"`
	CreatedAt string    `json:"created_at"`
	Metadata  *metadata `json:"metadata"`
	Events    []comment `json:"events"`
}

type metadata struct {
	Custom json.RawMessage `json:"custom"`
}

// commentEvent returns the audit's comment event, if any.
func (a audit) commentEvent() (comment, bool) {
	for _, e := range a.Events {
		if e.isComment() {
			return e, true
		}
	}
	return comment{}, false
}
