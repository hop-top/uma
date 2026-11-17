package linear_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// fakeLinear is a stand-in for Linear's GraphQL endpoint, answering the
// operations the adapter sends in the shapes of Linear's published schema
// (https://github.com/linear/linear/blob/master/packages/sdk/src/schema.graphql:
// CommentCreateInput.id, commentCreate, commentUpdate, commentDelete,
// query comment, query issue) until recorded exchanges replace it. Every
// error shape (not found, duplicate id, rate limit) is scripted from the
// descriptions in https://linear.app/developers/graphql and
// https://linear.app/developers/rate-limiting, not observed. Each account is
// a path prefix of the API URL with its own data, script and call record.
// It is an in-process ports.HTTPClient: no network is ever reached.
type fakeLinear struct {
	clock func() time.Time

	mu       sync.Mutex
	accounts map[string]*fakeAccount
}

type fakeAccount struct {
	userID   string
	comments map[string]*fakeComment
	issues   map[string]*fakeIssue
	script   []answer
	calls    []fakeCall
	// onApply runs after a comment is stored and before its answer: a
	// webhook racing the response.
	onApply func(fakeComment)
}

type fakeComment struct {
	ID, IssueID, Body, UserID string
	Created, Edited           time.Time
	Archived                  bool
}

type fakeIssue struct {
	ID, Identifier, Title, Team, StateType, StateName, Description string
	Creator                                                        string
	Labels                                                         []string
	Updated                                                        time.Time
}

// fakeCall is one request the account received.
type fakeCall struct {
	Op      string // commentCreate, commentUpdate, commentDelete, comment, issue
	Vars    map[string]any
	Auth    string
	Applied bool
	// Comment is the comment ID the call created, changed or read.
	Comment string
}

// answer is one scripted answer to a write. The zero value accepts.
type answer struct {
	apply, drop bool
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

const fakeHost = "linear.fake.test"

func newFakeLinear(clock func() time.Time) *fakeLinear {
	return &fakeLinear{clock: clock, accounts: map[string]*fakeAccount{}}
}

func (f *fakeLinear) account(prefix, userID string) *fakeAccount {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.accounts[prefix]
	if !ok {
		a = &fakeAccount{userID: userID, comments: map[string]*fakeComment{}, issues: map[string]*fakeIssue{}}
		f.accounts[prefix] = a
	}
	return a
}

func (f *fakeLinear) apiURL(prefix string) string {
	return "https://" + fakeHost + "/" + prefix + "/graphql"
}

func (f *fakeLinear) respond(prefix string, as ...answer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].script = append(f.accounts[prefix].script, as...)
}

func (f *fakeLinear) callsOf(prefix string) []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.accounts[prefix].calls)
}

func (f *fakeLinear) putIssue(prefix string, i fakeIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i.Team == "" {
		i.Team = "ENG"
	}
	if i.StateType == "" {
		i.StateType, i.StateName = "started", "In Progress"
	}
	f.accounts[prefix].issues[i.ID] = &i
}

func (f *fakeLinear) hasIssue(prefix, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.accounts[prefix].issues[id]
	return ok
}

func (f *fakeLinear) putComment(prefix string, c fakeComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cc := c
	f.accounts[prefix].comments[c.ID] = &cc
}

func (f *fakeLinear) comment(prefix, id string) (fakeComment, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.accounts[prefix].comments[id]
	if !ok {
		return fakeComment{}, false
	}
	return *c, true
}

