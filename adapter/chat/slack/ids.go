package slack

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
)

// A Slack message is named by its conversation and its ts: ts is unique
// per conversation, not per workspace ("the unique (per-channel)
// timestamp", https://docs.slack.dev/reference/events/message/), so the
// provider message ID the adapter reports and takes back is
// "<conversation ID>:<ts>", such as "C0123ABC:1503435956.000247". Two
// conversations may hold a message with one ts; their IDs differ.
func messageID(conversation, ts string) string { return conversation + ":" + ts }

// tsPattern is the shape of a ts: epoch seconds, a dot and six digits.
var tsPattern = regexp.MustCompile(`^[0-9]{9,12}\.[0-9]{6}$`)

// parseMessageID splits a provider message ID into its conversation and ts.
func parseMessageID(id string) (conversation, ts string, ok bool) {
	conversation, ts, ok = strings.Cut(id, ":")
	return conversation, ts, ok && slackID.MatchString(conversation) && tsPattern.MatchString(ts)
}

// slackTime parses a ts; the zero time for a malformed one.
func slackTime(ts string) time.Time {
	secs, frac, ok := strings.Cut(ts, ".")
	if !ok || !tsPattern.MatchString(ts) {
		return time.Time{}
	}
	s, _ := strconv.ParseInt(secs, 10, 64)
	micro, _ := strconv.ParseInt(frac, 10, 64)
	return time.Unix(s, micro*1000).UTC()
}

// formatTS renders t as a ts, microsecond precision.
func formatTS(t time.Time) string {
	t = t.UTC()
	return strconv.FormatInt(t.Unix(), 10) + "." + leftPad6(t.Nanosecond()/1000)
}

func leftPad6(n int) string {
	s := strconv.Itoa(n)
	return strings.Repeat("0", 6-len(s)) + s
}

// Reference kinds of the routes the adapter reads: the room and thread
// kinds are core's (model.RefKindRoom, model.RefKindThread); a direct
// message to a user is a RefKindUser reference in UserNamespace.

func (x *Instance) roomRef(conversation string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindRoom, ID: conversation}
}

func (x *Instance) threadRef(conversation, threadTS string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindThread, ID: threadTS, ParentID: conversation}
}

func (x *Instance) userRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: UserNamespace, Kind: RefKindUser, ID: id}
}

// target is where an effect lands: a conversation, optionally one thread
// of it, or a user to open a direct message with.
type target struct {
	conversation string
	thread       string
	user         string
}

// parseRoute checks that r is a room, thread or user route of the
// instance's channel.
func (x *Instance) parseRoute(r model.ExternalRef) (target, error) {
	bad := final(model.Validation, "route is not a Slack room, thread or user of this channel")
	if r.Channel != x.channel.Ref {
		return target{}, bad
	}
	switch {
	case r.Namespace == Namespace && r.Kind == model.RefKindRoom && r.ParentID == "" && slackID.MatchString(r.ID):
		return target{conversation: r.ID}, nil
	case r.Namespace == Namespace && r.Kind == model.RefKindThread && slackID.MatchString(r.ParentID) && tsPattern.MatchString(r.ID):
		return target{conversation: r.ParentID, thread: r.ID}, nil
	case r.Namespace == UserNamespace && r.Kind == RefKindUser && r.ParentID == "" && slackID.MatchString(r.ID):
		return target{user: r.ID}, nil
	}
	return target{}, bad
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}
