package imap

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// Receiver timing and bounds.
const (
	// maxConnectFailures consecutive failed connections end Run with the
	// last failure; the engine's receiver supervision takes over.
	maxConnectFailures = 5
	maxBackoff         = time.Minute
	fetchBatch         = 50
	sinkTimeout        = 2 * time.Minute
)

// sinkError is a sink refusal: it ends Run with the sink's error.
type sinkError struct{ err error }

func (e *sinkError) Error() string { return e.err.Error() }
func (e *sinkError) Unwrap() error { return e.err }

// Run is the channel's receiver. It keeps one authenticated session on the
// mailbox and, each round, examines the mailbox (read-only: flags never
// change) and delivers every message whose UID is above the cursor, in UID
// order, advancing the cursor after each accepted message. Between rounds
// it waits in IDLE, re-issued at most every Config.IdleRestartMS, or, on a
// server without IDLE, for Config.PollIntervalMS.
//
// The session is replaced before the credential expires (package auth
// reconnect margin): IMAP cannot re-authenticate a session. A lost
// connection reconnects with backoff; maxConnectFailures consecutive failed
// connections end Run with the last failure, as do a non-retryable failure
// (rejected credential, unsupported server) and a sink error, so the
// message is fetched again by the next receiver.
//
// The instance reports its link to the host (adapter.ReceiverReporter):
// connected while an authenticated session stands, not connected when it
// ends, for a planned reconnect too, and while a connection is retried.
//
// A message expunged before its body is fetched is skipped and reported as
// ObservationVanished once a search by UID confirms it is gone; a message that
// is there and cannot be delivered is skipped and reported as
// ObservationSkipped. The cursor stays before a message that failed for a
// reason a retry can fix.
//
// A UIDVALIDITY change voids every stored UID: Run reports ObservationGap,
// counts the gap in its state and resynchronizes every message received
// since the last delivered one (or since the initial sync window when none
// was delivered). Event IDs do not depend on UIDs, so ingress drops the
// messages it already holds.
func (i *Instance) Run(ctx context.Context, sink adapter.Sink) error {
	if i.closed.Load() {
		return i.errClosed()
	}
	if i.deps.Credentials == nil {
		return i.noCredentials(PurposeIMAP)
	}
	failures, early := 0, 0
	for {
		s, err := i.connect(ctx)
		if err == nil {
			due := !s.auth.ReconnectAt.IsZero() && !i.now().Before(s.auth.ReconnectAt)
			var rounds int
			i.link(true)
			rounds, err = i.serve(ctx, s, sink)
			i.link(false)
			s.close()
			if rounds > 0 {
				failures = 0
			}
			if err == nil {
				// A planned reconnect before credential expiry. A credential
				// that is already due at connect time reconnects with backoff,
				// so a host handing out short-lived tokens is not hammered.
				if !due {
					early = 0
					continue
				}
				early++
				if err := i.pause(ctx, early); err != nil {
					return err
				}
				continue
			}
		}
		var se *sinkError
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.As(err, &se):
			return se.err
		case !retryable(err):
			return err
		}
		failures++
		if failures >= maxConnectFailures {
			return err
		}
		if err := i.pause(ctx, failures); err != nil {
			return err
		}
	}
}

// jitterFraction is the spread of the poll interval and of the IDLE
// restart: +-10%.
const jitterFraction = 0.1

// spread is d +-10% (adapter.Spread) from the factory's jitter source.
func (i *Instance) spread(d time.Duration) time.Duration {
	return adapter.Spread(d, jitterFraction, i.f.opts.Jitter)
}

// pause waits the backoff of the nth consecutive failure.
func (i *Instance) pause(ctx context.Context, n int) error {
	d := i.f.opts.ReconnectBackoff << min(n-1, 16)
	return i.waiter().Wait(ctx, min(d, maxBackoff))
}

