package github_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// fakeReaction is one reaction in the documented "reaction" shape.
type fakeReaction struct {
	ID      int64
	Content string
	UserID  int64
	Login   string
}

func reactionJSON(r fakeReaction, at time.Time) map[string]any {
	return map[string]any{
		"id": r.ID, "node_id": "REA_kwDO" + strconv.FormatInt(r.ID, 10), "content": r.Content,
		"user":       map[string]any{"login": r.Login, "id": r.UserID, "type": "User", "site_admin": false},
		"created_at": at.UTC().Format(time.RFC3339),
	}
}

const (
	commentsDocsURL  = "https://docs.github.com/rest/issues/comments#update-an-issue-comment"
	reactionsDocsURL = "https://docs.github.com/rest/reactions/reactions"
)

func notFound(w http.ResponseWriter, docs string) {
	writeJSON(w, http.StatusNotFound, nil, `{"message":"Not Found","documentation_url":"`+docs+`","status":"404"}`)
}

// mutationPath is a parsed comment or reaction path of a repository.
type mutationPath struct {
	repo string
	// target is "comment:repo#id" or "issue:repo#number"; empty for the
	// comment itself (see comment).
	target   string
	comment  int64
	reaction int64
	// reactions is set for the reactions collection or one of its members.
	reactions bool
}

// parseMutationPath parses
//
//	repos/{o}/{r}/issues/comments/{id}
//	repos/{o}/{r}/issues/comments/{id}/reactions[/{rid}]
//	repos/{o}/{r}/issues/{n}/reactions[/{rid}]
func parseMutationPath(p string) (mutationPath, bool) {
	s := strings.Split(p, "/")
	if len(s) < 6 || s[0] != "repos" || s[3] != "issues" {
		return mutationPath{}, false
	}
	m := mutationPath{repo: s[1] + "/" + s[2]}
	num := func(v string) int64 { n, _ := strconv.ParseInt(v, 10, 64); return n }
	if s[4] == "comments" {
		m.comment = num(s[5])
		if m.comment == 0 {
			return mutationPath{}, false
		}
		m.target = "comment:" + m.repo + "#" + s[5]
		switch {
		case len(s) == 6:
			return m, true
		case len(s) >= 7 && s[6] == "reactions" && len(s) <= 8:
			m.reactions = true
			if len(s) == 8 {
				m.reaction = num(s[7])
			}
			return m, m.reaction != 0 || len(s) == 7
		}
		return mutationPath{}, false
	}
	if num(s[4]) == 0 || s[5] != "reactions" || len(s) > 7 {
		return mutationPath{}, false
	}
	m.target, m.reactions = "issue:"+m.repo+"#"+s[4], true
	if len(s) == 7 {
		if m.reaction = num(s[6]); m.reaction == 0 {
			return mutationPath{}, false
		}
	}
	return m, true
}

// take consumes the next scripted answer; f.mu is held.
func (a *fakeAccount) take() answer {
	if len(a.script) == 0 {
		return accepted
	}
	var ans answer
	ans, a.script = a.script[0], a.script[1:]
	return ans
}

// serveMutation answers the comment edit and delete and the reaction
// endpoints, and reports whether the request was one of them.
func (f *fakeGitHub) serveMutation(w http.ResponseWriter, r *http.Request, a *fakeAccount, rest string, c fakeCall, body []byte) bool {
	m, ok := parseMutationPath(rest)
	if !ok {
		return false
	}
	switch {
	case !m.reactions && (r.Method == http.MethodPatch || r.Method == http.MethodDelete):
		f.mutateComment(w, r, a, m, c, body)
	case m.reactions && r.Method == http.MethodGet && m.reaction == 0:
		f.listReactions(w, r, a, m, c)
	case m.reactions && r.Method == http.MethodPost && m.reaction == 0:
		f.createReaction(w, a, m, c, body)
	case m.reactions && r.Method == http.MethodDelete && m.reaction != 0:
		f.deleteReaction(w, a, m, c)
	default:
		return false
	}
	return true
}

// findComment returns the comment id of repo and where it is stored; f.mu
// is held.
func (a *fakeAccount) findComment(repo string, id int64) (issue string, i int, ok bool) {
	for issue, cs := range a.comments {
		if !strings.HasPrefix(issue, repo+"#") {
			continue
		}
		for i, cm := range cs {
			if cm.ID == id {
				return issue, i, true
			}
		}
	}
	return "", 0, false
}

// issueOf is the "owner/repo#number" of the issue a comment or reaction
// path belongs to; f.mu is held.
func (a *fakeAccount) issueOf(m mutationPath) string {
	if m.comment == 0 {
		return strings.TrimPrefix(m.target, "issue:")
	}
	issue, _, _ := a.findComment(m.repo, m.comment)
	return issue
}

