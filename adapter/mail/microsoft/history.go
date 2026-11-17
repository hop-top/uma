package microsoft

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/adapter/staging"
	"hop.top/uma/model"
)

// historyVersion is the version of the History cursor.
const historyVersion = 1

// maxHistoryTop is the largest $top Graph accepts on a message list.
const maxHistoryTop = 1000

// historyCursor is what a History cursor holds: the scope it was issued
// for and the paging state of the list. The request URL is never stored or
// followed from a cursor: it is rebuilt from the request each time, so a
// cursor cannot name another path, folder or mailbox. Param and Token are
// the paging parameter of the last @odata.nextLink ($skiptoken or $skip)
// and its value, empty for the first page; Top is the $top the read was
// opened with, kept so the token pages the same list; Skip is the number of
// that page's messages already returned (a later, smaller Limit takes a
// page in parts, so the page is read again and its head dropped). Mailbox
// is the configured folder being walked, set only when Config.HistoryMailboxes
// lists any; it must be one of them.
type historyCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"k"`
	Mailbox string `json:"m,omitempty"`
	Param   string `json:"p,omitempty"`
	Token   string `json:"t,omitempty"`
	Top     int    `json:"s"`
	Skip    int    `json:"n,omitempty"`
}

func (c historyCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// maxPagingToken bounds a paging token a cursor carries.
const maxPagingToken = 4096

// pagingParams are the query parameters Graph pages a list with.
var pagingParams = []string{"$skiptoken", "$skip"}

// decodeHistoryCursor parses a cursor issued for scope. A cursor that is
// malformed, of another version, for another read or with an unknown paging
// parameter is model.Validation, before any Graph call.
func (i *instance) decodeHistoryCursor(s, scope string) (historyCursor, error) {
	folders := i.cfg.historyFolders()
	bad := &model.Error{Code: model.Validation, SafeMessage: "history cursor was not issued for this read", Retry: model.NoRetry}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return historyCursor{}, bad
	}
	var c historyCursor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.More() || c.Version != historyVersion || c.Scope != scope || c.Skip < 0 ||
		c.Top < 1 || c.Top > maxHistoryTop || len(c.Token) > maxPagingToken ||
		(c.Token == "") != (c.Param == "") || (c.Token == "" && c.Skip == 0 && (c.Mailbox == "" || c.Mailbox == folders[0])) ||
		(len(i.cfg.HistoryMailboxes) == 0) != (c.Mailbox == "") || (c.Mailbox != "" && !slices.Contains(folders, c.Mailbox)) ||
		(c.Param != "" && !slices.Contains(pagingParams, c.Param)) {
		return historyCursor{}, bad
	}
	return c, nil
}

// pagingToken checks that link, an @odata.nextLink, is the next page of the
// list req reads (the same endpoint path, $filter, $orderby and $select; at
// most $top and the paging parameter differ) and returns the paging
// parameter and value. Anything else is not followed.
func (i *instance) pagingToken(link string, req adapter.HistoryRequest, folder string, top int) (param, token string, ok bool) {
	got, err := url.Parse(link)
	want, werr := url.Parse(i.historyURL(req, folder, top, "", ""))
	if err != nil || werr != nil || !i.c.sameOrigin(link) || graphPath(got.Path) != graphPath(want.Path) || got.Fragment != "" {
		return "", "", false
	}
	gq, wq := got.Query(), want.Query()
	for k, v := range gq {
		switch {
		case k == "$top":
		case slices.Contains(pagingParams, k):
			if param != "" || len(v) != 1 || v[0] == "" || len(v[0]) > maxPagingToken {
				return "", "", false
			}
			param, token = k, v[0]
		case !slices.Equal(v, wq[k]):
			return "", "", false
		}
	}
	for k := range wq {
		if _, in := gq[k]; !in && k != "$top" {
			return "", "", false
		}
	}
	return param, token, param != ""
}

// graphKeySegment is a path segment addressing an entity by key,
// segment('value'), the form Graph writes in its own nextLink.
var graphKeySegment = regexp.MustCompile(`([A-Za-z]+)\('([^'/]*)'\)`)

