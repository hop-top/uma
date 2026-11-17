package github_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
)

// fakeGitHub is a GitHub REST server for the endpoints the adapter calls,
// answering in the documented shapes of "Create an issue comment" and
// "List issue comments" (REST API version 2022-11-28). Each account is a
// path prefix ("/<account>/"), its own API root with its own comments,
// script and call record.
type fakeGitHub struct {
	srv   *httptest.Server
	clock func() time.Time

	mu       sync.Mutex
	accounts map[string]*fakeAccount
}

// fakeAccount is one GitHub account behind an API root.
type fakeAccount struct {
	login    string
	userID   int64
	nextID   int64
	comments map[string][]fakeComment // by "owner/repo#number"
	// issues are issue objects GET /repos/{o}/{r}/issues/{n} answers, by
	// "owner/repo#number".
	issues map[string]map[string]any
	script []answer
	calls  []fakeCall
	// reactions are the reactions on a comment ("comment:owner/repo#id")
	// or on an issue ("issue:owner/repo#number"); nextReaction numbers
	// them.
	reactions    map[string][]fakeReaction
	nextReaction int64
	// ownEditsOnly makes the account's edits and deletions of other users'
	// comments answer 403, as a token without write access to the
	// repository would.
	ownEditsOnly bool
	// onApply runs after a comment is stored and before its answer: a
	// webhook racing the response.
	onApply func(fakeComment)
	// listAnswers are status answers the next comment or issue listings
	// give instead of the listing.
	listAnswers []answer
}

type fakeComment struct {
	ID        int64
	Issue     string
	Body      string
	UserID    int64
	Login     string
	CreatedAt time.Time
	// UpdatedAt is the last edit, CreatedAt when zero.
	UpdatedAt time.Time
	// PullRequest marks a comment on a pull request.
	PullRequest bool
}

func (c fakeComment) updated() time.Time {
	if c.UpdatedAt.IsZero() {
		return c.CreatedAt
	}
	return c.UpdatedAt
}

// fakeCall is one request the account received.
type fakeCall struct {
	Method, Path, Query, Body, Auth string
	Applied                         bool
	Comment                         int64
	// Reaction is the reaction a reaction request created or removed, and
	// Content its content.
	Reaction int64
	Content  string
	// Issue is the "owner/repo#number" of the issue the comment of a
	// comment or reaction request belongs to.
	Issue string
}

// answer is one scripted answer to a comment creation. The zero value
// accepts.
type answer struct {
	// apply stores the comment before answering; drop closes the
	// connection instead of answering.
	apply, drop bool
	// unreachable fails the dial: the request never reaches GitHub.
	unreachable bool
	status      int
	header      map[string]string
	body        string
}

var (
	accepted    = answer{apply: true}
	answerLost  = answer{apply: true, drop: true}
	requestLost = answer{drop: true}
	unreachable = answer{unreachable: true}
)

func statusAnswer(code int, body string, header map[string]string) answer {
	return answer{status: code, body: body, header: header}
}

const docsURL = "https://docs.github.com/rest/issues/comments#create-an-issue-comment"

func newFakeGitHub(t *testing.T, clock func() time.Time) *fakeGitHub {
	f := &fakeGitHub{clock: clock, accounts: map[string]*fakeAccount{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// account returns the account under prefix, creating it with the given
// bot identity.
func (f *fakeGitHub) account(prefix string, userID int64) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{
			login: "uma-sandbox[bot]", userID: userID, nextID: 1000, comments: map[string][]fakeComment{},
			issues: map[string]map[string]any{}, reactions: map[string][]fakeReaction{},
		}
		f.accounts[prefix] = a
	}
	return a
}

// client returns the HTTP client the adapter uses: the server's TLS
// client, failing the dial of a comment creation scripted unreachable.
// The failed attempt is recorded as an unapplied call.
func (f *fakeGitHub) client() *http.Client {
	base := f.srv.Client().Transport
	host := strings.TrimPrefix(f.srv.URL, "https://")
	return &http.Client{CheckRedirect: noRedirect, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != host {
			// Never reach a real host from a test.
			return nil, fmt.Errorf("fake GitHub: refusing request to %s", r.URL.Host)
		}
		prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if r.Method == http.MethodPost || r.Method == http.MethodPatch || r.Method == http.MethodDelete {
			f.mu.Lock()
			a := f.accounts[prefix]
			if a != nil && len(a.script) > 0 && a.script[0].unreachable {
				a.script = a.script[1:]
				c := fakeCall{Method: r.Method, Path: "/" + rest, Auth: r.Header.Get("Authorization")}
				if r.GetBody != nil {
					if b, err := r.GetBody(); err == nil {
						raw, _ := io.ReadAll(b)
						c.Body = string(raw)
					}
				}
				a.calls = append(a.calls, c)
				f.mu.Unlock()
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			f.mu.Unlock()
		}
		return base.RoundTrip(r)
	})}
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func (f *fakeGitHub) apiURL(prefix string) string { return f.srv.URL + "/" + prefix + "/" }

func (f *fakeGitHub) respond(prefix string, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.script = append(a.script, as...)
}

func (f *fakeGitHub) calls(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[prefix]; a != nil {
		return slices.Clone(a.calls)
	}
	return nil
}

func (f *fakeGitHub) stored(prefix, issue string) []fakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accounts[prefix].comments[issue])
}

