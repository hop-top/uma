package twilio

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

// dateSentAfter is the list filter "sent on or after": Twilio documents
// it as DateSent>=YYYY-MM-DD, a whole UTC day, inclusive.
const dateSentAfter = "DateSent>"

// directionInbound is the direction of a message Twilio received.
const directionInbound = "inbound"

// InboundQuery selects the inbound messages of a history window.
type InboundQuery struct {
	// To is the channel's own address, which inbound messages arrive to.
	// Required.
	To string
	// From limits the window to one sender, a conversation's route; empty
	// is every sender.
	From string
	// Since starts the window. Twilio dates have one-second resolution, so
	// the second holding Since is in the window.
	Since time.Time
	// Until ends the window exclusively: a message sent at Until is out.
	// Zero leaves the window open.
	Until time.Time
	// After resumes after this position, the cursor of the page before;
	// zero starts at Since.
	After Position
	// Limit caps the messages of the page, at least 1.
	Limit int
}

// InboundPage is one page of a history window, oldest first.
type InboundPage struct {
	Messages []Message
	// Next is the cursor after the page's last message; empty, the window
	// holds no more.
	Next string
	// Gap reports inbound messages the listing could not place in the
	// window: past the pages one call reads, or without a readable date.
	Gap bool
}

// ListInbound returns the first q.Limit inbound messages of a window,
// oldest first: messages Twilio received (direction inbound) to q.To, from
// q.From when set, sent at or after q.Since, before q.Until when set, and after q.After. Outbound
// messages are never history: they are the channel's own sends, which no
// webhook reports as received.
//
// Twilio lists newest first, sorted by date_sent, and its DateSent filter
// is documented with day granularity only. So the listing filters on the
// UTC day holding the window's lower bound, reads pages from the newest
// until one holds a message before that bound (later pages are older
// still) or the list ends, keeps the window's messages, and orders them
// by Position: date_sent, then SID for messages of one second, a total
// order that the same read repeats. Each page reads the window from its
// lower bound to now again, so pages hold no message twice and a message
// that arrives meanwhile comes after the cursor. Past listMaxPages the
// oldest part of the window is out of reach: the page holds the oldest
// messages read, with Gap set.
//
// Failures map as for reconcile listings: 429 or error 20429 is
// rate_limited with the Retry-After Twilio stated, 401, 403 or error
// 20003 unauthorized, other 4xx permanent, the rest transient.
func (c *Client) ListInbound(ctx context.Context, accountSID string, cred ports.Credential, q InboundQuery) (InboundPage, error) {
	if q.To == "" || q.Limit < 1 {
		return InboundPage{}, model.NewError(model.Validation, "inbound listing needs a recipient and a limit")
	}
	lower := q.Since.UTC().Truncate(time.Second)
	if q.After.Sent.After(lower) {
		lower = q.After.Sent.UTC()
	}
	query := url.Values{}
	query.Set("To", q.To)
	if q.From != "" {
		query.Set("From", q.From)
	}
	if !lower.IsZero() {
		query.Set(dateSentAfter, lower.Truncate(24*time.Hour).Format(time.RFC3339))
	}
	query.Set("PageSize", strconv.Itoa(listPageSize))
	path := "/Messages.json?" + query.Encode()

	var (
		window   []positioned
		seen     = map[string]bool{}
		undated  bool
		complete bool
	)
	for range listMaxPages {
		var page struct {
			Messages    []Message `json:"messages"`
			NextPageURI string    `json:"next_page_uri"`
		}
		if err := c.do(ctx, http.MethodGet, accountSID, path, cred, nil, &page); err != nil {
			return InboundPage{}, readFailure(err)
		}
		older := false
		for _, m := range page.Messages {
			mine := m.Direction == directionInbound && m.To == q.To && (q.From == "" || m.From == q.From)
			sent, ok := m.SentAt()
			switch {
			case !ok:
				undated = undated || mine
				continue
			case sent.Before(lower):
				older = true
				continue
			case !q.Until.IsZero() && !sent.Before(q.Until):
				continue
			}
			p := Position{Sent: sent, SID: m.SID}
			if !mine || seen[m.SID] || (!q.After.IsZero() && p.compare(q.After) <= 0) {
				continue
			}
			seen[m.SID] = true
			window = append(window, positioned{p, m})
		}
		if older || page.NextPageURI == "" {
			complete = true
			break
		}
		next, ok := c.nextPage(accountSID, page.NextPageURI)
		if !ok {
			return InboundPage{}, &model.Error{Code: model.Permanent, SafeMessage: "Twilio's next page is off the account's message list", Retry: model.NoRetry}
		}
		path = next
	}
	slices.SortFunc(window, func(a, b positioned) int { return a.p.compare(b.p) })
	out := InboundPage{Gap: undated || !complete}
	for _, w := range window[:min(q.Limit, len(window))] {
		out.Messages = append(out.Messages, w.m)
	}
	if len(window) > q.Limit {
		out.Next = window[q.Limit-1].p.Cursor()
	}
	return out, nil
}

type positioned struct {
	p Position
	m Message
}

// SentAt is when Twilio sent the message, or received an inbound one
// (date_sent); ok is false when it has no readable date_sent.
func (m Message) SentAt() (time.Time, bool) {
	t, err := time.Parse(time.RFC1123Z, m.DateSent)
	return t.UTC(), err == nil
}

// Position is a message's place in a history window: its send time, at
// Twilio's one-second resolution, then its SID.
type Position struct {
	Sent time.Time
	SID  string
}

// IsZero reports whether p is no position: the window's start.
func (p Position) IsZero() bool { return p.SID == "" }

func (p Position) compare(o Position) int {
	if c := p.Sent.Compare(o.Sent); c != 0 {
		return c
	}
	return cmp.Compare(p.SID, o.SID)
}

// cursorVersion prefixes the cursors Cursor issues.
const cursorVersion = "h1"

// Cursor returns p as a history cursor.
func (p Position) Cursor() string {
	return cursorVersion + "." + strconv.FormatInt(p.Sent.Unix(), 10) + "." + p.SID
}

// ParseCursor reads a cursor Position.Cursor issued; empty is the zero
// position. Anything else, such as a raw next_page_uri, is
// model.Validation: the reader never follows a URL a caller supplies.
func ParseCursor(s string) (Position, error) {
	if s == "" {
		return Position{}, nil
	}
	bad := model.NewError(model.Validation, "unknown Twilio history cursor")
	parts := strings.Split(s, ".")
	if len(parts) != 3 || parts[0] != cursorVersion {
		return Position{}, bad
	}
	sec, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || sec <= 0 || (!validSID(parts[2], "SM") && !validSID(parts[2], "MM")) {
		return Position{}, bad
	}
	return Position{Sent: time.Unix(sec, 0).UTC(), SID: parts[2]}, nil
}
