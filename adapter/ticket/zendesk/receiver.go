package zendesk

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// The polling receiver reads the Incremental Ticket Event Export with full
// comments (GET /api/v2/incremental/ticket_events?include=comment_events),
// so comments a missed or failed webhook never delivered still arrive.
// Every comment it hands on carries its event's time as the receiver
// cursor, and a connection resumes from the engine's cursor
// (adapter.CursorSink), re-reading only events of that second or later;
// without one it starts Config.PollLookbackSeconds before now. Ingress deduplicates comments
// already read, since a poll and a webhook give a comment the same
// identity. Each
// comment's ticket is read once per page for its brand and state; comments
// of a ticket the credential cannot read are skipped. A page with events is
// followed by the next at once; an empty one waits the poll interval, +-10%.
// Zendesk withholds the most recent minute, so a comment arrives a minute
// or more after it was written.

type eventPage struct {
	Events      []ticketEvent `json:"ticket_events"`
	EndTime     int64         `json:"end_time"`
	EndOfStream bool          `json:"end_of_stream"`
}

type ticketEvent struct {
	ID        zid       `json:"id"`
	TicketID  zid       `json:"ticket_id"`
	Timestamp int64     `json:"timestamp"`
	Children  []comment `json:"child_events"`
}

// jitterFraction is the spread of the poll wait: +-10%.
const jitterFraction = 0.1

// Run polls until ctx ends or the instance closes. A failed read or a
// sink refusal ends Run with the typed failure, leaving the comment unread
// for the next connection.
func (i *Instance) Run(ctx context.Context, sink adapter.Sink) error {
	x := i.x
	if sink == nil {
		return model.NewError(model.Validation, "sink required")
	}
	if err := x.ready(ctx); err != nil {
		return err
	}
	start, err := x.resume(ctx, sink)
	if err != nil {
		return err
	}
	for {
		next, more, err := x.poll(ctx, sink, start)
		if err != nil {
			return err
		}
		if more && next > start {
			start = next
			continue
		}
		t := time.NewTimer(x.pollWait())
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-x.done:
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

// pollWait is the wait after an empty page: the poll interval +-10%
// (adapter.Spread), so channels do not poll in phase, never below
// minPollInterval, the pace the incremental export allows.
func (x *instance) pollWait() time.Duration {
	return max(adapter.Spread(x.cfg.pollInterval(), jitterFraction, x.jitter), minPollInterval)
}

// resume returns the start time of the first page: the engine's receiver
// cursor when the sink keeps one, else the configured lookback.
func (x *instance) resume(ctx context.Context, sink adapter.Sink) (int64, error) {
	if cs, ok := sink.(adapter.CursorSink); ok {
		c, err := cs.ReceiverCursor(ctx)
		if err != nil {
			return 0, err
		}
		if n, err := strconv.ParseInt(c, 10, 64); err == nil && n > 0 {
			return n, nil
		}
	}
	return x.now().Add(-x.cfg.pollLookback()).Unix(), nil
}

// resumeAt is the export start time that reads ev again: its timestamp,
// or the page's start when Zendesk gives none.
func resumeAt(ev ticketEvent, start int64) int64 {
	if ev.Timestamp > 0 {
		return ev.Timestamp
	}
	return start
}

// poll reads one page from start and hands its comments to sink. It
// returns the next start time and whether the page held events.
func (x *instance) poll(ctx context.Context, sink adapter.Sink, start int64) (int64, bool, error) {
	q := url.Values{"start_time": {strconv.FormatInt(start, 10)}, "include": {"comment_events"}}
	var page eventPage
	if err := x.api.do(ctx, http.MethodGet, "/api/v2/incremental/ticket_events", q, nil, &page); err != nil {
		return 0, false, readFailure(err)
	}
	tickets := map[zid]*ticket{}
	for _, ev := range page.Events {
		for _, c := range ev.Children {
			if !c.isComment() || c.ID == "" || ev.TicketID == "" {
				continue
			}
			t, err := x.ticket(ctx, tickets, ev.TicketID)
			if err != nil {
				return 0, false, err
			}
			if t == nil {
				continue
			}
			in, ok, err := x.commentIngress(ctx, commentIn{
				ticket: *t, comment: c, occurredAt: parseTime(c.CreatedAt), auditID: c.AuditID,
				mayEcho: c.Via != nil && c.Via.Channel == "api",
			})
			if err != nil {
				return 0, false, err
			}
			if !ok {
				continue
			}
			in.ReceiverCursor = strconv.FormatInt(resumeAt(ev, start), 10)
			if _, err := sink.Accept(ctx, in); err != nil {
				return 0, false, err
			}
		}
	}
	if len(page.Events) == 0 {
		return start, false, nil
	}
	next := page.EndTime
	if page.EndOfStream {
		// Zendesk returned everything up to a minute ago: nothing more can
		// arrive at end_time.
		next++
	}
	return next, true, nil
}

// ticket reads a ticket once per page; nil when the credential cannot see
// it (deleted or inaccessible).
func (x *instance) ticket(ctx context.Context, cache map[zid]*ticket, id zid) (*ticket, error) {
	if t, ok := cache[id]; ok {
		return t, nil
	}
	var out struct {
		Ticket ticket `json:"ticket"`
	}
	err := x.api.do(ctx, http.MethodGet, ticketPath(string(id)), nil, nil, &out)
	switch statusOf(err) {
	case http.StatusNotFound, http.StatusForbidden:
		cache[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, readFailure(err)
	}
	if out.Ticket.ID == "" {
		out.Ticket.ID = id
	}
	cache[id] = &out.Ticket
	return &out.Ticket, nil
}
