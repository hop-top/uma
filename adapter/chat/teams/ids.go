package teams

import (
	"strings"
	"unicode"

	"hop.top/uma/model"
)

// A Teams message is named by its conversation and its activity ID: an
// activity ID is assigned by Teams and unique within its conversation
// (https://learn.microsoft.com/en-us/azure/bot-service/rest-api/bot-framework-rest-connector-api-reference),
// so the provider message ID the adapter reports and takes back is
// "<conversation ID>|<activity ID>". The conversation is the base one: a
// channel's conversation ID without the ";messageid=<root>" suffix Teams
// adds for a thread, so a message keeps one ID whichever form an activity
// about it carries.
const idSeparator = "|"

// threadMarker separates a channel's conversation ID from the root message
// of one of its threads.
const threadMarker = ";messageid="

// maxIDBytes bounds the conversation, activity and account IDs the adapter
// takes.
const maxIDBytes = 512

func messageID(conversation, activity string) string {
	return baseConversation(conversation) + idSeparator + activity
}

// parseMessageID splits a provider message ID into its base conversation
// and activity ID.
func parseMessageID(id string) (conversation, activity string, ok bool) {
	conversation, activity, ok = strings.Cut(id, idSeparator)
	return conversation, activity, ok && validConversation(conversation) && !strings.Contains(conversation, threadMarker) && validID(activity)
}

// baseConversation strips a thread's suffix from a conversation ID.
func baseConversation(id string) string {
	base, _, _ := strings.Cut(id, threadMarker)
	return base
}

// threadRoot is the root message a conversation ID names after
// ";messageid=", "" for none.
func threadRoot(id string) string {
	_, root, ok := strings.Cut(id, threadMarker)
	if !ok || !validID(root) {
		return ""
	}
	return root
}

// threadConversation is the conversation ID of a channel thread.
func threadConversation(base, root string) string { return base + threadMarker + root }

// validID checks an activity or account ID: printable ASCII without spaces,
// the separator or path characters, at most maxIDBytes.
func validID(s string) bool {
	if s == "" || len(s) > maxIDBytes {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) || r == ' ' || strings.ContainsRune("|/\\?#%", r) {
			return false
		}
	}
	return true
}

// validConversation checks a conversation ID; it may carry a thread
// suffix, whose ";" and "=" an activity ID never holds.
func validConversation(s string) bool {
	base, root, threaded := strings.Cut(s, threadMarker)
	return validID(base) && !strings.ContainsAny(base, ";=") && (!threaded || validID(root))
}

func (x *Instance) roomRef(conversation string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindRoom, ID: baseConversation(conversation)}
}

func (x *Instance) threadRef(conversation, root string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindThread, ID: root, ParentID: baseConversation(conversation)}
}

func (x *Instance) userRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: UserNamespace, Kind: RefKindUser, ID: id}
}

// target is where an effect lands: a conversation, optionally a thread of
// it, or a user to open a personal chat with.
type target struct {
	conversation string
	thread       string
	user         string
}

// conversationID is the conversation ID a post to t goes to.
func (t target) conversationID() string {
	if t.thread != "" {
		return threadConversation(t.conversation, t.thread)
	}
	return t.conversation
}

// parseRoute checks that r is a room, thread or user route of the
// instance's channel.
func (x *Instance) parseRoute(r model.ExternalRef) (target, error) {
	bad := final(model.Validation, "route is not a Teams conversation, thread or user of this channel")
	if r.Channel != x.channel.Ref {
		return target{}, bad
	}
	base := func(s string) bool { return validConversation(s) && !strings.Contains(s, threadMarker) }
	switch {
	case r.Namespace == Namespace && r.Kind == model.RefKindRoom && r.ParentID == "" && base(r.ID):
		return target{conversation: r.ID}, nil
	case r.Namespace == Namespace && r.Kind == model.RefKindThread && base(r.ParentID) && validID(r.ID):
		return target{conversation: r.ParentID, thread: r.ID}, nil
	case r.Namespace == UserNamespace && r.Kind == RefKindUser && r.ParentID == "" && validID(r.ID):
		return target{user: r.ID}, nil
	}
	return target{}, bad
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}
