package jira_test

import (
	"encoding/json"
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

	"hop.top/uma/adapter/ticket/jira"
	"hop.top/uma/model"
)

// stand-in: fakeJira is a Jira Cloud REST API v3 test server answering in
// the shapes Atlassian documents for the endpoints the adapter uses
// (https://developer.atlassian.com/cloud/jira/platform/rest/v3/):
//
//	GET    /rest/api/3/issue/{id}
//	POST   /rest/api/3/issue/{id}/comment            (api-group-issue-comments, addComment)
//	GET    /rest/api/3/issue/{id}/comment            (getComments: startAt, maxResults, orderBy)
//	GET    /rest/api/3/issue/{id}/comment/{cid}      (getComment)
//	PUT    /rest/api/3/issue/{id}/comment/{cid}      (updateComment)
//	DELETE /rest/api/3/issue/{id}/comment/{cid}      (deleteComment)
//	GET    /rest/api/3/comment/{cid}/properties/{k}  (api-group-issue-comment-properties)
//
// Each account (site host) is separate. Whether comment creation honours
// the "properties" field of the request is an assumption taken from the
// documented schema (the recorded exchange confirms it); so is the
// "-created" ordering. A recording replaces this server's answers.
type fakeJira struct {
	t *testing.T

	mu       sync.Mutex
	accounts map[string]*fakeAccount
	nextID   int
	// now is the time stamped on comments the server creates.
	now func() time.Time
}

type fakeAccount struct {
	host     string
	issues   map[string]*fakeIssue
	comments map[string][]*fakeComment // by issue ID, in creation order
	script   []answer
	calls    []fakeCall
	// auth is the Authorization header of the last request.
	auth string
}

type fakeIssue struct {
	ID, Key, Project, Summary, Category, Status string
	Updated                                     time.Time
}

type fakeComment struct {
	ID, Issue, Author string
	Body              json.RawMessage
	Created, Updated  time.Time
	Props             map[string]json.RawMessage
}

// answer scripts what the server does with the next write: nothing special
// (zero), an error status without applying, or applying and then answering
// with an error status (applied).
type answer struct {
	status  int
	applied bool
	retry   string
	// dial fails the connection before it reaches the server.
	dial bool
	// reset applies the request normally and then drops the connection: the
	// answer is lost.
	reset bool
}

type fakeCall struct {
	Method, Path, Query, Body string
	Applied                   bool
	Comment                   string
	Status                    int
}

func newFakeJira(t *testing.T, now func() time.Time) *fakeJira {
	return &fakeJira{t: t, accounts: map[string]*fakeAccount{}, nextID: 10100, now: now}
}

// client reaches the account named by a request's host: requests are
// served in process, so no host needs to resolve.
func (f *fakeJira) client() *http.Client { return &http.Client{Transport: roundTrip(f.round)} }

type roundTrip func(*http.Request) (*http.Response, error)

func (r roundTrip) RoundTrip(req *http.Request) (*http.Response, error) { return r(req) }

func (f *fakeJira) round(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	a := f.accountLocked(r.URL.Host)
	if len(a.script) > 0 && a.script[0].dial && writes(r.Method) {
		s := a.script[0]
		a.script = a.script[1:]
		_ = s
		body := readBody(r)
		a.calls = append(a.calls, fakeCall{Method: r.Method, Path: r.URL.Path, Body: string(body)})
		f.mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	reset := len(a.script) > 0 && a.script[0].reset && writes(r.Method)
	if reset {
		a.script = a.script[1:]
	}
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	if reset {
		return nil, &net.OpError{Op: "read", Net: "tcp", Err: io.ErrUnexpectedEOF}
	}
	return rec.Result(), nil
}

func writes(m string) bool {
	return m == http.MethodPost || m == http.MethodPut || m == http.MethodDelete
}

func (f *fakeJira) account(host string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accountLocked(host)
}

func (f *fakeJira) accountLocked(host string) *fakeAccount {
	a, ok := f.accounts[host]
	if !ok {
		a = &fakeAccount{host: host, issues: map[string]*fakeIssue{}, comments: map[string][]*fakeComment{}}
		f.accounts[host] = a
	}
	return a
}

func (f *fakeJira) id() string {
	f.nextID++
	return strconv.Itoa(f.nextID)
}

func (f *fakeJira) putIssue(a *fakeAccount, id, key, category string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := map[string]string{"new": "To Do", "indeterminate": "In Progress", "done": "Done"}[category]
	a.issues[id] = &fakeIssue{ID: id, Key: key, Project: strings.Split(key, "-")[0], Summary: "Export fails", Category: category, Status: st, Updated: f.now()}
}

func (f *fakeJira) putComment(a *fakeAccount, c *fakeComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.comments[c.Issue] = append(a.comments[c.Issue], c)
}

func (f *fakeJira) respond(a *fakeAccount, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a.script = append(a.script, as...)
}

func (f *fakeJira) callsOf(a *fakeAccount) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(a.calls)
}

