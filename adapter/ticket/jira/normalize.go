package jira

import (
	"encoding/json"
	"errors"
	"slices"
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

// payload is the part of an admin webhook body the adapter reads. Comment
// events carry the comment; issue events the changelog of an update.
type payload struct {
	// Timestamp is the event time in epoch milliseconds.
	Timestamp    int64    `json:"timestamp"`
	WebhookEvent string   `json:"webhookEvent"`
	User         *user    `json:"user"`
	Issue        *issue   `json:"issue"`
	Comment      *comment `json:"comment"`
	Changelog    *struct {
		Items []struct {
			Field string `json:"field"`
		} `json:"items"`
	} `json:"changelog"`
}

var errMalformed = errors.New("jira: malformed webhook payload")

// normalize maps one delivery to the envelopes it reports, in order. Events
// the adapter does not consume map to none. eventID identifies the event;
// identifier is the unsigned webhook identifier, kept only as correlation.
// The event is named by the signed body's webhookEvent.
func (x *Instance) normalize(eventID, identifier string, body []byte) ([]delivery, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, errMalformed
	}
	switch p.WebhookEvent {
	case "jira:issue_created", "jira:issue_updated", "jira:issue_deleted", "comment_created", "comment_updated", "comment_deleted":
	default:
		return nil, nil
	}
	issueID := ""
	if p.Issue != nil {
		issueID = p.Issue.ID
	}
	if issueID == "" && p.Comment != nil {
		issueID = issueIDOfComment(p.Comment.Self)
	}
	if !idOK(issueID) || p.Issue == nil {
		return nil, errMalformed
	}
	n := normalizer{x: x, eventID: eventID, p: &p, issue: p.Issue, ref: workItemRef(x.channel.Ref, issueID)}
	n.destination = n.project()
	if n.destination == "" {
		return nil, errMalformed
	}
	if c := model.CorrelationID("jira-" + identifier); identifier != "" && c.Validate() == nil {
		n.correlation = c
	}
	// An issue event never carries a comment; a comment event always does.
	// The event is named by the signed body, so a payload that is not the
	// shape of the event it names is malformed.
	if isComment := strings.HasPrefix(p.WebhookEvent, "comment_"); isComment != (p.Comment != nil) {
		return nil, errMalformed
	}
	if p.Comment != nil {
		if !idOK(p.Comment.ID) {
			return nil, errMalformed
		}
		return n.comment()
	}
	return n.issueEvent()
}

// issueIDOfComment reads the issue ID from a comment's self URL,
// ".../issue/{id}/comment/{id}".
func issueIDOfComment(self string) string {
	segs := strings.Split(self, "/")
	for i, s := range segs {
		if s == "issue" && i+1 < len(segs) {
			return segs[i+1]
		}
	}
	return ""
}

type normalizer struct {
	x           *Instance
	eventID     string
	correlation model.CorrelationID
	p           *payload
	issue       *issue
	ref         model.ExternalRef
	destination string
}

// project is the project key of the issue: the destination inboxes bind.
func (n normalizer) project() string {
	if pr := n.issue.Fields.Project; pr != nil && pr.Key != "" {
		return pr.Key
	}
	if i := strings.LastIndex(n.issue.Key, "-"); i > 0 {
		return n.issue.Key[:i]
	}
	return ""
}

