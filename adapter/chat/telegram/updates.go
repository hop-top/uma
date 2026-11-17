package telegram

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"hop.top/uma/model"
)

// Schemas of the structured data the adapter reports.
const (
	// PayloadSchema is the payload of every message envelope: where the
	// message was sent and what Telegram said about it. Claims, never
	// identity.
	PayloadSchema  = "telegram.messages.message"
	PayloadVersion = 1
	// MediaSchema is the part of a message that carries media: what each
	// file is (kind, IDs, name, type, size, dimensions, duration), never
	// the file. A file_id is not a secret but names a file only the bot
	// that received it can fetch (getFile).
	MediaSchema  = "telegram.messages.media"
	MediaVersion = 1
)

type malformed string

func (m malformed) Error() string { return "telegram: malformed update: " + string(m) }

// normalize maps one update to the envelopes it reports. An update of a
// kind the adapter does not consume maps to none.
func (x *Instance) normalize(u update) ([]delivery, error) {
	switch {
	case u.Message != nil:
		return x.newMessage(*u.Message, "message")
	case u.ChannelPost != nil:
		return x.newMessage(*u.ChannelPost, "channel_post")
	case u.EditedMessage != nil:
		return x.edited(u.UpdateID, *u.EditedMessage, "message")
	case u.EditedChannelPost != nil:
		return x.edited(u.UpdateID, *u.EditedChannelPost, "channel_post")
	case u.MessageReaction != nil:
		return x.reaction(u.UpdateID, *u.MessageReaction)
	case u.CallbackQuery != nil:
		return x.callback(u.UpdateID, *u.CallbackQuery)
	}
	return nil, nil
}

// notOurs reports a message the adapter never reports: one the bot
// authored (it would loop, and uma knows what it sent by its provider
// message IDs), a business or ephemeral message, whose message_id is not
// one of the chat's.
func (x *Instance) notOurs(m message) bool {
	return (m.From != nil && m.From.ID == x.cfg.BotID) || m.BusinessConnectionID != "" || m.EphemeralMessageID != 0 || m.MessageID == 0
}

// checkMessage validates the fields every message update carries.
func checkMessage(m message) error {
	switch {
	case m.MessageID < 0 || m.Chat.ID == 0 || m.Date <= 0:
		return malformed("message_id, chat or date")
	}
	switch m.Chat.Type {
	case "private", "group", "supergroup", "channel":
		return nil
	}
	return malformed("chat type")
}

// author is who a message is from: the chat it speaks for when it was sent
// on behalf of one (an anonymous administrator, a channel), else its user.
// "For backward compatibility, if the message was sent on behalf of a chat,
// the field from contains a fake sender user in non-channel chats": sender_chat
// is read first.
func (x *Instance) author(m message) (ref model.ExternalRef, bot, ok bool) {
	switch {
	case m.SenderChat != nil && m.SenderChat.ID != 0:
		return x.chatIdentityRef(m.SenderChat.ID), false, true
	case m.From != nil && m.From.ID > 0:
		return x.userRef(m.From.ID), m.From.IsBot, true
	}
	return model.ExternalRef{}, false, false
}

