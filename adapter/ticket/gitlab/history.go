package gitlab

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"hash/fnv"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// historyPerPage is GitLab's largest page size, and the page size of a read
// whose first page asks for at least as many messages.
const historyPerPage = 100

// historyStepBack bounds how many pages a read steps back to find unread
// notes that deletions moved before the page it resumes on; past it the page
// reports a gap.
const historyStepBack = 3

// historyPageBudget bounds the pages one History call reads; past it the
// call returns what it has, with a cursor that moved on.
const historyPageBudget = 10

// maxHistoryPage bounds the page number a cursor may name.
const maxHistoryPage = 1 << 20

// historyEventPrefix starts the event ID of a history envelope, followed by
// its provider message ID: a read repeated is the same event.
const historyEventPrefix = "history:"

// historyCursorPrefix starts every history cursor the adapter issues.
const historyCursorPrefix = "gl1."

// historyCursor is where a history read resumes. It names no URL: the adapter
// rebuilds every request from the channel's API root. Scope binds it to the
// issue and the window it was issued for.
type historyCursor struct {
	Scope uint64 `json:"k"`
	// PerPage is the page size of the read, fixed by its first page so
	// page numbers keep their meaning.
	PerPage int `json:"n"`
	// Description reports the issue description considered.
	Description bool `json:"d,omitempty"`
	// Page holds the last note consumed (1 before any) and Last its ID (0
	// before any).
	Page int   `json:"p"`
	Last int64 `json:"l"`
}

