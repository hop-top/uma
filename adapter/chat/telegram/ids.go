package telegram

import (
	"strconv"

	"hop.top/uma/model"
)

// A Telegram message is named by its chat and its message_id: the ID is
// "Unique message identifier inside this chat"
// (https://core.telegram.org/bots/api#message), so two chats may hold a
// message with one ID, and the provider message ID the adapter reports and
// takes back is "<chat ID>:<message_id>", such as "-1001234567890:42". A
// chat ID of a group is negative; "-" is never the separator.
func messageID(chat, id int64) string {
	return strconv.FormatInt(chat, 10) + ":" + strconv.FormatInt(id, 10)
}

func idString(n int64) string { return strconv.FormatInt(n, 10) }

// Reference kinds. Rooms and threads are core's (model.RefKindRoom,
// model.RefKindThread) under Namespace; a user, and the chat a message was
// sent on behalf of (an anonymous administrator, a channel), are
// identities under UserNamespace.
const (
	// Namespace holds rooms (RefKindRoom, the chat ID) and threads
	// (RefKindThread, the message_thread_id of a forum topic under its chat
	// ID).
	Namespace = "telegram.messages"
	// UserNamespace is the identity namespace and the route of a private
	// chat: a user's ID is the ID of the chat the bot has with them.
	UserNamespace = "telegram.messages.user"
	// RefKindUser is a user or a bot; RefKindChat a group, supergroup or
	// channel speaking as itself (Message.sender_chat). User IDs are
	// positive and group and channel IDs negative, so one namespace holds
	// both without a clash.
	RefKindUser = "user"
	RefKindChat = "chat"
)

func (x *Instance) roomRef(chat int64) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindRoom, ID: idString(chat)}
}

func (x *Instance) threadRef(chat, thread int64) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindThread, ID: idString(thread), ParentID: idString(chat)}
}

func (x *Instance) userRef(id int64) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: UserNamespace, Kind: RefKindUser, ID: idString(id)}
}

func (x *Instance) chatIdentityRef(id int64) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: UserNamespace, Kind: RefKindChat, ID: idString(id)}
}
