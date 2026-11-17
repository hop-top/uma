package google

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// watchMargin is how long before a watch's expiry it is renewed at the
// latest, whatever the renewal interval.
const watchMargin = time.Hour

// jitterFraction is the spread of the poll wait and of the renewal time:
// +-10%.
const jitterFraction = 0.1

// errHistoryGone reports a stored historyId Gmail no longer holds
// (history.list answered 404): only a full synchronization recovers.
var errHistoryGone = errors.New("gmail: history id out of date")

// Run is the channel's receiver. Each round it keeps the users.watch alive
// (when a Pub/Sub topic is configured) and then reads the mailbox history
// from the stored historyId, so messages whose push notification was
// dropped, throttled (Gmail sends at most one per second per user) or never
// delivered still arrive. Rounds repeat after the poll interval (+-10%, so
// channels do not poll in phase), before
// the watch's renewal time, or as soon as a push notification arrives.
//
// The stored historyId advances only past history records every message of
// which the sink accepted: a message the sink refuses is read again next
// round. A round that fails part way through a page still stores the
// historyId past the records it handed over whole, so the next round does
// not hand them over again: a receiver that only meets the same failure
// accepts nothing, and the engine's Retry.MaxAttempts ends it. A sink error
// ends Run with that error.
func (i *instance) Run(ctx context.Context, sink adapter.Sink) error {
	for {
		next, err := i.round(ctx, sink)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		t := time.NewTimer(next)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-i.state.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

// round runs one watch check and one history round and returns the wait
// before the next round.
func (i *instance) round(ctx context.Context, sink adapter.Sink) (time.Duration, error) {
	s := i.c.session()
	st, err := i.loadState(ctx)
	if err != nil {
		return 0, err
	}
	wait := i.spread(i.cfg.pollInterval())
	if i.cfg.push() {
		next, err := i.maintainWatch(ctx, s, &st)
		switch {
		case err != nil && ctx.Err() != nil:
			return 0, ctx.Err()
		case err != nil:
			// History polling still covers the mailbox; the watch is
			// retried next round. The host is told push is failing.
			i.health.fail(watchFailureCode(err), permanentFailure(err), expiresBefore(st.WatchExpires, i.now().Add(wait)))
		default:
			i.health.ok()
			if err := i.saveState(ctx, st); err != nil {
				return 0, err
			}
			if d := next.Sub(i.now()); d < wait {
				wait = max(d, time.Second)
			}
		}
	}
	initial := st.HistoryID == 0
	err = i.syncHistory(ctx, s, sink, &st, true, 0)
	if errors.Is(err, errHistoryGone) {
		err = i.fullSync(ctx, s, sink, &st, !initial)
	}
	if err == nil {
		i.link.set(true)
	}
	return wait, err
}

// watchFailureCode is the degradation code of a failed users.watch:
// permission refused, another permanent refusal, or transient.
func watchFailureCode(err error) string {
	switch {
	case !permanentFailure(err):
		return CodePushUnavailable
	case model.IsCode(err, model.Unauthorized):
		return CodePushDenied
	}
	return CodePushRejected
}

// maintainWatch calls users.watch when there is no watch, it is due for
// renewal (Gmail recommends daily) or close to its expiry (Gmail stops
// notifying after 7 days without a call). It returns when the watch next
// needs renewing.
func (i *instance) maintainWatch(ctx context.Context, s *session, st *receiverState) (time.Time, error) {
	now := i.now()
	if st.WatchExpires.IsZero() || !now.Before(i.watchDue(*st)) {
		resp, err := s.do(ctx, request{method: "POST", url: i.c.api("/watch", nil), body: encode(gWatchRequest{
			TopicName: i.cfg.Topic, LabelIDs: []string{labelInbox}, LabelFilterBehavior: "INCLUDE",
		})})
		if err != nil {
			return time.Time{}, transient(err)
		}
		var w gWatchResponse
		if err := decode(resp.body, &w); err != nil {
			return time.Time{}, err
		}
		st.WatchedAt, st.WatchExpires = now, now.Add(7*24*time.Hour)
		if w.Expiration > 0 {
			st.WatchExpires = time.UnixMilli(int64(w.Expiration)).UTC()
		}
		st.WatchRenewAt = now.Add(i.spread(i.cfg.watchRenewInterval()))
	}
	return i.watchDue(*st), nil
}

// spread is d +-10% (adapter.Spread) from the factory's jitter source.
func (i *instance) spread(d time.Duration) time.Duration {
	return adapter.Spread(d, jitterFraction, i.f.opts.Jitter)
}

// watchDue is when the watch in st is renewed: its WatchRenewAt, never
// later than watchMargin before its expiry.
func (i *instance) watchDue(st receiverState) time.Time {
	due := st.WatchRenewAt
	if due.IsZero() {
		due = st.WatchedAt.Add(i.cfg.watchRenewInterval())
	}
	if late := st.WatchExpires.Add(-watchMargin); late.Before(due) {
		due = late
	}
	return due
}

// syncHistory reads history.list from st.HistoryID and hands every message
// added to the inbox to the sink. With persist the progress is saved after
// each page, and before a failure part way through one; otherwise (the push
// handler) st is only a local copy, at most maxPages pages are read (zero
// is no limit) and an unreadable message is left for the receiver round
// that moves the cursor past it to observe. A stored historyId of zero
// needs a full synchronization: syncHistory then reads nothing without
// persist and returns errHistoryGone with it.
//
// A page that is not the JSON asked for fails the round as transient
// before any of its records is handed over (see decodeAnswer); 404 is an
// out-of-date historyId (errHistoryGone).
func (i *instance) syncHistory(ctx context.Context, s *session, sink adapter.Sink, st *receiverState, persist bool, maxPages int) error {
	if st.HistoryID == 0 {
		if !persist {
			return nil
		}
		return errHistoryGone
	}
	start := st.HistoryID
	seen := map[string]bool{}
	token := ""
	for pages := 1; ; pages++ {
		q := url.Values{
			"startHistoryId": {start.String()},
			"historyTypes":   {"messageAdded", "labelAdded", "labelRemoved", "messageDeleted"},
			"maxResults":     {strconv.Itoa(i.f.opts.PageSize)},
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/history", q)})
		if model.IsCode(err, model.NotFound) {
			return errHistoryGone
		}
		if err != nil {
			return transient(err)
		}
		var page gHistoryList
		if err := decodeAnswer(resp.body, &page); err != nil {
			return err
		}
		saved := st.HistoryID
		fail := func(err error) error {
			if persist && st.HistoryID > saved {
				// Keep the records handed over whole; a failed save only
				// means they are handed over again.
				_ = i.saveState(ctx, *st)
			}
			return err
		}
		reported := map[string]bool{}
		for _, h := range page.History {
			for _, added := range h.MessagesAdded {
				m := added.Message
				// The history is the mailbox's, not the inbox's: only a
				// message that arrived in the inbox is inbound mail.
				if m.ID == "" || seen[m.ID] || skipLabels(m.LabelIDs) || !slices.Contains(m.LabelIDs, labelInbox) {
					continue
				}
				seen[m.ID] = true
				if err := i.ingest(ctx, s, sink, m.ID, persist); err != nil {
					return fail(err)
				}
			}
			// A thread's state is read as it is now, after the messages the
			// page added: once per page, whatever the records after this
			// one changed.
			for _, id := range h.stateThreads() {
				if reported[id] {
					continue
				}
				reported[id] = true
				if err := i.reportThread(ctx, s, sink, id, h.ID); err != nil {
					return fail(err)
				}
			}
			if h.ID > st.HistoryID {
				st.HistoryID = h.ID
			}
		}
		if page.NextPageToken == "" && page.HistoryID > st.HistoryID {
			st.HistoryID = page.HistoryID
		}
		if persist {
			if err := i.saveState(ctx, *st); err != nil {
				return err
			}
		}
		if page.NextPageToken == "" || (maxPages > 0 && pages >= maxPages) {
			return nil
		}
		token = page.NextPageToken
	}
}

// fullSync is the documented recovery when no usable historyId is stored:
// read the current historyId from the profile first (so nothing after it is
// missed), hand every inbox message received within the initial sync
// window to the sink, oldest first, then resume history from that
// historyId. gap marks a resync forced by an out-of-date historyId; it is
// counted in the state.
func (i *instance) fullSync(ctx context.Context, s *session, sink adapter.Sink, st *receiverState, gap bool) error {
	prof, err := i.getProfile(ctx, s)
	if err != nil {
		return err
	}
	if prof.HistoryID == 0 {
		// Every mailbox has a current history record; resuming from none
		// would run the full synchronization again each round.
		return errUnexpectedAnswer()
	}
	after := i.now().Add(-i.cfg.initialSyncWindow()).Unix()
	var ids, threads []string
	token := ""
	for {
		q := url.Values{
			"labelIds":   {labelInbox},
			"q":          {"after:" + strconv.FormatInt(after, 10)},
			"maxResults": {strconv.Itoa(i.f.opts.PageSize)},
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/messages", q)})
		if err != nil {
			return transient(err)
		}
		var page gMessageList
		if err := decodeAnswer(resp.body, &page); err != nil {
			return err
		}
		for _, m := range page.Messages {
			if m.ID != "" && !slices.Contains(ids, m.ID) {
				ids = append(ids, m.ID)
				if m.ThreadID != "" && !slices.Contains(threads, m.ThreadID) {
					threads = append(threads, m.ThreadID)
				}
			}
		}
		if page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	slices.Reverse(ids) // messages.list answers newest first
	for _, id := range ids {
		if err := i.ingest(ctx, s, sink, id, true); err != nil {
			return err
		}
	}
	// The state of the threads the synchronization brought in, which no
	// history record will report: it starts after them.
	slices.Reverse(threads)
	for _, id := range threads {
		if err := i.reportThread(ctx, s, sink, id, prof.HistoryID); err != nil {
			return err
		}
	}
	st.HistoryID = prof.HistoryID
	if gap {
		st.FullResyncs++
		st.LastFullResync = i.now().UTC()
	}
	return i.saveState(ctx, *st)
}

// ingest fetches message id and hands it to the sink. A message that can
// never be read (errUnreadable) is skipped, and observed as
// ObservationSkipped when observe is set: by the receiver round, which
// moves the cursor past it, and not by a push request, which leaves it for
// that round, so each pass of the cursor counts it once.
func (i *instance) ingest(ctx context.Context, s *session, sink adapter.Sink, id string, observe bool) error {
	in, skip, err := i.fetchIngress(ctx, s, sink, id)
	if errors.Is(err, errUnreadable) {
		if observe {
			i.observe(ctx, ObservationSkipped)
		}
		return nil
	}
	if err != nil || skip {
		return err
	}
	_, err = sink.Accept(ctx, in)
	return err
}

// skipLabels reports a message that is not inbound mail to accept: a draft,
// or a message the mailbox sent (sent to itself, it is in the inbox too).
func skipLabels(labels []string) bool {
	return slices.Contains(labels, labelDraft) || slices.Contains(labels, labelSent)
}
