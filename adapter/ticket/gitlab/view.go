package gitlab

import (
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The adapter reads the same issues and notes from webhook payloads and from
// REST answers, which name fields differently. Both are decoded into these
// views first, so the envelopes they produce agree.

// userView is a GitLab user as far as either source names it.
type userView struct {
	id       int64
	username string
}

// botUsername matches the usernames GitLab generates for the bot users of
// project and group access tokens (project_<id>_bot_<random>,
// group_<id>_bot_<random>).
var botUsername = regexp.MustCompile(`^(project|group)_[0-9]+_bot(_|$)`)

func (u userView) bot() bool { return botUsername.MatchString(u.username) }

// issueView is an issue.
type issueView struct {
	id, iid      int64
	title        string
	description  string
	state        string
	url          string
	confidential bool
	author       userView
	assigneeIDs  []int64
	labels       []string
	created      time.Time
	updated      time.Time
}

// noteView is a note on an issue.
type noteView struct {
	id       int64
	body     string
	author   userView
	internal bool
	system   bool
	created  time.Time
	updated  time.Time
}

// timeLayouts are the timestamp forms GitLab uses: ISO 8601 with a "Z"
// (REST answers and newer webhooks) and the older webhook form
// "2013-12-03 17:15:43 UTC" or with a numeric zone.
var timeLayouts = []string{
	time.RFC3339Nano, "2006-01-02 15:04:05 MST", "2006-01-02 15:04:05 -0700", "2006-01-02 15:04:05",
}

// parseTime parses a GitLab timestamp; the zero time for an empty or
// unknown one.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// REST answer shapes.

type apiUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type apiIssue struct {
	ID           int64     `json:"id"`
	IID          int64     `json:"iid"`
	Title        string    `json:"title"`
	Description  string    `json:"description"`
	State        string    `json:"state"`
	WebURL       string    `json:"web_url"`
	Confidential bool      `json:"confidential"`
	Author       apiUser   `json:"author"`
	Assignees    []apiUser `json:"assignees"`
	Labels       []string  `json:"labels"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
	References   struct {
		Full string `json:"full"`
	} `json:"references"`
}

// destination is the issue's project path_with_namespace, which inboxes
// bind and webhooks report: the issue's full reference
// ("group/project#iid"), else its web URL ("<host>/group/project/-/issues/<iid>").
func (i apiIssue) destination() string {
	if p, _, ok := strings.Cut(i.References.Full, "#"); ok && validFullName(p) {
		return p
	}
	if u, err := url.Parse(i.WebURL); err == nil {
		if p, _, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/-/issues/"); ok && validFullName(p) {
			return p
		}
	}
	return ""
}

func (i apiIssue) view() issueView {
	v := issueView{
		id: i.ID, iid: i.IID, title: i.Title, description: i.Description, state: i.State, url: i.WebURL,
		confidential: i.Confidential, author: userView{i.Author.ID, i.Author.Username},
		labels: i.Labels, created: parseTime(i.CreatedAt), updated: parseTime(i.UpdatedAt),
	}
	for _, a := range i.Assignees {
		v.assigneeIDs = append(v.assigneeIDs, a.ID)
	}
	return v
}

type apiNote struct {
	ID        int64   `json:"id"`
	Body      string  `json:"body"`
	Author    apiUser `json:"author"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	System    bool    `json:"system"`
	Internal  bool    `json:"internal"`
}

func (n apiNote) view() noteView {
	return noteView{
		id: n.ID, body: n.Body, author: userView{n.Author.ID, n.Author.Username},
		internal: n.Internal, system: n.System, created: parseTime(n.CreatedAt), updated: parseTime(n.UpdatedAt),
	}
}

func decodeIssue(b []byte) (apiIssue, bool) {
	var i apiIssue
	return i, json.Unmarshal(b, &i) == nil && i.ID != 0 && i.IID > 0
}

func decodeNote(b []byte) (apiNote, bool) {
	var n apiNote
	return n, json.Unmarshal(b, &n) == nil && n.ID != 0
}

func decodeNotes(b []byte) ([]apiNote, bool) {
	var ns []apiNote
	return ns, json.Unmarshal(b, &ns) == nil
}

func sortedLabels(l []string) []string {
	out := slices.Clone(l)
	slices.Sort(out)
	return out
}