func historyScopeKey(r issueRoute, since, until time.Time) uint64 {
	h := fnv.New64a()
	key := "issue\x00" + strconv.FormatInt(r.project, 10) + "#" + strconv.FormatInt(r.iid, 10) + "\x00" + strconv.FormatInt(since.UTC().UnixNano(), 10)
	if !until.IsZero() {
		key += "\x00" + strconv.FormatInt(until.UTC().UnixNano(), 10)
	}
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

func (c historyCursor) encode() string {
	b, _ := json.Marshal(c)
	return historyCursorPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// decodeHistoryCursor parses s, a cursor issued for scope. Anything else is
// model.Validation, before any provider call.
func decodeHistoryCursor(s string, scope uint64) (historyCursor, error) {
	bad := final(model.Validation, "history cursor not issued for this read")
	raw, ok := strings.CutPrefix(s, historyCursorPrefix)
	if !ok {
		return historyCursor{}, bad
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return historyCursor{}, bad
	}
	var c historyCursor
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil || dec.More() || c.Scope != scope || c.PerPage < 1 || c.PerPage > historyPerPage ||
		c.Page < 1 || c.Page > maxHistoryPage || c.Last < 0 {
		return historyCursor{}, bad
	}
	return c, nil
}

// History reads one page of past messages of one issue, oldest first,
// created at or after req.Since and before req.Until (zero is open), as the
// webhook reports them: the issue description (issue:<id>, when not empty)
// and the issue's notes (note:<id>), each with its project path as
// destination and its issue as work item reference, an internal note with an
// internal audience, and a note by account_id carrying uma's marker as the
// echo of its attempt. System notes (activity such as a label change) are
// skipped, as the webhook skips them. They carry no work item state: the
// issue's state now is no observation of the time a past message was
// written.
//
// The description is read with the issue on the first page, then the notes
// ("List project issue notes", sort=asc, order_by=created_at). Pages follow
// the X-Next-Page header; the cursor holds the page and the ID of the last
// note consumed, so a note deleted from the listing while the window is read
// neither repeats nor drops a message (see resume). A cursor the adapter did
// not issue for this route and window is model.Validation before any call.
//
// GitLab has no feed of a project's or an account's notes, so a read
// without req.Route is model.Unsupported. Failures map as for other reads:
// a rate limit is model.RateLimited with GitLab's wait, a missing issue
// model.NotFound.
func (x *Instance) History(ctx context.Context, req adapter.HistoryRequest) (adapter.HistoryPage, error) {
	if err := x.ready(); err != nil {
		return adapter.HistoryPage{}, err
	}
	switch {
	case req.Limit < 1:
		return adapter.HistoryPage{}, final(model.Validation, "history limit must be at least 1")
	case req.Since.IsZero():
		return adapter.HistoryPage{}, final(model.Validation, "history window start required")
	case req.Route == nil:
		return adapter.HistoryPage{}, final(model.Unsupported, "GitLab has no feed of notes across issues: read one issue's history")
	}
	route, err := x.parseRoute(*req.Route)
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	scope := historyScopeKey(route, req.Since, req.Until)
	cur := historyCursor{Scope: scope, PerPage: min(req.Limit, historyPerPage), Page: 1}
	if req.Cursor != "" {
		if cur, err = decodeHistoryCursor(req.Cursor, scope); err != nil {
			return adapter.HistoryPage{}, err
		}
	}
	h := &historyRead{x: x, route: route, ref: *req.Route, since: req.Since.UTC(), until: req.Until.UTC(), cur: cur, budget: historyPageBudget}
	var out []model.Envelope
	if !cur.Description {
		iss, err := x.getIssue(ctx, route)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		h.dest = iss.destination()
		if env := h.descriptionEnvelope(iss); env != nil {
			out = append(out, *env)
		}
		h.cur.Description = true
	}
	more, done, err := h.take(ctx, req.Limit-len(out))
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	page := adapter.HistoryPage{Envelopes: append(out, more...), Gap: h.gap}
	if !done {
		page.Next = h.cur.encode()
	}
	return page, nil
}

// historyRead is the state of one History call.
type historyRead struct {
	x      *Instance
	route  issueRoute
	ref    model.ExternalRef
	dest   string
	since  time.Time
	until  time.Time
	cur    historyCursor
	budget int
	gap    bool
}

func (h *historyRead) past(t time.Time) bool { return !h.until.IsZero() && !t.Before(h.until) }

func (h *historyRead) normalizer() normalizer {
	return normalizer{x: h.x, dest: h.dest, ref: h.ref, ev: &event{}}
}

// descriptionEnvelope is the issue description as the webhook reports it
// for a new issue, when it has text and an author and was created in the
// window.
func (h *historyRead) descriptionEnvelope(iss apiIssue) *model.Envelope {
	v := iss.view()
	if v.id == 0 || strings.TrimSpace(v.description) == "" || v.author.id == 0 || v.created.Before(h.since) || h.past(v.created) {
		return nil
	}
	env := h.normalizer().message(issueMessageID(v.id), v.author, bodyText(v.description), false, v.created, v.updated)
	env.EventID = historyEventPrefix + env.MessageID
	return &env
}

// page reads one page of the issue's notes, oldest first.
func (h *historyRead) page(ctx context.Context, page int) ([]apiNote, int, error) {
	a, err := h.x.call(ctx, spec{method: http.MethodGet, path: h.route.notesPath(), query: url.Values{
		"sort": {"asc"}, "order_by": {"created_at"},
		"per_page": {strconv.Itoa(h.cur.PerPage)}, "page": {strconv.Itoa(page)},
	}})
	if err != nil || !a.ok() {
		return nil, 0, h.x.readFailure(ctx, a, err)
	}
	ns, ok := decodeNotes(a.body)
	if !ok {
		return nil, 0, model.NewError(model.Transient, "GitLab answer is not a list of notes")
	}
	next, _ := strconv.Atoi(a.header.Get("X-Next-Page"))
	return ns, next, nil
}

// indexOf returns the position of the note with id in ns, -1 for none.
func indexOf(ns []apiNote, id int64) int {
	for i, n := range ns {
		if n.ID == id {
			return i
		}
	}
	return -1
}

// resume reads the page the cursor names and returns the notes after the
// last one consumed, and the page after them (0 for none). Note IDs are not
// ordered by creation time (imported notes keep their original time), so
// the position is the last note's ID, found on the page. A deletion before
// it moves it to an earlier page: up to historyStepBack earlier pages are
// searched. A last note that is itself gone cannot be found: the read
// resumes at the start of the page and reports a gap, which repeats at most
// notes the engine already holds.
func (h *historyRead) resume(ctx context.Context) ([]apiNote, int, int, error) {
	page := h.cur.Page
	ns, next, err := h.page(ctx, page)
	if err != nil {
		return nil, 0, 0, err
	}
	if h.cur.Last == 0 {
		return ns, next, page, nil
	}
	if i := indexOf(ns, h.cur.Last); i >= 0 {
		return ns[i+1:], next, page, nil
	}
	for back := 1; back <= historyStepBack && page-back >= 1; back++ {
		earlier, enext, err := h.page(ctx, page-back)
		if err != nil {
			return nil, 0, 0, err
		}
		if i := indexOf(earlier, h.cur.Last); i >= 0 {
			return earlier[i+1:], enext, page - back, nil
		}
	}
	h.gap = true
	return ns, next, page, nil
}

// take reads notes from the cursor's position and returns at most limit
// envelopes. done reports the notes read to the end of the window.
func (h *historyRead) take(ctx context.Context, limit int) ([]model.Envelope, bool, error) {
	if h.dest == "" {
		// A resumed read has not read the issue: the destination comes
		// from it.
		iss, err := h.x.getIssue(ctx, h.route)
		if err != nil {
			return nil, false, err
		}
		h.dest = iss.destination()
	}
	buf, next, page, err := h.resume(ctx)
	if err != nil {
		return nil, false, err
	}
	h.cur.Page = page
	var out []model.Envelope
	for {
		for len(buf) > 0 {
			n := buf[0]
			created := parseTime(n.CreatedAt)
			if h.past(created) {
				return out, true, nil
			}
			if !n.System && !created.Before(h.since) && n.Author.ID != 0 {
				if len(out) == limit {
					return out, false, nil
				}
				out = append(out, h.noteEnvelope(n))
			}
			buf = buf[1:]
			h.cur.Page, h.cur.Last = page, n.ID
		}
		if next == 0 {
			return out, true, nil
		}
		if h.budget == 0 {
			return out, false, nil
		}
		h.budget--
		// Everything before the next page is consumed: resume there.
		page = next
		if buf, next, err = h.page(ctx, page); err != nil {
			return nil, false, err
		}
	}
}

func (h *historyRead) noteEnvelope(n apiNote) model.Envelope {
	v := n.view()
	env := h.normalizer().noteMessage(hookNote{ID: v.id, Note: v.body, Internal: v.internal}, v.author, v.created, v.updated)
	env.EventID = historyEventPrefix + env.MessageID
	return env
}
