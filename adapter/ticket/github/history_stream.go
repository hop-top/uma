package github

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v92/github"

	"hop.top/uma/model"
)

// streamKind is the GitHub listing a history stream reads.
type streamKind int

const (
	// issueComments lists one issue's comments, ascending ID.
	issueComments streamKind = iota
	// repoIssues lists a repository's issues and pull requests, oldest
	// created first.
	repoIssues
	// repoComments lists a repository's issue and pull request comments,
	// oldest created first.
	repoComments
)

// histItem is one listed item: its ID, creation time and page, and the
// envelope it reports, nil for an item the adapter reports nothing for.
type histItem struct {
	id   int64
	at   time.Time
	page int
	env  *model.Envelope
}

// stream is one listing being read. pos is where the cursor resumes it:
// the page holding the last consumed item, or the page to read next once
// every earlier item is consumed. buf holds the loaded, unconsumed items
// after pos.Last; next is the page the Link header names after them.
type stream struct {
	kind   streamKind
	route  issueRoute
	pos    streamPos
	buf    []histItem
	next   int
	loaded bool
}

func (s *stream) consume() {
	it := s.buf[0]
	s.buf = s.buf[1:]
	s.pos = streamPos{Page: it.page, Last: it.id}
}

// head returns the next item of s that reports a message, consuming the
// items before it that report none. It is nil when s is read to its end,
// or when the page budget is spent (h.short).
func (h *historyRead) head(ctx context.Context, s *stream) (*histItem, error) {
	if !s.loaded {
		if err := h.load(ctx, s); err != nil {
			return nil, err
		}
	}
	for {
		for len(s.buf) > 0 && s.buf[0].env == nil && !h.past(s.buf[0]) {
			s.consume()
		}
		if len(s.buf) > 0 && h.past(s.buf[0]) {
			// Listings run oldest first: nothing after this is in the window.
			s.buf, s.next = nil, 0
			return nil, nil
		}
		if len(s.buf) > 0 {
			return &s.buf[0], nil
		}
		if s.next == 0 {
			return nil, nil
		}
		if h.budget == 0 {
			h.short = true
			return nil, nil
		}
		h.budget--
		// Everything before the next page is consumed: resume there.
		s.pos.Page = s.next
		items, next, err := h.list(ctx, s, s.next)
		if err != nil {
			return nil, err
		}
		s.buf, s.next = after(items, s.pos.Last), next
	}
}

// past reports whether it was created at or after the window's end.
func (h *historyRead) past(it histItem) bool {
	return !h.until.IsZero() && !it.at.Before(h.until)
}

// load reads the page s resumes on. Deletions before the last consumed
// item move it, and the unread items after it, to earlier pages: while the
// page read starts after that item, the page before it is read too, at
// most historyStepBack times; past that the call reports a gap.
func (h *historyRead) load(ctx context.Context, s *stream) error {
	page := s.pos.Page
	items, next, err := h.list(ctx, s, page)
	if err != nil {
		return err
	}
	buf := after(items, s.pos.Last)
	for back := 0; s.pos.Last > 0 && page > 1 && (len(items) == 0 || items[0].id > s.pos.Last); back++ {
		if back == historyStepBack {
			h.gap = true
			break
		}
		page--
		if items, _, err = h.list(ctx, s, page); err != nil {
			return err
		}
		var earlier []histItem
		for _, it := range after(items, s.pos.Last) {
			if len(buf) == 0 || it.id < buf[0].id {
				earlier = append(earlier, it)
			}
		}
		buf = append(earlier, buf...)
	}
	s.buf, s.next, s.loaded = buf, next, true
	return nil
}

// after returns the items with an ID above last.
func after(items []histItem, last int64) []histItem {
	var out []histItem
	for _, it := range items {
		if it.id > last {
			out = append(out, it)
		}
	}
	return out
}