// seed stores a comment as if written earlier, by userID.
func (f *fakeGitHub) seed(prefix, issue string, userID int64, body string, at time.Time) fakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.nextID++
	c := fakeComment{ID: a.nextID, Issue: issue, Body: body, UserID: userID, Login: "someone", CreatedAt: at}
	a.comments[issue] = append(a.comments[issue], c)
	return c
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil {
		writeJSON(w, http.StatusNotFound, nil, `{"message":"Not Found","documentation_url":"`+docsURL+`","status":"404"}`)
		return
	}
	c := fakeCall{Method: r.Method, Path: "/" + rest, Query: r.URL.RawQuery, Body: string(body), Auth: r.Header.Get("Authorization")}
	if f.serveMutation(w, r, a, rest, c, body) {
		return
	}
	if r.Method == http.MethodGet && (f.listRepo(w, r, a, rest, c) || f.read(w, a, rest, c)) {
		return
	}
	if r.Method == http.MethodPatch {
		f.update(w, a, rest, c, body)
		return
	}
	issue, ok := issuePath(rest)
	if !ok {
		writeJSON(w, http.StatusNotFound, nil, `{"message":"Not Found","documentation_url":"`+docsURL+`","status":"404"}`)
		return
	}
	switch r.Method {
	case http.MethodPost:
		f.create(w, a, issue, c, body)
	case http.MethodGet:
		f.list(w, r, a, issue, c)
	default:
		writeJSON(w, http.StatusNotFound, nil, `{"message":"Not Found"}`)
	}
}

// read answers "Get an issue comment" (repos/{o}/{r}/issues/comments/{id})
// and "Get an issue" (repos/{o}/{r}/issues/{n}), and reports whether the
// path was one of them.
func (f *fakeGitHub) read(w http.ResponseWriter, a *fakeAccount, p string, c fakeCall) bool {
	s := strings.Split(p, "/")
	if len(s) < 5 || s[0] != "repos" || s[3] != "issues" {
		return false
	}
	repo := s[1] + "/" + s[2]
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case len(s) == 6 && s[4] == "comments":
		id, err := strconv.ParseInt(s[5], 10, 64)
		if err != nil {
			return false
		}
		a.calls = append(a.calls, c)
		for issue, cs := range a.comments {
			if !strings.HasPrefix(issue, repo+"#") {
				continue
			}
			for _, cm := range cs {
				if cm.ID == id {
					out, _ := json.Marshal(commentJSON(cm))
					writeJSON(w, http.StatusOK, nil, string(out))
					return true
				}
			}
		}
	case len(s) == 5:
		if _, err := strconv.Atoi(s[4]); err != nil {
			return false
		}
		a.calls = append(a.calls, c)
		if iss, ok := a.issues[repo+"#"+s[4]]; ok {
			out, _ := json.Marshal(iss)
			writeJSON(w, http.StatusOK, nil, string(out))
			return true
		}
	default:
		return false
	}
	writeJSON(w, http.StatusNotFound, nil, `{"message":"Not Found","documentation_url":"`+docsURL+`","status":"404"}`)
	return true
}

// issuePath parses "repos/{owner}/{repo}/issues/{n}/comments".
func issuePath(p string) (string, bool) {
	s := strings.Split(p, "/")
	if len(s) != 6 || s[0] != "repos" || s[3] != "issues" || s[5] != "comments" {
		return "", false
	}
	if _, err := strconv.Atoi(s[4]); err != nil {
		return "", false
	}
	return s[1] + "/" + s[2] + "#" + s[4], true
}

