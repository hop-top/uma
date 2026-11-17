package linear

import (
	"bytes"
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

// hook is a webhook payload (https://linear.app/developers/webhooks): the
// fields common to data change events. Data is decoded per type.
type hook struct {
	Action           string                     `json:"action"`
	Type             string                     `json:"type"`
	Actor            *hookActor                 `json:"actor"`
	Data             json.RawMessage            `json:"data"`
	UpdatedFrom      map[string]json.RawMessage `json:"updatedFrom"`
	WebhookTimestamp float64                    `json:"webhookTimestamp"`
}

type hookActor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// hookIssue is the data of an Issue webhook.
type hookIssue struct {
	ID          string  `json:"id"`
	Identifier  string  `json:"identifier"`
	Title       string  `json:"title"`
	URL         string  `json:"url"`
	Description *string `json:"description"`
	CreatedAt   string  `json:"createdAt"`
	UpdatedAt   string  `json:"updatedAt"`
	CreatorID   string  `json:"creatorId"`
	AssigneeID  string  `json:"assigneeId"`

	Team *struct {
		Key string `json:"key"`
	} `json:"team"`
	State *struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// hookComment is the data of a Comment webhook.
type hookComment struct {
	ID         string          `json:"id"`
	Body       string          `json:"body"`
	CreatedAt  string          `json:"createdAt"`
	UpdatedAt  string          `json:"updatedAt"`
	EditedAt   string          `json:"editedAt"`
	IssueID    string          `json:"issueId"`
	UserID     string          `json:"userId"`
	ExternalID string          `json:"externalUserId"`
	BotActor   json.RawMessage `json:"botActor"`
	Issue      *struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		URL        string `json:"url"`
		Team       struct {
			Key string `json:"key"`
		} `json:"team"`
	} `json:"issue"`
}

var errMalformed = errors.New("linear: malformed webhook payload")

// issueView is an issue as the adapter reports it, from a webhook or a read.
type issueView struct {
	id, identifier, title, url string
	team                       string
	stateType, stateName       string
	creator, assignee          string
	labels                     []string
	description                string
	created, updated           time.Time
}

func viewOfHook(i *hookIssue) issueView {
	v := issueView{
		id: i.ID, identifier: i.Identifier, title: i.Title, url: i.URL, creator: i.CreatorID, assignee: i.AssigneeID,
		created: parseTime(i.CreatedAt), updated: parseTime(i.UpdatedAt),
	}
	if i.Description != nil {
		v.description = *i.Description
	}
	if i.Team != nil {
		v.team = i.Team.Key
	}
	if i.State != nil {
		v.stateType, v.stateName = i.State.Type, i.State.Name
	}
	for _, l := range i.Labels {
		v.labels = append(v.labels, l.Name)
	}
	return v
}

func viewOfIssue(i *gqlIssue) issueView {
	v := issueView{
		id: i.ID, identifier: i.Identifier, title: i.Title, url: i.URL, team: i.Team.Key,
		stateType: i.State.Type, stateName: i.State.Name, created: parseTime(i.CreatedAt), updated: parseTime(i.UpdatedAt),
	}
	if i.Description != nil {
		v.description = *i.Description
	}
	if i.Creator != nil {
		v.creator = i.Creator.ID
	}
	if i.Assignee != nil {
		v.assignee = i.Assignee.ID
	}
	for _, l := range i.Labels.Nodes {
		v.labels = append(v.labels, l.Name)
	}
	return v
}

// normalize maps one delivery to the envelopes it reports, in order. Events
// the adapter does not consume map to none. eventID identifies the event;
// deliveryID is the unsigned Linear-Delivery, kept only as correlation.
func (x *Instance) normalize(eventID, deliveryID string, h hook) ([]delivery, error) {
	if h.Type != "Issue" && h.Type != "Comment" {
		return nil, nil
	}
	if h.Action != "create" && h.Action != "update" && h.Action != "remove" {
		return nil, nil
	}
	n := normalizer{x: x, eventID: eventID, delivery: deliveryID, h: h}
	if h.Type == "Issue" {
		var i hookIssue
		if err := json.Unmarshal(h.Data, &i); err != nil || i.ID == "" || !validUUID(i.ID) {
			return nil, errMalformed
		}
		v := viewOfHook(&i)
		if v.team == "" {
			return nil, errMalformed
		}
		n.dest, n.ref = v.team, workItemRef(x.channel.Ref, v.id)
		return n.issue(v)
	}
	var c hookComment
	if err := json.Unmarshal(h.Data, &c); err != nil || c.ID == "" || !validUUID(c.ID) {
		return nil, errMalformed
	}
	if c.IssueID == "" || c.Issue == nil {
		// A comment on a project update, document or initiative is not an
		// issue message.
		return nil, nil
	}
	if !validUUID(c.Issue.ID) || c.Issue.Team.Key == "" {
		return nil, errMalformed
	}
	n.dest, n.ref = c.Issue.Team.Key, workItemRef(x.channel.Ref, c.Issue.ID)
	return n.comment(c)
}

type normalizer struct {
	x        *Instance
	eventID  string
	delivery string
	h        hook
	dest     string
	ref      model.ExternalRef
}

func (n normalizer) issue(v issueView) ([]delivery, error) {
	state := n.state(v)
	id := issueMessageID(v.id)
	switch n.h.Action {
	case "create":
		if strings.TrimSpace(v.description) != "" && v.creator != "" {
			env := n.message(model.EnvelopeMessage, id, v.creator, false, v.description, v.created, v.updated)
			env.WorkItem = &state
			return n.one(env), nil
		}
	case "remove":
		state.Binding = model.WorkItemBindingDeleted
	}
	out := n.one(n.workItem(state, v.updated))
	if _, edited := n.h.UpdatedFrom["description"]; edited && n.h.Action == "update" {
		if strings.TrimSpace(v.description) == "" {
			out = append(out, n.update(model.EnvelopeMessageDeleted, id, "", time.Time{}))
		} else if v.creator != "" {
			d := n.update(model.EnvelopeMessageEdited, id, v.description, v.updated)
			first := n.message(model.EnvelopeMessage, id, v.creator, false, v.description, v.created, v.updated)
			first.WorkItem = &state
			d.fallback = &model.Ingress{Call: d.ingress.Call, Envelope: first}
			out = append(out, d)
		}
	}
	return out, nil
}

func (n normalizer) comment(c hookComment) ([]delivery, error) {
	bot := isBot(c.BotActor) || n.h.Actor.isApp()
	author := firstSet(c.UserID, c.ExternalID)
	if author == "" && bot && n.h.Actor != nil {
		author = n.h.Actor.ID
	}
	id := commentMessageID(c.ID)
	updated := firstTime(parseTime(c.EditedAt), parseTime(c.UpdatedAt), parseTime(c.CreatedAt))
	switch n.h.Action {
	case "create":
		if author == "" {
			return nil, errMalformed
		}
		return n.one(n.newComment(c, author, bot)), nil
	case "update":
		if author == "" {
			return nil, errMalformed
		}
		d := n.echoOwnEdit(n.update(model.EnvelopeMessageEdited, id, bodyText(c.Body), updated), c.Body, author)
		d.fallback = &model.Ingress{Call: d.ingress.Call, Envelope: n.newComment(c, author, bot)}
		return []delivery{d}, nil
	case "remove":
		// Linear states no deletion time: an unordered update applies in
		// arrival order, where the last edit time would read as stale.
		return []delivery{n.update(model.EnvelopeMessageDeleted, id, "", time.Time{})}, nil
	}
	return nil, nil
}

// newComment is the message envelope of a comment. A comment webhook carries
// the issue's title and link but not its state, so the work item state it
// reports has no status; it binds the work item, and the issue's own
// webhooks and refetch report the rest. A comment by the channel's account
// carrying an echo marker reports the attempt it echoes.
func (n normalizer) newComment(c hookComment, author string, bot bool) model.Envelope {
	kind := model.EnvelopeMessage
	if bot {
		kind = model.EnvelopeAutomatedMessage
	}
	created := parseTime(c.CreatedAt)
	env := n.message(kind, commentMessageID(c.ID), author, bot, bodyText(c.Body), created, firstTime(parseTime(c.EditedAt), parseTime(c.UpdatedAt), created))
	if op, attempt, _, ok := parseMarker(c.Body); ok && n.x.cfg.AccountID != "" && author == n.x.cfg.AccountID {
		ch := n.x.channel.Ref
		env.References = append(env.References, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: op}, attempt))
	}
	state := model.WorkItemState{
		Subject: c.Issue.Title, URL: c.Issue.URL, Binding: model.WorkItemBindingActive,
		LocalFields: slices.Clone(n.x.cfg.LocalFields),
	}
	env.WorkItem = &state
	return env
}

