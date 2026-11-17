package github

import (
	"context"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// historyPerPage is GitHub's largest page size, and the page size of a
// read whose first page asks for at least as many messages.
const historyPerPage = 100

// historyStepBack bounds how many pages a listing steps back to find
// unread items that deletions moved before the page it resumes on; past
// it the page reports a gap.
const historyStepBack = 3

// historyPageBudget bounds the further pages one History call lists after
// each listing's first, such as pages of pull request comments it skips;
// past it the call returns what it has, with a cursor that moved on.
const historyPageBudget = 10

// historyEventPrefix starts the event ID of a history envelope, followed by
// its provider message ID: a read repeated is the same event.
const historyEventPrefix = "history:"

// History reads one page of past messages, oldest first, created at or
// after req.Since and before req.Until (zero is open), as the webhook
// reports them: an issue description
// (issue:<id>, when not empty) and comments (comment:<id>), each with its
// repository as destination and its issue as work item reference, and a
// comment by account_id carrying uma's marker as the echo of its attempt.
// They carry no work item state: the issue's state now is no observation
// of the time a past message was written. Reactions are not read.
//
// With req.Route, the messages of that issue: the description, read with
// the issue on the first page, then the issue's comments ("List issue
// comments", ascending ID). Without it, those of every repository in
// HistoryRepositories, merged by creation time: the repository's issues
// and its comments ("List repository issues" and "List issue comments for
// a repository", both sort=created, direction=asc); without repositories
// it is model.Disabled. Pull requests and their comments are skipped, as
// the webhook drops them; an issue route naming a pull request reads an
// empty window.
//
// GitHub's since filter is on update time, so the read asks for messages
// updated since the window started and keeps those created in it; its
// listings run oldest created first, so the read ends at the first message
// created at or after Until. Pages
// follow the Link header; the cursor holds each listing's page and last
// item ID, so a comment edited into the listing or deleted from it while
// the window is read neither repeats nor drops a message. A cursor the
// adapter did not issue for this route, repositories and window is
// model.Validation before any call. Failures map as for other reads: a
// rate limit is model.RateLimited with GitHub's wait, a missing issue or
// repository model.NotFound.
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
	what, streams, route, err := x.historyScope(req.Route)
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	key := historyScopeKey(what, req.Since, req.Until)
	cur := historyCursor{Scope: key, PerPage: min(req.Limit, historyPerPage), Description: route == nil}
	for range streams {
		cur.Streams = append(cur.Streams, streamPos{Page: 1})
	}
	if req.Cursor != "" {
		if cur, err = decodeHistoryCursor(req.Cursor, key, len(streams)); err != nil {
			return adapter.HistoryPage{}, err
		}
	}
	for i, s := range streams {
		s.pos = cur.Streams[i]
	}
	h := &historyRead{x: x, since: req.Since.UTC(), until: req.Until.UTC(), perPage: cur.PerPage, budget: historyPageBudget}
	var out []model.Envelope
	if !cur.Description {
		iss, err := x.getIssue(ctx, *route)
		if err != nil {
			return adapter.HistoryPage{}, err
		}
		if iss.PullRequestLinks != nil {
			return adapter.HistoryPage{}, nil
		}
		if it := h.issueItem(route.owner+"/"+route.repo, iss, 0); it.env != nil {
			out = append(out, *it.env)
		}
		cur.Description = true
	}
	more, done, err := h.take(ctx, streams, req.Limit-len(out))
	if err != nil {
		return adapter.HistoryPage{}, err
	}
	page := adapter.HistoryPage{Envelopes: append(out, more...), Gap: h.gap}
	if !done {
		for i, s := range streams {
			cur.Streams[i] = s.pos
		}
		page.Next = cur.encode()
	}
	return page, nil
}

// historyScope returns what a read covers, as a cursor scope, and its
// listings: the comments of the issue route names, or the issues and
// comments of each history repository.
func (x *Instance) historyScope(r *model.ExternalRef) (string, []*stream, *issueRoute, error) {
	if r != nil {
		route, err := x.parseRoute(*r)
		if err != nil {
			return "", nil, nil, err
		}
		what := "issue\x00" + route.owner + "/" + route.repo + "#" + strconv.Itoa(route.number)
		return what, []*stream{{kind: issueComments, route: route}}, &route, nil
	}
	if len(x.cfg.HistoryRepositories) == 0 {
		return "", nil, nil, final(model.Disabled, "channel-wide history needs history_repositories in the channel configuration")
	}
	what := "repos"
	var streams []*stream
	for _, full := range x.cfg.HistoryRepositories {
		owner, repo, _ := strings.Cut(full, "/")
		what += "\x00" + full
		streams = append(streams,
			&stream{kind: repoIssues, route: issueRoute{owner: owner, repo: repo}},
			&stream{kind: repoComments, route: issueRoute{owner: owner, repo: repo}})
	}
	return what, streams, nil, nil
}

// historyRead is the state of one History call.
type historyRead struct {
	x       *Instance
	since   time.Time
	until   time.Time
	perPage int
	// budget is the further pages the call may still list; short reports
	// a listing it stopped before; gap reports unread items it could not
	// find again.
	budget int
	short  bool
	gap    bool
}

// serverSince is the since parameter: the window start in whole seconds,
// one second early, as GitHub keeps what was updated after it.
func (h *historyRead) serverSince() time.Time {
	return h.since.Truncate(time.Second).Add(-time.Second)
}

// take merges the listings' next items, oldest first, into at most limit
// envelopes. done reports every listing read to its end.
func (h *historyRead) take(ctx context.Context, streams []*stream, limit int) ([]model.Envelope, bool, error) {
	var out []model.Envelope
	for {
		var best *stream
		var bestItem *histItem
		for _, s := range streams {
			it, err := h.head(ctx, s)
			if err != nil {
				return nil, false, err
			}
			if h.short {
				return out, false, nil
			}
			if it != nil && (bestItem == nil || it.at.Before(bestItem.at)) {
				best, bestItem = s, it
			}
		}
		if bestItem == nil {
			return out, true, nil
		}
		if len(out) == limit {
			return out, false, nil
		}
		out = append(out, *bestItem.env)
		best.consume()
	}
}
