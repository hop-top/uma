package imap

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

	"github.com/emersion/go-imap/v2"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
)

// historyVersion is the cursor format version.
const historyVersion = 1

// historyCursor is what a History cursor holds: the scope it was issued for
// and the position: the mailbox, its UIDVALIDITY, the last UID handled
// (delivered or skipped) and the INTERNALDATE of the last message delivered,
// in microseconds since the epoch.
type historyCursor struct {
	Version     int    `json:"v"`
	Scope       string `json:"k"`
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	LastUID     uint32 `json:"last_uid"`
	LastDate    int64  `json:"last_date,omitempty"`
}

func (c historyCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func errForeignCursor() error {
	return &model.Error{Code: model.Validation, SafeMessage: PurposeIMAP + ": history cursor was not issued for this read", Retry: model.NoRetry}
}

// decodeHistoryCursor parses a cursor issued for scope. Its mailbox must be
// one of mailboxes; a cursor with no UIDVALIDITY is the start of its
// mailbox, so it holds no position.
func decodeHistoryCursor(s, scope string, mailboxes []string) (historyCursor, error) {
	bad := errForeignCursor()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historyCursor{}, bad
	}
	var c historyCursor
	if json.Unmarshal(b, &c) != nil || c.Version != historyVersion || c.Scope != scope || !slices.Contains(mailboxes, c.Mailbox) ||
		(c.UIDValidity == 0 && (c.LastUID != 0 || c.LastDate != 0)) {
		return historyCursor{}, bad
	}
	return c, nil
}

