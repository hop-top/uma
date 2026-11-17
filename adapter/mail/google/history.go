package google

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
)

// History reads one page of the mailbox's past received mail, oldest first,
// that arrived at or after req.Since and before req.Until (zero is open),
// judged by Gmail's internalDate: the messages the receiver would also
// accept, labelled INBOX and neither DRAFT nor SENT (skipLabels). With
// req.Route, the address reference of the conversation's sender, only that
// sender's messages; without it the whole inbox. A read changes nothing and
// touches no receiver state.
//
// Each envelope is the one the receiver reports for the message: it is
// built by fetchIngress, the receiver's own mapping, so its message ID,
// destination and EventID agree and the engine imports a message once
// whichever way it arrived. Attachments are staged through req.Stager as the
// receiver stages them, under the same keys and bounds, so a message imported
// and then received holds its bytes once; without a stager they are described
// only.
//
// users.messages.list has no oldest-first order and its listing carries no
// dates, so the cursor holds a slice of time, never a pageToken
// (historyCursor). A call lists one slice, a day wide, of the window with
// labelIds=INBOX and a q of after: and before:, draining its pages; a slice
// whose first page does not end the listing is halved and listed again. It
// dates the listed messages (format=minimal), sorts them by (internalDate,
// ID), and fetches the first req.Limit; the remainder is carried in the
// cursor as IDs. A message listed but gone when read is absent; one Gmail
// answers whole but that can never be read (errUnreadable) is Gap.
// Throttling is returned as model.RateLimited with its RetryAfter, never
// waited out. A token under the gmail.metadata scope, which cannot use q,
// is model.Unsupported.
func (i *instance) History(ctx context.Context, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	switch {
	case req.Limit < 1:
		return adapter.HistoryPage{}, model.NewError(model.Validation, "history limit must be at least 1")
	case req.Since.IsZero():
		return adapter.HistoryPage{}, model.NewError(model.Validation, "history window start required")
	}
	from, err := i.historySender(req.Route)
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	scope := i.historyScope(req.Route, req.Since, req.Until)
	var cur historyCursor
	if req.Cursor != "" {
		if cur, err = decodeHistoryCursor(req.Cursor, scope); err != nil {
			return adapter.HistoryPage{}, err
		}
		// The bounds of the walk lie within the request's window.
		if cur.Lo < req.Since.Unix() || cur.Hi > i.historyEnd(req.Until) {
			return adapter.HistoryPage{}, model.NewError(model.Validation, "history cursor was not issued for this read")
		}
	} else {
		cur = historyCursor{Version: 1, Scope: scope, Lo: req.Since.Unix(), Hi: i.historyEnd(req.Until)}
	}
	s := i.c.session()
	w := historyWindow{since: req.Since.UnixMilli(), until: int64(1<<63 - 1)}
	if !req.Until.IsZero() {
		w.until = req.Until.UnixMilli()
	}
	var ids []string
	if len(cur.Rest) > 0 {
		// Delivery order, already dated and filtered by the call that
		// listed the slice.
		ids, cur.Rest = cur.Rest, nil
	} else {
		for n := 0; len(ids) == 0 && cur.Lo < cur.Hi && n < maxHistorySlices; n++ {
			var found []string
			if found, err = i.historySlice(ctx, s, &cur, from, w); err != nil {
				return adapter.HistoryPage{}, err
			}
			ids = found
		}
	}
	if len(ids) > req.Limit {
		ids, cur.Rest = ids[:req.Limit], slices.Clone(ids[req.Limit:])
	}
	var page adapter.HistoryPage
	for _, id := range ids {
		in, skip, err := i.fetchIngress(ctx, s, staging.HistorySink(req.Stager), id)
		if errors.Is(err, errUnreadable) {
			page.Gap = true
			continue
		}
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		// A cursor is the caller's to forge: whatever it names is judged
		// again, as read, against this request's scope.
		if skip || !historyInScope(in.Envelope, from, i.cfg.historyLabels(), req.Since, req.Until) {
			continue
		}
		page.Envelopes = append(page.Envelopes, in.Envelope)
	}
	if len(cur.Rest) > 0 || cur.Lo < cur.Hi {
		page.Next = cur.encode()
	}
	return page, nil
}

const (
	// historySliceSeconds is the width of a slice: one day.
	historySliceSeconds = 24 * 60 * 60
	// maxHistorySlices bounds the empty slices one call lists, so a window
	// far in the past does not cost a call its whole budget; the cursor
	// resumes where the call stopped.
	maxHistorySlices = 31
	// historyListMax is the page size of a slice listing, the most
	// users.messages.list returns.
	historyListMax = 500
)