// after starts a wait of d on the host waiter; fired closes when it
// elapses. stop ends the wait and returns once the waiter has returned, so
// no wait outlives the one the receiver is in.
func (i *Instance) after(ctx context.Context, d time.Duration) (fired <-chan struct{}, stop func()) {
	wctx, cancel := context.WithCancel(ctx)
	ch, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		if i.waiter().Wait(wctx, d) == nil {
			close(ch)
		}
	}()
	return ch, func() { cancel(); <-done }
}

func retryable(err error) bool {
	var me *model.Error
	return errors.As(err, &me) && me.Retry == model.RetrySameOperation
}

// lost maps a failed exchange on an open session: context errors stay,
// model errors keep their code, anything else is a lost connection.
func lost(ctx context.Context, err error) error {
	var me *model.Error
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &me):
		return err
	}
	return model.NewError(model.Unavailable, PurposeIMAP+": connection lost")
}

// serve runs rounds on one session until it must be replaced (nil error) or
// fails. It returns the number of completed rounds.
func (i *Instance) serve(ctx context.Context, s *session, sink adapter.Sink) (int, error) {
	idle := s.c.Caps().Has(imap.CapIdle) || s.c.Caps().Has(imap.CapIMAP4rev2)
	for rounds := 0; ; rounds++ {
		if err := i.round(ctx, s, sink); err != nil {
			return rounds, err
		}
		reconnect, err := i.wait(ctx, s, idle)
		if err != nil {
			return rounds + 1, err
		}
		if reconnect {
			return rounds + 1, nil
		}
	}
}

// wait blocks until the next round is due: new mailbox data during IDLE,
// the IDLE restart or poll interval, or the session's reconnect time, in
// which case it reports reconnect.
func (i *Instance) wait(ctx context.Context, s *session, idle bool) (reconnect bool, err error) {
	d := i.spread(i.cfg.pollInterval())
	if idle {
		d = min(i.spread(i.cfg.idleRestart()), MaxIdleRestart)
	}
	at := s.auth.ReconnectAt
	expiring := false
	if !at.IsZero() {
		left := at.Sub(i.now())
		if left <= 0 {
			return true, nil
		}
		if left <= d {
			d, expiring = left, true
		}
	}
	due := func() bool { return expiring || !at.IsZero() && !i.now().Before(at) }
	if !idle {
		fired, stop := i.after(ctx, d)
		defer stop()
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-s.c.Closed():
			return false, lost(ctx, errors.New("closed"))
		case <-fired:
			return due(), nil
		}
	}
	// A signal pending from the round is kept: it may report mail that
	// arrived after the round searched, so it costs at most one extra round.
	var cmd interface {
		Close() error
		Wait() error
	}
	if err := s.within(func() error {
		c, err := s.c.Idle()
		if err == nil {
			cmd = c
		}
		return err
	}); err != nil {
		return false, lost(ctx, err)
	}
	fired, stop := i.after(ctx, d)
	timedOut := false
	select {
	case <-ctx.Done():
	case <-s.c.Closed():
	case <-fired:
		timedOut = true
	case <-s.updates:
	}
	stop()
	err = s.within(func() error {
		if err := cmd.Close(); err != nil {
			return err
		}
		return cmd.Wait()
	})
	switch {
	case ctx.Err() != nil:
		return false, ctx.Err()
	case err != nil:
		return false, lost(ctx, err)
	}
	return timedOut && due(), nil
}

