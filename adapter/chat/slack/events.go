package slack

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Schemas of the structured data the adapter reports.
const (
	// PayloadSchema is the payload of every message envelope: where the
	// message was posted and what Slack said about it. Claims, never
	// identity.
	PayloadSchema  = "slack.chat.message"
	PayloadVersion = 1
	// FilesSchema is the only part of a message that has files and no
	// text: their IDs, names, types and sizes. The files themselves are
	// not fetched; their private URLs, which need the bot token, are not
	// reported.
	FilesSchema  = "slack.chat.files"
	FilesVersion = 1
)

// eventCallback is the Events API envelope
// (https://docs.slack.dev/apis/events-api/).
type eventCallback struct {
	Type      string          `json:"type"`
	TeamID    string          `json:"team_id"`
	APIAppID  string          `json:"api_app_id"`
	EventID   string          `json:"event_id"`
	Challenge string          `json:"challenge"`
	Event     json.RawMessage `json:"event"`
}

// slackEvent is the inner event: the message, app_mention and reaction
// shapes of https://docs.slack.dev/reference/events/.
type slackEvent struct {
	Type        string `json:"type"`
	Subtype     string `json:"subtype"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Text        string `json:"text"`
	TS          string `json:"ts"`
	ThreadTS    string `json:"thread_ts"`
	EventTS     string `json:"event_ts"`
	DeletedTS   string `json:"deleted_ts"`
	Files       []file `json:"files"`
	Edited      *struct {
		User string `json:"user"`
		TS   string `json:"ts"`
	} `json:"edited"`
	// Message is the new state of a message_changed event's message.
	Message *slackEvent `json:"message"`
	// Fields of messages read through the Web API.
	Blocks     json.RawMessage `json:"blocks"`
	ReplyCount int             `json:"reply_count"`
	Metadata   *struct {
		EventType    string         `json:"event_type"`
		EventPayload map[string]any `json:"event_payload"`
	} `json:"metadata"`
	Reactions []struct {
		Name  string   `json:"name"`
		Users []string `json:"users"`
	} `json:"reactions"`
	// Reaction events.
	Reaction string `json:"reaction"`
	Item     *struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
}

type file struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int64  `json:"size"`
}

// event answers an Events API delivery: the JSON body of a request, or the
// payload of a Socket Mode envelope, which is "identical to the typical
// Events API payloads"
// (https://docs.slack.dev/apis/events-api/using-socket-mode/).
func (x *Instance) event(ctx context.Context, sink adapter.Sink, body []byte) verdict {
	var cb eventCallback
	if err := json.Unmarshal(body, &cb); err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	switch cb.Type {
	case "url_verification":
		// Only a request URL is verified so; the handler checked the
		// signature, and https://docs.slack.dev/reference/events/url_verification/
		// accepts the challenge as the plain text body.
		if cb.Challenge == "" {
			return statusAnswer(http.StatusBadRequest)
		}
		return verdict{status: http.StatusOK, contentType: "text/plain; charset=utf-8", body: []byte(cb.Challenge)}
	case "event_callback":
	default:
		// app_rate_limited and whatever Slack adds: nothing to report.
		return statusAnswer(http.StatusOK)
	}
	if len(cb.Event) == 0 || cb.EventID == "" {
		return statusAnswer(http.StatusBadRequest)
	}
	var kind struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(cb.Event, &kind); err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	// An event of another team or app, or of a type the adapter does not
	// consume (whose fields it does not read), is acknowledged: not an
	// error, not this channel's.
	if !consumed(kind.Type) || cb.TeamID != x.cfg.TeamID || (x.cfg.AppID != "" && cb.APIAppID != x.cfg.AppID) {
		return statusAnswer(http.StatusOK)
	}
	var ev slackEvent
	if err := json.Unmarshal(cb.Event, &ev); err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	batch, err := x.normalize(cb.EventID, ev)
	if err != nil {
		return statusAnswer(http.StatusBadRequest)
	}
	return x.deliver(ctx, sink, batch, statusAnswer(http.StatusOK))
}

func consumed(eventType string) bool {
	switch eventType {
	case "message", "app_mention", "reaction_added", "reaction_removed",
		"channel_archive", "channel_unarchive", "group_archive", "group_unarchive":
		return true
	}
	return false
}

type malformed string

func (m malformed) Error() string { return "slack: malformed event: " + string(m) }

// normalize maps one event to the envelopes it reports. Events the adapter
// does not consume map to none.
func (x *Instance) normalize(eventID string, ev slackEvent) ([]delivery, error) {
	switch ev.Type {
	case "message":
		switch ev.Subtype {
		case "message_changed":
			return x.edited(eventID, ev)
		case "message_deleted":
			return x.deleted(eventID, ev)
		case "", "thread_broadcast", "file_share", "me_message", "bot_message":
			return x.newMessage(ev)
		}
		return nil, nil
	case "app_mention":
		return x.newMessage(ev)
	case "reaction_added", "reaction_removed":
		return x.reaction(eventID, ev)
	case "channel_archive", "channel_unarchive", "group_archive", "group_unarchive":
		return x.archiveState(eventID, ev)
	}
	return nil, nil
}

// own reports whether the event's author is the app's bot.
func (x *Instance) own(user, botID string) bool {
	return user == x.cfg.BotUserID || (x.cfg.BotID != "" && botID == x.cfg.BotID)
}

// mentions reports whether text mentions the bot user: "<@U0123>", or the
// older "<@U0123|name>" form
// (https://docs.slack.dev/messaging/formatting-message-text/).
func (x *Instance) mentions(text string) bool {
	return strings.Contains(text, "<@"+x.cfg.BotUserID+">") || strings.Contains(text, "<@"+x.cfg.BotUserID+"|")
}

func direct(conversation, channelType string) bool {
	return channelType == "im" || (channelType == "" && strings.HasPrefix(conversation, "D"))
}

// newMessage reports a new message: a message or app_mention event.
//
// Rules, in order: a message the app's own bot authored is never reported
// (it would loop, and uma knows what it sent by its provider IDs); a
// message and the app_mention of the same message are one event (the event
// ID derives from the message, not from Slack's two event IDs, so core
// sees one); with MentionsOnly a channel message that does not mention the
// bot is dropped, a direct message never is; a bot other than this app's
// is an automated message from a bot sender.
func (x *Instance) newMessage(ev slackEvent) ([]delivery, error) {
	if !slackID.MatchString(ev.Channel) || !tsPattern.MatchString(ev.TS) {
		return nil, malformed("message channel or ts")
	}
	if x.own(ev.User, ev.BotID) {
		return nil, nil
	}
	mentioned := ev.Type == "app_mention" || x.mentions(ev.Text)
	if x.cfg.MentionsOnly && !mentioned && !direct(ev.Channel, ev.ChannelType) {
		return nil, nil
	}
	env, ok := x.messageEnvelope(ev, ev.Type, mentioned)
	if !ok {
		return nil, nil
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

// messageEnvelope is the message envelope of m, posted in conversation
// m.Channel, reported by an event of type eventType. It reports false for
// a message with no author or nothing to say.
func (x *Instance) messageEnvelope(m slackEvent, eventType string, mentioned bool) (model.Envelope, bool) {
	author, kind := m.User, ""
	if m.BotID != "" || m.Subtype == "bot_message" {
		kind = model.ContactKindBot
	}
	if author == "" {
		author = m.BotID
	}
	content, ok := messageContent(m.Text, m.Files)
	if !ok || !slackID.MatchString(author) {
		return model.Envelope{}, false
	}
	id := messageID(m.Channel, m.TS)
	env := model.Envelope{
		EventID: "message:" + id, MessageID: id, Destination: x.cfg.TeamID, Kind: model.EnvelopeMessage,
		Sender: x.userRef(author), SenderKind: kind, Assurance: "verified",
		References: x.references(m.Channel, m.ThreadTS, m.TS), Content: content,
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: slackTime(m.TS),
	}
	if kind == model.ContactKindBot {
		env.Kind = model.EnvelopeAutomatedMessage
	}
	if tsPattern.MatchString(m.ThreadTS) && m.ThreadTS != m.TS {
		// Slack reports a thread reply's parent as the thread's root only.
		env.InReplyTo = messageID(m.Channel, m.ThreadTS)
	}
	env.Payload = x.payload(m, eventType, mentioned)
	return env, true
}

// references are the scoping references of a message: its room and its
// thread, the one it replies in or, for a message outside a thread, the one
// it roots: a reply to it carries its ts as thread_ts, so a per-thread inbox
// keys a conversation on the root message from its first word. With no
// message (a slash command) there is no thread.
func (x *Instance) references(conversation, threadTS, ts string) []model.ExternalRef {
	refs := []model.ExternalRef{x.roomRef(conversation)}
	root := threadTS
	if !tsPattern.MatchString(root) {
		root = ts
	}
	if tsPattern.MatchString(root) {
		refs = append(refs, x.threadRef(conversation, root))
	}
	return refs
}

func (x *Instance) payload(m slackEvent, eventType string, mentioned bool) model.Extension {
	data := map[string]any{
		"team_id": x.cfg.TeamID, "conversation": m.Channel, "ts": m.TS, "event_type": eventType, "mentioned": mentioned,
	}
	if m.ChannelType != "" {
		data["channel_type"] = m.ChannelType
	}
	if tsPattern.MatchString(m.ThreadTS) && m.ThreadTS != m.TS {
		data["thread_ts"] = m.ThreadTS
	}
	if m.Subtype != "" {
		data["subtype"] = m.Subtype
	}
	if m.BotID != "" {
		data["bot_id"] = m.BotID
	}
	b, _ := json.Marshal(data)
	return model.Extension{Name: PayloadSchema, Version: PayloadVersion, Data: b}
}

// messageContent is a message's content: its text, in Slack's own encoding
// (control characters as entities, mentions and links as <...>), or, for a
// message with files and no text, their description.
func messageContent(text string, files []file) (model.Content, bool) {
	if strings.TrimSpace(text) != "" {
		return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}, true
	}
	if len(files) == 0 {
		return model.Content{}, false
	}
	type described struct {
		ID       string `json:"id"`
		Name     string `json:"name,omitempty"`
		Mimetype string `json:"mimetype,omitempty"`
		Size     int64  `json:"size,omitempty"`
	}
	out := make([]described, 0, len(files))
	for _, f := range files {
		out = append(out, described(f))
	}
	data, _ := json.Marshal(map[string]any{"files": out})
	return model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: FilesSchema, Version: FilesVersion, Data: data}}}}, true
}

// edited reports a message_changed event as an edit of the message. Slack
// sends message_changed for more than edits (link unfurls, thread
// bookkeeping); only a message carrying "edited" was edited. An edit of
// the bot's own message is not reported, as the message itself is not.
func (x *Instance) edited(eventID string, ev slackEvent) ([]delivery, error) {
	m := ev.Message
	if m == nil || !slackID.MatchString(ev.Channel) || !tsPattern.MatchString(m.TS) {
		return nil, malformed("message_changed")
	}
	if m.Edited == nil || !tsPattern.MatchString(m.Edited.TS) || x.own(m.User, m.BotID) {
		return nil, nil
	}
	m.Channel, m.ChannelType = ev.Channel, ev.ChannelType
	first, ok := x.messageEnvelope(*m, "message", x.mentions(m.Text))
	if !ok {
		return nil, nil
	}
	editor := m.Edited.User
	if !slackID.MatchString(editor) {
		editor = m.User
	}
	env := first
	env.EventID, env.Kind = eventID, model.EnvelopeMessageEdited
	env.InReplyTo = "" // only a new message carries a reply parent
	env.Sender, env.OccurredAt, env.SourceRevision = x.userRef(editor), slackTime(m.Edited.TS), m.Edited.TS
	if editor != first.Sender.ID {
		// Someone else edited the message: a person, as far as Slack says.
		env.SenderKind = ""
	}
	env.References = nil
	env.Payload = model.Extension{}
	return []delivery{{ingress: x.ingress(env), fallback: ptr(x.ingress(first))}}, nil
}

// deleted reports a message_deleted event. Slack names no deleter, so the
// deletion is a system actor's.
func (x *Instance) deleted(eventID string, ev slackEvent) ([]delivery, error) {
	if !slackID.MatchString(ev.Channel) || !tsPattern.MatchString(ev.DeletedTS) {
		return nil, malformed("message_deleted")
	}
	at := slackTime(ev.TS)
	env := model.Envelope{
		EventID: eventID, MessageID: messageID(ev.Channel, ev.DeletedTS), Destination: x.cfg.TeamID,
		Kind: model.EnvelopeMessageDeleted, SystemActor: true, Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: at,
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

// reaction reports a reaction on a message by a user other than the bot.
// A reaction by a name the adapter has no emoji for is dropped; see
// reactionNames.
func (x *Instance) reaction(eventID string, ev slackEvent) ([]delivery, error) {
	it := ev.Item
	if it == nil || it.Type != "message" {
		return nil, nil
	}
	if !slackID.MatchString(it.Channel) || !tsPattern.MatchString(it.TS) || !slackID.MatchString(ev.User) {
		return nil, malformed("reaction item or user")
	}
	emoji, ok := reactionEmoji(ev.Reaction)
	if !ok || ev.User == x.cfg.BotUserID {
		return nil, nil
	}
	kind := model.EnvelopeReactionAdded
	if ev.Type == "reaction_removed" {
		kind = model.EnvelopeReactionRemoved
	}
	env := model.Envelope{
		EventID: eventID, MessageID: messageID(it.Channel, it.TS), Destination: x.cfg.TeamID, Kind: kind,
		Sender: x.userRef(ev.User), Assurance: "verified",
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: emoji}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: slackTime(ev.EventTS),
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

func (x *Instance) ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	if c := model.CorrelationID("slack-" + env.EventID); c.Validate() == nil {
		in.Correlation = c
	}
	return in
}

func ptr[T any](v T) *T { return &v }