// newMessage reports a new message or channel post.
//
// Rules, in order: a message the bot authored is never reported; in a group,
// supergroup or channel, with MentionsOnly, a message that does not address
// the bot is dropped (a private chat is never); a message by another bot is
// an automated message from a bot sender. A message with no text, caption
// or media, such as a service message, has nothing to say and is dropped.
func (x *Instance) newMessage(m message, updateType string) ([]delivery, error) {
	if err := checkMessage(m); err != nil {
		return nil, err
	}
	if x.notOurs(m) {
		return nil, nil
	}
	if cmd, ok := x.command(m); ok {
		if !cmd.ours {
			return nil, nil
		}
		return x.commandDelivery(m, cmd)
	}
	addressed := x.addressed(m)
	if x.cfg.MentionsOnly && m.Chat.Type != "private" && !addressed {
		return nil, nil
	}
	env, ok := x.messageEnvelope(m, updateType, addressed)
	if !ok {
		return nil, nil
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

// messageEnvelope is the message envelope of m. It reports false for a
// message with no author or nothing to say.
func (x *Instance) messageEnvelope(m message, updateType string, addressed bool) (model.Envelope, bool) {
	sender, bot, ok := x.author(m)
	content, said := messageContent(m)
	if !ok || !said {
		return model.Envelope{}, false
	}
	id := messageID(m.Chat.ID, m.MessageID)
	env := model.Envelope{
		EventID: "message:" + id, MessageID: id, Destination: idString(x.cfg.BotID), Kind: model.EnvelopeMessage,
		Sender: sender, Assurance: "verified", References: x.references(m.Chat.ID, m.MessageThreadID), Content: content,
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: time.Unix(m.Date, 0).UTC(),
	}
	if bot {
		env.Kind, env.SenderKind = model.EnvelopeAutomatedMessage, model.ContactKindBot
	}
	if r := m.ReplyToMessage; r != nil && r.MessageID > 0 && (m.MessageThreadID <= 0 || r.MessageID != m.MessageThreadID) {
		// In a forum topic Telegram points the reply of a plain post at the
		// topic's root message: that is thread membership, not a reply.
		env.InReplyTo = messageID(m.Chat.ID, r.MessageID)
	}
	env.Payload = x.payload(m, updateType, addressed)
	return env, true
}

// references are the scoping references of a message: its chat, and its
// topic when Telegram names one (message_thread_id: a forum topic of a
// supergroup or of a private chat with topics). A message outside any topic
// has no thread: Telegram has none to give it, and replies are linked to
// one message only, never to a root.
func (x *Instance) references(chat, thread int64) []model.ExternalRef {
	refs := []model.ExternalRef{x.roomRef(chat)}
	if thread > 0 {
		refs = append(refs, x.threadRef(chat, thread))
	}
	return refs
}

func (x *Instance) payload(m message, updateType string, addressed bool) model.Extension {
	data := map[string]any{
		"bot_id": x.cfg.BotID, "chat": m.Chat.ID, "chat_type": m.Chat.Type, "message_id": m.MessageID,
		"update_type": updateType, "addressed": addressed,
	}
	if m.MessageThreadID > 0 {
		data["thread"] = m.MessageThreadID
	}
	if m.ReplyToMessage != nil && m.ReplyToMessage.MessageID > 0 {
		data["reply_to"] = m.ReplyToMessage.MessageID
	}
	if m.MediaGroupID != "" {
		data["media_group"] = m.MediaGroupID
	}
	if m.ViaBot != nil {
		data["via_bot"] = m.ViaBot.ID
	}
	b, _ := json.Marshal(data)
	return model.Extension{Name: PayloadSchema, Version: PayloadVersion, Data: b}
}

// addressed reports whether a message is meant for the bot: it mentions it
// (@username, or a text mention naming its user), or replies to one of its
// messages.
func (x *Instance) addressed(m message) bool {
	if m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && m.ReplyToMessage.From.ID == x.cfg.BotID {
		return true
	}
	return x.mentions(m.Text, m.Entities) || x.mentions(m.Caption, m.CaptionEntities)
}

func (x *Instance) mentions(text string, entities []entity) bool {
	for _, e := range entities {
		switch e.Type {
		case "mention":
			if strings.EqualFold(entityText(text, e), "@"+x.cfg.BotUsername) {
				return true
			}
		case "text_mention":
			if e.User != nil && e.User.ID == x.cfg.BotID {
				return true
			}
		}
	}
	return false
}

// entityText is the text an entity covers. Telegram counts entity offsets
// and lengths "in UTF-16 code units"; a range outside the text is empty.
func entityText(text string, e entity) string {
	u := utf16.Encode([]rune(text))
	if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(u) {
		return ""
	}
	return string(utf16.Decode(u[e.Offset : e.Offset+e.Length]))
}

// messageContent is a message's content: its text, or the caption of its
// media, as Telegram sends it (entities are not applied), then a
// description of the media. Nothing is fetched. It reports false for a
// message that says nothing.
func messageContent(m message) (model.Content, bool) {
	var parts []model.Part
	text := m.Text
	if strings.TrimSpace(text) == "" {
		text = m.Caption
	}
	if strings.TrimSpace(text) != "" {
		parts = append(parts, model.Part{Kind: model.PartText, Text: text})
	}
	if media := describeMedia(m); len(media) > 0 {
		data, _ := json.Marshal(map[string]any{"media": media})
		parts = append(parts, model.Part{Kind: model.PartStructured, Structured: &model.Extension{Name: MediaSchema, Version: MediaVersion, Data: data}})
	}
	return model.Content{Parts: parts}, len(parts) > 0
}

type described struct {
	Kind         string `json:"kind"`
	FileID       string `json:"file_id,omitempty"`
	FileUniqueID string `json:"file_unique_id,omitempty"`
	Name         string `json:"name,omitempty"`
	MimeType     string `json:"mime_type,omitempty"`
	Size         int64  `json:"size,omitempty"`
	Width        int    `json:"width,omitempty"`
	Height       int    `json:"height,omitempty"`
	Duration     int    `json:"duration,omitempty"`
}

// describeMedia lists what a message carries besides text: a photo (its
// largest size), a file of one of the file kinds, and the kinds with no
// file (a contact, location, venue, poll or dice), named by kind alone.
func describeMedia(m message) []described {
	var out []described
	if n := len(m.Photo); n > 0 {
		best := m.Photo[0]
		for _, p := range m.Photo[1:] {
			if p.Width*p.Height > best.Width*best.Height {
				best = p
			}
		}
		out = append(out, described{Kind: "photo", FileID: best.FileID, FileUniqueID: best.FileUniqueID, Size: best.FileSize, Width: best.Width, Height: best.Height})
	}
	for _, f := range []struct {
		kind string
		file *fileInfo
	}{
		{"document", m.Document}, {"animation", m.Animation}, {"audio", m.Audio}, {"video", m.Video},
		{"video_note", m.VideoNote}, {"voice", m.Voice}, {"sticker", m.Sticker},
	} {
		if f.file != nil {
			out = append(out, described{
				Kind: f.kind, FileID: f.file.FileID, FileUniqueID: f.file.FileUniqueID, Name: f.file.FileName,
				MimeType: f.file.MimeType, Size: f.file.FileSize, Width: f.file.Width, Height: f.file.Height, Duration: f.file.Duration,
			})
		}
	}
	for _, f := range []struct {
		kind string
		raw  json.RawMessage
	}{{"contact", m.Contact}, {"location", m.Location}, {"venue", m.Venue}, {"poll", m.Poll}, {"dice", m.Dice}} {
		if len(f.raw) > 0 && string(f.raw) != "null" {
			out = append(out, described{Kind: f.kind})
		}
	}
	return out
}

// edited reports an edited_message or edited_channel_post as an edit of the
// message. The editor is the author: Telegram does not name another.
// "This update may at times be triggered by changes to message fields that
// are either unavailable or not actively used by your bot", so an edit
// whose content is unchanged reaches core as one and is no change there.
// The revision is edit_date, in whole seconds: two edits within one second
// cannot be told apart, and the later may be taken for the earlier.
func (x *Instance) edited(updateID int64, m message, updateType string) ([]delivery, error) {
	if err := checkMessage(m); err != nil {
		return nil, err
	}
	if x.notOurs(m) || m.EditDate <= 0 {
		return nil, nil
	}
	// An edited command is neither a message uma holds nor a second command.
	if _, isCommand := x.command(m); isCommand {
		return nil, nil
	}
	first, ok := x.messageEnvelope(m, updateType, x.addressed(m))
	if !ok {
		return nil, nil
	}
	env := first
	env.EventID, env.Kind = "update:"+idString(updateID), model.EnvelopeMessageEdited
	env.OccurredAt, env.SourceRevision = time.Unix(m.EditDate, 0).UTC(), strconv.FormatInt(m.EditDate, 10)
	env.References, env.Payload, env.InReplyTo = nil, model.Extension{}, ""
	return []delivery{{ingress: x.ingress(env), fallback: ptr(x.ingress(first))}}, nil
}

// reaction reports the reactions a user changed on a message: one
// envelope for each emoji the user's reaction list lost and each it gained.
// Updates about reactions need the bot to be an administrator of the chat
// and "message_reaction" in allowed_updates, and are never sent for
// reactions bots set. A custom emoji or paid reaction has no emoji to
// report and is dropped.
func (x *Instance) reaction(updateID int64, r reactionUpdate) ([]delivery, error) {
	if r.Chat.ID == 0 || r.MessageID <= 0 {
		return nil, malformed("reaction chat or message")
	}
	var sender model.ExternalRef
	switch {
	case r.User != nil && r.User.ID > 0:
		if r.User.ID == x.cfg.BotID {
			return nil, nil
		}
		sender = x.userRef(r.User.ID)
	case r.ActorChat != nil && r.ActorChat.ID != 0:
		sender = x.chatIdentityRef(r.ActorChat.ID)
	default:
		return nil, malformed("reaction user")
	}
	at := time.Unix(r.Date, 0).UTC()
	var out []delivery
	// The event ID names the update and the change's place in it: one update
	// reports several, and an emoji is no safe part of an ID.
	emit := func(kind, emoji string) {
		env := model.Envelope{
			EventID: "update:" + idString(updateID) + ":" + strconv.Itoa(len(out)), MessageID: messageID(r.Chat.ID, r.MessageID),
			Destination: idString(x.cfg.BotID), Kind: kind, Sender: sender, Assurance: "verified",
			Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: emoji}}},
			Audience: model.Audience{Visibility: model.Public}, OccurredAt: at,
		}
		out = append(out, delivery{ingress: x.ingress(env)})
	}
	oldSet, newSet := emojiSet(r.OldReaction), emojiSet(r.NewReaction)
	for _, e := range r.OldReaction {
		if emoji := e.Emoji; e.Type == "emoji" && emoji != "" && !newSet[emoji] {
			emit(model.EnvelopeReactionRemoved, emoji)
		}
	}
	for _, e := range r.NewReaction {
		if emoji := e.Emoji; e.Type == "emoji" && emoji != "" && !oldSet[emoji] {
			emit(model.EnvelopeReactionAdded, emoji)
		}
	}
	return out, nil
}

func emojiSet(rs []reactionType) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		if r.Type == "emoji" && r.Emoji != "" {
			out[r.Emoji] = true
		}
	}
	return out
}

func (x *Instance) ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	if c := model.CorrelationID("telegram-" + env.EventID); c.Validate() == nil {
		in.Correlation = c
	}
	return in
}

func ptr[T any](v T) *T { return &v }