func (n normalizer) issueEvent() ([]delivery, error) {
	iss := n.issue
	state := n.state(iss)
	updated := firstSet(parseTime(iss.Fields.Updated), n.eventTime())
	switch n.p.WebhookEvent {
	case "jira:issue_created":
		author := firstUser(iss.Fields.Creator, iss.Fields.Reporter)
		if text := bodyText(iss.Fields.Description); text != "" && author != nil {
			env := n.message(iss, issueMessageID(iss.ID), author, text, firstSet(parseTime(iss.Fields.Created), updated), updated)
			env.WorkItem = &state
			return n.one(env), nil
		}
	case "jira:issue_deleted":
		state.Binding = model.WorkItemBindingDeleted
	}
	out := n.one(n.workItem(state, updated))
	if n.p.WebhookEvent == "jira:issue_updated" && n.descriptionChanged() {
		id := issueMessageID(iss.ID)
		if text := bodyText(iss.Fields.Description); text == "" {
			out = append(out, n.update(model.EnvelopeMessageDeleted, id, "", time.Time{}, n.p.User))
		} else if author := firstUser(iss.Fields.Creator, iss.Fields.Reporter); author != nil {
			d := n.update(model.EnvelopeMessageEdited, id, text, updated, n.p.User)
			first := n.message(iss, id, author, text, firstSet(parseTime(iss.Fields.Created), updated), updated)
			first.WorkItem = &state
			d.fallback = n.fallbackOf(d, first)
			out = append(out, d)
		}
	}
	return out, nil
}

// descriptionChanged reports a changelog that changed the description.
func (n normalizer) descriptionChanged() bool {
	if n.p.Changelog == nil {
		return false
	}
	for _, it := range n.p.Changelog.Items {
		if it.Field == "description" {
			return true
		}
	}
	return false
}

func (n normalizer) comment() ([]delivery, error) {
	c := n.p.Comment
	id := commentMessageID(c.ID)
	created, updated := parseTime(c.Created), parseTime(c.Updated)
	switch n.p.WebhookEvent {
	case "comment_created":
		if c.Author == nil || c.Author.AccountID == "" {
			return nil, errMalformed
		}
		env := n.commentMessage(c, firstSet(created, n.eventTime()), firstSet(updated, created, n.eventTime()))
		if n.issue.Fields.Status != nil {
			state := n.state(n.issue)
			env.WorkItem = &state
		}
		return n.one(env), nil
	case "comment_updated":
		if c.Author == nil || c.Author.AccountID == "" {
			return nil, errMalformed
		}
		editor := firstUser(c.UpdateAuthor, n.p.User, c.Author)
		at := firstSet(updated, n.eventTime())
		d := n.update(model.EnvelopeMessageEdited, id, bodyText(c.Body), at, editor)
		d.ingress.Envelope.Audience = audienceOf(*c)
		if n.x.cfg.AccountID != "" && editor.AccountID == n.x.cfg.AccountID {
			d.ingress.Envelope = n.echo(d.ingress.Envelope, c)
		}
		d.fallback = n.fallbackOf(d, n.commentMessage(c, firstSet(created, at), at))
		return []delivery{d}, nil
	case "comment_deleted":
		// Jira states no deletion time: an unordered update applies in
		// arrival order, where the last edit time would read as stale.
		return []delivery{n.update(model.EnvelopeMessageDeleted, id, "", time.Time{}, n.p.User)}, nil
	}
	return nil, nil
}

// commentMessage is the message envelope of a comment. A comment by the
// channel's account carrying an echo property reports the attempt it
// echoes.
func (n normalizer) commentMessage(c *comment, created, updated time.Time) model.Envelope {
	env := n.message(n.issue, commentMessageID(c.ID), c.Author, bodyText(c.Body), created, updated)
	env.Audience = audienceOf(*c)
	if n.x.own(*c) {
		env = n.echo(env, c)
	}
	return env
}

// echo adds the echo reference named by the comment's echo property, when
// the payload carries one.
func (n normalizer) echo(env model.Envelope, c *comment) model.Envelope {
	if op, attempt, ok := echoOf(c.Properties); ok {
		ch := n.x.channel.Ref
		env.References = append(env.References, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: op}, attempt))
	}
	return env
}

func audienceOf(c comment) model.Audience {
	if c.restricted() {
		return model.Audience{Visibility: model.Internal}
	}
	return model.Audience{Visibility: model.Public}
}

func (n normalizer) one(env model.Envelope) []delivery {
	return []delivery{{ingress: n.ingress(env)}}
}

func (n normalizer) ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	in.Correlation = n.correlation
	return in
}

