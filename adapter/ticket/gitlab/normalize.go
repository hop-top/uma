package gitlab

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
)

// delivery is one envelope of a webhook delivery, with the envelope to
// record instead when the first targets an unknown message.
type delivery struct {
	ingress  model.Ingress
	fallback *model.Ingress
}

// Webhook payload shapes, from the GitLab webhook events documentation.
// Only the fields the adapter reads are decoded.

type hookUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type hookProject struct {
	ID                int64  `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
}

type hookLabel struct {
	Title string `json:"title"`
}

// hookIssue is the issue of an issue event (object_attributes) and of a
// note event (issue).
type hookIssue struct {
	ID           int64       `json:"id"`
	IID          int64       `json:"iid"`
	Title        string      `json:"title"`
	Description  string      `json:"description"`
	State        string      `json:"state"`
	Action       string      `json:"action"`
	URL          string      `json:"url"`
	Confidential bool        `json:"confidential"`
	AuthorID     int64       `json:"author_id"`
	AssigneeIDs  []int64     `json:"assignee_ids"`
	Labels       []hookLabel `json:"labels"`
	CreatedAt    string      `json:"created_at"`
	UpdatedAt    string      `json:"updated_at"`
}

type hookNote struct {
	ID           int64  `json:"id"`
	Note         string `json:"note"`
	NoteableType string `json:"noteable_type"`
	AuthorID     int64  `json:"author_id"`
	Internal     bool   `json:"internal"`
	System       bool   `json:"system"`
	Action       string `json:"action"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

type hookChange struct {
	Previous json.RawMessage `json:"previous"`
	Current  json.RawMessage `json:"current"`
}

type event struct {
	ObjectKind       string                `json:"object_kind"`
	User             hookUser              `json:"user"`
	Project          hookProject           `json:"project"`
	ObjectAttributes json.RawMessage       `json:"object_attributes"`
	Issue            *hookIssue            `json:"issue"`
	Labels           []hookLabel           `json:"labels"`
	Changes          map[string]hookChange `json:"changes"`
}

var errMalformed = errors.New("gitlab: malformed webhook payload")

// Event headers of the deliveries the adapter consumes: the event types of
// issues and of comments, for confidential issues too.
const (
	eventIssue = "issue"
	eventNote  = "note"
)

// eventKind maps an X-Gitlab-Event value to the payload it names, "" for an
// event the adapter does not consume.
func eventKind(header string) string {
	switch header {
	case "Issue Hook", "Confidential Issue Hook":
		return eventIssue
	case "Note Hook", "Confidential Note Hook":
		return eventNote
	}
	return ""
}

// normalize maps one delivery to the envelopes it reports, in order. Events
// the adapter does not consume map to none. eventID identifies the event;
// correlation is the delivery's own ID, kept only as correlation. kind is
// the X-Gitlab-Event header's payload: a payload that is not the shape of
// the event it names is malformed.
func (x *Instance) normalize(eventID, correlation, kind string, payload []byte) ([]delivery, error) {
	if kind == "" {
		return nil, nil
	}
	var ev event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, errMalformed
	}
	if ev.ObjectKind == "work_item" {
		// Work items other than issues (tasks, epics) are not issues.
		return nil, nil
	}
	if ev.ObjectKind != kind {
		return nil, errMalformed
	}
	if ev.Project.ID <= 0 || !validFullName(ev.Project.PathWithNamespace) {
		return nil, errMalformed
	}
	n := normalizer{x: x, eventID: eventID, correlation: correlation, ev: &ev, dest: ev.Project.PathWithNamespace}
	if kind == eventIssue {
		var attrs hookIssue
		if err := json.Unmarshal(ev.ObjectAttributes, &attrs); err != nil || attrs.ID == 0 || attrs.IID <= 0 || attrs.Action == "" {
			return nil, errMalformed
		}
		n.ref = workItemRef(x.channel.Ref, ev.Project.ID, attrs.IID)
		return n.issue(attrs)
	}
	var attrs hookNote
	if err := json.Unmarshal(ev.ObjectAttributes, &attrs); err != nil || attrs.ID == 0 || attrs.NoteableType == "" {
		return nil, errMalformed
	}
	if attrs.NoteableType != "Issue" || attrs.System {
		// Notes on merge requests, commits and snippets are not issue
		// messages, and system notes (a state or label change) are activity
		// the issue event reports.
		return nil, nil
	}
	if ev.Issue == nil || ev.Issue.ID == 0 || ev.Issue.IID <= 0 || attrs.AuthorID == 0 {
		return nil, errMalformed
	}
	n.ref = workItemRef(x.channel.Ref, ev.Project.ID, ev.Issue.IID)
	return n.comment(attrs)
}