// list reads one page of s's listing, returning its items and the next
// page the Link header names (0 for none).
func (h *historyRead) list(ctx context.Context, s *stream, page int) ([]histItem, int, error) {
	x, r := h.x, s.route
	since := h.serverSince()
	lo := gh.ListOptions{Page: page, PerPage: h.perPage}
	var items []histItem
	var resp *gh.Response
	var err error
	switch s.kind {
	case repoIssues:
		var iss []*gh.Issue
		iss, resp, err = call(ctx, x, func(ctx context.Context) ([]*gh.Issue, *gh.Response, error) {
			return x.client.Issues.ListByRepo(ctx, r.owner, r.repo, &gh.IssueListByRepoOptions{
				State: "all", Sort: "created", Direction: "asc", Since: since, ListOptions: lo,
			})
		})
		for _, i := range iss {
			repo := r.owner + "/" + r.repo
			if full, ok := repoOfURL(i.GetRepositoryURL()); ok {
				repo = full
			}
			items = append(items, h.issueItem(repo, i, page))
		}
	default:
		opts := &gh.IssueListCommentsOptions{Since: &since, ListOptions: lo}
		if s.kind == repoComments {
			opts.Sort, opts.Direction = new("created"), new("asc")
		}
		var cs []*gh.IssueComment
		cs, resp, err = call(ctx, x, func(ctx context.Context) ([]*gh.IssueComment, *gh.Response, error) {
			return x.client.Issues.ListComments(ctx, r.owner, r.repo, r.number, opts)
		})
		for _, c := range cs {
			items = append(items, h.commentItem(s, c, page))
		}
	}
	if err != nil {
		return nil, 0, x.readFailure(ctx, resp, err)
	}
	return items, resp.NextPage, nil
}

// issueItem is an issue of repo as a history item: its description, when
// the webhook would report one (not a pull request, a body, an author)
// created in the window.
func (h *historyRead) issueItem(repo string, iss *gh.Issue, page int) histItem {
	it := histItem{id: iss.GetID(), at: iss.GetCreatedAt().Time, page: page}
	u := iss.GetUser()
	if iss.GetID() == 0 || iss.GetNumber() <= 0 || iss.PullRequestLinks != nil || strings.TrimSpace(iss.GetBody()) == "" ||
		u.GetID() == 0 || it.at.Before(h.since) || h.past(it) {
		return it
	}
	n := normalizer{x: h.x, repo: repo, ref: workItemRef(h.x.channel.Ref, repo, iss.GetNumber())}
	env := n.message(kindFor(u), issueMessageID(iss.GetID()), u, bodyText(iss.GetBody()), iss.GetCreatedAt().Time, iss.GetUpdatedAt().Time)
	env.EventID = historyEventPrefix + env.MessageID
	it.env = &env
	return it
}

// commentItem is a comment as a history item: its message, when the
// webhook would report one (an author, not on a pull request) created in
// the window. A repository listing names the issue only in issue_url.
func (h *historyRead) commentItem(s *stream, c *gh.IssueComment, page int) histItem {
	it := histItem{id: c.GetID(), at: c.GetCreatedAt().Time, page: page}
	if c.GetID() == 0 || c.GetUser().GetID() == 0 || it.at.Before(h.since) || onPullRequest(c.GetHTMLURL()) {
		return it
	}
	repo, number := s.route.owner+"/"+s.route.repo, s.route.number
	if s.kind == repoComments {
		var ok bool
		if repo, number, ok = issueOfURL(c.GetIssueURL()); !ok {
			return it
		}
	}
	n := normalizer{x: h.x, repo: repo, ref: workItemRef(h.x.channel.Ref, repo, number)}
	env := n.commentMessage(c)
	env.EventID = historyEventPrefix + env.MessageID
	it.env = &env
	return it
}

// pathSegments returns the path segments of a URL.
func pathSegments(raw string) []string {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	return strings.Split(strings.Trim(u.Path, "/"), "/")
}

// onPullRequest reports a comment's html_url naming a pull request
// (.../{owner}/{repo}/pull/{n}): the comment endpoints list pull request
// comments too, and their issue_url does not tell them apart.
func onPullRequest(htmlURL string) bool {
	s := pathSegments(htmlURL)
	return len(s) >= 4 && s[len(s)-2] == "pull"
}

// issueOfURL parses an API issue URL (.../repos/{owner}/{repo}/issues/{n}).
func issueOfURL(raw string) (string, int, bool) {
	s := pathSegments(raw)
	if len(s) < 5 || s[len(s)-5] != "repos" || s[len(s)-2] != "issues" {
		return "", 0, false
	}
	n, err := strconv.Atoi(s[len(s)-1])
	full := s[len(s)-4] + "/" + s[len(s)-3]
	if err != nil || n <= 0 || !validFullName(full) {
		return "", 0, false
	}
	return full, n, true
}

// repoOfURL parses an API repository URL (.../repos/{owner}/{repo}).
func repoOfURL(raw string) (string, bool) {
	s := pathSegments(raw)
	if len(s) < 3 || s[len(s)-3] != "repos" {
		return "", false
	}
	full := s[len(s)-2] + "/" + s[len(s)-1]
	return full, validFullName(full)
}
