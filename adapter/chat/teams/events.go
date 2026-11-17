package teams

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"hop.top/uma/model"
)

// Schemas of the structured data the adapter reports.
const (
	// PayloadSchema is the payload of every message envelope: where the
	// message was posted and what Teams said about it. Claims, never
	// identity.
	PayloadSchema  = "microsoft.teams.message"
	PayloadVersion = 1
	// AttachmentsSchema is the only part of a message that has attachments
	// and no text: their content types and names. The files and cards
	// themselves are not fetched or kept.
	AttachmentsSchema  = "microsoft.teams.attachments"
	AttachmentsVersion = 1
)

type malformed string

func (m malformed) Error() string { return "teams: malformed activity: " + string(m) }

// own reports whether account is the bot itself: its "28:<app ID>"
// account, or the recipient of the activity, which is the bot.
func (x *Instance) own(act activity, id string) bool {
	return id == x.cfg.botAccount() || (act.Recipient.ID != "" && id == act.Recipient.ID)
}

// senderKind is the contact kind of an account: bots have "28:" IDs and
// Teams marks them with the role "bot".
func senderKind(a account) string {
	if strings.HasPrefix(a.ID, "28:") || a.Role == "bot" {
		return model.ContactKindBot
	}
	return ""
}

// mentioned reports whether the activity mentions the bot: a mention
// entity naming its account
// (https://learn.microsoft.com/en-us/microsoftteams/platform/bots/how-to/conversations/channel-and-group-conversations).
func (x *Instance) mentioned(act activity) bool {
	for _, e := range act.Entities {
		if e.Type == "mention" && e.Mentioned != nil && x.own(act, e.Mentioned.ID) {
			return true
		}
	}
	return false
}

