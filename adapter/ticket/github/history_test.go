package github_test

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	github "hop.top/uma/adapter/ticket/github"
	"hop.top/uma/model"
)

// histSince is the window start of the history tests.
var histSince = t0.Add(-3 * time.Hour)

const histUser int64 = 6000001

// readHistory reads the window of route from histSince in pages of at most
// limit envelopes and returns each page.
func readHistory(t *testing.T, inst adapter.Instance, route *model.ExternalRef, limit int) []adapter.HistoryPage {
	t.Helper()
	var pages []adapter.HistoryPage
	cursor := ""
	for range 100 {
		p, err := inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: route, Since: histSince, Cursor: cursor, Limit: limit})
		if err != nil {
			t.Fatalf("History(cursor %q): %v", cursor, err)
		}
		if len(p.Envelopes) > limit {
			t.Fatalf("History(limit %d) = %d envelopes", limit, len(p.Envelopes))
		}
		pages = append(pages, p)
		if p.Next == "" {
			return pages
		}
		if p.Next == cursor {
			t.Fatalf("cursor %q did not advance", cursor)
		}
		cursor = p.Next
	}
	t.Fatal("history not read after 100 pages")
	return nil
}

func historyIDs(pages []adapter.HistoryPage) []string {
	var out []string
	for _, p := range pages {
		for _, e := range p.Envelopes {
			out = append(out, e.MessageID)
		}
	}
	return out
}

func comment(id int64, issue string, user int64, body string, created time.Time) fakeComment {
	login := "user-" + strconv.FormatInt(user, 10)
	if user == botID {
		login = "uma-sandbox[bot]"
	}
	return fakeComment{ID: id, Issue: issue, Body: body, UserID: user, Login: login, CreatedAt: created}
}

// An issue's history is its description, then its comments, oldest first
// in pages of the limit, as the webhook reports them: a comment by the
// account carrying uma's marker is the echo of its attempt. What was
// created before the window is left out, even when edited in it.
func TestHistoryIssueDescriptionThenComments(t *testing.T) {
	r := newSendRig(t)
	issue := sandboxRepo + "#7"
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "Exports fail.", t0.Add(-2*time.Hour), false)
	old := comment(100, issue, histUser, "Before the window.", t0.Add(-5*time.Hour))
	old.UpdatedAt = t0.Add(-time.Hour)
	r.gh.putComment("a", old)
	r.gh.putComment("a", comment(101, issue, histUser, "First.", t0.Add(-90*time.Minute)))
	r.gh.putComment("a", comment(102, issue, botID, "On it.\n\n<!-- uma:"+string(opA.ID)+":"+string(tryOne)+" -->", t0.Add(-80*time.Minute)))
	r.gh.putComment("a", comment(103, issue, histUser, "Thanks.", t0.Add(-70*time.Minute)))
	route := issueRoute("7")

	for _, limit := range []int{2, 1, 3, 100} {
		pages := readHistory(t, r.inst, &route, limit)
		if got, want := historyIDs(pages), []string{"issue:3000000007", "comment:101", "comment:102", "comment:103"}; !slices.Equal(got, want) {
			t.Fatalf("limit %d: history = %q, want %q", limit, got, want)
		}
	}
	envs := readHistory(t, r.inst, &route, 10)[0].Envelopes
	for _, e := range envs {
		if e.EventID != "history:"+e.MessageID || e.Destination != sandboxRepo || len(e.References) == 0 || e.References[0] != wiRef("7", sandboxRepo) ||
			e.WorkItem != nil || e.Audience.Visibility != model.Public || e.SourceRevision == "" {
			t.Errorf("envelope %q = %+v", e.MessageID, e)
		}
	}
	desc, human, own := envs[0], envs[1], envs[2]
	if desc.Kind != model.EnvelopeMessage || desc.Sender != userRef(strconv.FormatInt(histUser, 10)) || desc.Content.Parts[0].Text != "Exports fail." ||
		!desc.OccurredAt.Equal(t0.Add(-2*time.Hour)) {
		t.Errorf("description = %+v", desc)
	}
	if human.Kind != model.EnvelopeMessage || human.SenderKind != "" || human.Content.Parts[0].Text != "First." || len(human.References) != 1 {
		t.Errorf("comment = %+v", human)
	}
	echo := model.EchoRef(chanA, opA, tryOne)
	if own.Kind != model.EnvelopeAutomatedMessage || own.SenderKind != model.ContactKindBot || own.Content.Parts[0].Text != "On it." || !slices.Contains(own.References, echo) {
		t.Errorf("own comment = %+v, want the echo of its attempt without the marker", own)
	}

	// Requests: the issue once per first page, then the comments listing
	// with the page size of the first page and since one second early.
	var issueReads int
	for _, c := range r.gh.calls("a") {
		if c.Method != http.MethodGet || c.Auth != "Bearer "+apiToken {
			t.Fatalf("call %+v, want an authorized read", c)
		}
		if c.Path == "/repos/"+sandboxRepo+"/issues/7" {
			issueReads++
			continue
		}
		q, _ := url.ParseQuery(c.Query)
		if c.Path != "/repos/"+sandboxRepo+"/issues/7/comments" || q.Get("since") != histSince.Add(-time.Second).Format(time.RFC3339) || q.Get("per_page") == "" {
			t.Fatalf("call %s?%s", c.Path, c.Query)
		}
	}
	if issueReads != 5 {
		t.Fatalf("issue reads = %d, want one per read", issueReads)
	}
}