func (f *fakeLinear) commentsOn(prefix, issue string) []fakeComment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeComment
	for _, c := range f.accounts[prefix].comments {
		if c.IssueID == issue {
			out = append(out, *c)
		}
	}
	slices.SortFunc(out, func(a, b fakeComment) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// Do implements ports.HTTPClient.
func (f *fakeLinear) Do(r *http.Request) (*http.Response, error) {
	if r.URL.Host != fakeHost {
		return nil, fmt.Errorf("fake Linear: refusing request to %s", r.URL.Host)
	}
	prefix, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	f.mu.Lock()
	a := f.accounts[prefix]
	f.mu.Unlock()
	if a == nil || r.Method != http.MethodPost || json.Unmarshal(raw, &req) != nil {
		return reply(http.StatusBadRequest, nil, `{"errors":[{"message":"Bad request"}]}`), nil
	}
	call := fakeCall{Vars: req.Variables, Auth: r.Header.Get("Authorization")}
	switch {
	case strings.Contains(req.Query, "commentCreate("):
		call.Op = "commentCreate"
	case strings.Contains(req.Query, "commentUpdate("):
		call.Op = "commentUpdate"
	case strings.Contains(req.Query, "commentDelete("):
		call.Op = "commentDelete"
	case strings.Contains(req.Query, "comment(id"):
		call.Op = "comment"
	case strings.Contains(req.Query, "issue(id"):
		call.Op = "issue"
	}
	switch call.Op {
	case "comment", "issue":
		return f.read(a, call), nil
	case "":
		return reply(http.StatusBadRequest, nil, `{"errors":[{"message":"Syntax Error"}]}`), nil
	}
	return f.write(a, call)
}

func reply(code int, h map[string]string, body string) *http.Response {
	resp := &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewBufferString(body))}
	for k, v := range h {
		resp.Header.Set(k, v)
	}
	return resp
}

func gqlErr(msg, typ string) string {
	return fmt.Sprintf(`{"errors":[{"message":%q,"extensions":{"type":%q,"userError":true}}],"data":null}`, msg, typ)
}

