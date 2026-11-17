package discord

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// historyCursor is what a History cursor holds: the scope it was issued
// for and the last message ID returned.
type historyCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"k"`
	After   string `json:"a"`
}

func (c historyCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeHistoryCursor(s, scope string) (historyCursor, error) {
	bad := final(model.Validation, "history cursor was not issued for this read")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historyCursor{}, bad
	}
	var c historyCursor
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || c.Scope != scope || !isSnowflake(c.After) {
		return historyCursor{}, bad
	}
	return c, nil
}

// historyScope names what a read covers, for its cursors.
func historyScope(route *model.ExternalRef, channels []string, since, until time.Time) string {
	h := sha256.New()
	if route != nil {
		h.Write([]byte(route.Namespace + "/" + route.Kind + "/" + route.ParentID + "/" + route.ID))
	} else {
		h.Write([]byte("*" + strings.Join(channels, ",")))
	}
	h.Write([]byte("@" + strconv.FormatInt(since.UnixMicro(), 10)))
	if !until.IsZero() {
		h.Write([]byte("<" + strconv.FormatInt(until.UnixMicro(), 10)))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// flagHasThread marks a message that heads a thread ("HAS_THREAD").
const flagHasThread = 1 << 5

// History reads one page of past messages, oldest first, posted at or
// after req.Since and before req.Until (zero is open), as the gateway
// reports them: message envelopes (the
// bot's own included, as automated messages from a bot sender, which core
// skips when it holds them already), each with the application as
// destination and its room, and thread for a thread's message, as
// references.
//
// With req.Route, one channel: a room, a thread, or a user's direct
// message channel (Create DM, which answers the existing one). Without it,
// the channels of Config.HistoryChannels merged by ID (a thread among them
// is read as a thread, its parent looked up once); without any it is
// model.Disabled.
//
// Get Channel Messages pages by snowflake: after an ID, at most 100, newest
// first. Snowflakes order messages in time across channels, so one cursor,
// the last ID returned, serves every channel: a page reads each channel
// after it, merges, and keeps the oldest. The window starts at the
// snowflake of req.Since and ends before that of req.Until, the first
// message at or after it ending its channel's read. A cursor names its
// scope: one issued for another route, channels or window is
// model.Validation before any call. A
// message heading a thread reports a gap: the thread's messages are not in
// its channel's history; read the thread's route for them.
func (x *Instance) History(ctx context.Context, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	if err := x.ready(); err != nil {
		return adapter.HistoryPage{}, err
	}
	switch {
	case req.Limit < 1:
		return adapter.HistoryPage{}, final(model.Validation, "history limit must be at least 1")
	case req.Since.IsZero():
		return adapter.HistoryPage{}, final(model.Validation, "history window start required")
	}
	scope := historyScope(req.Route, x.cfg.HistoryChannels, req.Since, req.Until)
	cur := historyCursor{Version: 1, Scope: scope, After: beforeSnowflake(snowflakeAt(req.Since))}
	if req.Cursor != "" {
		var err error
		if cur, err = decodeHistoryCursor(req.Cursor, scope); err != nil {
			return adapter.HistoryPage{}, err
		}
	}
	type source struct{ channel, parent string }
	var sources []source
	if req.Route != nil {
		t, err := x.parseRoute(*req.Route)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		channel, out, err := x.channelOf(ctx, t)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		if out.Failure != nil {
			return adapter.HistoryPage{}, out.Failure
		}
		sources = []source{{channel, t.parent}}
	} else {
		if len(x.cfg.HistoryChannels) == 0 {
			return adapter.HistoryPage{}, model.NewError(model.Disabled, "channel-wide history needs history_channels")
		}
		for _, c := range x.cfg.HistoryChannels {
			parent, err := x.threadParent(ctx, c)
			if err != nil {
				_, ferr := failure(ctx, err, readCall)
				return adapter.HistoryPage{}, ferr
			}
			sources = append(sources, source{c, parent})
		}
	}
	limit := min(req.Limit, pageSize)
	// Until's snowflake, rounded up to the millisecond a message's ID
	// carries: a message at Until or later is outside the window.
	end := ""
	if !req.Until.IsZero() {
		end = snowflakeAt(req.Until.Add(time.Millisecond - 1))
	}
	type found struct {
		m      message
		parent string
	}
	var all []found
	more := false
	for _, s := range sources {
		page, err := x.messagesAfter(ctx, s.channel, cur.After, limit)
		if err != nil {
			_, ferr := failure(ctx, err, readCall)
			return adapter.HistoryPage{}, ferr
		}
		ended := false
		for _, m := range page {
			if !isSnowflake(m.ID) || compareSnowflake(m.ID, cur.After) <= 0 {
				continue
			}
			if end != "" && compareSnowflake(m.ID, end) >= 0 {
				ended = true
				continue
			}
			m.ChannelID = s.channel
			all = append(all, found{m, s.parent})
		}
		more = more || (len(page) >= limit && !ended)
	}
	slices.SortFunc(all, func(a, b found) int { return compareSnowflake(a.m.ID, b.m.ID) })
	if len(all) > limit {
		all, more = all[:limit], true
	}
	var page adapter.HistoryPage
	for _, f := range all {
		if f.m.Flags&flagHasThread != 0 {
			page.Gap = true
		}
		env, ok := x.messageEnvelope(f.m, f.parent, x.mentions(f.m))
		if !ok {
			continue
		}
		env.EventID = "history:" + env.MessageID
		page.Envelopes = append(page.Envelopes, env)
	}
	if more && len(all) > 0 {
		cur.After = all[len(all)-1].m.ID
		page.Next = cur.encode()
	}
	return page, nil
}

// beforeSnowflake is the snowflake just before s, so that after it
// includes s; "0" stays "0".
func beforeSnowflake(s string) string {
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return "0"
	}
	return strconv.FormatUint(n-1, 10)
}

// Refetch reads a message's current state (Get Channel Message) and returns
// it as the gateway reports an edit: a message_edited envelope by its
// author, at its edited_timestamp, else its timestamp. A message whose
// content Discord withholds (no MESSAGE_CONTENT intent, no mention) cannot
// be reported and is model.Unsupported; a message that is gone is
// model.NotFound.
func (x *Instance) Refetch(ctx context.Context, req adapter.RefetchRequest) (model.Envelope, error) {
	if err := x.ready(); err != nil {
		return model.Envelope{}, err
	}
	t := req.Target
	channel, id, ok := parseMessageID(t.ID)
	if t.Channel != x.channel.Ref || t.Namespace != model.ProviderMessageNamespace || t.Kind != model.RefKindMessage || !ok {
		return model.Envelope{}, final(model.Validation, "target is not a Discord message of this channel")
	}
	m, gone, err := x.readMessage(ctx, channel, id)
	if gone {
		return model.Envelope{}, final(model.NotFound, "message not found")
	}
	if err != nil {
		_, ferr := failure(ctx, err, readCall)
		return model.Envelope{}, ferr
	}
	m.ChannelID = channel
	parent, err := x.threadParent(ctx, channel)
	if err != nil {
		_, ferr := failure(ctx, err, readCall)
		return model.Envelope{}, ferr
	}
	env, ok := x.messageEnvelope(m, parent, x.mentions(m))
	if !ok {
		return model.Envelope{}, final(model.Unsupported, "Discord withholds this message's content")
	}
	env.Kind, env.References, env.Payload, env.InReplyTo = model.EnvelopeMessageEdited, nil, model.Extension{}, ""
	env.SourceRevision = m.Timestamp
	if m.EditedTimestamp != nil && *m.EditedTimestamp != "" {
		env.SourceRevision = *m.EditedTimestamp
		if at, err := time.Parse(time.RFC3339Nano, *m.EditedTimestamp); err == nil {
			env.OccurredAt = at.UTC()
		}
	}
	return env, nil
}

var (
	_ adapter.HistoryReader = (*Instance)(nil)
	_ adapter.Refetcher     = (*Instance)(nil)
)