// round examines the mailbox, handles a UIDVALIDITY change and delivers the
// messages above the cursor.
func (i *Instance) round(ctx context.Context, s *session, sink adapter.Sink) error {
	cur, err := i.loadCursor(ctx)
	if err != nil {
		return err
	}
	mbox := i.cfg.mailbox()
	var sel *imap.SelectData
	if err := s.within(func() error {
		var err error
		sel, err = s.c.Select(mbox, &imap.SelectOptions{ReadOnly: true, CondStore: s.c.Caps().Has(imap.CapCondStore)}).Wait()
		return err
	}); err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) && ctx.Err() == nil {
			// NO or BAD: the mailbox is missing or inaccessible, possibly
			// while it is being recreated.
			return model.NewError(model.Unavailable, PurposeIMAP+": mailbox cannot be examined")
		}
		return lost(ctx, err)
	}
	if sel.UIDValidity == 0 {
		return &model.Error{Code: model.Unsupported, SafeMessage: PurposeIMAP + ": server reports no UIDVALIDITY", Retry: model.NoRetry}
	}
	now := i.now()
	switch {
	case cur.UIDValidity == 0 || cur.Mailbox != mbox:
		cur = cursor{Mailbox: mbox, UIDValidity: sel.UIDValidity, Since: now.Add(-i.cfg.initialSyncWindow())}
		if err := i.saveCursor(ctx, cur); err != nil {
			return err
		}
	case cur.UIDValidity != sel.UIDValidity:
		since := cur.Since
		if cur.LastDate.After(since) {
			since = cur.LastDate
		}
		if since.IsZero() {
			since = now.Add(-i.cfg.initialSyncWindow())
		}
		cur = cursor{
			Mailbox: mbox, UIDValidity: sel.UIDValidity, LastDate: cur.LastDate, Since: since,
			Gaps: cur.Gaps + 1, GapAt: now,
		}
		if err := i.saveCursor(ctx, cur); err != nil {
			return err
		}
		i.observe(ctx, ObservationGap)
	}
	led, err := i.loadLedger(ctx)
	if err != nil {
		return err
	}
	if err := i.adoptImported(ctx, led); err != nil {
		return err
	}
	if !cur.Since.IsZero() || sel.UIDNext == 0 || uint32(sel.UIDNext) > cur.LastUID+1 {
		// Something above the cursor.
		if err := i.deliverNew(ctx, s, sink, &cur, led, sel.UIDValidity, uint32(sel.UIDNext)); err != nil {
			return err
		}
	}
	return i.syncState(ctx, s, sink, led, sel, cur.LastUID)
}

// deliverNew delivers the messages above the cursor in UID order. During a
// catch-up pass (cur.Since set) messages received before Since are skipped,
// and once the pass completes the cursor moves to uidNext-1.
func (i *Instance) deliverNew(ctx context.Context, s *session, sink adapter.Sink, cur *cursor, led *ledger, uidValidity, uidNext uint32) error {
	var set imap.UIDSet
	set.AddRange(imap.UID(cur.LastUID+1), 0)
	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{set}}
	if !cur.Since.IsZero() {
		// SEARCH SINCE compares dates only, in a time zone the server picks:
		// a day earlier, refined by INTERNALDATE below.
		criteria.Since = cur.Since.Add(-24 * time.Hour)
	}
	var found *imap.SearchData
	if err := s.within(func() error {
		var err error
		found, err = s.c.UIDSearch(criteria, nil).Wait()
		return err
	}); err != nil {
		return lost(ctx, err)
	}
	var uids []uint32
	for _, u := range found.AllUIDs() {
		// "n:*" always matches the highest UID, even below n.
		if uint32(u) > cur.LastUID {
			uids = append(uids, uint32(u))
		}
	}
	slices.Sort(uids)
	for len(uids) > 0 {
		batch := uids[:min(fetchBatch, len(uids))]
		uids = uids[len(batch):]
		if err := i.deliverBatch(ctx, s, sink, cur, led, uidValidity, batch); err != nil {
			return err
		}
	}
	if !cur.Since.IsZero() {
		if uidNext > 0 && uidNext-1 > cur.LastUID {
			cur.LastUID = uidNext - 1
		}
		cur.Since = time.Time{}
		return i.saveCursor(ctx, *cur)
	}
	return nil
}

