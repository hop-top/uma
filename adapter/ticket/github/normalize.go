package github

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/model"
)

// delivery is one envelope of a webhook delivery, with the envelope to
// record instead when the first targets an unknown message.
type delivery struct {
	ingress  model.Ingress
	fallback *model.Ingress
}

// event is the part of an issues or issue_comment payload the adapter
// reads, in go-github's types. go-github's EditChange lacks the transfer
// fields, so changes are decoded here.
type event struct {
	Action     string           `json:"action"`
	Issue      *gh.Issue        `json:"issue"`
	Comment    *gh.IssueComment `json:"comment"`
	Repository *gh.Repository   `json:"repository"`
	Sender     *gh.User         `json:"sender"`
	Changes    *struct {
		Body          *gh.EditBody   `json:"body"`
		NewIssue      *gh.Issue      `json:"new_issue"`
		NewRepository *gh.Repository `json:"new_repository"`
	} `json:"changes"`
}

var errMalformed = errors.New("github: malformed webhook payload")

// normalize maps one delivery to the envelopes it reports, in order. Events
// the adapter does not consume map to none. eventID identifies the event;
// deliveryID is the unsigned X-GitHub-Delivery, kept only as correlation.
// kind is the unsigned X-GitHub-Event: a payload that is not the shape of the
// event it names is malformed, so a header cannot turn one signed body into
// another event.
func (x *Instance) normalize(eventID, deliveryID, kind string, payload []byte) ([]delivery, error) {
	if kind != "issues" && kind != "issue_comment" {
		return nil, nil
	}
	var ev event
	if err := json.Unmarshal(payload, &ev); err != nil {
		return nil, errMalformed
	}
	if (kind == "issue_comment") != (ev.Comment != nil) {
		// An issues payload never carries a comment; an issue_comment one
		// always does.
		return nil, errMalformed
	}
	iss, repo := ev.Issue, ev.Repository.GetFullName()
	if iss == nil || iss.GetNumber() <= 0 || iss.GetID() == 0 || ev.Action == "" || !validFullName(repo) {
		return nil, errMalformed
	}
	if iss.PullRequestLinks != nil {
		// A pull request is not an issue work item.
		return nil, nil
	}
	n := normalizer{x: x, eventID: eventID, delivery: deliveryID, ev: &ev, repo: repo, ref: workItemRef(x.channel.Ref, repo, iss.GetNumber())}
	if kind == "issues" {
		return n.issue()
	}
	return n.comment()
}

func validFullName(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	return ok && validName(owner) && validName(repo)
}

type normalizer struct {
	x        *Instance
	eventID  string
	delivery string
	ev       *event
	repo     string
	ref      model.ExternalRef
}

func (n normalizer) issue() ([]delivery, error) {
	iss := n.ev.Issue
	state := n.state(iss)
	switch n.ev.Action {
	case "opened":
		if body := strings.TrimSpace(iss.GetBody()); body != "" && iss.GetUser().GetID() != 0 {
			env := n.message(kindFor(iss.GetUser()), issueMessageID(iss.GetID()), iss.GetUser(), iss.GetBody(), iss.GetCreatedAt().Time, iss.GetUpdatedAt().Time)
			env.WorkItem = &state
			return n.one(env), nil
		}
	case "deleted":
		state.Binding = model.WorkItemBindingDeleted
	case "transferred":
		c := n.ev.Changes
		if c == nil || c.NewIssue.GetNumber() <= 0 || !validFullName(c.NewRepository.GetFullName()) {
			return nil, errMalformed
		}
		next := workItemRef(n.x.channel.Ref, c.NewRepository.GetFullName(), c.NewIssue.GetNumber())
		state.Binding, state.Successor = model.WorkItemBindingMoved, &next
	}
	out := n.one(n.workItem(state, iss.GetUpdatedAt().Time))
	if n.ev.Action == "edited" && n.ev.Changes != nil && n.ev.Changes.Body != nil {
		id := issueMessageID(iss.GetID())
		if strings.TrimSpace(iss.GetBody()) == "" {
			out = append(out, n.update(model.EnvelopeMessageDeleted, id, "", time.Time{}))
		} else if iss.GetUser().GetID() != 0 {
			text := bodyText(iss.GetBody())
			d := n.echoOwnEdit(n.update(model.EnvelopeMessageEdited, id, text, iss.GetUpdatedAt().Time), iss.GetBody())
			first := n.message(kindFor(iss.GetUser()), id, iss.GetUser(), text, iss.GetCreatedAt().Time, iss.GetUpdatedAt().Time)
			first.WorkItem = &state
			d.fallback = &model.Ingress{Call: d.ingress.Call, Envelope: first}
			out = append(out, d)
		}
	}
	return out, nil
}

