package jira

import (
	"encoding/json"
	"strconv"
	"time"
)

// The parts of Jira's user, comment and issue objects the adapter reads,
// as the REST API and webhooks send them.

type user struct {
	AccountID   string `json:"accountId"`
	DisplayName string `json:"displayName"`
	// AccountType is "atlassian" (a person), "app" (an app or bot) or
	// "customer" (a Jira Service Management customer).
	AccountType string `json:"accountType"`
}

type comment struct {
	ID           string          `json:"id"`
	Self         string          `json:"self"`
	Author       *user           `json:"author"`
	UpdateAuthor *user           `json:"updateAuthor"`
	Body         json.RawMessage `json:"body"`
	Created      string          `json:"created"`
	Updated      string          `json:"updated"`
	Visibility   *struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"visibility"`
	JSDPublic  *bool           `json:"jsdPublic"`
	Properties []propertyEntry `json:"properties"`
}

// restricted reports a comment only a group or role may read, or a Jira
// Service Management internal note.
func (c comment) restricted() bool {
	return (c.Visibility != nil && c.Visibility.Type != "") || (c.JSDPublic != nil && !*c.JSDPublic)
}

type issue struct {
	ID     string `json:"id"`
	Key    string `json:"key"`
	Self   string `json:"self"`
	Fields struct {
		Summary     string          `json:"summary"`
		Description json.RawMessage `json:"description"`
		Status      *struct {
			Name           string `json:"name"`
			StatusCategory struct {
				Key string `json:"key"`
			} `json:"statusCategory"`
		} `json:"status"`
		Labels  []string `json:"labels"`
		Project *struct {
			Key string `json:"key"`
		} `json:"project"`
		Reporter *user  `json:"reporter"`
		Creator  *user  `json:"creator"`
		Assignee *user  `json:"assignee"`
		Created  string `json:"created"`
		Updated  string `json:"updated"`
	} `json:"fields"`
}

// jiraTimeLayout is the timestamp format of Jira's REST API and webhooks:
// ISO 8601 with milliseconds and an offset without a colon.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

// parseTime reads a Jira timestamp; the zero time when s is not one.
func parseTime(s string) time.Time {
	for _, layout := range []string{jiraTimeLayout, time.RFC3339Nano, "2006-01-02T15:04:05-0700"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// idOK reports whether s is a positive decimal integer without sign or
// leading zero: how Jira writes issue and comment IDs.
func idOK(s string) bool {
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == s
}