func (f *fakeJira) commentsOf(a *fakeAccount, issue string) []*fakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(a.comments[issue])
}

func adfOf(text string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"version": 1, "type": "doc", "content": []any{
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}},
	}})
	return b
}

func jiraTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000-0700") }

func userJSON(id string) map[string]any {
	return map[string]any{"accountId": id, "displayName": "User " + id, "accountType": "atlassian", "active": true}
}

func (f *fakeJira) commentJSON(a *fakeAccount, c *fakeComment) map[string]any {
	return map[string]any{
		"self": "https://" + a.host + "/rest/api/3/issue/" + c.Issue + "/comment/" + c.ID, "id": c.ID,
		"author": userJSON(c.Author), "updateAuthor": userJSON(c.Author), "body": c.Body,
		"created": jiraTime(c.Created), "updated": jiraTime(c.Updated), "jsdPublic": true,
	}
}

func (f *fakeJira) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accountLocked(r.URL.Host)
	a.auth = r.Header.Get("Authorization")
	body := readBody(r)
	call := fakeCall{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery, Body: string(body)}
	finish := func(status int) {
		call.Status = status
		a.calls = append(a.calls, call)
	}
	// The API gateway prefixes the path with /ex/jira/{cloudid}.
	_, api, _ := strings.Cut(r.URL.Path, "/rest/api/3/")
	seg := strings.Split(api, "/")
	// Scripted errors apply to writes, in call order.
	var sc answer
	if writes(r.Method) && len(a.script) > 0 {
		sc, a.script = a.script[0], a.script[1:]
	}
	fail := func(apply func()) bool {
		if sc.status == 0 {
			return false
		}
		if sc.applied {
			apply()
			call.Applied = true
		}
		if sc.retry != "" {
			w.Header().Set("Retry-After", sc.retry)
		}
		finish(sc.status)
		writeJSON(w, sc.status, map[string]any{"errorMessages": []string{"scripted"}, "errors": map[string]any{}})
		return true
	}
	switch {
	case len(seg) == 2 && seg[0] == "issue" && r.Method == http.MethodGet:
		iss, ok := a.issues[seg[1]]
		if !ok {
			writeJSON(w, http.StatusNotFound, notFound())
			return
		}
		writeJSON(w, http.StatusOK, f.issueJSON(a, iss))
	case len(seg) == 3 && seg[0] == "issue" && seg[2] == "comment" && r.Method == http.MethodPost:
		var in struct {
			Body       json.RawMessage `json:"body"`
			Properties []struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(body, &in)
		create := func() *fakeComment {
			c := &fakeComment{ID: f.id(), Issue: seg[1], Author: ownAccount, Body: in.Body, Created: f.now(), Updated: f.now(), Props: map[string]json.RawMessage{}}
			for _, p := range in.Properties {
				c.Props[p.Key] = p.Value
			}
			a.comments[seg[1]] = append(a.comments[seg[1]], c)
			call.Comment = c.ID
			return c
		}
		if _, ok := a.issues[seg[1]]; !ok {
			finish(http.StatusNotFound)
			writeJSON(w, http.StatusNotFound, notFound())
			return
		}
		if fail(func() { create() }) {
			return
		}
		c := create()
		call.Applied = true
		finish(http.StatusCreated)
		writeJSON(w, http.StatusCreated, f.commentJSON(a, c))
	case len(seg) == 3 && seg[0] == "issue" && seg[2] == "comment" && r.Method == http.MethodGet:
		f.list(w, r, a, seg[1])
		finish(http.StatusOK)
	case len(seg) == 4 && seg[0] == "issue" && seg[2] == "comment":
		f.one(w, a, seg, &call, fail, finish, body, r.Method)
	case len(seg) == 4 && seg[0] == "comment" && seg[2] == "properties" && r.Method == http.MethodGet:
		for _, cs := range a.comments {
			for _, c := range cs {
				if c.ID == seg[1] {
					if v, ok := c.Props[seg[3]]; ok {
						writeJSON(w, http.StatusOK, map[string]any{"key": seg[3], "value": v})
						return
					}
				}
			}
		}
		writeJSON(w, http.StatusNotFound, notFound())
	default:
		http.NotFound(w, r)
	}
}