// Until ends the window exclusively: the description and comments created
// at or after it are left out, and the read ends there instead of listing
// on, across pages.
func TestHistoryUntilEndsTheWindow(t *testing.T) {
	r := newSendRig(t)
	issue := sandboxRepo + "#7"
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "Exports fail.", t0.Add(-2*time.Hour), false)
	until := t0.Add(-70 * time.Minute)
	r.gh.putComment("a", comment(101, issue, histUser, "First.", t0.Add(-90*time.Minute)))
	r.gh.putComment("a", comment(102, issue, histUser, "Just before.", until.Add(-time.Second)))
	r.gh.putComment("a", comment(103, issue, histUser, "At Until.", until))
	r.gh.putComment("a", comment(104, issue, histUser, "After.", until.Add(time.Hour)))
	route := issueRoute("7")
	hr := r.inst.(adapter.HistoryReader)
	var got []string
	cursor := ""
	for range 20 {
		p, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Until: until, Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range p.Envelopes {
			got = append(got, e.MessageID)
		}
		if cursor = p.Next; cursor == "" {
			break
		}
	}
	if want := []string{"issue:3000000007", "comment:101", "comment:102"}; !slices.Equal(got, want) {
		t.Fatalf("window = %v, want %v", got, want)
	}
	early, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Until: histSince.Add(time.Minute), Limit: 10})
	if err != nil || len(early.Envelopes) != 0 || early.Next != "" {
		t.Fatalf("window ending before the description = %+v, %v; want nothing", early, err)
	}
	other := histSince
	p, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: other, Until: until, Limit: 1})
	if err != nil || p.Next == "" {
		t.Fatalf("History = %+v, %v", p, err)
	}
	_, err = hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: other, Until: until.Add(time.Second), Cursor: p.Next, Limit: 1})
	if model.CodeOf(err) != model.Validation {
		t.Fatalf("cursor of another Until: %v, want validation", err)
	}
}

// Descriptions the webhook does not report are left out: empty, or
// written before the window.
func TestHistoryDescriptionOnlyWhenReported(t *testing.T) {
	for name, iss := range map[string]struct {
		body    string
		created time.Time
	}{
		"empty":         {" \n", t0.Add(-time.Hour)},
		"before window": {"Old.", histSince.Add(-time.Second)},
	} {
		t.Run(name, func(t *testing.T) {
			r := newSendRig(t)
			r.gh.putIssue("a", sandboxRepo, 7, histUser, iss.body, iss.created, false)
			r.gh.putComment("a", comment(101, sandboxRepo+"#7", histUser, "First.", t0.Add(-time.Hour)))
			route := issueRoute("7")
			if got := historyIDs(readHistory(t, r.inst, &route, 10)); !slices.Equal(got, []string{"comment:101"}) {
				t.Fatalf("history = %q", got)
			}
		})
	}
}

