package github_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// list answers "List issue comments" (repos/{o}/{r}/issues/{n}/comments).
func (f *fakeGitHub) list(w http.ResponseWriter, r *http.Request, a *fakeAccount, issue string, c fakeCall) {
	since := sinceOf(r)
	f.mu.Lock()
	a.calls = append(a.calls, c)
	if f.scriptedList(w, a) {
		return
	}
	cs := slices.Clone(a.comments[issue])
	f.mu.Unlock()
	// Ascending ID; since keeps what was updated at or after it.
	slices.SortFunc(cs, func(x, y fakeComment) int { return cmp.Compare(x.ID, y.ID) })
	var all []map[string]any
	for _, cm := range cs {
		if !cm.updated().Before(since) {
			all = append(all, commentJSON(cm))
		}
	}
	f.paginate(w, r, all)
}

// sinceOf is a listing's since parameter, zero when absent.
func sinceOf(r *http.Request) time.Time {
	since, _ := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
	return since
}

// paginate answers one page of all, per_page (default 30, at most 100)
// and page (default 1) as GitHub reads them, with a Link header naming the
// next page when there is one.
func (f *fakeGitHub) paginate(w http.ResponseWriter, r *http.Request, all []map[string]any) {
	q := r.URL.Query()
	perPage, page := 30, 1
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil && v > 0 {
		perPage = min(v, 100)
	}
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		page = v
	}
	start := min((page-1)*perPage, len(all))
	end := min(start+perPage, len(all))
	h := map[string]string{}
	if end < len(all) {
		next := *r.URL
		nq := next.Query()
		nq.Set("page", strconv.Itoa(page+1))
		next.RawQuery = nq.Encode()
		h["Link"] = fmt.Sprintf(`<%s%s>; rel="next"`, f.srv.URL, next.RequestURI())
	}
	out, _ := json.Marshal(append([]map[string]any{}, all[start:end]...))
	writeJSON(w, http.StatusOK, h, string(out))
}

// stand-in: the repository listings history reads, until recorded. They
// answer in the documented shapes of "List issue comments for a
// repository" (https://docs.github.com/en/rest/issues/comments#list-issue-comments-for-a-repository)
// and "List repository issues"
// (https://docs.github.com/en/rest/issues/issues#list-repository-issues):
// since keeps what was updated at or after it, sort=created with
// direction=asc orders by creation (ties by ID), pull requests and their
// comments are included, a pull request issue carries a pull_request
// object and a comment on one an html_url naming /pull/.

// listRepo answers GET repos/{o}/{r}/issues/comments and GET
// repos/{o}/{r}/issues, and reports whether the path was one of them.
func (f *fakeGitHub) listRepo(w http.ResponseWriter, r *http.Request, a *fakeAccount, p string, c fakeCall) bool {
	s := strings.Split(p, "/")
	issues := len(s) == 4 && s[3] == "issues"
	comments := len(s) == 5 && s[3] == "issues" && s[4] == "comments"
	if s[0] != "repos" || (!issues && !comments) {
		return false
	}
	repo := s[1] + "/" + s[2]
	since := sinceOf(r)
	q := r.URL.Query()
	f.mu.Lock()
	a.calls = append(a.calls, c)
	if f.scriptedList(w, a) {
		return true
	}
	var all []map[string]any
	if comments {
		var cs []fakeComment
		for issue, list := range a.comments {
			if strings.HasPrefix(issue, repo+"#") {
				cs = append(cs, list...)
			}
		}
		slices.SortFunc(cs, func(x, y fakeComment) int {
			if q.Get("sort") == "created" {
				if d := x.CreatedAt.Compare(y.CreatedAt); d != 0 {
					return d
				}
			}
			return cmp.Compare(x.ID, y.ID)
		})
		for _, cm := range cs {
			if !cm.updated().Before(since) {
				all = append(all, commentJSON(cm))
			}
		}
	} else {
		var iss []map[string]any
		for key, i := range a.issues {
			state, _ := i["state"].(string)
			if strings.HasPrefix(key, repo+"#") && (q.Get("state") == "all" || state == cmp.Or(q.Get("state"), "open")) {
				out := maps.Clone(i)
				out["repository_url"] = "https://api.github.com/repos/" + repo
				iss = append(iss, out)
			}
		}
		at := func(i map[string]any, k string) time.Time {
			t, _ := time.Parse(time.RFC3339, i[k].(string))
			return t
		}
		slices.SortFunc(iss, func(x, y map[string]any) int {
			if d := at(x, "created_at").Compare(at(y, "created_at")); d != 0 {
				return d
			}
			return cmp.Compare(x["number"].(int), y["number"].(int))
		})
		if q.Get("direction") != "asc" {
			slices.Reverse(iss)
		}
		for _, i := range iss {
			if !at(i, "updated_at").Before(since) {
				all = append(all, i)
			}
		}
	}
	f.mu.Unlock()
	f.paginate(w, r, all)
	return true
}

// putIssue stores issue number n of repo, opened by userID with body at
// created and last updated then; pr makes it a pull request.
func (f *fakeGitHub) putIssue(prefix, repo string, n int, userID int64, body string, created time.Time, pr bool) {
	f.setIssue(prefix, repo, n, userID, "open", nil, created)
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.accounts[prefix].issues[repo+"#"+strconv.Itoa(n)]
	i["body"], i["created_at"] = body, created.UTC().Format(time.RFC3339)
	if pr {
		i["pull_request"] = map[string]any{"url": "https://api.github.com/repos/" + repo + "/pulls/" + strconv.Itoa(n)}
		i["html_url"] = "https://github.com/" + repo + "/pull/" + strconv.Itoa(n)
	}
}

// hasIssue reports whether issue number n of repo is stored.
func (f *fakeGitHub) hasIssue(prefix, repo string, n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.accounts[prefix].issues[repo+"#"+strconv.Itoa(n)]
	return ok
}

// deleteComment removes comment id, as a deletion on GitHub would.
func (f *fakeGitHub) deleteComment(prefix string, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	for issue, cs := range a.comments {
		a.comments[issue] = slices.DeleteFunc(cs, func(c fakeComment) bool { return c.ID == id })
	}
}

// failLists makes the account's next listings answer as given.
func (f *fakeGitHub) failLists(prefix string, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].listAnswers = append(f.accounts[prefix].listAnswers, as...)
}

// scriptedList answers a listing with the next scripted answer, if any,
// unlocking f.mu either way when it answers. Called with f.mu held.
func (f *fakeGitHub) scriptedList(w http.ResponseWriter, a *fakeAccount) bool {
	if len(a.listAnswers) == 0 {
		return false
	}
	ans := a.listAnswers[0]
	a.listAnswers = a.listAnswers[1:]
	f.mu.Unlock()
	writeJSON(w, ans.status, ans.header, ans.body)
	return true
}