// historyCursor is what a History cursor holds: the scope it was issued for
// and the state of the walk. [Lo, Hi) is the part of the window still to be
// listed, in epoch seconds (Hi is fixed on the first call: the lesser of
// Until and the time then); Rest is the IDs of the slice being delivered not
// yet delivered, in delivery order.
type historyCursor struct {
	Version int      `json:"v"`
	Scope   string   `json:"k"`
	Lo      int64    `json:"lo"`
	Hi      int64    `json:"hi"`
	Rest    []string `json:"rest,omitempty"`
}

func (c historyCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeHistoryCursor refuses, before any provider call, a cursor that is
// malformed, of another version or issued for another read.
func decodeHistoryCursor(s, scope string) (historyCursor, error) {
	bad := model.NewError(model.Validation, "history cursor was not issued for this read")
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historyCursor{}, bad
	}
	var c historyCursor
	if json.Unmarshal(b, &c) != nil || c.Version != 1 || c.Scope != scope || c.Lo < 0 || c.Hi < c.Lo || len(c.Rest) > historyListMax ||
		slices.Contains(c.Rest, "") {
		return historyCursor{}, bad
	}
	return c, nil
}

// historyScope names what a read covers, for its cursors: the sender route,
// the mailbox and label, and the window.
func (i *instance) historyScope(route *model.ExternalRef, since, until time.Time) string {
	h := sha256.New()
	if route != nil {
		h.Write([]byte(route.Namespace + "/" + route.Kind + "/" + route.ID))
	} else {
		h.Write([]byte("*"))
	}
	h.Write([]byte("#" + i.cfg.userID() + "/" + strings.Join(i.cfg.historyLabels(), ",")))
	h.Write([]byte("@" + strconv.FormatInt(since.UnixMicro(), 10)))
	if !until.IsZero() {
		h.Write([]byte("<" + strconv.FormatInt(until.UnixMicro(), 10)))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// historyEnd is the first window's upper bound in epoch seconds: Until, else
// now, rounded up to the second so the messages of the current one are in.
func (i *instance) historyEnd(until time.Time) int64 {
	t := i.now()
	if !until.IsZero() && until.Before(t) {
		t = until
	}
	return (t.UnixMilli() + 999) / 1000
}

// historySender is the identity key of the sender a route names, empty for
// the whole inbox. The route must be an address reference of this channel.
func (i *instance) historySender(route *model.ExternalRef) (string, error) {
	if route == nil {
		return "", nil
	}
	if route.Channel != i.ch.Ref || route.Namespace != IdentityNamespace || route.Kind != refKindAddress ||
		!bareAddress(route.ID) || strings.ContainsAny(route.ID, "(){}\"'\\") {
		return "", model.NewError(model.Validation, "history route must be an address of this channel")
	}
	return identityAddress(route.ID), nil
}

// historyInScope reports whether env, as read, is a message of this read's
// scope: labelled with any of labels and not DRAFT, SENT, SPAM or TRASH, received within
// [since, until) (zero until is open), and from the sender when the read is
// scoped to one.
func historyInScope(env model.Envelope, from string, labels []string, since, until time.Time) bool {
	var p messagePayload
	if json.Unmarshal(env.Payload.Data, &p) != nil || !hasAnyLabel(p.LabelIDs, labels) || skipLabels(p.LabelIDs) ||
		slices.Contains(p.LabelIDs, labelSpam) || slices.Contains(p.LabelIDs, labelTrash) {
		return false
	}
	if env.OccurredAt.UnixMilli() < since.UnixMilli() || (!until.IsZero() && env.OccurredAt.UnixMilli() >= until.UnixMilli()) {
		return false
	}
	return from == "" || env.Sender.ID == from
}

// hasAnyLabel reports whether have holds any of want.
func hasAnyLabel(have, want []string) bool {
	return slices.ContainsFunc(want, func(l string) bool { return slices.Contains(have, l) })
}

// historyWindow is the window in epoch milliseconds, [since, until).
type historyWindow struct{ since, until int64 }

func (w historyWindow) has(ms int64) bool { return ms >= w.since && ms < w.until }

// historySlice lists the next slice of cur, advances cur past it and returns
// the IDs of the messages in it that belong to the window, in delivery order:
// (internalDate, ID). A slice is a day; one whose listing does not end on its
// first page is halved. Messages Gmail lists without a date or that are gone
// are left out. The 1 s margin on both q bounds makes the listing a superset
// whichever way Gmail rounds after: and before: (the record's IMAP search
// widens by a day for the same reason); the exact bound is judged on the
// message's own internalDate.
//
// messages.list documents no order (the recorded history answers newest
// first), and the slice is sorted here without relying on one.
func (i *instance) historySlice(ctx context.Context, s *session, cur *historyCursor, from string, w historyWindow) ([]string, error) {
	lo, hi := cur.Lo, min(cur.Hi, cur.Lo+historySliceSeconds)
	// The slice is listed once per configured label and the listings are
	// united: a message under two labels is one ID, so it is read once, and
	// the slice is dated and sorted as a whole, which keeps delivery oldest
	// first across labels. A slice is halved when any label's listing does
	// not end on its first page.
	labels := i.cfg.historyLabels()
	var listed []string
	for {
		listed = listed[:0]
		more := false
		for _, label := range labels {
			ids, m, err := i.historyList(ctx, s, label, lo, hi, from, hi-lo <= 1)
			if err != nil {
				return nil, err
			}
			if more = m; more {
				break
			}
			for _, id := range ids {
				if !slices.Contains(listed, id) {
					listed = append(listed, id)
				}
			}
		}
		if !more {
			break
		}
		hi = lo + (hi-lo)/2
	}
	cur.Lo = hi
	type dated struct {
		id string
		ms int64
	}
	var found []dated
	for _, id := range listed {
		resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/messages/"+url.PathEscape(id), url.Values{"format": {"minimal"}})})
		if model.IsCode(err, model.NotFound) {
			continue // gone between the list and the get
		}
		if err != nil {
			return nil, transient(err)
		}
		m, err := decodeMessage(resp.body, id)
		if err != nil {
			return nil, err
		}
		ms := int64(m.InternalDate)
		if ms == 0 {
			return nil, errUnexpectedAnswer()
		}
		if skipLabels(m.LabelIDs) || !hasAnyLabel(m.LabelIDs, labels) || !w.has(ms) || ms < lo*1000 || ms >= hi*1000 {
			continue
		}
		found = append(found, dated{id, ms})
	}
	slices.SortFunc(found, func(a, b dated) int {
		if a.ms != b.ms {
			return int(a.ms - b.ms)
		}
		return strings.Compare(a.id, b.id)
	})
	ids := make([]string, len(found))
	for n, f := range found {
		ids[n] = f.id
	}
	return ids, nil
}

// historyList lists the messages of label in [lo, hi) with q after:/before:
// (and from:). It drains every page when all, else it reads the first and
// reports more when Gmail has another.
func (i *instance) historyList(ctx context.Context, s *session, label string, lo, hi int64, from string, all bool) (ids []string, more bool, err error) {
	search := "after:" + strconv.FormatInt(lo-1, 10) + " before:" + strconv.FormatInt(hi+1, 10)
	if from != "" {
		search += " from:" + from
	}
	token := ""
	for {
		q := url.Values{
			"labelIds":   {label},
			"q":          {search},
			"maxResults": {strconv.Itoa(i.f.opts.historyListMax())},
		}
		if token != "" {
			q.Set("pageToken", token)
		}
		resp, err := s.do(ctx, request{method: "GET", url: i.c.api("/messages", q)})
		if err != nil {
			if metadataScope(resp) {
				return nil, false, &model.Error{Code: model.Unsupported, SafeMessage: "gmail history needs a read scope beyond gmail.metadata", Retry: model.NoRetry}
			}
			return nil, false, transient(err)
		}
		var page gMessageList
		if err := decodeAnswer(resp.body, &page); err != nil {
			return nil, false, err
		}
		for _, m := range page.Messages {
			if m.ID != "" && !slices.Contains(ids, m.ID) {
				ids = append(ids, m.ID)
			}
		}
		switch {
		case page.NextPageToken == "":
			return ids, false, nil
		case !all && token == "":
			return nil, true, nil
		}
		token = page.NextPageToken
	}
}

// metadataScope reports a 403 that names the gmail.metadata scope: it cannot
// use the q parameter or the raw format, whatever the mailbox holds.
func metadataScope(r response) bool {
	if r.status != http.StatusForbidden {
		return false
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal(r.body, &e) == nil && strings.Contains(strings.ToLower(e.Error.Message), "metadata scope")
}

var _ adapter.HistoryReader = (*instance)(nil)