// bodyText is body without an echo marker.
func bodyText(body string) string {
	if _, _, rest, ok := parseMarker(body); ok {
		return rest
	}
	return body
}

// echoOwnEdit makes d, an edit, the echo of the attempt named by the marker
// of rawBody when the channel's own account made it. An edit by anyone else
// names no attempt, whatever the body says: the marker may be a copy, or
// survive a person's edit of uma's comment. An application token's edits
// are reported with the application as actor, so the comment's author is
// compared too.
func (n normalizer) echoOwnEdit(d delivery, rawBody, author string) delivery {
	op, attempt, _, ok := parseMarker(rawBody)
	acct := n.x.cfg.AccountID
	if !ok || acct == "" || n.h.Actor == nil || (n.h.Actor.ID != acct && (!n.h.Actor.isApp() || author != acct)) {
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
	if c := model.CorrelationID("linear-" + n.delivery); c.Validate() == nil {
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

func (n normalizer) message(kind, id, author string, bot bool, text string, created, updated time.Time) model.Envelope {
	env := n.base(kind, created)
	env.MessageID, env.Sender, env.SourceRevision = id, n.user(author), revision(firstTime(updated, created))
	if bot {
		env.SenderKind = model.ContactKindBot
	}
	env.Content = textContent(text)
	return env
}

// update is an edit or deletion of the message id, by the event's actor.
func (n normalizer) update(kind, id, text string, at time.Time) delivery {
	env := n.base(kind, at)
	env.MessageID = id
	if a := n.h.Actor; a != nil {
		env.Sender = n.user(a.ID)
		if a.isApp() {
			env.SenderKind = model.ContactKindBot
		}
	}
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

// state is the provider view of an issue. Linear's workflow state types
// triage, backlog, unstarted and started are open; completed, canceled and
// duplicate are closed. The raw status is "<type>:<state name>".
func (n normalizer) state(v issueView) model.WorkItemState {
	s := model.WorkItemState{
		Subject: v.title, URL: v.url, Binding: model.WorkItemBindingActive,
		LocalFields: slices.Clone(n.x.cfg.LocalFields),
	}
	switch v.stateType {
	case "triage", "backlog", "unstarted", "started":
		s.Status = model.WorkItemOpen
	case "completed", "canceled", "duplicate":
		s.Status = model.WorkItemClosed
	}
	if v.stateType != "" {
		s.RawStatus = v.stateType
		if v.stateName != "" {
			s.RawStatus += ":" + v.stateName
		}
	}
	if v.creator != "" {
		s.Requester = n.user(v.creator)
	}
	if v.assignee != "" {
		s.Assignees = []model.ExternalRef{n.user(v.assignee)}
	}
	if len(v.labels) > 0 {
		names := slices.Clone(v.labels)
		slices.Sort(names)
		data, _ := json.Marshal(names)
		s.Fields = append(s.Fields, model.Extension{Name: FieldLabels, Version: 1, Data: data})
	}
	return s
}

func (n normalizer) user(id string) model.ExternalRef {
	if id == "" {
		return model.ExternalRef{}
	}
	return model.ExternalRef{Channel: n.x.channel.Ref, Namespace: Namespace, Kind: RefKindUser, ID: id}
}

// isBot reports whether a botActor field names a bot: anything but absent,
// null or empty.
func isBot(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && string(t) != "null" && string(t) != `""` && string(t) != "{}"
}

// isApp reports whether the actor is an application or integration rather
// than a person.
func (a *hookActor) isApp() bool {
	if a == nil {
		return false
	}
	switch strings.ToLower(a.Type) {
	case "oauthclient", "integration", "application", "app":
		return true
	}
	return false
}

func textContent(text string) model.Content {
	if text == "" {
		return model.Content{}
	}
	return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func revision(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func firstTime(ts ...time.Time) time.Time {
	for _, t := range ts {
		if !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

func firstSet(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