func (n normalizer) comment() ([]delivery, error) {
	c := n.ev.Comment
	if c == nil || c.GetID() == 0 || c.GetUser().GetID() == 0 {
		return nil, errMalformed
	}
	id := commentMessageID(c.GetID())
	updated := c.GetUpdatedAt().Time
	switch n.ev.Action {
	case "created":
		out := n.one(n.newComment(c))
		if c.GetReactions().GetTotalCount() > 0 {
			out = append(out, n.reactionSummary(c, c.GetCreatedAt().Time)...)
		}
		return out, nil
	case "edited":
		d := n.echoOwnEdit(n.update(model.EnvelopeMessageEdited, id, n.commentText(c), updated), c.GetBody())
		first := n.newComment(c)
		d.fallback = &model.Ingress{Call: d.ingress.Call, Envelope: first}
		return append([]delivery{d}, n.reactionSummary(c, updated)...), nil
	case "deleted":
		// GitHub states no deletion time: an unordered update applies in
		// arrival order, where the last edit time would read as stale.
		return []delivery{n.update(model.EnvelopeMessageDeleted, id, "", time.Time{})}, nil
	}
	return nil, nil
}

// newComment is the message envelope of a comment, with the issue state.
func (n normalizer) newComment(c *gh.IssueComment) model.Envelope {
	env := n.commentMessage(c)
	state := n.state(n.ev.Issue)
	env.WorkItem = &state
	return env
}

// commentMessage is the message envelope of a comment. A comment by the
// channel's account carrying an echo marker reports the attempt it echoes.
func (n normalizer) commentMessage(c *gh.IssueComment) model.Envelope {
	env := n.message(kindFor(c.GetUser()), commentMessageID(c.GetID()), c.GetUser(), n.commentText(c), c.GetCreatedAt().Time, c.GetUpdatedAt().Time)
	if op, attempt, _, ok := parseMarker(c.GetBody()); ok && n.x.own(c) {
		ch := n.x.channel.Ref
		env.References = append(env.References, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: op}, attempt))
	}
	return env
}

// commentText is a comment body without an echo marker.
func (n normalizer) commentText(c *gh.IssueComment) string { return bodyText(c.GetBody()) }

// bodyText is body without an echo marker.
func bodyText(body string) string {
	if _, _, rest, ok := parseMarker(body); ok {
		return rest
	}
	return body
}

// echoOwnEdit makes d, an edit, the echo of the attempt named by the
// marker of rawBody when the channel's own account made it. An edit by
// anyone else names no attempt, whatever the body says: the marker may be
// a copy, or survive a person's edit of uma's comment.
func (n normalizer) echoOwnEdit(d delivery, rawBody string) delivery {
	op, attempt, _, ok := parseMarker(rawBody)
	if !ok || n.x.cfg.AccountID == 0 || n.ev.Sender.GetID() != n.x.cfg.AccountID {
		return d
	}
	ch := n.x.channel.Ref
	d.ingress.Envelope.References = append(d.ingress.Envelope.References, model.EchoRef(ch, model.OperationRef{Tenant: ch.Tenant, ID: op}, attempt))
	return d
}

// reactionSummary is the count-only snapshot of a comment's reactions, from
// the rollup its payload carries, as a system actor's reaction envelope.
// GitHub sends no reaction webhooks and names no reactor in the rollup, so
// counts are all the adapter learns, and only when a comment payload
// arrives. The rollup lists every content, so the snapshot is whole. A
// payload without a rollup reports nothing.
func (n normalizer) reactionSummary(c *gh.IssueComment, at time.Time) []delivery {
	r := c.GetReactions()
	if r == nil {
		return nil
	}
	counts := []model.ReactionCount{}
	for _, rc := range reactionContents {
		var v *int
		switch rc.content {
		case "+1":
			v = r.PlusOne
		case "-1":
			v = r.MinusOne
		case "laugh":
			v = r.Laugh
		case "confused":
			v = r.Confused
		case "heart":
			v = r.Heart
		case "hooray":
			v = r.Hooray
		case "rocket":
			v = r.Rocket
		case "eyes":
			v = r.Eyes
		}
		if v != nil && *v > 0 {
			counts = append(counts, model.ReactionCount{Emoji: rc.emoji, Count: *v})
		}
	}
	data, err := json.Marshal(model.ReactionSummary{Counts: counts, Complete: true})
	if err != nil {
		return nil
	}
	env := n.base(model.EnvelopeReactionAdded, at)
	env.EventID += "/reactions"
	env.MessageID, env.SystemActor = commentMessageID(c.GetID()), true
	env.SourceRevision = revision(firstSet(c.GetUpdatedAt().Time, c.GetCreatedAt().Time))
	env.Content = model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{
		Name: model.SchemaReactionSummary, Version: 1, Data: data,
	}}}}
	return n.one(env)
}

