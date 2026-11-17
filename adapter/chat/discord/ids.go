package discord

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
)

// Snowflakes, as "Reference" defines them
// (https://docs.discord.com/developers/reference): a 64-bit integer,
// always a string in JSON, whose top 42 bits are milliseconds since the
// Discord epoch, the first second of 2015: "(snowflake >> 22) +
// 1420070400000". A snowflake built from a time pages by time with before
// and after.
const discordEpochMS = 1420070400000

// isSnowflake reports whether s is a snowflake: decimal digits of a
// non-zero 64-bit unsigned integer.
func isSnowflake(s string) bool {
	if s == "" || len(s) > 20 || s[0] == '0' {
		return false
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// snowflakeTime is the time a snowflake was minted; the zero time for one
// that is not a snowflake.
func snowflakeTime(s string) time.Time {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || !isSnowflake(s) {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + discordEpochMS).UTC()
}

// snowflakeAt is the smallest snowflake of the millisecond t falls in, "0"
// before the epoch.
func snowflakeAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpochMS
	if ms <= 0 {
		return "0"
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// lessSnowflake orders two snowflakes numerically.
func lessSnowflake(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// compareSnowflake is -1, 0 or 1 as a is before, equal to or after b.
func compareSnowflake(a, b string) int {
	switch {
	case a == b:
		return 0
	case lessSnowflake(a, b):
		return -1
	}
	return 1
}

// A message is named by its channel and its ID: "<channel ID>:<message
// ID>". Message IDs are unique across Discord, but every route that reads
// or changes a message names its channel, so the channel travels with it.
func messageID(channel, message string) string { return channel + ":" + message }

// parseMessageID splits a provider message ID into its channel and message.
func parseMessageID(id string) (channel, message string, ok bool) {
	channel, message, ok = strings.Cut(id, ":")
	return channel, message, ok && isSnowflake(channel) && isSnowflake(message)
}

func (x *Instance) roomRef(channel string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindRoom, ID: channel}
}

func (x *Instance) threadRef(parent, thread string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: Namespace, Kind: model.RefKindThread, ID: thread, ParentID: parent}
}

func (x *Instance) userRef(id string) model.ExternalRef {
	return model.ExternalRef{Channel: x.channel.Ref, Namespace: UserNamespace, Kind: RefKindUser, ID: id}
}

// target is where an effect lands: a channel (a thread is one), with the
// parent of a thread, or a user whose direct message channel it opens.
type target struct {
	channel string
	parent  string
	user    string
}

// parseRoute checks that r is a room, thread or user route of the
// instance's channel.
func (x *Instance) parseRoute(r model.ExternalRef) (target, error) {
	bad := final(model.Validation, "route is not a Discord room, thread or user of this channel")
	if r.Channel != x.channel.Ref {
		return target{}, bad
	}
	switch {
	case r.Namespace == Namespace && r.Kind == model.RefKindRoom && r.ParentID == "" && isSnowflake(r.ID):
		return target{channel: r.ID}, nil
	case r.Namespace == Namespace && r.Kind == model.RefKindThread && isSnowflake(r.ParentID) && isSnowflake(r.ID):
		return target{channel: r.ID, parent: r.ParentID}, nil
	case r.Namespace == UserNamespace && r.Kind == RefKindUser && r.ParentID == "" && isSnowflake(r.ID):
		return target{user: r.ID}, nil
	}
	return target{}, bad
}

// references are the scoping references of a message in channel: its room
// and, for a message in a thread, the thread. A thread's room is its
// parent channel, so a per-room inbox keeps a thread's messages with the
// channel it hangs off.
func (x *Instance) references(channel, parent string) []model.ExternalRef {
	if parent == "" {
		return []model.ExternalRef{x.roomRef(channel)}
	}
	return []model.ExternalRef{x.roomRef(parent), x.threadRef(parent, channel)}
}

// Thread channel types ("Channel Object", Channel Types,
// https://docs.discord.com/developers/resources/channel):
// ANNOUNCEMENT_THREAD 10, PUBLIC_THREAD 11, PRIVATE_THREAD 12.
func isThreadType(t int) bool { return t == 10 || t == 11 || t == 12 }

// learnChannel records what a payload said about a channel: its type and,
// for a thread, its parent.
func (x *Instance) learnChannel(id string, typ int, parent string) {
	if !isSnowflake(id) {
		return
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if len(x.threads) >= maxKnownChannels {
		clear(x.threads)
	}
	if isThreadType(typ) && isSnowflake(parent) {
		x.threads[id] = parent
	} else {
		x.threads[id] = ""
	}
}

// maxKnownChannels bounds the channel cache; past it the cache starts over.
const maxKnownChannels = 4096

// threadParent returns the parent of channel when it is a thread, "" when
// it is not. A gateway message names its channel and not the channel's
// type, so an unknown channel is read once (Get Channel) and remembered.
func (x *Instance) threadParent(ctx context.Context, channel string) (string, error) {
	x.mu.Lock()
	parent, known := x.threads[channel]
	x.mu.Unlock()
	if known {
		return parent, nil
	}
	var ch struct {
		ID       string `json:"id"`
		Type     int    `json:"type"`
		ParentID string `json:"parent_id"`
	}
	if err := x.call(ctx, http.MethodGet, "channels/"+channel, nil, &ch); err != nil {
		return "", err
	}
	x.learnChannel(channel, ch.Type, ch.ParentID)
	if isThreadType(ch.Type) && isSnowflake(ch.ParentID) {
		return ch.ParentID, nil
	}
	return "", nil
}

func final(code model.ErrorCode, msg string) *model.Error {
	return &model.Error{Code: code, SafeMessage: msg, Retry: model.NoRetry}
}