// historyScope names what a read covers, for its cursors: the mailbox, the
// sender of a conversation-scoped read and the window.
func historyScope(mailboxes []string, listed bool, sender string, since, until time.Time) string {
	h := sha256.New()
	if listed {
		h.Write([]byte("L" + strconv.Itoa(len(mailboxes))))
	}
	for _, mailbox := range mailboxes {
		h.Write([]byte(strconv.Itoa(len(mailbox)) + ":" + mailbox))
	}
	h.Write([]byte("|" + sender))
	h.Write([]byte("@" + strconv.FormatInt(since.UnixMicro(), 10)))
	if !until.IsZero() {
		h.Write([]byte("<" + strconv.FormatInt(until.UnixMicro(), 10)))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// History reads one page of past mail from Config.Mailbox, or the mailboxes
// of Config.HistoryMailboxes one after the other in the listed order when it
// lists any (never the sent mailbox), received at or after req.Since and before req.Until (zero is
// open) by INTERNALDATE, in UID order, which is arrival order. Each envelope
// is the one the receiver delivers for the same message, built by the same
// mapping: the same Message-ID, destination and event ID, so the sink stores
// a message once whichever path brought it first.
//
// With req.Route, an address route of this channel, only mail from that
// sender (SEARCH FROM, which matches substrings, so the sender is checked
// again on the parsed message).
//
// The cursor holds {mailbox, uid_validity, last_uid, last_date} and a hash of
// the read's scope, base64url JSON; one issued for another read is
// model.Validation before any connection. The read is read-only against
// the mailbox (BODY.PEEK, read-only SELECT) and leaves the receiver's cursor
// and ledger alone; it records the messages it returns in the import ledger
// (state_import.go), which the receiver adopts, so their flags, moves and
// deletion are reported as for delivered mail. Attachments are staged through
// req.Stager as the receiver stages them (same bounds, same keys, so a message
// imported and then received holds its bytes once); without a stager they are
// described only. It opens its own session and closes it before
// returning.
//
// A message that cannot be delivered (malformed, no usable From) is a Gap, and
// so is a UIDVALIDITY change since the cursor: the window then restarts at
// the cursor's last_date under the new value, as the receiver resynchronizes.
// A mailbox that cannot be selected is model.Unavailable.
func (i *Instance) History(ctx context.Context, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	if i.closed.Load() {
		return adapter.HistoryPage{}, i.errClosed()
	}
	if i.deps.Credentials == nil {
		return adapter.HistoryPage{}, i.noCredentials(PurposeIMAP)
	}
	switch {
	case req.Limit < 1:
		return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: PurposeIMAP + ": history limit must be at least 1", Retry: model.NoRetry}
	case req.Since.IsZero():
		return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: PurposeIMAP + ": history window start required", Retry: model.NoRetry}
	}
	sender := ""
	if req.Route != nil {
		var err error
		if sender, err = i.routeAddress(*req.Route); err != nil {
			return adapter.HistoryPage{}, err
		}
	}
	mboxes := i.cfg.historyMailboxes()
	scope := historyScope(mboxes, len(i.cfg.HistoryMailboxes) > 0, sender, req.Since, req.Until)
	cur := historyCursor{Version: historyVersion, Scope: scope, Mailbox: mboxes[0]}
	if req.Cursor != "" {
		var err error
		if cur, err = decodeHistoryCursor(req.Cursor, scope, mboxes); err != nil {
			return adapter.HistoryPage{}, err
		}
		// The cursor is the caller's to forge: the scope hash is computable
		// from values the caller knows. Its mailbox was checked against the
		// configuration above; its date must lie in the window, so a restart
		// at it cannot read outside.
		if cur.LastDate != 0 {
			d := time.UnixMicro(cur.LastDate)
			if d.Before(req.Since) || !req.Until.IsZero() && !d.Before(req.Until) {
				return adapter.HistoryPage{}, errForeignCursor()
			}
		}
	}
	s, err := i.connect(ctx)
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	defer s.close()
	page := adapter.HistoryPage{}
	// The walk is mailbox by mailbox, in the configured order: one is read
	// to its end before the next, and a call that ends a mailbox short of
	// Limit goes on into the next. Delivery is oldest first within a
	// mailbox, not across them.
	for idx := slices.Index(mboxes, cur.Mailbox); ; {
		mbox := mboxes[idx]
		sel, err := i.selectReadOnly(ctx, s, mbox)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		if len(i.cfg.HistoryMailboxes) > 0 {
			if err := i.checkHistoryMailbox(ctx, s, mbox); err != nil {
				return adapter.HistoryPage{}, err
			}
		}
		since := req.Since
		switch {
		case cur.UIDValidity == 0:
			cur = historyCursor{Version: historyVersion, Scope: scope, Mailbox: mbox, UIDValidity: sel.UIDValidity}
		case cur.UIDValidity != sel.UIDValidity:
			// UIDs of the earlier value are void: restart at the last message
			// delivered, under the new value.
			page.Gap = true
			cur = historyCursor{Version: historyVersion, Scope: scope, Mailbox: mbox, UIDValidity: sel.UIDValidity, LastDate: cur.LastDate}
			if d := time.UnixMicro(cur.LastDate); cur.LastDate != 0 && d.After(since) {
				since = d
			}
		}
		imported := map[uint32]entry{}
		uids, err := i.historyUIDs(ctx, s, cur.LastUID, since, req.Until, sender)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		for len(uids) > 0 && len(page.Envelopes) < req.Limit {
			batch := uids[:min(fetchBatch, len(uids))]
			meta, err := i.historyMeta(ctx, s, batch)
			if err != nil {
				return adapter.HistoryPage{}, err
			}
			n := 0
			for _, uid := range batch {
				if len(page.Envelopes) == req.Limit {
					break
				}
				n++
				cur.LastUID = uid
				m, ok := meta[uid]
				if !ok || m.internalDate.Before(since) || !req.Until.IsZero() && !m.internalDate.Before(req.Until) {
					// Expunged since the search, or outside the window the date
					// search only approximates.
					continue
				}
				f, err := i.fetchMessage(ctx, s, m, cur.UIDValidity)
				if err != nil {
					return adapter.HistoryPage{}, err
				}
				if len(f.raw) == 0 {
					page.Gap = page.Gap || m.size == 0
					continue
				}
				in, ok, err := i.ingress(ctx, staging.HistorySink(req.Stager), f)
				if err != nil {
					return adapter.HistoryPage{}, err
				}
				if !ok {
					page.Gap = true
					continue
				}
				if sender != "" && in.Envelope.Sender.ID != sender {
					continue
				}
				cur.LastDate = m.internalDate.UnixMicro()
				page.Envelopes = append(page.Envelopes, in.Envelope)
				if mid := in.Envelope.MessageID; mid != "" {
					imported[uid] = entry{MessageID: mid, Seen: m.flags.has(imap.FlagSeen), Flagged: m.flags.has(imap.FlagFlagged)}
				}
			}
			uids = uids[n:]
		}
		// Recorded under the mailbox the messages came from, with its
		// UIDVALIDITY, before the walk moves on.
		if err := i.followImported(ctx, mbox, cur.UIDValidity, imported); err != nil {
			return adapter.HistoryPage{}, err
		}
		switch {
		case len(uids) > 0:
			page.Next = cur.encode()
			return page, nil
		case idx+1 == len(mboxes):
			return page, nil
		}
		idx++
		cur = historyCursor{Version: historyVersion, Scope: scope, Mailbox: mboxes[idx]}
		if len(page.Envelopes) >= req.Limit {
			page.Next = cur.encode()
			return page, nil
		}
	}
}