func (n normalizer) one(env model.Envelope) []delivery {
	return []delivery{{ingress: n.ingress(env)}}
}

func (n normalizer) ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	if c := model.CorrelationID("github-" + n.delivery); c.Validate() == nil {
		in.Correlation = c
	}
	return in
}

func (n normalizer) base(kind string, at time.Time) model.Envelope {
	return model.Envelope{
		EventID: n.eventID, Destination: n.repo, Kind: kind,
		References: []model.ExternalRef{n.ref}, Audience: model.Audience{Visibility: model.Public},
		OccurredAt: at.UTC(), SourceRevision: revision(at), Assurance: "verified",
	}
}

func (n normalizer) message(kind, id string, author *gh.User, text string, created, updated time.Time) model.Envelope {
	env := n.base(kind, created)
	env.MessageID, env.Sender, env.SourceRevision = id, n.user(author), revision(firstSet(updated, created))
	env.SenderKind = senderKind(author)
	env.Content = textContent(text)
	return env
}

// update is an edit or deletion of the message id, by the event sender.
func (n normalizer) update(kind, id, text string, at time.Time) delivery {
	env := n.base(kind, at)
	env.MessageID, env.Sender, env.SenderKind = id, n.user(n.ev.Sender), senderKind(n.ev.Sender)
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

// state is the provider view of iss.
func (n normalizer) state(iss *gh.Issue) model.WorkItemState {
	s := model.WorkItemState{
		Subject: iss.GetTitle(), URL: iss.GetHTMLURL(), RawStatus: iss.GetState(),
		Binding: model.WorkItemBindingActive, LocalFields: slices.Clone(n.x.cfg.LocalFields),
	}
	switch iss.GetState() {
	case "open":
		s.Status = model.WorkItemOpen
	case "closed":
		s.Status = model.WorkItemClosed
	}
	if r := iss.GetStateReason(); r != "" && iss.GetState() != "" {
		s.RawStatus = iss.GetState() + ":" + r
	}
	if u := iss.GetUser(); u.GetID() != 0 {
		s.Requester = n.user(u)
	}
	for _, a := range iss.Assignees {
		if a.GetID() != 0 {
			s.Assignees = append(s.Assignees, n.user(a))
		}
	}
	if len(iss.Labels) > 0 {
		names := make([]string, 0, len(iss.Labels))
		for _, l := range iss.Labels {
			names = append(names, l.GetName())
		}
		slices.Sort(names)
		data, _ := json.Marshal(names)
		s.Fields = append(s.Fields, model.Extension{Name: FieldLabels, Version: 1, Data: data})
	}
	if m := iss.Milestone; m != nil {
		data, _ := json.Marshal(struct {
			Number int    `json:"number"`
			Title  string `json:"title"`
		}{m.GetNumber(), m.GetTitle()})
		s.Fields = append(s.Fields, model.Extension{Name: FieldMilestone, Version: 1, Data: data})
	}
	return s
}

func (n normalizer) user(u *gh.User) model.ExternalRef {
	if u.GetID() == 0 {
		return model.ExternalRef{}
	}
	return model.ExternalRef{Channel: n.x.channel.Ref, Namespace: Namespace, Kind: RefKindUser, ID: strconv.FormatInt(u.GetID(), 10)}
}

// kindFor marks messages authored by bot accounts automated.
func kindFor(u *gh.User) string {
	if u.GetType() == "Bot" {
		return model.EnvelopeAutomatedMessage
	}
	return model.EnvelopeMessage
}

// senderKind is the sender kind of an account: a bot for a GitHub App or
// other Bot account, a person (empty) for User and Organization accounts.
// The account type comes from the signed webhook payload.
func senderKind(u *gh.User) string {
	if u.GetType() == "Bot" {
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
