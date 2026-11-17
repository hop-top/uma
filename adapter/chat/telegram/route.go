package telegram

import (
	"strconv"
	"strings"

	"hop.top/uma/model"
)

// parseMessageID splits a provider message ID into its chat and message_id.
func parseMessageID(s string) (chat, id int64, ok bool) {
	c, m, found := strings.Cut(s, ":")
	if !found {
		return 0, 0, false
	}
	chat, err1 := parseID(c)
	id, err2 := strconv.ParseInt(m, 10, 64)
	return chat, id, err1 == nil && err2 == nil && id > 0 && strconv.FormatInt(id, 10) == m
}

// parseID reads a chat or user ID in canonical decimal form: no sign but
// "-", no leading zeros, and never zero.
func parseID(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	if n == 0 || strconv.FormatInt(n, 10) != s {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

// target is where an effect lands: a chat, optionally one topic of it.
type target struct {
	chat   int64
	thread int64
}

// parseRoute checks that r is a room, thread or user route of the
// instance's channel. A user route is the private chat with the user, whose
// chat ID is the user's.
func (x *Instance) parseRoute(r model.ExternalRef) (target, error) {
	bad := final(model.Validation, "route is not a Telegram chat, topic or user of this channel")
	if r.Channel != x.channel.Ref {
		return target{}, bad
	}
	switch {
	case r.Namespace == Namespace && r.Kind == model.RefKindRoom && r.ParentID == "":
		if chat, err := parseID(r.ID); err == nil {
			return target{chat: chat}, nil
		}
	case r.Namespace == Namespace && r.Kind == model.RefKindThread:
		chat, err1 := parseID(r.ParentID)
		thread, err2 := parseID(r.ID)
		if err1 == nil && err2 == nil && thread > 0 {
			return target{chat: chat, thread: thread}, nil
		}
	case r.Namespace == UserNamespace && r.Kind == RefKindUser && r.ParentID == "":
		if user, err := parseID(r.ID); err == nil && user > 0 {
			return target{chat: user}, nil
		}
	}
	return target{}, bad
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}