func (f *fakeGitHub) create(w http.ResponseWriter, a *fakeAccount, issue string, c fakeCall, body []byte) {
	var req struct {
		Body string `json:"body"`
	}
	f.mu.Lock()
	ans := accepted
	if len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	if json.Unmarshal(body, &req) != nil || req.Body == "" {
		ans = statusAnswer(http.StatusUnprocessableEntity, `{"message":"Validation Failed","errors":[{"resource":"IssueComment","code":"missing_field","field":"body"}],"documentation_url":"`+docsURL+`","status":"422"}`, nil)
	}
	var stored fakeComment
	if ans.apply {
		a.nextID++
		stored = fakeComment{ID: a.nextID, Issue: issue, Body: req.Body, UserID: a.userID, Login: a.login, CreatedAt: f.clock().UTC()}
		a.comments[issue] = append(a.comments[issue], stored)
		c.Applied, c.Comment = true, stored.ID
	}
	a.calls = append(a.calls, c)
	hook := a.onApply
	f.mu.Unlock()
	if ans.apply && hook != nil {
		hook(stored)
	}
	switch {
	case ans.drop:
		hijackClose(w)
	case ans.status != 0:
		writeJSON(w, ans.status, ans.header, ans.body)
	default:
		out, _ := json.Marshal(commentJSON(stored))
		writeJSON(w, http.StatusCreated, map[string]string{"Location": "https://api.github.com/repos/" + strings.Replace(issue, "#", "/issues/", 1)}, string(out))
	}
}

// update answers "Update an issue" (PATCH repos/{o}/{r}/issues/{n}) for
// state and state_reason, against an issue seeded with setIssue. A closed
// issue without a reason closes as completed and a reopened one reads
// reopened, as GitHub records them.
//
// stand-in: the assignees, labels and milestone fields, until recorded.
// They follow "Update an issue"
// (https://docs.github.com/en/rest/issues/issues#update-an-issue): an
// array replaces the issue's whole set, an empty array clears it, and a
// null milestone removes the current one. Logins and label names are taken
// as given; GitHub's 422 for an unknown assignee is scripted, not derived.
func (f *fakeGitHub) update(w http.ResponseWriter, a *fakeAccount, p string, c fakeCall, body []byte) {
	s := strings.Split(p, "/")
	f.mu.Lock()
	ans := accepted
	if len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	var key string
	if len(s) == 5 && s[0] == "repos" && s[3] == "issues" {
		key = s[1] + "/" + s[2] + "#" + s[4]
	}
	iss, ok := a.issues[key]
	if !ok && ans.status == 0 && !ans.drop {
		a.calls = append(a.calls, c)
		f.mu.Unlock()
		writeJSON(w, http.StatusNotFound, nil, `{"message":"Not Found","documentation_url":"`+issueDocsURL+`","status":"404"}`)
		return
	}
	var req struct {
		State       *string         `json:"state"`
		StateReason *string         `json:"state_reason"`
		Body        *string         `json:"body"`
		Assignees   *[]string       `json:"assignees"`
		Labels      *[]string       `json:"labels"`
		Milestone   json.RawMessage `json:"milestone"`
	}
	_ = json.Unmarshal(body, &req)
	if ok && ans.apply {
		if req.Body != nil {
			iss["body"] = *req.Body
		}
		if req.Assignees != nil {
			users := []any{}
			for _, login := range *req.Assignees {
				users = append(users, map[string]any{"login": login, "id": userIDOf(login), "type": "User", "site_admin": false})
			}
			iss["assignees"] = users
		}
		if req.Labels != nil {
			labels := []any{}
			for i, name := range *req.Labels {
				labels = append(labels, map[string]any{"id": 100 + i, "name": name})
			}
			iss["labels"] = labels
		}
		if len(req.Milestone) > 0 {
			var n *int
			_ = json.Unmarshal(req.Milestone, &n)
			if n == nil {
				iss["milestone"] = nil
			} else {
				iss["milestone"] = map[string]any{"number": *n, "title": "Milestone " + strconv.Itoa(*n), "state": "open"}
			}
		}
		if req.State != nil {
			iss["state"] = *req.State
			reason := any(nil)
			switch {
			case req.StateReason != nil:
				reason = *req.StateReason
			case *req.State == "closed":
				reason = "completed"
			}
			iss["state_reason"] = reason
		}
		iss["updated_at"] = f.clock().UTC().Format(time.RFC3339)
		c.Applied = true
	}
	a.calls = append(a.calls, c)
	out, _ := json.Marshal(iss)
	f.mu.Unlock()
	switch {
	case ans.drop:
		hijackClose(w)
	case ans.status != 0:
		writeJSON(w, ans.status, ans.header, ans.body)
	default:
		writeJSON(w, http.StatusOK, nil, string(out))
	}
}