// deliverBatch fetches and delivers one batch of UIDs, ascending.
func (i *Instance) deliverBatch(ctx context.Context, s *session, sink adapter.Sink, cur *cursor, led *ledger, uidValidity uint32, batch []uint32) error {
	set := imap.UIDSetNum(uidsOf(batch)...)
	var meta []*fetchMeta
	if err := s.within(func() error {
		var err error
		meta, err = fetchMetadata(s, set)
		return err
	}); err != nil {
		return lost(ctx, err)
	}
	byUID := map[uint32]*fetchMeta{}
	for _, m := range meta {
		byUID[m.uid] = m
	}
	for _, uid := range batch {
		m, ok := byUID[uid]
		switch {
		case !ok:
			// Found by the search, absent from the metadata answer.
			gone, err := i.confirmVanished(ctx, s, uid)
			if err != nil {
				return err
			}
			if !gone {
				return errNoAnswer()
			}
		case cur.Since.IsZero() || !m.internalDate.Before(cur.Since):
			res, err := i.receiveMessage(ctx, s, sink, m, cur.UIDValidity)
			if err != nil {
				return err
			}
			if res.ok {
				cur.LastDate = m.internalDate
				if err := i.follow(ctx, sink, led, cur.Since.IsZero() && res.duplicate, uidValidity, m, res.messageID); err != nil {
					return err
				}
			}
		}
		// Past here the message is delivered, gone, undeliverable (reported)
		// or outside the catch-up window; every other outcome returned above
		// with the cursor where it was.
		cur.LastUID = uid
		if err := i.saveLedger(ctx, led); err != nil {
			return err
		}
		if err := i.saveCursor(ctx, *cur); err != nil {
			return err
		}
	}
	return nil
}

// fetchAttempts bounds the fetches of one message's body in a round when the
// server answers with nothing for a UID the mailbox still holds.
const fetchAttempts = 3

// receiveMessage fetches and delivers one message. It reports false when the
// message was skipped: gone from the mailbox, or fetched but undeliverable.
// Both are reported to the observer. An error leaves the message for the next
// round: the connection failed, the server answered nothing for a message it
// still holds, or the sink refused it.
func (i *Instance) receiveMessage(ctx context.Context, s *session, sink adapter.Sink, m *fetchMeta, uidValidity uint32) (delivery, error) {
	for range fetchAttempts {
		f, err := i.fetchMessage(ctx, s, m, uidValidity)
		if err != nil {
			return delivery{}, err
		}
		if len(f.raw) > 0 {
			res, err := i.deliver(ctx, sink, f)
			if err == nil && !res.ok {
				// A mailbox does not change the bytes of a message it holds:
				// fetching these again parses the same way.
				i.observe(ctx, ObservationSkipped)
			}
			return res, err
		}
		// No bytes for a UID the metadata fetch returned: expunged since, a
		// message of no bytes, or a server that cannot read the message now.
		gone, err := i.confirmVanished(ctx, s, m.uid)
		if err != nil || gone {
			return delivery{}, err
		}
		if m.size == 0 {
			i.observe(ctx, ObservationSkipped)
			return delivery{}, nil
		}
	}
	return delivery{}, errNoAnswer()
}

// errNoAnswer is the failure of a message the server holds but answers
// nothing for: the next round asks again.
func errNoAnswer() error {
	return model.NewError(model.Unavailable, PurposeIMAP+": message cannot be fetched")
}

// confirmVanished reports whether uid is gone from the mailbox, and observes
// the message vanished when it is. A server answers a FETCH of an expunged UID
// with OK and no data (RFC 3501 section 6.4.8, RFC 9051 section 6.4.9) and may
// send an EXPUNGE during any UID command (RFC 3501 section 7.4.1, RFC 9051
// section 7.5.1); that carries a sequence number, not a UID, and an empty
// answer alone cannot be told from a server that fails to read a message it
// holds, so a UID SEARCH decides.
func (i *Instance) confirmVanished(ctx context.Context, s *session, uid uint32) (bool, error) {
	var found *imap.SearchData
	if err := s.within(func() error {
		var err error
		found, err = s.c.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{imap.UIDSetNum(imap.UID(uid))}}, nil).Wait()
		return err
	}); err != nil {
		return false, lost(ctx, err)
	}
	if slices.Contains(found.AllUIDs(), imap.UID(uid)) {
		return false, nil
	}
	i.observe(ctx, ObservationVanished)
	return true, nil
}

// delivery is what became of one message the receiver handed to the sink:
// ok when it was delivered or already held (duplicate), with its Message-ID
// as the sink knows it.
type delivery struct {
	ok, duplicate bool
	messageID     string
}