// newMessage reports a message activity. A message the bot sent is never
// reported (it would loop, and uma knows what it sent by its provider IDs);
// a bot's message is an automated message from a bot sender. A message
// with nothing to say, such as the data of a legacy Action.Submit, is
// dropped.
func (x *Instance) newMessage(act activity) ([]delivery, error) {
	if !validConversation(act.Conversation.ID) || !validID(act.ID) || !validID(act.From.ID) {
		return nil, malformed("message conversation, id or from")
	}
	if x.own(act, act.From.ID) {
		return nil, nil
	}
	env, ok := x.messageEnvelope(act)
	if !ok {
		return nil, nil
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

// messageEnvelope is the message envelope of act. It reports false for a
// message with nothing to say.
func (x *Instance) messageEnvelope(act activity) (model.Envelope, bool) {
	content, ok := messageContent(act.Text, act.Attachments)
	if !ok {
		return model.Envelope{}, false
	}
	kind := senderKind(act.From)
	id := messageID(act.Conversation.ID, act.ID)
	env := model.Envelope{
		EventID: "message:" + id, MessageID: id, Destination: x.cfg.TenantID, Kind: model.EnvelopeMessage,
		Sender: x.userRef(act.From.ID), SenderKind: kind, Assurance: "verified",
		References: x.references(act), Content: content,
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: act.at(),
	}
	if kind == model.ContactKindBot {
		env.Kind = model.EnvelopeAutomatedMessage
	}
	if validID(act.ReplyToID) {
		env.InReplyTo = messageID(act.Conversation.ID, act.ReplyToID)
	}
	env.Payload = x.payload(act)
	return env, true
}

// references are the scoping references of a message: its conversation
// and, in a channel, its thread: the one its conversation ID names, or,
// for a root post that names none, the one it roots. Chats have no
// threads.
func (x *Instance) references(act activity) []model.ExternalRef {
	refs := []model.ExternalRef{x.roomRef(act.Conversation.ID)}
	if !act.channel() {
		return refs
	}
	root := threadRoot(act.Conversation.ID)
	if root == "" {
		root = act.ID
	}
	if validID(root) {
		refs = append(refs, x.threadRef(act.Conversation.ID, root))
	}
	return refs
}

func (x *Instance) payload(act activity) model.Extension {
	data := map[string]any{
		"tenant_id": x.cfg.TenantID, "conversation": act.Conversation.ID, "activity_id": act.ID,
		"conversation_type": act.Conversation.ConversationType, "mentioned": x.mentioned(act),
	}
	if act.ReplyToID != "" && validID(act.ReplyToID) {
		data["reply_to_id"] = act.ReplyToID
	}
	if act.From.AADObjectID != "" {
		data["aad_object_id"] = act.From.AADObjectID
	}
	if cd := act.ChannelData; cd.Team != nil && cd.Team.ID != "" {
		data["team_id"] = cd.Team.ID
	}
	if cd := act.ChannelData; cd.Channel != nil && cd.Channel.ID != "" {
		data["teams_channel_id"] = cd.Channel.ID
	}
	b, _ := json.Marshal(data)
	return model.Extension{Name: PayloadSchema, Version: PayloadVersion, Data: b}
}

// messageContent is a message's content: its text as Teams sent it, or,
// for a message with attachments and no text, their description. The
// text/html rendering Teams attaches to a message alongside its text is
// not an attachment of its own.
func messageContent(text string, atts []attachment) (model.Content, bool) {
	if strings.TrimSpace(text) != "" {
		return model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}, true
	}
	type described struct {
		ContentType string `json:"content_type"`
		Name        string `json:"name,omitempty"`
	}
	var out []described
	for _, a := range atts {
		if a.ContentType == "" || a.ContentType == "text/html" {
			continue
		}
		out = append(out, described{ContentType: a.ContentType, Name: a.Name})
	}
	if len(out) == 0 {
		return model.Content{}, false
	}
	data, _ := json.Marshal(map[string]any{"attachments": out})
	return model.Content{Parts: []model.Part{{Kind: model.PartStructured, Structured: &model.Extension{Name: AttachmentsSchema, Version: AttachmentsVersion, Data: data}}}}, true
}

// edited reports a messageUpdate activity whose eventType is editMessage
// as an edit of the message: the activity carries the message's ID and its
// whole new text. Other updates (undeleteMessage) are not reported. An edit
// of the bot's own message is not reported, as the message itself is not.
func (x *Instance) edited(act activity) ([]delivery, error) {
	if act.ChannelData.EventType != "editMessage" {
		return nil, nil
	}
	if !validConversation(act.Conversation.ID) || !validID(act.ID) || !validID(act.From.ID) {
		return nil, malformed("messageUpdate conversation, id or from")
	}
	if x.own(act, act.From.ID) {
		return nil, nil
	}
	first, ok := x.messageEnvelope(act)
	if !ok {
		return nil, nil
	}
	env := first
	// Teams gives a messageUpdate no ID of its own: the activity's ID is
	// the message's. The edit is named by its time and new text, so a
	// redelivery is the same event and another edit another.
	sum := sha256.Sum256([]byte(act.Text))
	env.EventID = "edit:" + first.MessageID + ":" + act.Timestamp + ":" + hex.EncodeToString(sum[:8])
	env.Kind, env.SourceRevision = model.EnvelopeMessageEdited, act.Timestamp
	env.References, env.Payload, env.InReplyTo = nil, model.Extension{}, ""
	if !validID(act.Timestamp) {
		return nil, malformed("messageUpdate timestamp")
	}
	return []delivery{{ingress: x.ingress(env), fallback: ptr(x.ingress(first))}}, nil
}

// deleted reports a messageDelete activity whose eventType is
// softDeleteMessage. Teams names the account that deleted it as from; the
// bot's own deletions are not reported.
func (x *Instance) deleted(act activity) ([]delivery, error) {
	if act.ChannelData.EventType != "softDeleteMessage" {
		return nil, nil
	}
	if !validConversation(act.Conversation.ID) || !validID(act.ID) {
		return nil, malformed("messageDelete conversation or id")
	}
	if x.own(act, act.From.ID) {
		return nil, nil
	}
	id := messageID(act.Conversation.ID, act.ID)
	env := model.Envelope{
		EventID: "delete:" + id, MessageID: id, Destination: x.cfg.TenantID,
		Kind: model.EnvelopeMessageDeleted, Assurance: "verified",
		Audience: model.Audience{Visibility: model.Public}, OccurredAt: act.at(),
	}
	if validID(act.From.ID) {
		env.Sender, env.SenderKind = x.userRef(act.From.ID), senderKind(act.From)
	} else {
		env.SystemActor = true
	}
	return []delivery{{ingress: x.ingress(env)}}, nil
}

// reactions reports a messageReaction activity: one envelope for each
// reaction added or removed, on the message replyToId names. A reaction
// the adapter has no emoji for is dropped; see reactionEmoji.
func (x *Instance) reactions(act activity) ([]delivery, error) {
	if !validConversation(act.Conversation.ID) || !validID(act.ReplyToID) || !validID(act.From.ID) {
		return nil, malformed("messageReaction conversation, replyToId or from")
	}
	if x.own(act, act.From.ID) {
		return nil, nil
	}
	id := messageID(act.Conversation.ID, act.ReplyToID)
	// The activity's own ID names one delivery of the reactions; Teams
	// documents IDs for them such as "f:9f78d1f3". Without one, the
	// message, reactor and time stand in.
	event := "reaction:" + act.ID
	if !validID(act.ID) {
		event = "reaction:" + id + ":" + act.From.ID + ":" + act.Timestamp
	}
	var out []delivery
	add := func(rs []reaction, kind, verb string) {
		for _, r := range rs {
			emoji, ok := reactionEmoji(r.Type)
			if !ok {
				continue
			}
			env := model.Envelope{
				EventID:   event + ":" + verb + ":" + r.Type,
				MessageID: id, Destination: x.cfg.TenantID, Kind: kind,
				Sender: x.userRef(act.From.ID), SenderKind: senderKind(act.From), Assurance: "verified",
				Content:  model.Content{Parts: []model.Part{{Kind: model.PartText, Text: emoji}}},
				Audience: model.Audience{Visibility: model.Public}, OccurredAt: act.at(),
			}
			out = append(out, delivery{ingress: x.ingress(env)})
		}
	}
	add(act.ReactionsAdded, model.EnvelopeReactionAdded, "added")
	add(act.ReactionsRemoved, model.EnvelopeReactionRemoved, "removed")
	return out, nil
}

func (x *Instance) ingress(env model.Envelope) model.Ingress {
	in := model.Ingress{Envelope: env}
	if c := model.CorrelationID("teams-" + env.EventID); c.Validate() == nil {
		in.Correlation = c
	}
	return in
}

func ptr[T any](v T) *T { return &v }
