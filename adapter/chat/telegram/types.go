package telegram

import "encoding/json"

// The Bot API types the adapter reads, with the fields it uses: Update,
// Message, User, Chat, MessageEntity, CallbackQuery and
// MessageReactionUpdated of https://core.telegram.org/bots/api. Every
// "Integer" is an int64: "it has at most 52 significant bits, so a 64-bit
// integer or double-precision float type are safe for storing this
// identifier".

// update is one Update. "At most one of the optional fields can be present
// in any given update."
type update struct {
	UpdateID          int64           `json:"update_id"`
	Message           *message        `json:"message"`
	EditedMessage     *message        `json:"edited_message"`
	ChannelPost       *message        `json:"channel_post"`
	EditedChannelPost *message        `json:"edited_channel_post"`
	CallbackQuery     *callbackQuery  `json:"callback_query"`
	MessageReaction   *reactionUpdate `json:"message_reaction"`
}

type user struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

// chat is a Chat. Type is "private", "group", "supergroup" or "channel".
type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *user  `json:"user"`
}

// message is a Message. MessageID is 0 for an ephemeral message, which
// has an ephemeral_message_id instead.
type message struct {
	MessageID            int64    `json:"message_id"`
	MessageThreadID      int64    `json:"message_thread_id"`
	From                 *user    `json:"from"`
	SenderChat           *chat    `json:"sender_chat"`
	Date                 int64    `json:"date"`
	EditDate             int64    `json:"edit_date"`
	Chat                 chat     `json:"chat"`
	ReplyToMessage       *message `json:"reply_to_message"`
	ViaBot               *user    `json:"via_bot"`
	EphemeralMessageID   int64    `json:"ephemeral_message_id"`
	BusinessConnectionID string   `json:"business_connection_id"`
	MediaGroupID         string   `json:"media_group_id"`
	Text                 string   `json:"text"`
	Entities             []entity `json:"entities"`
	Caption              string   `json:"caption"`
	CaptionEntities      []entity `json:"caption_entities"`

	// What a message can carry besides text, described and never fetched.
	Photo     []photoSize     `json:"photo"`
	Document  *fileInfo       `json:"document"`
	Animation *fileInfo       `json:"animation"`
	Audio     *fileInfo       `json:"audio"`
	Video     *fileInfo       `json:"video"`
	VideoNote *fileInfo       `json:"video_note"`
	Voice     *fileInfo       `json:"voice"`
	Sticker   *fileInfo       `json:"sticker"`
	Contact   json.RawMessage `json:"contact"`
	Location  json.RawMessage `json:"location"`
	Venue     json.RawMessage `json:"venue"`
	Poll      json.RawMessage `json:"poll"`
	Dice      json.RawMessage `json:"dice"`
}

type photoSize struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	FileSize     int64  `json:"file_size"`
}

// fileInfo is the part of Document, Animation, Audio, Video, VideoNote,
// Voice and Sticker the adapter describes.
type fileInfo struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	FileName     string `json:"file_name"`
	MimeType     string `json:"mime_type"`
	FileSize     int64  `json:"file_size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	Duration     int    `json:"duration"`
}

type callbackQuery struct {
	ID      string   `json:"id"`
	From    user     `json:"from"`
	Message *message `json:"message"`
	// InlineMessageID names a message sent through inline mode, which has
	// no chat the adapter can scope it by.
	InlineMessageID string `json:"inline_message_id"`
	Data            string `json:"data"`
}

type reactionType struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
}

type reactionUpdate struct {
	Chat        chat           `json:"chat"`
	MessageID   int64          `json:"message_id"`
	User        *user          `json:"user"`
	ActorChat   *chat          `json:"actor_chat"`
	Date        int64          `json:"date"`
	OldReaction []reactionType `json:"old_reaction"`
	NewReaction []reactionType `json:"new_reaction"`
}