// deliver stages and hands one message to the sink. It reports not ok for a
// message that cannot be delivered. A sink or staging failure ends Run, so
// the cursor stays and the next receiver fetches the message again.
func (i *Instance) deliver(ctx context.Context, sink adapter.Sink, f fetched) (delivery, error) {
	actx, cancel := context.WithTimeout(ctx, sinkTimeout)
	defer cancel()
	in, ok, err := i.ingress(actx, sink, f)
	var rec model.IngressReceipt
	if err == nil && ok {
		rec, err = sink.Accept(actx, in)
	}
	switch {
	case err == nil:
		return delivery{ok: ok, duplicate: rec.Duplicate, messageID: in.Envelope.MessageID}, nil
	case ctx.Err() != nil:
		return delivery{}, ctx.Err()
	}
	return delivery{}, &sinkError{err: err}
}

// follow starts following a delivered message: its mailbox, UID and flags.
// A message already held that arrives again under a new UID, a copy moved
// back into the mailbox, is reported when announce is set.
func (i *Instance) follow(ctx context.Context, sink adapter.Sink, led *ledger, announce bool, uidValidity uint32, m *fetchMeta, messageID string) error {
	if messageID == "" {
		return nil
	}
	e := entry{MessageID: messageID, Seen: m.flags.has(imap.FlagSeen), Flagged: m.flags.has(imap.FlagFlagged)}
	led.box(i.cfg.mailbox(), uidValidity).set(m.uid, e)
	led.dirty = true
	if !announce {
		return nil
	}
	si, err := i.serverInfo(ctx, nil)
	if err != nil {
		return nil // the report is a courtesy; the state is followed all the same
	}
	return i.reportState(ctx, sink, si, i.cfg.mailbox(), e)
}

type fetchMeta struct {
	uid          uint32
	internalDate time.Time
	size         int64
	flags        flagSet
}

// flagSet is the flags of a message.
type flagSet []imap.Flag

func (f flagSet) has(x imap.Flag) bool { return slices.Contains(f, x) }

func fetchMetadata(s *session, set imap.UIDSet) ([]*fetchMeta, error) {
	msgs, err := s.c.Fetch(set, &imap.FetchOptions{UID: true, InternalDate: true, RFC822Size: true, Flags: true}).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]*fetchMeta, 0, len(msgs))
	for _, m := range msgs {
		// Data the server volunteers because another session changed the
		// mailbox (the flags of a message it just marked \Deleted) reaches
		// the command in progress as an entry without the items asked for.
		// It is not the answer and must not stand for it.
		if m.InternalDate.IsZero() {
			continue
		}
		out = append(out, &fetchMeta{uid: uint32(m.UID), internalDate: m.InternalDate, size: m.RFC822Size, flags: m.Flags})
	}
	return out, nil
}

// fetchMessage fetches the message bytes without setting \Seen, or the
// header alone when the message exceeds the size bound.
func (i *Instance) fetchMessage(ctx context.Context, s *session, m *fetchMeta, uidValidity uint32) (fetched, error) {
	f := fetched{uid: m.uid, uidValidity: uidValidity, internalDate: m.internalDate, size: m.size}
	section := &imap.FetchItemBodySection{Peek: true}
	if m.size > i.cfg.maxMessageBytes() {
		section.Specifier, f.headerOnly = imap.PartSpecifierHeader, true
	}
	if err := s.within(func() error {
		msgs, err := s.c.Fetch(imap.UIDSetNum(imap.UID(m.uid)), &imap.FetchOptions{
			UID: true, BodySection: []*imap.FetchItemBodySection{section},
		}).Collect()
		if err != nil {
			return err
		}
		for _, msg := range msgs {
			if uint32(msg.UID) == m.uid {
				f.raw = msg.FindBodySection(section)
			}
		}
		return nil
	}); err != nil {
		return fetched{}, lost(ctx, err)
	}
	return f, nil
}

func uidsOf(ns []uint32) []imap.UID {
	out := make([]imap.UID, len(ns))
	for i, n := range ns {
		out[i] = imap.UID(n)
	}
	return out
}