func (f *fakeLinear) write(a *fakeAccount, c fakeCall) (*http.Response, error) {
	f.mu.Lock()
	ans := accepted
	if len(a.script) > 0 {
		ans, a.script = a.script[0], a.script[1:]
	}
	if ans.unreachable {
		a.calls = append(a.calls, c)
		f.mu.Unlock()
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	input, _ := c.Vars["input"].(map[string]any)
	var stored fakeComment
	body := ""
	status := http.StatusOK
	switch c.Op {
	case "commentCreate":
		id, _ := input["id"].(string)
		issue, _ := input["issueId"].(string)
		text, _ := input["body"].(string)
		_, known := a.issues[issue]
		_, dup := a.comments[id]
		switch {
		case ans.status == 0 && (!known || text == ""):
			status, body = http.StatusBadRequest, gqlErr("Entity not found: Issue - Could not find referenced Issue.", "invalid input")
			if known {
				body = gqlErr("body is required", "invalid input")
			}
		case ans.status == 0 && dup:
			status, body = http.StatusBadRequest, gqlErr("Duplicate comment id", "invalid input")
		case ans.apply:
			stored = fakeComment{ID: id, IssueID: issue, Body: text, UserID: a.userID, Created: f.clock().UTC()}
			a.comments[id] = &stored
			c.Applied, c.Comment = true, id
			body = fmt.Sprintf(`{"data":{"commentCreate":{"success":true,"comment":{"id":%q}}}}`, id)
		}
	case "commentUpdate":
		id, _ := c.Vars["id"].(string)
		text, _ := input["body"].(string)
		cm, ok := a.comments[id]
		switch {
		case ans.status == 0 && (!ok || cm.Archived):
			status, body = http.StatusBadRequest, gqlErr("Entity not found: Comment", "invalid input")
		case ans.apply:
			cm.Body, cm.Edited = text, f.clock().UTC()
			c.Applied, c.Comment = true, id
			body = fmt.Sprintf(`{"data":{"commentUpdate":{"success":true,"comment":{"id":%q}}}}`, id)
		}
	case "commentDelete":
		id, _ := c.Vars["id"].(string)
		cm, ok := a.comments[id]
		switch {
		case ans.status == 0 && (!ok || cm.Archived):
			status, body = http.StatusBadRequest, gqlErr("Entity not found: Comment", "invalid input")
		case ans.apply:
			cm.Archived = true
			c.Applied, c.Comment = true, id
			body = `{"data":{"commentDelete":{"success":true}}}`
		}
	}
	a.calls = append(a.calls, c)
	hook := a.onApply
	f.mu.Unlock()
	if c.Applied && c.Op == "commentCreate" && hook != nil {
		hook(stored)
	}
	switch {
	case ans.drop:
		return nil, errors.New("connection reset by peer")
	case ans.status != 0:
		return reply(ans.status, ans.header, ans.body), nil
	}
	return reply(status, nil, body), nil
}

func ts(t time.Time) string {
	if t.IsZero() {
		return "null"
	}
	return `"` + t.UTC().Format("2006-01-02T15:04:05.000Z") + `"`
}

func (f *fakeLinear) read(a *fakeAccount, c fakeCall) *http.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := c.Vars["id"].(string)
	c.Comment = id
	a.calls = append(a.calls, c)
	if len(a.script) > 0 && a.script[0].status != 0 && !a.script[0].apply {
		// A scripted status answers the next read too: a failing read.
		ans := a.script[0]
		a.script = a.script[1:]
		return reply(ans.status, ans.header, ans.body)
	}
	if c.Op == "issue" {
		i, ok := a.issues[id]
		if !ok {
			return reply(http.StatusBadRequest, nil, gqlErr("Entity not found: Issue - Could not find referenced Issue.", "invalid input"))
		}
		labels := []string{}
		for _, l := range i.Labels {
			labels = append(labels, fmt.Sprintf(`{"name":%q}`, l))
		}
		creator := "null"
		if i.Creator != "" {
			creator = fmt.Sprintf(`{"id":%q,"name":"Someone"}`, i.Creator)
		}
		return reply(http.StatusOK, nil, fmt.Sprintf(
			`{"data":{"issue":{"id":%q,"identifier":%q,"number":1,"title":%q,"url":"https://linear.app/example/issue/%s","description":%q,"createdAt":%s,"updatedAt":%s,"archivedAt":null,"trashed":false,"team":{"id":"t1","key":%q},"state":{"id":"s1","name":%q,"type":%q},"creator":%s,"assignee":null,"labels":{"nodes":[%s]}}}}`,
			i.ID, i.Identifier, i.Title, i.Identifier, i.Description, ts(f.clock()), ts(i.Updated), i.Team, i.StateName, i.StateType, creator, strings.Join(labels, ",")))
	}
	cm, ok := a.comments[id]
	if !ok {
		return reply(http.StatusBadRequest, nil, gqlErr("Entity not found: Comment", "invalid input"))
	}
	iss := a.issues[cm.IssueID]
	archived, edited := "null", "null"
	if cm.Archived {
		archived = ts(f.clock())
	}
	if !cm.Edited.IsZero() {
		edited = ts(cm.Edited)
	}
	upd := cm.Created
	if !cm.Edited.IsZero() {
		upd = cm.Edited
	}
	team, ident, title := "ENG", "ENG-1", "Export fails"
	if iss != nil {
		team, ident, title = iss.Team, iss.Identifier, iss.Title
	}
	return reply(http.StatusOK, nil, fmt.Sprintf(
		`{"data":{"comment":{"id":%q,"body":%q,"createdAt":%s,"updatedAt":%s,"editedAt":%s,"archivedAt":%s,"url":"https://linear.app/example/issue/%s#comment-1","issueId":%q,"parentId":null,"user":{"id":%q,"name":"Someone","app":false},"botActor":null,"issue":{"id":%q,"identifier":%q,"title":%q,"url":"https://linear.app/example/issue/%s","team":{"key":%q}}}}}`,
		cm.ID, cm.Body, ts(cm.Created), ts(upd), edited, archived, ident, cm.IssueID, cm.UserID, cm.IssueID, ident, title, ident, team))
}
