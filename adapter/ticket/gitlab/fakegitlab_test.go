package gitlab_test

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
)

// fakeGitLab is a stand-in for the GitLab REST API v4 endpoints the adapter
// calls, until a sandbox recording replaces it (docs/recording/gitlab.md).
// Shapes follow the documented answers:
//
//   - notes: https://docs.gitlab.com/api/notes/ (create 201, edit 200, delete
//     204, list newest first by default, 20 per page by default, X-Next-Page)
//   - issues: https://docs.gitlab.com/api/issues/ (single issue, edit with
//     state_event close and reopen; state is "opened" or "closed")
//   - rate limits: https://docs.gitlab.com/administration/settings/user_and_ip_rate_limits/
//     (429 with Retry-After seconds and RateLimit-Reset epoch)
//
// Each account is a path prefix ("/<account>/api/v4/"), its own API root
// with its own issues, notes, script and call record. Quick actions in note
// bodies are not run: a test that cares reads the stored body.
type fakeGitLab struct {
	srv   *httptest.Server
	clock func() time.Time
	// token is the API token the fake accepts.
	token string

	mu       sync.Mutex
	accounts map[string]*fakeAccount
}

type fakeAccount struct {
	userID   int64
	username string
	nextNote int64
	issues   map[string]*fakeIssue // by "project#iid"
	notes    []*fakeNote
	script   []answer
	calls    []fakeCall
	// noOwnership makes the account's edits and deletes of other users'
	// notes answer 403.
	noOwnership bool
}

type fakeIssue struct {
	ID, IID, Project int64
	Path             string
	Title            string
	Description      string
	State            string
	Author           int64
	Labels           []string
	Created, Updated time.Time
}

type fakeNote struct {
	ID               int64
	Issue            string
	Body             string
	Author           int64
	Username         string
	Internal, System bool
	Created, Updated time.Time
}

// fakeCall is one request the account received.
type fakeCall struct {
	Method, Path, Query, Body, Auth string
	Applied                         bool
	Note                            int64
}

// answer is one scripted answer to the next request. The zero value serves
// the request as usual.
type answer struct {
	// apply performs the write before the failure below, so the answer is
	// lost though the effect applied; drop closes the connection.
	apply, drop bool
	// unreachable fails the dial: the request never reaches GitLab.
	unreachable bool
	status      int
	header      map[string]string
	body        string
}

var (
	accepted    = answer{}
	answerLost  = answer{apply: true, drop: true}
	requestLost = answer{drop: true}
	unreachable = answer{unreachable: true}
)

func statusAnswer(code int, body string, header map[string]string) answer {
	return answer{status: code, body: body, header: header}
}

func newFakeGitLab(t *testing.T, clock func() time.Time) *fakeGitLab {
	f := &fakeGitLab{clock: clock, token: apiToken, accounts: map[string]*fakeAccount{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitLab) account(prefix string, userID int64) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{userID: userID, username: "project_" + strconv.FormatInt(sandboxProject, 10) + "_bot_fixture", nextNote: 1000, issues: map[string]*fakeIssue{}}
		f.accounts[prefix] = a
	}
	return a
}

func (f *fakeGitLab) apiURL(prefix string) string { return f.srv.URL + "/" + prefix + "/api/v4/" }