// A route naming a pull request reads nothing: the webhook drops pull
// requests.
func TestHistoryPullRequestRouteIsEmpty(t *testing.T) {
	r := newSendRig(t)
	r.gh.putIssue("a", sandboxRepo, 8, histUser, "A change.", t0.Add(-time.Hour), true)
	r.gh.putComment("a", fakeComment{ID: 101, Issue: sandboxRepo + "#8", Body: "LGTM", UserID: histUser, Login: "u", CreatedAt: t0.Add(-time.Hour), PullRequest: true})
	route := issueRoute("8")
	p, err := r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Limit: 10})
	if err != nil || len(p.Envelopes) != 0 || p.Next != "" {
		t.Fatalf("History = %+v, %v; want an empty window", p, err)
	}
	if n := len(r.gh.calls("a")); n != 1 {
		t.Fatalf("calls = %d, want the issue read only", n)
	}
}

// Channel-wide history merges the history repositories' descriptions and
// comments by creation time, each with its own repository as destination,
// and skips pull requests and their comments.
func TestHistoryChannelMergesRepositories(t *testing.T) {
	const other = "example/uma-other"
	r := newSendRig(t)
	cfg := replyConfig(r.gh.apiURL("a"))
	cfg.HistoryRepositories = []string{sandboxRepo, other}
	inst := r.open(cfg)
	at := func(m int) time.Time { return t0.Add(-time.Duration(m) * time.Minute) }
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "Exports fail.", at(120), false)
	r.gh.putIssue("a", other, 1, histUser, "Imports fail.", at(100), false)
	r.gh.putIssue("a", sandboxRepo, 8, histUser, "A change.", at(95), true)
	r.gh.putIssue("a", sandboxRepo, 9, histUser, "Too old.", histSince.Add(-time.Hour), false)
	r.gh.putComment("a", comment(201, sandboxRepo+"#7", histUser, "a", at(90)))
	r.gh.putComment("a", fakeComment{ID: 202, Issue: sandboxRepo + "#8", Body: "LGTM", UserID: histUser, Login: "u", CreatedAt: at(85), PullRequest: true})
	r.gh.putComment("a", comment(203, other+"#1", histUser, "b", at(80)))
	r.gh.putComment("a", comment(204, sandboxRepo+"#9", histUser, "c", at(60)))
	want := []string{"issue:3000000007", "issue:3000000001", "comment:201", "comment:203", "comment:204"}
	for _, limit := range []int{1, 2, 10} {
		pages := readHistory(t, inst, nil, limit)
		if got := historyIDs(pages); !slices.Equal(got, want) {
			t.Fatalf("limit %d: history = %q, want %q", limit, got, want)
		}
	}
	envs := readHistory(t, inst, nil, 10)[0].Envelopes
	for i, w := range []struct {
		dest, number string
	}{{sandboxRepo, "7"}, {other, "1"}, {sandboxRepo, "7"}, {other, "1"}, {sandboxRepo, "9"}} {
		if envs[i].Destination != w.dest || envs[i].References[0] != wiRef(w.number, w.dest) {
			t.Errorf("%s: destination %q references %+v, want %s#%s", envs[i].MessageID, envs[i].Destination, envs[i].References, w.dest, w.number)
		}
	}
	for _, c := range r.gh.calls("a") {
		q, _ := url.ParseQuery(c.Query)
		if q.Get("sort") != "created" || q.Get("direction") != "asc" || (strings.HasSuffix(c.Path, "/issues") && q.Get("state") != "all") {
			t.Fatalf("listing %s?%s, want oldest created first, every state", c.Path, c.Query)
		}
	}
}

// Without history repositories a channel-wide read is refused before any
// call; a read of one issue needs none.
func TestHistoryChannelNeedsRepositories(t *testing.T) {
	r := newSendRig(t)
	_, err := r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Since: histSince, Limit: 10})
	wantCode(t, err, model.Disabled)
	if n := len(r.gh.calls("a")); n != 0 {
		t.Fatalf("calls = %d, want none", n)
	}
}