// graphPath is p in one spelling: key segments as segment/value (Graph
// answers mailFolders('inbox') for a list asked as mailFolders/inbox) and
// lower case (mailbox names and well-known folder names are not case
// sensitive), so a nextLink is compared with the list it continues.
func graphPath(p string) string {
	return strings.ToLower(graphKeySegment.ReplaceAllString(p, "$1/$2"))
}

// historyScope names what a read covers, for its cursors: the route, the
// mailbox and the window.
func (i *instance) historyScope(route *model.ExternalRef, since, until time.Time) string {
	h := sha256.New()
	h.Write([]byte(i.cfg.mailboxPath()))
	if len(i.cfg.HistoryMailboxes) > 0 {
		h.Write([]byte("/" + strconv.Itoa(len(i.cfg.HistoryMailboxes))))
		for _, f := range i.cfg.HistoryMailboxes {
			h.Write([]byte("/" + strconv.Itoa(len(f)) + ":" + f))
		}
	}
	if route != nil {
		h.Write([]byte("|" + route.Namespace + "/" + route.Kind + "/" + route.ID))
	} else {
		h.Write([]byte("|*"))
	}
	h.Write([]byte("@" + strconv.FormatInt(since.UnixMicro(), 10)))
	if !until.IsZero() {
		h.Write([]byte("<" + strconv.FormatInt(until.UnixMicro(), 10)))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// gMessageList is a page of a message list.
type gMessageList struct {
	Value    []gMessage `json:"value"`
	NextLink string     `json:"@odata.nextLink"`
}

// History reads one page of past messages, oldest first, received at or
// after req.Since and before req.Until (zero is open), as the receiver
// reports them: the envelope of each is the one fetchIngress builds, by the
// same mapping (normalize), so a message imported and received is one
// message. The window is the inbox of the configured mailbox, the receive
// scope, or the folders of Config.HistoryMailboxes when it lists any,
// walked one after the other in the listed order (a message already held
// in an earlier listed folder is read there only); drafts are skipped as
// inbound skips them, and no other folder is read. With req.Route, an address reference of this channel, only the
// messages that address wrote (from/emailAddress/address).
//
// The list is mailFolders('inbox')/messages with
//
//	$filter=receivedDateTime ge {Since} and receivedDateTime lt {Until}
//	$orderby=receivedDateTime asc
//
// (the property of $orderby first in $filter, as the list documents), the
// receiver's Prefer headers (immutable IDs, text bodies) and the inbound
// $select. Graph times carry whole seconds, so the bounds are rounded up
// to the second: a message at t is in the window exactly when
// Since <= t < Until. Unverified against Graph until the cassette is
// recorded: lt on receivedDateTime together with $orderby asc on the
// inbox, and internetMessageHeaders in the $select of a list.
//
// The cursor is base64url JSON {v, k, p, t, s[, n]}: the paging parameter
// and token of the @odata.nextLink, never the link. The request is rebuilt
// from req each time, so no cursor can point the credential at another
// path; a nextLink that is not the next page of this same list is refused.
// k hashes the route, mailbox and window; a cursor issued for another read
// is model.Validation.
//
// A mailbox the credential cannot reach (403 or 404 on the list) is
// model.Unavailable. Throttling and server errors return with their
// Retry-After, as every Graph call does; nothing here sleeps or retries.
// A message of the list without an ID cannot be read and is reported as
// Gap; a message gone between pages, or an attachment list that is, is
// simply absent. History reads and writes no receiver state. It stages file
// attachments through req.Stager as the receiver does (stageAttachments: the
// same keys, bounds and refusals, so a message imported and received holds
// its bytes once), one $value request per staged attachment; a throttled or
// failed download returns with its Retry-After and the cursor unchanged.
// Without a stager attachments are described only.
func (i *instance) History(ctx context.Context, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	switch {
	case req.Limit < 1:
		return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: "history limit must be at least 1", Retry: model.NoRetry}
	case req.Since.IsZero():
		return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: "history window start required", Retry: model.NoRetry}
	}
	if r := req.Route; r != nil && (r.Channel != i.ch.Ref || r.Namespace != IdentityNamespace || r.Kind != refKindAddress || r.ID == "") {
		return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: "history route is not an address of this channel", Retry: model.NoRetry}
	}
	scope := i.historyScope(req.Route, req.Since, req.Until)
	cur := historyCursor{Version: historyVersion, Scope: scope}
	if req.Cursor == "" {
		cur.Top = min(req.Limit, maxHistoryTop)
		if len(i.cfg.HistoryMailboxes) > 0 {
			cur.Mailbox = i.cfg.HistoryMailboxes[0]
		}
	} else {
		var err error
		if cur, err = i.decodeHistoryCursor(req.Cursor, scope); err != nil {
			return adapter.HistoryPage{}, err
		}
	}
	folders := i.cfg.historyFolders()
	idx := 0
	if cur.Mailbox != "" {
		idx = slices.Index(folders, cur.Mailbox)
	}
	s := i.c.session()
	sink := staging.HistorySink(req.Stager)
	var page adapter.HistoryPage
	// The walk is folder by folder, in the configured order: a folder is
	// read to its end before the next, and a call that ends a folder short
	// of Limit goes on into the next.
	for {
		folder := folders[idx]
		if err := i.checkHistoryFolder(ctx, s, folder); err != nil {
			return adapter.HistoryPage{}, err
		}
		first := cur.Param == ""
		resp, err := s.do(ctx, request{method: "GET", url: i.historyURL(req, folder, cur.Top, cur.Param, cur.Token), prefer: []string{preferImmutableID, preferTextBody}})
		if err != nil {
			return adapter.HistoryPage{}, historyError(resp.status, err, first)
		}
		var list gMessageList
		if err := decode(resp.body, &list); err != nil {
			return adapter.HistoryPage{}, err
		}
		var nextParam, nextToken string
		if list.NextLink != "" {
			var ok bool
			if nextParam, nextToken, ok = i.pagingToken(list.NextLink, req, folder, cur.Top); !ok {
				return adapter.HistoryPage{}, &model.Error{Code: model.Permanent, SafeMessage: "graph list page without a usable link", Retry: model.NoRetry}
			}
		}
		if cur.Skip > len(list.Value) {
			return adapter.HistoryPage{}, &model.Error{Code: model.Validation, SafeMessage: "history cursor was not issued for this read", Retry: model.NoRetry}
		}
		items := list.Value[cur.Skip:]
		taken := min(len(items), req.Limit-len(page.Envelopes))
		for _, m := range items[:taken] {
			if m.ID == "" {
				page.Gap = true
				continue
			}
			if m.IsDraft {
				continue
			}
			if idx > 0 {
				// A message copied into several listed folders is read in the
				// first of them only.
				dup, err := i.heldEarlier(ctx, s, folders[:idx], m.InternetMessageID)
				if err != nil {
					return adapter.HistoryPage{}, err
				}
				if dup {
					continue
				}
			}
			var atts []gAttachment
			if m.HasAttachments {
				if atts, err = i.attachments(ctx, s, m.ID); err != nil {
					return adapter.HistoryPage{}, err
				}
			}
			st, err := i.stageAttachments(ctx, s, sink, m.ID, i.destination(m), atts)
			if err != nil {
				return adapter.HistoryPage{}, err
			}
			page.Envelopes = append(page.Envelopes, i.normalize(m, atts, st).Envelope)
		}
		switch {
		case taken < len(items):
			cur.Skip += taken
			page.Next = cur.encode()
			return page, nil
		case list.NextLink != "":
			cur.Param, cur.Token, cur.Skip = nextParam, nextToken, 0
			page.Next = cur.encode()
			return page, nil
		case idx+1 == len(folders):
			return page, nil
		}
		idx++
		cur.Mailbox, cur.Param, cur.Token, cur.Skip = folders[idx], "", "", 0
		if len(page.Envelopes) >= req.Limit {
			page.Next = cur.encode()
			return page, nil
		}
	}
}