func (f *fakeGitLab) client() *http.Client {
	base := f.srv.Client().Transport
	host := strings.TrimPrefix(f.srv.URL, "https://")
	return &http.Client{CheckRedirect: noRedirect, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != host {
			return nil, fmt.Errorf("fake GitLab: refusing request to %s", r.URL.Host)
		}
		prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if r.Method != http.MethodGet {
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

func (f *fakeGitLab) respond(prefix string, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.script = append(a.script, as...)
}

func (f *fakeGitLab) calls(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a := f.accounts[prefix]; a != nil {
		return slices.Clone(a.calls)
	}
	return nil
}

func (f *fakeGitLab) setIssue(prefix string, project, iid int64, author int64, state string, at time.Time) *fakeIssue {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	key := issueKey(project, iid)
	iss, ok := a.issues[key]
	if !ok {
		iss = &fakeIssue{
			ID: 3_000_000 + iid, IID: iid, Project: project, Path: sandboxPath, Title: "Export fails",
			Author: author, Created: at,
		}
		a.issues[key] = iss
	}
	iss.State, iss.Updated = state, at
	return iss
}

func (f *fakeGitLab) hasIssue(prefix string, project, iid int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.accounts[prefix].issues[issueKey(project, iid)]
	return ok
}

func (f *fakeGitLab) issue(prefix string, project, iid int64) (fakeIssue, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.accounts[prefix].issues[issueKey(project, iid)]
	if !ok {
		return fakeIssue{}, false
	}
	return *iss, true
}

func issueKey(project, iid int64) string {
	return strconv.FormatInt(project, 10) + "#" + strconv.FormatInt(iid, 10)
}

// seed stores a note as if written earlier, by userID.
func (f *fakeGitLab) seed(prefix string, project, iid int64, userID int64, body string, at time.Time) *fakeNote {
	return f.putNote(prefix, fakeNote{Issue: issueKey(project, iid), Body: body, Author: userID, Username: "someone", Created: at, Updated: at})
}

func (f *fakeGitLab) putNote(prefix string, n fakeNote) *fakeNote {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	if n.ID == 0 {
		a.nextNote++
		n.ID = a.nextNote
	}
	if n.Updated.IsZero() {
		n.Updated = n.Created
	}
	a.notes = append(a.notes, &n)
	return &n
}

func (f *fakeGitLab) note(prefix string, id int64) (fakeNote, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.accounts[prefix].notes {
		if n.ID == id {
			return *n, true
		}
	}
	return fakeNote{}, false
}

func ts(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

func noteJSON(n fakeNote) map[string]any {
	project, iid, _ := strings.Cut(n.Issue, "#")
	p, _ := strconv.Atoi(project)
	i, _ := strconv.Atoi(iid)
	return map[string]any{
		"id": n.ID, "type": nil, "body": n.Body,
		"author":     map[string]any{"id": n.Author, "username": n.Username, "name": n.Username, "state": "active"},
		"created_at": ts(n.Created), "updated_at": ts(n.Updated), "system": n.System,
		"noteable_id": 3_000_000 + i, "noteable_type": "Issue", "project_id": p, "noteable_iid": i,
		"resolvable": false, "internal": n.Internal,
	}
}

func issueJSON(i fakeIssue) map[string]any {
	labels := i.Labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id": i.ID, "iid": i.IID, "project_id": i.Project, "title": i.Title, "description": i.Description,
		"state": i.State, "created_at": ts(i.Created), "updated_at": ts(i.Updated), "labels": labels,
		"author":    map[string]any{"id": i.Author, "username": "person" + strconv.FormatInt(i.Author, 10), "state": "active"},
		"assignees": []any{}, "web_url": "https://gitlab.example.com/" + i.Path + "/-/issues/" + strconv.FormatInt(i.IID, 10),
		"confidential": false, "references": map[string]any{"full": i.Path + "#" + strconv.FormatInt(i.IID, 10)},
	}
}

func writeJSON(w http.ResponseWriter, code int, header map[string]string, body string) {
	for k, v := range header {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func hijackClose(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

const notFound = `{"message":"404 Not found"}`

func (f *fakeGitLab) serve(w http.ResponseWriter, r *http.Request) {
	prefix, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil {
		writeJSON(w, http.StatusNotFound, nil, notFound)
		return
	}
	c := fakeCall{Method: r.Method, Path: "/" + rest, Query: r.URL.RawQuery, Body: string(body), Auth: r.Header.Get("Authorization")}
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		f.record(a, c)
		writeJSON(w, http.StatusUnauthorized, nil, `{"message":"401 Unauthorized"}`)
		return
	}
	// api/v4/projects/{p}/issues/{i}[/notes[/{id}]]
	s := strings.Split(rest, "/")
	if len(s) < 6 || s[0] != "api" || s[1] != "v4" || s[2] != "projects" || s[4] != "issues" {
		f.record(a, c)
		writeJSON(w, http.StatusNotFound, nil, notFound)
		return
	}
	key := s[3] + "#" + s[5]
	f.mu.Lock()
	ans := accepted
	if len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	f.mu.Unlock()
	switch {
	case len(s) == 6:
		f.issueCall(w, r, a, key, c, body, ans)
	case len(s) == 7 && s[6] == "notes":
		f.notesCall(w, r, a, key, c, body, ans)
	case len(s) == 8 && s[6] == "notes":
		id, _ := strconv.ParseInt(s[7], 10, 64)
		f.noteCall(w, r, a, key, id, c, body, ans)
	default:
		f.record(a, c)
		writeJSON(w, http.StatusNotFound, nil, notFound)
	}
}

func (f *fakeGitLab) record(a *fakeAccount, c fakeCall) {
	f.mu.Lock()
	a.calls = append(a.calls, c)
	f.mu.Unlock()
}

// scripted answers the request with ans when it fails; true when it did.
func (f *fakeGitLab) scripted(w http.ResponseWriter, ans answer) bool {
	switch {
	case ans.drop:
		hijackClose(w)
		return true
	case ans.status != 0:
		writeJSON(w, ans.status, ans.header, ans.body)
		return true
	}
	return false
}

func (f *fakeGitLab) issueCall(w http.ResponseWriter, r *http.Request, a *fakeAccount, key string, c fakeCall, body []byte, ans answer) {
	f.mu.Lock()
	iss, ok := a.issues[key]
	if ok && r.Method == http.MethodPut && (ans.apply || (ans.status == 0 && !ans.drop)) {
		var req struct {
			StateEvent  string  `json:"state_event"`
			Description *string `json:"description"`
		}
		_ = json.Unmarshal(body, &req)
		switch req.StateEvent {
		case "close":
			iss.State = "closed"
		case "reopen":
			iss.State = "opened"
		}
		if req.Description != nil {
			iss.Description = *req.Description
		}
		iss.Updated = f.clock().UTC()
		c.Applied = true
	}
	a.calls = append(a.calls, c)
	var out []byte
	if ok {
		out, _ = json.Marshal(issueJSON(*iss))
	}
	f.mu.Unlock()
	switch {
	case f.scripted(w, ans):
	case !ok:
		writeJSON(w, http.StatusNotFound, nil, notFound)
	case r.Method == http.MethodGet || r.Method == http.MethodPut:
		writeJSON(w, http.StatusOK, nil, string(out))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, nil, `{"message":"405 Method Not Allowed"}`)
	}
}

func (f *fakeGitLab) notesCall(w http.ResponseWriter, r *http.Request, a *fakeAccount, key string, c fakeCall, body []byte, ans answer) {
	f.mu.Lock()
	_, issueOK := a.issues[key]
	f.mu.Unlock()
	if !issueOK {
		f.record(a, c)
		writeJSON(w, http.StatusNotFound, nil, notFound)
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Body string `json:"body"`
		}
		var stored fakeNote
		f.mu.Lock()
		if json.Unmarshal(body, &req) != nil || req.Body == "" {
			ans = statusAnswer(http.StatusBadRequest, `{"error":"body is missing"}`, nil)
		}
		if ans.apply || (ans.status == 0 && !ans.drop) {
			a.nextNote++
			now := f.clock().UTC()
			stored = fakeNote{ID: a.nextNote, Issue: key, Body: req.Body, Author: a.userID, Username: a.username, Created: now, Updated: now}
			a.notes = append(a.notes, &stored)
			c.Applied, c.Note = true, stored.ID
		}
		a.calls = append(a.calls, c)
		f.mu.Unlock()
		if f.scripted(w, ans) {
			return
		}
		out, _ := json.Marshal(noteJSON(stored))
		writeJSON(w, http.StatusCreated, nil, string(out))
	case http.MethodGet:
		f.listNotes(w, r, a, key, c, ans)
	default:
		f.record(a, c)
		writeJSON(w, http.StatusMethodNotAllowed, nil, `{"message":"405 Method Not Allowed"}`)
	}
}

// listNotes answers "List project issue notes": newest first unless
// sort=asc, ordered by created_at, per_page and page, with X-Next-Page.
func (f *fakeGitLab) listNotes(w http.ResponseWriter, r *http.Request, a *fakeAccount, key string, c fakeCall, ans answer) {
	q := r.URL.Query()
	f.mu.Lock()
	a.calls = append(a.calls, c)
	var ns []fakeNote
	for _, n := range a.notes {
		if n.Issue == key {
			ns = append(ns, *n)
		}
	}
	f.mu.Unlock()
	slices.SortStableFunc(ns, func(x, y fakeNote) int {
		if d := x.Created.Compare(y.Created); d != 0 {
			return d
		}
		return int(x.ID - y.ID)
	})
	if q.Get("sort") != "asc" {
		slices.Reverse(ns)
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if perPage < 1 || perPage > 100 {
		perPage = 20
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	lo := min((page-1)*perPage, len(ns))
	hi := min(lo+perPage, len(ns))
	out := []any{}
	for _, n := range ns[lo:hi] {
		out = append(out, noteJSON(n))
	}
	h := map[string]string{"X-Page": strconv.Itoa(page), "X-Per-Page": strconv.Itoa(perPage)}
	if hi < len(ns) {
		h["X-Next-Page"] = strconv.Itoa(page + 1)
	} else {
		h["X-Next-Page"] = ""
	}
	if f.scripted(w, ans) {
		return
	}
	b, _ := json.Marshal(out)
	writeJSON(w, http.StatusOK, h, string(b))
}

func (f *fakeGitLab) noteCall(w http.ResponseWriter, r *http.Request, a *fakeAccount, key string, id int64, c fakeCall, body []byte, ans answer) {
	f.mu.Lock()
	var n *fakeNote
	for _, x := range a.notes {
		if x.ID == id && x.Issue == key {
			n = x
		}
	}
	c.Note = id
	deny := a.noOwnership && n != nil && n.Author != a.userID && r.Method != http.MethodGet
	if n != nil && !deny && r.Method != http.MethodGet && (ans.apply || (ans.status == 0 && !ans.drop)) {
		switch r.Method {
		case http.MethodPut:
			var req struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(body, &req)
			n.Body, n.Updated = req.Body, f.clock().UTC()
		case http.MethodDelete:
			a.notes = slices.DeleteFunc(a.notes, func(x *fakeNote) bool { return x.ID == id })
		}
		c.Applied = true
	}
	a.calls = append(a.calls, c)
	var out []byte
	if n != nil {
		out, _ = json.Marshal(noteJSON(*n))
	}
	f.mu.Unlock()
	switch {
	case f.scripted(w, ans):
	case n == nil:
		writeJSON(w, http.StatusNotFound, nil, notFound)
	case deny:
		writeJSON(w, http.StatusForbidden, nil, `{"message":"403 Forbidden"}`)
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		writeJSON(w, http.StatusOK, nil, string(out))
	}
}

// edit stores description as the issue's, as an update would.
func (f *fakeGitLab) edit(prefix string, project, iid int64, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].issues[issueKey(project, iid)].Description = description
}

// setCreated sets the issue's creation time.
func (f *fakeGitLab) setCreated(prefix string, project, iid int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].issues[issueKey(project, iid)].Created = at
}

// setNoteBody replaces a stored note's body, as an edit would.
func (f *fakeGitLab) setNoteBody(prefix string, id int64, body string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.accounts[prefix].notes {
		if n.ID == id {
			n.Body, n.Updated = body, at
		}
	}
}