// validFullName checks a project's path_with_namespace: at least two path
// segments (namespaces and the project) of GitLab path characters.
var pathSegment = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,255}$`)

func validFullName(s string) bool {
	segs := strings.Split(s, "/")
	if len(segs) < 2 {
		return false
	}
	for _, p := range segs {
		if !pathSegment.MatchString(p) || p == "." || p == ".." {
			return false
		}
	}
	return true
}

type normalizer struct {
	x           *Instance
	eventID     string
	correlation string
	ev          *event
	dest        string
	ref         model.ExternalRef
}

func (n normalizer) issueView(a hookIssue, labels []hookLabel) issueView {
	v := issueView{
		id: a.ID, iid: a.IID, title: a.Title, description: a.Description, state: a.State, url: a.URL,
		confidential: a.Confidential, assigneeIDs: a.AssigneeIDs,
		created: parseTime(a.CreatedAt), updated: parseTime(a.UpdatedAt), author: n.author(a.AuthorID),
	}
	for _, l := range labels {
		v.labels = append(v.labels, l.Title)
	}
	return v
}

// author returns the user with id; the event's user names the username
// only when it is that user (an event names the actor, not the author).
func (n normalizer) author(id int64) userView {
	if id != 0 && id == n.ev.User.ID {
		return userView{id: id, username: n.ev.User.Username}
	}
	return userView{id: id}
}

func (n normalizer) issue(a hookIssue) ([]delivery, error) {
	labels := n.ev.Labels
	if len(labels) == 0 {
		labels = a.Labels
	}
	iss := n.issueView(a, labels)
	state := n.state(iss)
	switch a.Action {
	case "open":
		if strings.TrimSpace(iss.description) != "" && iss.author.id != 0 {
			env := n.message(issueMessageID(iss.id), iss.author, iss.description, false, iss.created, iss.updated)
			env.WorkItem = &state
			return n.one(env), nil
		}
	}
	out := n.one(n.workItem(state, iss.updated))
	if _, ok := n.ev.Changes["description"]; ok && a.Action == "update" {
		id := issueMessageID(iss.id)
		if strings.TrimSpace(iss.description) == "" {
			out = append(out, n.update(model.EnvelopeMessageDeleted, id, "", false, time.Time{}))
		} else if iss.author.id != 0 {
			text := bodyText(iss.description)
			d := n.echoOwnEdit(n.update(model.EnvelopeMessageEdited, id, text, false, iss.updated), iss.description)
			first := n.message(id, iss.author, text, false, iss.created, iss.updated)
			first.WorkItem = &state
			d.fallback = &model.Ingress{Call: d.ingress.Call, Envelope: first}
			out = append(out, d)
		}
	}
	return out, nil
}

func (n normalizer) comment(a hookNote) ([]delivery, error) {
	id := noteMessageID(a.ID)
	created, updated := parseTime(a.CreatedAt), parseTime(a.UpdatedAt)
	author := n.author(a.AuthorID)
	action := a.Action
	if action == "" {
		// Older GitLab versions state no action: a note never edited has
		// equal times.
		action = "create"
		if updated.After(created) {
			action = "update"
		}
	}
	switch action {
	case "create":
		env := n.noteMessage(a, author, created, updated)
		state := n.state(n.issueView(*n.ev.Issue, n.ev.Issue.Labels))
		env.WorkItem = &state
		return n.one(env), nil
	case "update":
		d := n.echoOwnEdit(n.update(model.EnvelopeMessageEdited, id, bodyText(a.Note), a.Internal, updated), a.Note)
		first := n.noteMessage(a, author, created, updated)
		state := n.state(n.issueView(*n.ev.Issue, n.ev.Issue.Labels))
		first.WorkItem = &state
		d.fallback = &model.Ingress{Call: d.ingress.Call, Envelope: first}
		return []delivery{d}, nil
	}
	return nil, nil
}

// noteMessage is the message envelope of a note. A note by the channel's
// account carrying an echo marker reports the attempt it echoes.
func (n normalizer) noteMessage(a hookNote, author userView, created, updated time.Time) model.Envelope {
	env := n.message(noteMessageID(a.ID), author, bodyText(a.Note), a.Internal, created, updated)
	if op, attempt, _, ok := parseMarker(a.Note); ok && n.x.cfg.AccountID != 0 && author.id == n.x.cfg.AccountID {
		ch := n.x.channel.Ref
		env.References = append(env.References, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: op}, attempt))
	}
	return env
}

// echoOwnEdit makes d, an edit, the echo of the attempt named by the marker
// of rawBody when the channel's own account made it. An edit by anyone else
// names no attempt, whatever the body says: the marker may be a copy, or
// survive a person's edit of uma's note.
func (n normalizer) echoOwnEdit(d delivery, rawBody string) delivery {
	op, attempt, _, ok := parseMarker(rawBody)
	if !ok || n.x.cfg.AccountID == 0 || n.ev.User.ID != n.x.cfg.AccountID {
		return d
	}
	ch := n.x.channel.Ref
	d.ingress.Envelope.References = append(d.ingress.Envelope.References, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: op}, attempt))
	return d
}

func (n normalizer) one(env model.Envelope) []delivery {
	return []delivery{{ingress: n.ingress(env)}}
}

func (n normalizer) ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	if c := model.CorrelationID("gitlab-" + n.correlation); n.correlation != "" && c.Validate() == nil {
		in.Correlation = c
	}
	return in
}

func (n normalizer) base(kind string, at time.Time) model.Envelope {
	return model.Envelope{
		EventID: n.eventID, Destination: n.dest, Kind: kind,
		References: []model.ExternalRef{n.ref}, Audience: model.Audience{Visibility: model.Public},
		OccurredAt: at.UTC(), SourceRevision: revision(at), Assurance: "verified",
	}
}

// audience is the visibility of a note: an internal note is visible to
// project members only.
func audience(internal bool) model.Audience {
	if internal {
		return model.Audience{Visibility: model.Internal}
	}
	return model.Audience{Visibility: model.Public}
}

func (n normalizer) message(id string, author userView, text string, internal bool, created, updated time.Time) model.Envelope {
	kind := model.EnvelopeMessage
	if author.bot() {
		kind = model.EnvelopeAutomatedMessage
	}
	env := n.base(kind, created)
	env.MessageID, env.Sender, env.SourceRevision = id, n.user(author.id), revision(firstSet(updated, created))
	env.SenderKind = senderKind(author)
	env.Audience = audience(internal)
	env.Content = textContent(text)
	return env
}

// update is an edit or deletion of the message id, by the event's user.
func (n normalizer) update(kind, id, text string, internal bool, at time.Time) delivery {
	env := n.base(kind, at)
	actor := userView{id: n.ev.User.ID, username: n.ev.User.Username}
	env.MessageID, env.Sender, env.SenderKind = id, n.user(actor.id), senderKind(actor)
	env.Audience = audience(internal)
	if kind == model.EnvelopeMessageEdited {
		env.Content = textContent(text)
	}
	return delivery{ingress: n.ingress(env)}
}

func (n normalizer) workItem(state model.WorkItemState, at time.Time) model.Envelope {
	env := n.base(model.EnvelopeWorkItem, at)
	env.WorkItem = &state
	return env
}

// state is the provider view of iss. GitLab names an issue's state opened
// or closed; the work item status is open or closed.
func (n normalizer) state(iss issueView) model.WorkItemState {
	s := model.WorkItemState{
		Subject: iss.title, URL: iss.url, RawStatus: iss.state,
		Binding: model.WorkItemBindingActive, LocalFields: slices.Clone(n.x.cfg.LocalFields),
	}
	switch iss.state {
	case "opened":
		s.Status = model.WorkItemOpen
	case "closed":
		s.Status = model.WorkItemClosed
	}
	if iss.author.id != 0 {
		s.Requester = n.user(iss.author.id)
	}
	for _, id := range iss.assigneeIDs {
		if id != 0 {
			s.Assignees = append(s.Assignees, n.user(id))
		}
	}
	if len(iss.labels) > 0 {
		data, _ := json.Marshal(sortedLabels(iss.labels))
		s.Fields = append(s.Fields, model.Extension{Name: FieldLabels, Version: 1, Data: data})
	}
	return s
}

func (n normalizer) user(id int64) model.ExternalRef {
	if id == 0 {
		return model.ExternalRef{}
	}
	return model.ExternalRef{Channel: n.x.channel.Ref, Namespace: Namespace, Kind: RefKindUser, ID: strconv.FormatInt(id, 10)}
}

// senderKind is the sender kind of a user: a bot for the bot user of an
// access token, a person (empty) otherwise. GitLab's webhooks carry no
// account type, so the generated bot username is the signal.
func senderKind(u userView) string {
	if u.bot() {
		return model.ContactKindBot
	}
	return ""
}

func textContent(text string) model.Content {
	if text == "" {
		return model.Content{}
	}
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
}

func revision(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func firstSet(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}