func notFound() map[string]any {
	return map[string]any{"errorMessages": []string{"Issue does not exist or you do not have permission to see it."}, "errors": map[string]any{}}
}

func (f *fakeJira) issueJSON(a *fakeAccount, iss *fakeIssue) map[string]any {
	return map[string]any{"id": iss.ID, "key": iss.Key, "self": "https://" + a.host + "/rest/api/3/issue/" + iss.ID, "fields": map[string]any{
		"summary": iss.Summary, "labels": []string{}, "project": map[string]any{"key": iss.Project},
		"status":   map[string]any{"name": iss.Status, "statusCategory": map[string]any{"key": iss.Category}},
		"reporter": userJSON("reporter-1"), "creator": userJSON("reporter-1"),
		"created": jiraTime(iss.Updated), "updated": jiraTime(iss.Updated),
	}}
}

// list answers getComments: newest first for orderBy=-created.
func (f *fakeJira) list(w http.ResponseWriter, r *http.Request, a *fakeAccount, issue string) {
	if _, ok := a.issues[issue]; !ok {
		writeJSON(w, http.StatusNotFound, notFound())
		return
	}
	q := r.URL.Query()
	list := slices.Clone(a.comments[issue])
	if q.Get("orderBy") == "-created" {
		slices.Reverse(list)
	}
	start, _ := strconv.Atoi(q.Get("startAt"))
	max, _ := strconv.Atoi(q.Get("maxResults"))
	if max <= 0 {
		max = 100
	}
	out := []map[string]any{}
	for i := start; i < len(list) && i < start+max; i++ {
		out = append(out, f.commentJSON(a, list[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"startAt": start, "maxResults": max, "total": len(list), "comments": out})
}

func (f *fakeJira) one(w http.ResponseWriter, a *fakeAccount, seg []string, call *fakeCall, fail func(func()) bool, finish func(int), body []byte, method string) {
	var found *fakeComment
	for _, c := range a.comments[seg[1]] {
		if c.ID == seg[3] {
			found = c
		}
	}
	call.Comment = seg[3]
	switch method {
	case http.MethodGet:
		if found == nil {
			finish(http.StatusNotFound)
			writeJSON(w, http.StatusNotFound, notFound())
			return
		}
		finish(http.StatusOK)
		writeJSON(w, http.StatusOK, f.commentJSON(a, found))
	case http.MethodPut:
		var in struct {
			Body       json.RawMessage `json:"body"`
			Properties []struct {
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			} `json:"properties"`
		}
		_ = json.Unmarshal(body, &in)
		apply := func() {
			if found == nil {
				return
			}
			found.Body, found.Updated = in.Body, f.now()
			for _, p := range in.Properties {
				found.Props[p.Key] = p.Value
			}
		}
		if found == nil {
			finish(http.StatusNotFound)
			writeJSON(w, http.StatusNotFound, notFound())
			return
		}
		if fail(apply) {
			return
		}
		apply()
		call.Applied = true
		finish(http.StatusOK)
		writeJSON(w, http.StatusOK, f.commentJSON(a, found))
	case http.MethodDelete:
		if found == nil {
			finish(http.StatusNotFound)
			writeJSON(w, http.StatusNotFound, notFound())
			return
		}
		apply := func() {
			a.comments[seg[1]] = slices.DeleteFunc(a.comments[seg[1]], func(c *fakeComment) bool { return c.ID == seg[3] })
		}
		if fail(apply) {
			return
		}
		apply()
		call.Applied = true
		finish(http.StatusNoContent)
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json;charset=UTF-8")
	w.WriteHeader(code)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// ownAccount is the accountId the credential acts as.
const ownAccount = "5b10ac8d82e05b22cc7d4ef5"

var _ = jira.Namespace
var _ = model.Public

func readBody(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	b, _ := io.ReadAll(r.Body)
	return b
}