// A cursor is honored only for the read it was issued for; anything else
// is validation before any call.
func TestHistoryCursorBoundToItsRead(t *testing.T) {
	r := newSendRig(t)
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "", t0.Add(-2*time.Hour), false)
	for i := range int64(3) {
		r.gh.putComment("a", comment(101+i, sandboxRepo+"#7", histUser, "x", t0.Add(-time.Hour)))
	}
	route, other := issueRoute("7"), issueRoute("8")
	hr := r.inst.(adapter.HistoryReader)
	p, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Limit: 1})
	if err != nil || p.Next == "" {
		t.Fatalf("History = %+v, %v", p, err)
	}
	calls := len(r.gh.calls("a"))
	want := func(name string) model.ErrorCode {
		if name == "channel-wide" {
			// The channel has no history repositories.
			return model.Disabled
		}
		return model.Validation
	}
	for name, req := range map[string]adapter.HistoryRequest{
		"other issue":   {Route: &other, Since: histSince, Cursor: p.Next, Limit: 1},
		"other since":   {Route: &route, Since: histSince.Add(time.Second), Cursor: p.Next, Limit: 1},
		"channel-wide":  {Since: histSince, Cursor: p.Next, Limit: 1},
		"next page URL": {Route: &route, Since: histSince, Cursor: "https://api.github.com/repositories/1/issues/7/comments?page=2", Limit: 1},
		"not base64":    {Route: &route, Since: histSince, Cursor: "gh1.%%%", Limit: 1},
		"truncated":     {Route: &route, Since: histSince, Cursor: p.Next[:len(p.Next)-4], Limit: 1},
		"zero limit":    {Route: &route, Since: histSince, Limit: 0},
		"no window":     {Route: &route, Limit: 1},
		"foreign route": {Route: &model.ExternalRef{Channel: chanA, Namespace: "other", Kind: model.RefKindWorkItem, ID: "7", ParentID: sandboxRepo}, Since: histSince, Limit: 1},
		"other channel": {Route: &model.ExternalRef{Channel: model.ChannelRef{Tenant: tenant, ID: "00000000-0000-7000-8000-00000000c0b2"}, Namespace: github.Namespace, Kind: model.RefKindWorkItem, ID: "7", ParentID: sandboxRepo}, Since: histSince, Limit: 1},
	} {
		_, err := hr.History(t.Context(), req)
		if model.CodeOf(err) != want(name) {
			t.Errorf("%s: History = %v, want %s", name, err, want(name))
		}
	}
	if n := len(r.gh.calls("a")); n != calls {
		t.Fatalf("calls = %d, want %d: a refused cursor reads nothing", n, calls)
	}
}

// Comments deleted or edited into the listing while the window is read
// neither drop nor repeat a message: the cursor resumes after the last
// comment read, stepping back pages the deletions emptied.
func TestHistoryResumesAcrossDeletionsAndEdits(t *testing.T) {
	r := newSendRig(t)
	issue := sandboxRepo + "#7"
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "", t0.Add(-2*time.Hour), false)
	for i := range int64(10) {
		r.gh.putComment("a", comment(101+i, issue, histUser, "x", t0.Add(-time.Hour+time.Duration(i)*time.Minute)))
	}
	route := issueRoute("7")
	hr := r.inst.(adapter.HistoryReader)
	read := func(cursor string) adapter.HistoryPage {
		t.Helper()
		p, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Cursor: cursor, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var got []string
	p := read("")
	got = append(got, historyIDs([]adapter.HistoryPage{p})...)
	r.gh.deleteComment("a", 101)
	r.gh.deleteComment("a", 102)
	p = read(p.Next)
	got = append(got, historyIDs([]adapter.HistoryPage{p})...)
	// Three deletions move the next unread comment to the page before the
	// one the cursor resumes on.
	for _, id := range []int64{103, 104, 105} {
		r.gh.deleteComment("a", id)
	}
	edited := comment(50, issue, histUser, "Old, edited now.", histSince.Add(-time.Hour))
	edited.UpdatedAt = t0
	r.gh.putComment("a", edited)
	for p.Next != "" {
		p = read(p.Next)
		if p.Gap {
			t.Fatalf("page %q reports a gap", historyIDs([]adapter.HistoryPage{p}))
		}
		got = append(got, historyIDs([]adapter.HistoryPage{p})...)
	}
	want := []string{"comment:101", "comment:102", "comment:103", "comment:104", "comment:105", "comment:106", "comment:107", "comment:108", "comment:109", "comment:110"}
	if !slices.Equal(got, want) {
		t.Fatalf("history = %q, want %q", got, want)
	}
}

