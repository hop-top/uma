package discord

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Schemas of the structured data the adapter reports.
const (
	// PayloadSchema is the payload of every message envelope: where the
	// message was posted and what Discord said about it. Claims, never
	// identity.
	PayloadSchema  = "discord.chat.message"
	PayloadVersion = 1
	// AttachmentsSchema is the only part of a message that has attachments
	// and no text: their IDs, names, types and sizes. The files are not
	// fetched.
	AttachmentsSchema  = "discord.chat.attachments"
	AttachmentsVersion = 1
)

// Message types reported ("Message Types",
// https://docs.discord.com/developers/resources/message): DEFAULT and
// REPLY. Everything else is a system message or an application command's
// own message.
const (
	messageDefault = 0
	messageReply   = 19
)

type user struct {
	ID       string `json:"id"`
	Bot      bool   `json:"bot"`
	System   bool   `json:"system"`
	Username string `json:"username"`
}

type attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
}

// message is a message object
// (https://docs.discord.com/developers/resources/message), with the
// guild_id of the gateway's MESSAGE_CREATE and MESSAGE_UPDATE.
type message struct {
	ID               string          `json:"id"`
	ChannelID        string          `json:"channel_id"`
	GuildID          string          `json:"guild_id"`
	Author           *user           `json:"author"`
	Content          string          `json:"content"`
	Timestamp        string          `json:"timestamp"`
	EditedTimestamp  *string         `json:"edited_timestamp"`
	Mentions         []user          `json:"mentions"`
	Attachments      []attachment    `json:"attachments"`
	Embeds           json.RawMessage `json:"embeds"`
	Components       json.RawMessage `json:"components"`
	Nonce            json.RawMessage `json:"nonce"`
	WebhookID        string          `json:"webhook_id"`
	ApplicationID    string          `json:"application_id"`
	Type             int             `json:"type"`
	Flags            int             `json:"flags"`
	MessageReference *struct {
		MessageID string `json:"message_id"`
		ChannelID string `json:"channel_id"`
	} `json:"message_reference"`
	Reactions []struct {
		Count int  `json:"count"`
		Me    bool `json:"me"`
		Emoji struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"emoji"`
	} `json:"reactions"`
}

// nonce is the message's nonce as a string, "" for none.
func (m message) nonce() string {
	var s string
	if json.Unmarshal(m.Nonce, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(m.Nonce, &n) == nil {
		return n.String()
	}
	return ""
}

func (m message) componentsRaw() json.RawMessage { return m.Components }

// emptyJSON reports whether raw is absent, null or an empty array.
func emptyJSON(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null" || s == "[]"
}

// own reports whether the application authored m: its bot user, or the
// webhook of its interaction responses.
func (x *Instance) own(m message) bool {
	return (m.Author != nil && m.Author.ID == x.cfg.BotUserID) || (m.WebhookID != "" && m.WebhookID == x.cfg.ApplicationID)
}

// mentions reports whether m mentions the bot: in its mentions, or as
// "<@ID>" or the deprecated "<@!ID>" in its text
// (https://docs.discord.com/developers/reference).
func (x *Instance) mentions(m message) bool {
	for _, u := range m.Mentions {
		if u.ID == x.cfg.BotUserID {
			return true
		}
	}
	return strings.Contains(m.Content, "<@"+x.cfg.BotUserID+">") || strings.Contains(m.Content, "<@!"+x.cfg.BotUserID+">")
}

// delivery is one envelope to record, with the envelope to record instead
// when the first targets a message the sink does not know.
type delivery struct {
	ingress  model.Ingress
	fallback *model.Ingress
}

// messageEnvelope is the envelope of a message in channel (parent: the
// thread's parent channel, "" outside a thread). It reports false for a
// message that is not a user's message or has nothing to say, such as a
// guild message whose content Discord withheld for want of the
// MESSAGE_CONTENT intent.
func (x *Instance) messageEnvelope(m message, parent string, mentioned bool) (model.Envelope, bool) {
	if m.Author == nil || !isSnowflake(m.Author.ID) || !isSnowflake(m.ID) || !isSnowflake(m.ChannelID) {
		return model.Envelope{}, false
	}
	if m.Type != messageDefault && m.Type != messageReply {
		return model.Envelope{}, false
	}
	content, ok := messageContent(m.Content, m.Attachments)
	if !ok {
		return model.Envelope{}, false
	}
	id := messageID(m.ChannelID, m.ID)
	env := model.Envelope{
		EventID: "message:" + id, MessageID: id, Destination: x.cfg.ApplicationID, Kind: model.EnvelopeMessage,
		Sender: x.userRef(m.Author.ID), Assurance: "verified", References: x.references(m.ChannelID, parent),
		Content: content, Audience: model.Audience{Visibility: model.Public}, OccurredAt: messageTime(m),
	}
	if m.Author.Bot || m.Author.System || m.WebhookID != "" {
		env.Kind, env.SenderKind = model.EnvelopeAutomatedMessage, model.ContactKindBot
	}
	if r := m.MessageReference; m.Type == messageReply && r != nil && isSnowflake(r.MessageID) {
		// Only a reply (type 19) answers its referenced message; a
		// forward or crosspost carries a reference without replying.
		channel := m.ChannelID
		if isSnowflake(r.ChannelID) {
			channel = r.ChannelID
		}
		env.InReplyTo = messageID(channel, r.MessageID)
	}
	env.Payload = x.payload(m, parent, mentioned)
	return env, true
}

// messageTime is when m was posted: its timestamp, else its ID's.
func messageTime(m message) time.Time {
	if t, err := time.Parse(time.RFC3339Nano, m.Timestamp); err == nil {
		return t.UTC()
	}
	return snowflakeTime(m.ID)
}

func (x *Instance) payload(m message, parent string, mentioned bool) model.Extension {
	data := map[string]any{
		"application_id": x.cfg.ApplicationID, "channel_id": m.ChannelID, "message_id": m.ID, "mentioned": mentioned, "type": m.Type,
	}
	if m.GuildID != "" {
		data["guild_id"] = m.GuildID
	}
	if parent != "" {
		data["parent_id"] = parent
	}
	if m.WebhookID != "" {
		data["webhook_id"] = m.WebhookID
	}
	if m.ApplicationID != "" {
		data["message_application_id"] = m.ApplicationID
	}
	if r := m.MessageReference; r != nil && isSnowflake(r.MessageID) {
		data["reply_to"] = r.MessageID
	}
	b, _ := json.Marshal(data)
	return model.Extension{Name: PayloadSchema, Version: PayloadVersion, Data: b}
}

// messageContent is a message's content: its text, as Discord sent it
// (markdown and mentions as written), or, for a message with attachments
// and no text, their description.
func messageContent(text string, files []attachment) (model.Content, bool) {
	if strings.TrimSpace(text) != "" {
		return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}, true
	}
	if len(files) == 0 {
		return model.Content{}, false
	}
	type described struct {
		ID          string `json:"id"`
		Filename    string `json:"filename,omitempty"`
		ContentType string `json:"content_type,omitempty"`
		Size        int64  `json:"size,omitempty"`
	}
	out := make([]described, 0, len(files))
	for _, f := range files {
		out = append(out, described(f))
	}
	data, _ := json.Marshal(map[string]any{"attachments": out})
	return model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: AttachmentsSchema, Version: AttachmentsVersion, Data: data}}}}, true
}

// direct reports whether m was posted outside a guild: a direct message.
// The gateway's MESSAGE_CREATE carries guild_id for a guild message, and
// omits it for an ephemeral one, which never reaches a bot's gateway as a
// message of someone else.
func direct(m message) bool { return m.GuildID == "" }

// created reports a new message (MESSAGE_CREATE). Rules, in order: what the
// application authored is never reported (it would loop, and uma knows what
// it sent by its provider IDs); with MentionsOnly a guild message that does
// not mention the bot is dropped, a direct message never is; a bot, a
// system user or a webhook is an automated message from a bot sender.
func (x *Instance) created(ctx context.Context, m message) ([]delivery, error) {
	if x.own(m) {
		return nil, nil
	}
	mentioned := x.mentions(m)
	if x.cfg.mentionsOnly() && !mentioned && !direct(m) {
		return nil, nil
	}
	parent, err := x.parentOf(ctx, m)
	if err != nil {
		return nil, err
	}
	env, ok := x.messageEnvelope(m, parent, mentioned)
	if !ok {
		return nil, nil
	}
	return []delivery{{ingress: ingress(env)}}, nil
}

// parentOf is the thread parent of m's channel; a direct message is never
// in a thread.
func (x *Instance) parentOf(ctx context.Context, m message) (string, error) {
	if direct(m) || !isSnowflake(m.ChannelID) {
		return "", nil
	}
	return x.threadParent(ctx, m.ChannelID)
}

// updated reports an edit (MESSAGE_UPDATE carrying edited_timestamp). An
// update without one is Discord's own bookkeeping, such as an embed
// unfurled, and is dropped. Only a message's author edits its content, so
// the editor is the author. An edit of a message uma does not hold records
// the message instead.
func (x *Instance) updated(ctx context.Context, m message) ([]delivery, error) {
	if m.EditedTimestamp == nil || *m.EditedTimestamp == "" || x.own(m) {
		return nil, nil
	}
	edited, err := time.Parse(time.RFC3339Nano, *m.EditedTimestamp)
	if err != nil {
		return nil, nil
	}
	mentioned := x.mentions(m)
	if x.cfg.mentionsOnly() && !mentioned && !direct(m) {
		return nil, nil
	}
	parent, err := x.parentOf(ctx, m)
	if err != nil {
		return nil, err
	}
	first, ok := x.messageEnvelope(m, parent, mentioned)
	if !ok {
		return nil, nil
	}
	env := first
	env.EventID = "edit:" + first.MessageID + ":" + strings.TrimSpace(*m.EditedTimestamp)
	env.Kind, env.OccurredAt, env.SourceRevision = model.EnvelopeMessageEdited, edited.UTC(), *m.EditedTimestamp
	env.References, env.Payload, env.InReplyTo = nil, model.Extension{}, ""
	return []delivery{{ingress: ingress(env), fallback: ptr(ingress(first))}}, nil
}

// deleted reports MESSAGE_DELETE. Discord names no deleter, so the deletion
// is a system actor's.
func (x *Instance) deleted(d struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
}) []delivery {
	if !isSnowflake(d.ID) || !isSnowflake(d.ChannelID) {
		return nil
	}
	id := messageID(d.ChannelID, d.ID)
	env := model.Envelope{
		EventID: "delete:" + id, MessageID: id, Destination: x.cfg.ApplicationID, Kind: model.EnvelopeMessageDeleted,
		SystemActor: true, Assurance: "verified", Audience: model.Audience{Visibility: model.Public}, OccurredAt: x.now().UTC(),
	}
	return []delivery{{ingress: ingress(env)}}
}

// reactionEvent is MESSAGE_REACTION_ADD and MESSAGE_REACTION_REMOVE.
type reactionEvent struct {
	UserID    string `json:"user_id"`
	ChannelID string `json:"channel_id"`
	MessageID string `json:"message_id"`
	GuildID   string `json:"guild_id"`
	Emoji     struct {
		ID   *string `json:"id"`
		Name string  `json:"name"`
	} `json:"emoji"`
}

// reaction reports a reaction by a user other than the bot. A custom emoji
// (one with an ID) has no Unicode form and is dropped. eventID names the
// dispatch: a reactor may add, remove and add the same emoji again, and
// each is an event of its own.
func (x *Instance) reaction(eventID string, r reactionEvent, removed bool) []delivery {
	if !isSnowflake(r.UserID) || !isSnowflake(r.ChannelID) || !isSnowflake(r.MessageID) || r.UserID == x.cfg.BotUserID {
		return nil
	}
	if (r.Emoji.ID != nil && *r.Emoji.ID != "") || model.ValidateEmoji(r.Emoji.Name) != nil {
		return nil
	}
	kind := model.EnvelopeReactionAdded
	if removed {
		kind = model.EnvelopeReactionRemoved
	}
	env := model.Envelope{
		EventID: eventID, MessageID: messageID(r.ChannelID, r.MessageID), Destination: x.cfg.ApplicationID, Kind: kind,
		Sender: x.userRef(r.UserID), Assurance: "verified",
		Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: r.Emoji.Name}}},
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: x.now().UTC(),
	}
	return []delivery{{ingress: ingress(env)}}
}

func ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	if c := model.CorrelationID("discord-" + env.EventID); c.Validate() == nil {
		in.Correlation = c
	}
	return in
}

// deliver hands each envelope to sink in order and returns the first
// refusal. An edit of a message the sink does not know records the message
// instead (its first observation). A deletion or a reaction of a message
// the sink does not hold, and a state report of a conversation it does not
// hold, is dropped: there is nothing to apply it to.
func deliver(ctx context.Context, sink adapter.Sink, batch []delivery, cursor string) error {
	for _, d := range batch {
		in := d.ingress
		in.ReceiverCursor = cursor
		_, err := sink.Accept(ctx, in)
		if model.IsCode(err, model.NotFound) && d.fallback != nil {
			fb := *d.fallback
			fb.ReceiverCursor = cursor
			_, err = sink.Accept(ctx, fb)
		}
		if model.IsCode(err, model.NotFound) && dropsWhenUnknown(d.ingress.Envelope.Kind) {
			err = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func dropsWhenUnknown(kind string) bool {
	switch kind {
	case model.EnvelopeMessageDeleted, model.EnvelopeReactionAdded, model.EnvelopeReactionRemoved, model.EnvelopeConversationState:
		return true
	}
	return false
}

func ptr[T any](v T) *T { return &v }