// checkHistoryFolder refuses a configured folder that is, by ID, one of the
// refusedHistoryFolders: a name is refused at configuration, an ID can be
// told only by asking Graph. Well-known names that may be walked are not
// asked about.
func (i *instance) checkHistoryFolder(ctx context.Context, s *session, folder string) error {
	if len(i.cfg.HistoryMailboxes) == 0 || folder == folderInbox || folder == folderArchive {
		return nil
	}
	f, err := i.folder(ctx, s, folder)
	if model.IsCode(err, model.NotFound) {
		return nil // the list reports a folder Graph does not have
	}
	if err != nil {
		return err
	}
	for _, name := range refusedHistoryFolders {
		r, err := i.folder(ctx, s, name)
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if r.ID == f.ID {
			return &model.Error{Code: model.Validation, SafeMessage: "history_mailboxes names a folder history never reads", Retry: model.NoRetry}
		}
	}
	return nil
}

// heldEarlier reports whether a message with this Internet message ID is in
// any of folders, the listed folders walked before the current one.
func (i *instance) heldEarlier(ctx context.Context, s *session, folders []string, internetID string) (bool, error) {
	if internetID == "" {
		return false, nil
	}
	for _, f := range folders {
		q := url.Values{
			"$select": {"id"},
			"$filter": {"internetMessageId eq '" + strings.ReplaceAll(internetID, "'", "''") + "'"},
			"$top":    {"1"},
		}
		resp, err := s.do(ctx, request{method: "GET", url: i.c.api(i.cfg.mailboxPath()+"/mailFolders/"+url.PathEscape(f)+"/messages", q), prefer: []string{preferImmutableID}})
		if err != nil {
			if resp.status == http.StatusNotFound {
				continue
			}
			return false, transient(err)
		}
		var list gMessageList
		if err := decode(resp.body, &list); err != nil {
			return false, err
		}
		if len(list.Value) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// historyURL is the list request of a read, at the page param=token names
// (the first when empty).
func (i *instance) historyURL(req adapter.HistoryRequest, folder string, top int, param, token string) string {
	filter := "receivedDateTime ge " + graphTime(req.Since)
	if !req.Until.IsZero() {
		filter += " and receivedDateTime lt " + graphTime(req.Until)
	}
	if req.Route != nil {
		filter += " and from/emailAddress/address eq '" + strings.ReplaceAll(req.Route.ID, "'", "''") + "'"
	}
	q := url.Values{
		"$select":  {messageSelect},
		"$filter":  {filter},
		"$orderby": {"receivedDateTime asc"},
		"$top":     {strconv.Itoa(top)},
	}
	if param != "" {
		q.Set(param, token)
	}
	return i.c.api(i.cfg.mailboxPath()+"/mailFolders/"+url.PathEscape(folder)+"/messages", q)
}

// graphTime formats t, rounded up to the second, as Graph writes a
// dateTimeOffset.
func graphTime(t time.Time) string {
	t = t.UTC()
	if r := t.Truncate(time.Second); !r.Equal(t) {
		t = r.Add(time.Second)
	}
	return t.Format("2006-01-02T15:04:05Z")
}

// historyError maps a failed list call. A mailbox the credential cannot
// reach (403, 404) is Unavailable on the first page; on a later one a 404
// or 410 is a link Graph no longer accepts, a cursor error.
func historyError(status int, err error, first bool) error {
	switch status {
	case http.StatusForbidden:
		return &model.Error{Code: model.Unavailable, SafeMessage: "graph mailbox not accessible", Retry: model.NoRetry}
	case http.StatusNotFound, http.StatusGone:
		if !first {
			return &model.Error{Code: model.Validation, SafeMessage: "history cursor was not issued for this read", Retry: model.NoRetry}
		}
		if status == http.StatusNotFound {
			return &model.Error{Code: model.Unavailable, SafeMessage: "graph mailbox not accessible", Retry: model.NoRetry}
		}
	}
	return transient(err)
}

var _ adapter.HistoryReader = (*instance)(nil)