const issueDocsURL = "https://docs.github.com/rest/issues/issues#update-an-issue"

// userIDOf is the stable numeric ID the fake gives a login it has not
// seen: GitHub answers an assignee as a user object.
func userIDOf(login string) int64 {
	var h int64
	for _, r := range strings.ToLower(login) {
		h = h*31 + int64(r)
	}
	return 8_000_000 + h%1_000_000
}

// issueFields returns the assignee logins, label names and milestone
// number (0 for none) the fake holds for the issue under key
// "owner/repo#number".
func (f *fakeGitHub) issueFields(prefix, key string) (assignees, labels []string, milestone int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss := f.accounts[prefix].issues[key]
	for _, u := range iss["assignees"].([]any) {
		assignees = append(assignees, u.(map[string]any)["login"].(string))
	}
	for _, l := range iss["labels"].([]any) {
		labels = append(labels, l.(map[string]any)["name"].(string))
	}
	if m, ok := iss["milestone"].(map[string]any); ok {
		milestone = m["number"].(int)
	}
	return assignees, labels, milestone
}

// setIssue stores issue number n of repo in the documented issue shape,
// opened by userID, in state with reason (nil for none).
func (f *fakeGitHub) setIssue(prefix, repo string, n int, userID int64, state string, reason any, updated time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	num := strconv.Itoa(n)
	f.accounts[prefix].issues[repo+"#"+num] = map[string]any{
		"id": 3000000000 + n, "node_id": "I_kwDO" + num, "number": n,
		"url":      "https://api.github.com/repos/" + repo + "/issues/" + num,
		"html_url": "https://github.com/" + repo + "/issues/" + num,
		"title":    "Export fails on large files", "body": "Exports over 2 GB stop at 99%.",
		"user":      map[string]any{"login": "reporter", "id": userID, "type": "User", "site_admin": false},
		"labels":    []any{map[string]any{"id": 1, "name": "bug"}},
		"assignees": []any{}, "state": state, "state_reason": reason, "locked": false, "comments": 0,
		"created_at": "2026-10-11T09:00:00Z", "updated_at": updated.UTC().Format(time.RFC3339),
		"author_association": "OWNER",
	}
}

// issueState returns the state of the issue under key "owner/repo#number".
func (f *fakeGitHub) issueState(prefix, key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.accounts[prefix].issues[key]
	if !ok {
		return "", false
	}
	state, _ := iss["state"].(string)
	return state, true
}

// commentJSON is the documented issue comment object.
func commentJSON(c fakeComment) map[string]any {
	repo, n, _ := strings.Cut(c.Issue, "#")
	at := c.CreatedAt.Format(time.RFC3339)
	updated := at
	if !c.UpdatedAt.IsZero() {
		updated = c.UpdatedAt.Format(time.RFC3339)
	}
	page := "issues"
	if c.PullRequest {
		page = "pull"
	}
	userType := "User"
	if strings.HasSuffix(c.Login, "[bot]") {
		userType = "Bot"
	}
	return map[string]any{
		"id": c.ID, "node_id": "IC_kwDO" + strconv.FormatInt(c.ID, 10),
		"url":        "https://api.github.com/repos/" + repo + "/issues/comments/" + strconv.FormatInt(c.ID, 10),
		"html_url":   "https://github.com/" + repo + "/" + page + "/" + n + "#issuecomment-" + strconv.FormatInt(c.ID, 10),
		"issue_url":  "https://api.github.com/repos/" + repo + "/issues/" + n,
		"body":       c.Body,
		"user":       map[string]any{"login": c.Login, "id": c.UserID, "type": userType, "site_admin": false},
		"created_at": at, "updated_at": updated, "author_association": "NONE",
	}
}

func writeJSON(w http.ResponseWriter, code int, header map[string]string, body string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Api-Version-Selected", "2022-11-28")
	if _, ok := header["X-RateLimit-Remaining"]; !ok {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-RateLimit-Reset", "1700003600")
		w.Header().Set("X-RateLimit-Resource", "core")
	}
	for k, v := range header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// hijackClose drops the connection without an answer.
func hijackClose(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

// commentRef is the provider message reference of comment id on ch.
func commentRef(ch model.ChannelRef, id int64) model.ExternalRef {
	return model.ProviderMessageRef(ch, "comment:"+strconv.FormatInt(id, 10))
}
