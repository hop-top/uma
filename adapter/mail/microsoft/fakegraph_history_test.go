package microsoft

import (
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/conformance"
	"hop.top/uma/model"
)

var (
	reGe     = regexp.MustCompile(`^receivedDateTime ge (\S+)`)
	reLt     = regexp.MustCompile(` and receivedDateTime lt (\S+)`)
	reSender = regexp.MustCompile(` and from/emailAddress/address eq '((?:[^']|'')*)'$`)
)

// listInbox answers GET .../mailFolders/inbox/messages as Microsoft Learn
// documents List messages: $filter on receivedDateTime (and the sender),
// $orderby ascending with the $orderby property first in $filter (else
// InefficientFilter), $top pages, and @odata.nextLink carrying $skiptoken.
// Messages tie-break on ID. Only the folder's messages are listed, drafts that sit
// there included (the adapter skips them).
func (g *fakeGraph) listInbox(w http.ResponseWriter, r *http.Request, mb *fakeMailbox, folder string) {
	q := r.URL.Query()
	f := q.Get("$filter")
	if id, ok := strings.CutPrefix(f, "internetMessageId eq '"); ok {
		// A lookup by Internet message ID: the messages of the folder with it.
		id = strings.ReplaceAll(strings.TrimSuffix(id, "'"), "''", "'")
		g.mu.Lock()
		hits := []gMessage{}
		for _, mid := range mb.order {
			if fm := mb.messages[mid]; fm != nil && fm.folder == folder && fm.m.InternetMessageID == id {
				hits = append(hits, gMessage{ID: fm.m.ID})
			}
		}
		g.mu.Unlock()
		writeJSON(w, http.StatusOK, gMessageList{Value: hits})
		return
	}
	ge := reGe.FindStringSubmatch(f)
	if ge == nil || q.Get("$orderby") != "receivedDateTime asc" {
		graphError(w, http.StatusBadRequest, "InefficientFilter")
		return
	}
	lo, err := time.Parse(time.RFC3339, ge[1])
	if err != nil {
		graphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	var hi time.Time
	if lt := reLt.FindStringSubmatch(f); lt != nil {
		if hi, err = time.Parse(time.RFC3339, lt[1]); err != nil {
			graphError(w, http.StatusBadRequest, "BadRequest")
			return
		}
	}
	sender := ""
	if sm := reSender.FindStringSubmatch(f); sm != nil {
		sender = strings.ReplaceAll(sm[1], "''", "'")
	}
	top, _ := strconv.Atoi(q.Get("$top"))
	if top < 1 || top > 1000 {
		graphError(w, http.StatusBadRequest, "BadRequest")
		return
	}
	g.mu.Lock()
	var hits []gMessage
	for _, id := range mb.order {
		fm := mb.messages[id]
		if fm == nil || fm.folder != folder {
			continue
		}
		at := fm.m.ReceivedDateTime
		if at.Before(lo) || (!hi.IsZero() && !at.Before(hi)) {
			continue
		}
		if sender != "" && (fm.m.From == nil || !strings.EqualFold(fm.m.From.EmailAddress.Address, sender)) {
			continue
		}
		hits = append(hits, fm.view())
	}
	g.mu.Unlock()
	slices.SortStableFunc(hits, func(a, b gMessage) int {
		if c := a.ReceivedDateTime.Compare(b.ReceivedDateTime); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	pos, _ := strconv.Atoi(q.Get("$skiptoken"))
	pos = min(pos, len(hits))
	end := min(pos+top, len(hits))
	page := gMessageList{Value: hits[pos:end]}
	if page.Value == nil {
		page.Value = []gMessage{}
	}
	if end < len(hits) {
		nq := url.Values{}
		for k, v := range q {
			nq[k] = v
		}
		nq.Set("$skiptoken", strconv.Itoa(end))
		page.NextLink = g.srv.URL + r.URL.Path + "?" + encodeQuery(nq)
	}
	writeJSON(w, http.StatusOK, page)
}

var _ conformance.HistoryProvider = (*graphProvider)(nil)

// HoldHistory makes ev a past inbox message received at, with no delta
// change: no receiver round lists it. A message the provider already
// delivered keeps its content and takes the time.
func (p *graphProvider) HoldHistory(ch model.ChannelRef, ev conformance.InboundEvent, at time.Time) error {
	if ev.Visibility != "" && ev.Visibility != model.Public {
		return conformance.ErrNotExpressible
	}
	mb := p.box(ch)
	p.g.mu.Lock()
	defer p.g.mu.Unlock()
	if fm, ok := mb.messages[ev.MessageID]; ok {
		fm.m.ReceivedDateTime = at
		return nil
	}
	mb.order = append(mb.order, ev.MessageID)
	mb.messages[ev.MessageID] = &fakeMessage{folder: "inbox", modified: fakeStamp(), m: gMessage{
		ID: ev.MessageID, InternetMessageID: "<" + ev.MessageID + "@peer.test>", ConversationID: "conv-" + ev.Sender,
		Subject: "subject " + ev.MessageID, ReceivedDateTime: at,
		From:         &gRecipient{EmailAddress: gEmailAddress{Name: claimsName(ev.Claims), Address: ev.Sender}},
		ToRecipients: []gRecipient{{EmailAddress: gEmailAddress{Address: ev.Destination}}},
		Body:         &gItemBody{ContentType: "text", Content: ev.Text},
	}}
	mb.events[ev.MessageID] = ev.EventID
	return nil
}