// fallbackOf is the ingress recording env in place of d, an edit of a
// message the sink does not know.
func (n normalizer) fallbackOf(d delivery, env model.Envelope) *model.Ingress {
	in := n.ingress(env)
	in.Call = d.ingress.Call
	return &in
}

// eventTime is the event's own time (epoch milliseconds), zero when the
// payload has none.
func (n normalizer) eventTime() time.Time {
	if n.p.Timestamp <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(n.p.Timestamp).UTC()
}

func (n normalizer) base(kind string, at time.Time) model.Envelope {
	return model.Envelope{
		EventID: n.eventID, Destination: n.destination, Kind: kind,
		References: []model.ExternalRef{n.ref}, Audience: model.Audience{Visibility: model.Public},
		OccurredAt: at.UTC(), SourceRevision: revision(at), Assurance: "verified",
	}
}

func (n normalizer) message(_ *issue, id string, author *user, text string, created, updated time.Time) model.Envelope {
	env := n.base(kindFor(author), created)
	env.MessageID, env.Sender, env.SourceRevision = id, n.user(author), revision(firstSet(updated, created))
	env.SenderKind = senderKind(author)
	env.Content = textContent(text)
	return env
}

// update is an edit or deletion of the message id, by editor.
func (n normalizer) update(kind, id, text string, at time.Time, editor *user) delivery {
	env := n.base(kind, at)
	env.MessageID, env.Sender, env.SenderKind = id, n.user(editor), senderKind(editor)
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

// state is the provider view of iss. The status maps from the status
// category: "new" and "indeterminate" (to do, in progress) are open, "done"
// is closed; the raw status is the status name.
func (n normalizer) state(iss *issue) model.WorkItemState {
	s := model.WorkItemState{
		Subject: iss.Fields.Summary, URL: n.x.issueURL(iss.Key),
		Binding: model.WorkItemBindingActive, LocalFields: slices.Clone(n.x.cfg.LocalFields),
	}
	if st := iss.Fields.Status; st != nil {
		s.RawStatus = st.Name
		switch st.StatusCategory.Key {
		case "new", "indeterminate":
			s.Status = model.WorkItemOpen
		case "done":
			s.Status = model.WorkItemClosed
		}
	}
	if u := firstUser(iss.Fields.Reporter, iss.Fields.Creator); u != nil && u.AccountID != "" {
		s.Requester = n.user(u)
	}
	if a := iss.Fields.Assignee; a != nil && a.AccountID != "" {
		s.Assignees = append(s.Assignees, n.user(a))
	}
	if len(iss.Fields.Labels) > 0 {
		names := slices.Clone(iss.Fields.Labels)
		slices.Sort(names)
		data, _ := json.Marshal(names)
		s.Fields = append(s.Fields, model.Extension{Name: FieldLabels, Version: 1, Data: data})
	}
	return s
}

// issueURL is the browser URL of an issue.
func (x *Instance) issueURL(key string) string {
	if key == "" {
		return ""
	}
	return x.cfg.site() + "/browse/" + key
}

func (n normalizer) user(u *user) model.ExternalRef {
	if u == nil || u.AccountID == "" {
		return model.ExternalRef{}
	}
	return model.ExternalRef{Channel: n.x.channel.Ref, Namespace: Namespace, Kind: RefKindUser, ID: u.AccountID}
}

func firstUser(us ...*user) *user {
	for _, u := range us {
		if u != nil && u.AccountID != "" {
			return u
		}
	}
	return nil
}

// kindFor marks messages authored by apps automated.
func kindFor(u *user) string {
	if u != nil && u.AccountType == "app" {
		return model.EnvelopeAutomatedMessage
	}
	return model.EnvelopeMessage
}

// senderKind is the sender kind of an account: a bot for an app account, a
// person (empty) otherwise (Atlassian and customer accounts). The account
// type comes from the signed webhook payload.
func senderKind(u *user) string {
	if u != nil && u.AccountType == "app" {
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
