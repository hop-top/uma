package slack

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// historyScanPages is how many pages of historyPage messages one read of a
// time range takes before the range counts as too large; a variable for
// tests.
var historyScanPages = 5

const (
	// historyInitialSpan is the first time range a read of the oldest
	// messages asks for; it doubles while the range holds too few and
	// halves while it holds more than a read takes.
	historyInitialSpan = 6 * time.Hour
	// historyMinSpan is the narrowest range: a second holding more than a
	// read takes is a gap.
	historyMinSpan = time.Second
	// historyHorizon is how far past the host's now a read looks, for the
	// difference between the host's clock and Slack's.
	historyHorizon = 5 * time.Minute
)

// historyCursor is what a History cursor holds: the scope it was issued
// for, the position of the last message returned (ts, then conversation)
// and, for a thread, Slack's own cursor.
type historyCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"k"`
	TS      string `json:"ts,omitempty"`
	Conv    string `json:"c,omitempty"`
	Slack   string `json:"s,omitempty"`
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
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || c.Scope != scope || (c.TS != "" && !tsPattern.MatchString(c.TS)) {
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

// historyMessage is a message with its conversation.
type historyMessage struct {
	conv string
	m    slackEvent
}

func (a historyMessage) before(b historyMessage) int {
	if c := strings.Compare(a.m.TS, b.m.TS); c != 0 {
		return c
	}
	return strings.Compare(a.conv, b.conv)
}

// History reads one page of past messages, oldest first, posted at or
// after req.Since and before req.Until (zero is open), as the webhook
// reports them: message envelopes (the
// bot's own included, as automated messages from a bot sender, which core
// skips when it holds them already), each with its team as destination
// and its room, and thread for a reply, as references.
//
// With req.Route, one conversation: a room, a user's direct message (found
// with conversations.open and prevent_creation, so a read never creates
// one; a user with none has an empty window) or a thread (its parent and
// replies). Without it, the conversations of Config.HistoryChannels
// merged by time; without any it is model.Disabled.
//
// conversations.history lists newest first
// (https://docs.slack.dev/reference/methods/conversations.history/), so a
// read does not page through it. A page asks for the time range after the
// cursor, from a few hours wide, doubling it while it holds fewer
// messages than the page needs and halving it while Slack holds more than
// a read takes, so the oldest messages come first without a scan of the
// window. A thread is read through conversations.replies, which lists
// oldest first, with Slack's own cursor wrapped in the adapter's. The
// cursor also names its scope: one issued for another route, channels or
// window is model.Validation before any call. Until bounds the range a
// page asks for and drops a message at or after it.
//
// Gap reports what a read cannot hold: a second with more messages than a
// read takes, and a message with a thread, whose replies are not in a
// room's history; read the thread's route for them. Slack limits the
// method to one request a minute and 15 messages a page for apps outside
// the Marketplace installed from 29 May 2025; a rate limit is
// model.RateLimited with Slack's wait.
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
	var t target
	if req.Route != nil {
		var err error
		if t, err = x.parseRoute(*req.Route); err != nil {
			return adapter.HistoryPage{}, err
		}
	} else if len(x.cfg.HistoryChannels) == 0 {
		return adapter.HistoryPage{}, model.NewError(model.Disabled, "channel-wide history needs history_channels")
	}
	scope := historyScope(req.Route, x.cfg.HistoryChannels, req.Since, req.Until)
	cur := historyCursor{Version: 1, Scope: scope}
	if req.Cursor != "" {
		var err error
		if cur, err = decodeHistoryCursor(req.Cursor, scope); err != nil {
			return adapter.HistoryPage{}, err
		}
	}
	var page adapter.HistoryPage
	var err error
	if t.thread != "" {
		page, err = x.threadHistory(ctx, t, req, cur)
	} else {
		page, err = x.roomsHistory(ctx, t, req, cur)
	}
	if err != nil {
		_, err = failure(ctx, err, readCall)
		return adapter.HistoryPage{}, err
	}
	return page, nil
}

// normalMessage reports whether a message read from Slack is one the
// webhook would report: a message with an author and something to say.
func normalMessage(m slackEvent) bool {
	switch m.Subtype {
	case "", "thread_broadcast", "file_share", "me_message", "bot_message":
		return m.Type == "message" || m.Type == ""
	}
	return false
}

// envelope is the history envelope of a message read from Slack.
func (x *Instance) historyEnvelope(conv string, m slackEvent) (model.Envelope, bool) {
	m.Channel, m.Type = conv, "message"
	if !normalMessage(m) || !tsPattern.MatchString(m.TS) {
		return model.Envelope{}, false
	}
	env, ok := x.messageEnvelope(m, "message", x.mentions(m.Text))
	if !ok {
		return model.Envelope{}, false
	}
	// A read repeated is the same event.
	env.EventID = "history:" + env.MessageID
	return env, true
}

// threadHistory reads a thread: conversations.replies, oldest first, with
// the parent first.
func (x *Instance) threadHistory(ctx context.Context, t target, req adapter.HistoryRequest, cur historyCursor) (adapter.HistoryPage, error) {
	args := readArgs{Channel: t.conversation, TS: t.thread, Oldest: formatTS(req.Since), Inclusive: true, Limit: min(req.Limit, historyPage), Cursor: cur.Slack}
	var res readReply
	if err := x.call(ctx, "conversations.replies", &args, &res); err != nil {
		if ae, ok := errors.AsType[*apiError](err); ok && ae.code == "invalid_cursor" {
			return adapter.HistoryPage{}, final(model.Validation, "history cursor was not issued for this read")
		}
		return adapter.HistoryPage{}, err
	}
	var page adapter.HistoryPage
	since, until, bounded := formatTS(req.Since), untilTS(req.Until), !req.Until.IsZero()
	ended := false
	for _, m := range res.Messages {
		// The answer first holds the parent, whatever the range says of it.
		if m.TS < since {
			continue
		}
		if bounded && m.TS >= until {
			// Replies come oldest first: nothing after this is in the window.
			ended = true
			break
		}
		if env, ok := x.historyEnvelope(t.conversation, m); ok {
			page.Envelopes = append(page.Envelopes, env)
		}
	}
	if !ended && res.HasMore && res.ResponseMetadata.NextCursor != "" {
		cur.Slack = res.ResponseMetadata.NextCursor
		page.Next = cur.encode()
	}
	return page, nil
}

// roomsHistory reads the oldest messages after the cursor of one
// conversation or of every configured one, merged.
func (x *Instance) roomsHistory(ctx context.Context, t target, req adapter.HistoryRequest, cur historyCursor) (adapter.HistoryPage, error) {
	channels := x.cfg.HistoryChannels
	if req.Route != nil {
		conv, ok, err := x.historyConversation(ctx, t)
		if err != nil || !ok {
			return adapter.HistoryPage{}, err
		}
		channels = []string{conv}
	}
	after := formatTS(req.Since)
	exclusive := false
	if cur.TS != "" {
		after, exclusive = cur.TS, true
	}
	var all []historyMessage
	var more, gap bool
	need := req.Limit
	if exclusive {
		need++ // the cursor's own message comes back first
	}
	for _, conv := range channels {
		ms, m, g, err := x.oldest(ctx, conv, after, req.Until, need)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		more, gap = more || m, gap || g
		for _, e := range ms {
			if !req.Until.IsZero() && e.TS >= untilTS(req.Until) {
				continue
			}
			hm := historyMessage{conv: conv, m: e}
			if exclusive && (e.TS < cur.TS || (e.TS == cur.TS && conv <= cur.Conv)) {
				continue
			}
			all = append(all, hm)
		}
	}
	slices.SortFunc(all, historyMessage.before)
	if len(all) > req.Limit {
		all, more = all[:req.Limit], true
	}
	page := adapter.HistoryPage{Gap: gap}
	var last historyMessage
	for _, hm := range all {
		if hm.m.ReplyCount > 0 || (hm.m.ThreadTS != "" && hm.m.ThreadTS == hm.m.TS) {
			page.Gap = true
		}
		last = hm
		if env, ok := x.historyEnvelope(hm.conv, hm.m); ok {
			page.Envelopes = append(page.Envelopes, env)
		}
	}
	if more && last.m.TS != "" {
		cur.TS, cur.Conv = last.m.TS, last.conv
		page.Next = cur.encode()
	}
	return page, nil
}

// historyConversation resolves the conversation of a route to read. A user
// with no direct message has no window: ok is false.
func (x *Instance) historyConversation(ctx context.Context, t target) (conv string, ok bool, err error) {
	if t.user == "" {
		return t.conversation, true, nil
	}
	var res struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	err = x.call(ctx, "conversations.open", map[string]any{"users": t.user, "prevent_creation": true}, &res)
	if ae, isAPI := errors.AsType[*apiError](err); isAPI && slices.Contains(missingCodes, ae.code) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return res.Channel.ID, slackID.MatchString(res.Channel.ID), nil
}

// untilTS is the timestamp a message must be before to be in a window
// ending at until, rounded up to Slack's microsecond.
func untilTS(until time.Time) string { return formatTS(until.Add(time.Microsecond - 1)) }

// oldest reads the oldest limit messages of a conversation after ts
// (inclusive) and before until when set, by time ranges. more reports that messages may remain
// beyond what it returns; gap, that a range was too dense to read whole.
func (x *Instance) oldest(ctx context.Context, conv, after string, until time.Time, limit int) (msgs []slackEvent, more, gap bool, err error) {
	horizon := x.now().Add(historyHorizon)
	if !until.IsZero() && until.Before(horizon) {
		horizon = until
	}
	lo := slackTime(after)
	span := historyInitialSpan
	// ceiling is the narrowest end found to hold more than a read takes, and
	// dense what that read returned, its newest messages.
	var ceiling time.Time
	var dense []slackEvent
	finish := func(ms []slackEvent, partial bool, end time.Time) ([]slackEvent, bool, bool, error) {
		slices.SortFunc(ms, func(a, b slackEvent) int { return strings.Compare(a.TS, b.TS) })
		more = end.Before(horizon)
		if len(ms) > limit {
			ms, more = ms[:limit], true
		}
		return ms, more, partial, nil
	}
	for {
		hi := lo.Add(span)
		if hi.After(horizon) {
			hi = horizon
		}
		if !ceiling.IsZero() && !hi.Before(ceiling) {
			// Widening came back to a range that is too dense: what it holds
			// is all a read can have.
			return finish(dense, true, ceiling)
		}
		ms, truncated, err := x.readRange(ctx, conv, after, formatTS(hi))
		if err != nil {
			return nil, false, false, err
		}
		if truncated {
			if span > historyMinSpan {
				ceiling, dense = hi, ms
				span = max(span/2, historyMinSpan)
				continue
			}
			return finish(ms, true, hi)
		}
		if len(ms) >= limit || !hi.Before(horizon) {
			return finish(ms, false, hi)
		}
		span *= 2
	}
}

// readRange reads the messages of conversation conv from ts oldest to ts
// latest, both inclusive, newest first, up to historyScanPages pages;
// truncated reports more beyond them.
func (x *Instance) readRange(ctx context.Context, conv, oldest, latest string) (msgs []slackEvent, truncated bool, err error) {
	args := readArgs{Channel: conv, Oldest: oldest, Latest: latest, Inclusive: true}
	complete, err := x.readPages(ctx, "conversations.history", args, historyScanPages, func(m slackEvent) bool {
		msgs = append(msgs, m)
		return false
	})
	return msgs, !complete, err
}