// Deletions moving the unread comments further back than the reader steps
// are reported as a gap.
func TestHistoryDeletionsPastTheStepBackAreAGap(t *testing.T) {
	r := newSendRig(t)
	issue := sandboxRepo + "#7"
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "", t0.Add(-2*time.Hour), false)
	for i := range int64(8) {
		r.gh.putComment("a", comment(101+i, issue, histUser, "x", t0.Add(-time.Hour+time.Duration(i)*time.Minute)))
	}
	route := issueRoute("7")
	hr := r.inst.(adapter.HistoryReader)
	cursor := ""
	for range 5 {
		p, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Cursor: cursor, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		cursor = p.Next
	}
	for id := int64(101); id <= 104; id++ {
		r.gh.deleteComment("a", id)
	}
	p, err := hr.History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Cursor: cursor, Limit: 1})
	if err != nil || !p.Gap {
		t.Fatalf("History = %+v, %v; want a gap", p, err)
	}
}

// A run of pull request comments longer than the page budget ends the call
// early with a cursor that moved on; the window still reads whole.
func TestHistoryPageBudgetMovesOn(t *testing.T) {
	r := newSendRig(t)
	cfg := replyConfig(r.gh.apiURL("a"))
	cfg.HistoryRepositories = []string{sandboxRepo}
	inst := r.open(cfg)
	r.gh.putIssue("a", sandboxRepo, 8, histUser, "", histSince.Add(-time.Hour), true)
	r.gh.putIssue("a", sandboxRepo, 7, histUser, "", histSince.Add(-time.Hour), false)
	for i := range int64(30) {
		r.gh.putComment("a", fakeComment{ID: 101 + i, Issue: sandboxRepo + "#8", Body: "x", UserID: histUser, Login: "u", CreatedAt: t0.Add(-time.Hour + time.Duration(i)*time.Second), PullRequest: true})
	}
	r.gh.putComment("a", comment(200, sandboxRepo+"#7", histUser, "Real.", t0.Add(-time.Minute)))
	pages := readHistory(t, inst, nil, 1)
	if got := historyIDs(pages); !slices.Equal(got, []string{"comment:200"}) {
		t.Fatalf("history = %q", got)
	}
	if len(pages) < 2 || len(pages[0].Envelopes) != 0 {
		t.Fatalf("pages = %d, first %d envelopes; want the budget to end the first call early", len(pages), len(pages[0].Envelopes))
	}
}

// Failed reads map as the adapter's other reads do.
func TestHistoryFailures(t *testing.T) {
	route := issueRoute("7")
	t.Run("missing issue", func(t *testing.T) {
		r := newSendRig(t)
		_, err := r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Limit: 10})
		wantCode(t, err, model.NotFound)
	})
	t.Run("rate limited", func(t *testing.T) {
		r := newSendRig(t)
		r.gh.putIssue("a", sandboxRepo, 7, histUser, "", t0, false)
		reset := t0.Add(10 * time.Minute)
		r.gh.failLists("a", statusAnswer(http.StatusForbidden, `{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com/rest/using-the-rest-api/rate-limits-for-the-rest-api"}`,
			map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Limit": "5000", "X-RateLimit-Reset": strconv.FormatInt(reset.Unix(), 10)}))
		_, err := r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Limit: 10})
		e := wantCode(t, err, model.RateLimited)
		if e.RetryAfter != 10*time.Minute {
			t.Fatalf("retry after %v, want until the reset", e.RetryAfter)
		}
	})
	t.Run("server error", func(t *testing.T) {
		r := newSendRig(t)
		r.gh.putIssue("a", sandboxRepo, 7, histUser, "", t0, false)
		r.gh.failLists("a", statusAnswer(http.StatusBadGateway, `{"message":"Server Error"}`, nil))
		_, err := r.inst.(adapter.HistoryReader).History(t.Context(), adapter.HistoryRequest{Route: &route, Since: histSince, Limit: 10})
		wantCode(t, err, model.Transient)
	})
}