// checkHistoryMailbox refuses a listed mailbox the server marks as sent,
// drafts, junk or trash (RFC 6154), which no name check can tell. A server
// without SPECIAL-USE reports none.
func (i *Instance) checkHistoryMailbox(ctx context.Context, s *session, mbox string) error {
	caps := s.c.Caps()
	if !caps.Has(imap.CapSpecialUse) && !caps.Has(imap.CapIMAP4rev2) {
		return nil
	}
	var list []*imap.ListData
	if err := s.within(func() error {
		var err error
		list, err = s.c.List("", mbox, &imap.ListOptions{ReturnSpecialUse: true}).Collect()
		return err
	}); err != nil {
		return lost(ctx, err)
	}
	for _, d := range list {
		for _, a := range d.Attrs {
			for _, r := range []imap.MailboxAttr{imap.MailboxAttrSent, imap.MailboxAttrDrafts, imap.MailboxAttrJunk, imap.MailboxAttrTrash} {
				if strings.EqualFold(string(a), string(r)) {
					return &model.Error{Code: model.Validation, SafeMessage: PurposeIMAP + ": history_mailboxes names a " + string(r) + " mailbox, which history never reads", Retry: model.NoRetry}
				}
			}
		}
	}
	return nil
}

// selectReadOnly examines mbox, as the receiver does: NO or BAD is a mailbox
// that cannot be examined; a server without UIDVALIDITY is unsupported.
func (i *Instance) selectReadOnly(ctx context.Context, s *session, mbox string) (*imap.SelectData, error) {
	var sel *imap.SelectData
	if err := s.within(func() error {
		var err error
		sel, err = s.c.Select(mbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		return err
	}); err != nil {
		var ie *imap.Error
		if errors.As(err, &ie) && ctx.Err() == nil {
			return nil, model.NewError(model.Unavailable, PurposeIMAP+": mailbox cannot be examined")
		}
		return nil, lost(ctx, err)
	}
	if sel.UIDValidity == 0 {
		return nil, &model.Error{Code: model.Unsupported, SafeMessage: PurposeIMAP + ": server reports no UIDVALIDITY", Retry: model.NoRetry}
	}
	return sel, nil
}

// historyUIDs is the UIDs above last the window may hold, ascending. SEARCH
// SINCE and BEFORE compare dates only, in a time zone the server picks: the
// bounds are widened by a day and the caller refines by INTERNALDATE.
func (i *Instance) historyUIDs(ctx context.Context, s *session, last uint32, since, until time.Time, sender string) ([]uint32, error) {
	var set imap.UIDSet
	set.AddRange(imap.UID(last+1), 0)
	c := &imap.SearchCriteria{UID: []imap.UIDSet{set}, Since: since.Add(-24 * time.Hour)}
	if !until.IsZero() {
		c.Before = until.Add(24 * time.Hour)
	}
	if sender != "" {
		c.Header = []imap.SearchCriteriaHeaderField{{Key: "From", Value: sender}}
	}
	var found *imap.SearchData
	if err := s.within(func() error {
		var err error
		found, err = s.c.UIDSearch(c, nil).Wait()
		return err
	}); err != nil {
		return nil, lost(ctx, err)
	}
	var uids []uint32
	for _, u := range found.AllUIDs() {
		// "n:*" always matches the highest UID, even below n.
		if uint32(u) > last {
			uids = append(uids, uint32(u))
		}
	}
	slices.Sort(uids)
	return uids, nil
}

// historyMeta is the metadata of uids by UID; a UID the server no longer holds
// is absent.
func (i *Instance) historyMeta(ctx context.Context, s *session, uids []uint32) (map[uint32]*fetchMeta, error) {
	var meta []*fetchMeta
	if err := s.within(func() error {
		var err error
		meta, err = fetchMetadata(s, imap.UIDSetNum(uidsOf(uids)...))
		return err
	}); err != nil {
		return nil, lost(ctx, err)
	}
	out := make(map[uint32]*fetchMeta, len(meta))
	for _, m := range meta {
		out[m.uid] = m
	}
	return out, nil
}