// answerWrite finishes a write whose scripted answer is ans; it reports
// whether it answered. f.mu has been released.
func answerWrite(w http.ResponseWriter, ans answer) bool {
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

func (f *fakeGitHub) mutateComment(w http.ResponseWriter, r *http.Request, a *fakeAccount, m mutationPath, c fakeCall, body []byte) {
	f.mu.Lock()
	ans := a.take()
	issue, i, found := a.findComment(m.repo, m.comment)
	c.Comment, c.Issue = m.comment, issue
	forbidden := found && a.ownEditsOnly && a.comments[issue][i].UserID != a.userID
	var out []byte
	if found && !forbidden && ans.apply {
		c.Applied = true
		cm := &a.comments[issue][i]
		if r.Method == http.MethodPatch {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.Unmarshal(body, &req)
			cm.Body, cm.UpdatedAt = req.Body, f.clock().UTC()
			out, _ = json.Marshal(commentJSON(*cm))
		} else {
			a.comments[issue] = append(a.comments[issue][:i:i], a.comments[issue][i+1:]...)
		}
	}
	a.calls = append(a.calls, c)
	f.mu.Unlock()
	switch {
	case answerWrite(w, ans):
	case !found:
		notFound(w, commentsDocsURL)
	case forbidden:
		writeJSON(w, http.StatusForbidden, nil, `{"message":"Resource not accessible by integration","documentation_url":"`+commentsDocsURL+`","status":"403"}`)
	case r.Method == http.MethodDelete:
		writeJSON(w, http.StatusNoContent, nil, "")
	default:
		writeJSON(w, http.StatusOK, nil, string(out))
	}
}

func (f *fakeGitHub) listReactions(w http.ResponseWriter, r *http.Request, a *fakeAccount, m mutationPath, c fakeCall) {
	q := r.URL.Query()
	f.mu.Lock()
	c.Issue = a.issueOf(m)
	a.calls = append(a.calls, c)
	if m.comment != 0 {
		if _, _, found := a.findComment(m.repo, m.comment); !found {
			f.mu.Unlock()
			notFound(w, reactionsDocsURL)
			return
		}
	}
	var all []map[string]any
	for _, rx := range a.reactions[m.target] {
		if want := q.Get("content"); want == "" || want == rx.Content {
			all = append(all, reactionJSON(rx, f.clock()))
		}
	}
	f.mu.Unlock()
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

// validContents are the reaction contents GitHub accepts.
var validContents = []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"}

func (f *fakeGitHub) createReaction(w http.ResponseWriter, a *fakeAccount, m mutationPath, c fakeCall, body []byte) {
	var req struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	ans := a.take()
	validContent := false
	for _, v := range validContents {
		validContent = validContent || v == req.Content
	}
	target := true
	c.Issue = a.issueOf(m)
	if m.comment != 0 {
		_, _, target = a.findComment(m.repo, m.comment)
	}
	status := http.StatusCreated
	var rx fakeReaction
	if validContent && target && ans.apply {
		c.Applied = true
		existing := false
		for _, e := range a.reactions[m.target] {
			if e.UserID == a.userID && e.Content == req.Content {
				rx, existing = e, true
			}
		}
		if existing {
			status = http.StatusOK
		} else {
			a.nextReaction++
			rx = fakeReaction{ID: 7000 + a.nextReaction, Content: req.Content, UserID: a.userID, Login: a.login}
			a.reactions[m.target] = append(a.reactions[m.target], rx)
		}
		c.Reaction = rx.ID
	}
	c.Content = req.Content
	a.calls = append(a.calls, c)
	f.mu.Unlock()
	switch {
	case answerWrite(w, ans):
	case !target:
		notFound(w, reactionsDocsURL)
	case !validContent:
		writeJSON(w, http.StatusUnprocessableEntity, nil, `{"message":"Validation Failed","errors":[{"resource":"Reaction","code":"custom","field":"content","message":"Content is not included in the list"}],"documentation_url":"`+reactionsDocsURL+`","status":"422"}`)
	default:
		out, _ := json.Marshal(reactionJSON(rx, f.clock()))
		writeJSON(w, status, nil, string(out))
	}
}

func (f *fakeGitHub) deleteReaction(w http.ResponseWriter, a *fakeAccount, m mutationPath, c fakeCall) {
	f.mu.Lock()
	ans := a.take()
	rs := a.reactions[m.target]
	idx := -1
	for i, rx := range rs {
		if rx.ID == m.reaction {
			idx = i
		}
	}
	c.Reaction, c.Issue = m.reaction, a.issueOf(m)
	if idx >= 0 {
		c.Content = rs[idx].Content
	}
	if idx >= 0 && ans.apply {
		c.Applied = true
		a.reactions[m.target] = append(rs[:idx:idx], rs[idx+1:]...)
	}
	a.calls = append(a.calls, c)
	f.mu.Unlock()
	switch {
	case answerWrite(w, ans):
	case idx < 0:
		notFound(w, reactionsDocsURL)
	default:
		writeJSON(w, http.StatusNoContent, nil, "")
	}
}

// seedReaction stores a reaction on target ("comment:owner/repo#id" or
// "issue:owner/repo#number") by userID.
func (f *fakeGitHub) seedReaction(prefix, target string, userID int64, content string) fakeReaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	a.nextReaction++
	rx := fakeReaction{ID: 7000 + a.nextReaction, Content: content, UserID: userID, Login: "someone"}
	a.reactions[target] = append(a.reactions[target], rx)
	return rx
}

func (f *fakeGitHub) reactionsOn(prefix, target string) []fakeReaction {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeReaction(nil), f.accounts[prefix].reactions[target]...)
}

// callsTo returns the calls with the method.
func (f *fakeGitHub) callsWith(prefix, method string) []fakeCall {
	var out []fakeCall
	for _, c := range f.calls(prefix) {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGitHub) setOwnEditsOnly(prefix string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts[prefix].ownEditsOnly = v
}

// putComment stores comment cm, with its own ID, as if written earlier; a
// stored comment with that ID is replaced.
func (f *fakeGitHub) putComment(prefix string, cm fakeComment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.accounts[prefix]
	if issue, i, ok := a.findComment(strings.SplitN(cm.Issue, "#", 2)[0], cm.ID); ok {
		a.comments[issue][i] = cm
		return
	}
	a.comments[cm.Issue] = append(a.comments[cm.Issue], cm)
}
