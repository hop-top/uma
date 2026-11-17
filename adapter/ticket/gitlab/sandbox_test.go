package gitlab_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Recordings run against a real sandbox project, issue and account, but
// cassettes and the code under test only ever see these placeholders, in
// every mode. The sandbox transport translates between them and the real
// ones: requests leave the adapter naming the placeholders and reach GitLab
// with the real values; answers come back with every real identifier
// replaced.
const (
	sandboxIssue   int64 = 4242
	sandboxIssueID int64 = 3_004_242
	sandboxAccount int64 = 9100001
	// sandboxLogin replaces the account's username, sandboxUser any other
	// user's (and sandboxOtherID their ID).
	sandboxLogin   = "uma-sandbox-account"
	sandboxUser    = "uma-sandbox-user"
	sandboxOtherID = 9100002
	// sandboxHost replaces the GitLab host.
	sandboxHost = "gitlab.example.com"
	sandboxAPI  = "https://" + sandboxHost + "/api/v4/"
)

// sandbox is the real sandbox a recording reaches, read from
// GITLAB_SANDBOX_URL (optional), GITLAB_SANDBOX_PROJECT_ID,
// GITLAB_SANDBOX_PROJECT_PATH, GITLAB_SANDBOX_ISSUE, GITLAB_SANDBOX_ACCOUNT_ID
// and GITLAB_SANDBOX_TOKEN.
type sandbox struct {
	api     *url.URL
	project int64
	path    string
	issue   int64
	account int64
	token   string
}

// sandboxFromEnv returns the sandbox the environment names, or false when a
// value is missing or malformed.
func sandboxFromEnv() (sandbox, bool) {
	raw := os.Getenv("GITLAB_SANDBOX_URL")
	if raw == "" {
		raw = "https://gitlab.com/api/v4/"
	}
	api, err0 := url.Parse(raw)
	project, err1 := strconv.ParseInt(os.Getenv("GITLAB_SANDBOX_PROJECT_ID"), 10, 64)
	issue, err2 := strconv.ParseInt(os.Getenv("GITLAB_SANDBOX_ISSUE"), 10, 64)
	account, err3 := strconv.ParseInt(os.Getenv("GITLAB_SANDBOX_ACCOUNT_ID"), 10, 64)
	s := sandbox{api: api, project: project, path: os.Getenv("GITLAB_SANDBOX_PROJECT_PATH"), issue: issue, account: account, token: os.Getenv("GITLAB_SANDBOX_TOKEN")}
	ok := err0 == nil && api.Scheme == "https" && api.Host != "" && err1 == nil && project > 0 && err2 == nil && issue > 0 && err3 == nil && account > 0 &&
		strings.Contains(s.path, "/") && s.token != ""
	return s, ok
}

// transport returns a RoundTripper that sends requests naming the
// placeholders to next with the real host, project and issue, and returns
// answers with every real identifier replaced by its placeholder: the host,
// project path and ID, the issue's number and ID, the account's ID, user
// names, avatar and profile URLs, and emails. Other issue numbers pass
// through unchanged.
func (s sandbox) transport(next http.RoundTripper) http.RoundTripper {
	realProject := strconv.FormatInt(s.project, 10)
	issuePath := regexp.MustCompile(`/issues/` + strconv.FormatInt(s.issue, 10) + `\b`)
	issueRef := regexp.MustCompile(`#` + strconv.FormatInt(s.issue, 10) + `\b`)
	projectID := regexp.MustCompile(`\b` + realProject + `\b`)
	toPlaceholders := func(v string) string {
		v = strings.ReplaceAll(v, s.api.Host, sandboxHost)
		v = strings.ReplaceAll(v, url.PathEscape(s.path), url.PathEscape(sandboxPath))
		v = strings.ReplaceAll(v, s.path, sandboxPath)
		v = projectID.ReplaceAllString(v, strconv.FormatInt(sandboxProject, 10))
		v = issueRef.ReplaceAllString(v, "#"+strconv.FormatInt(sandboxIssue, 10))
		return issuePath.ReplaceAllString(v, "/issues/"+strconv.FormatInt(sandboxIssue, 10))
	}
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		u := *r.URL
		u.Host, out.Host = s.api.Host, s.api.Host
		real := strings.TrimSuffix(s.api.Path, "/") + "/"
		if rest, ok := strings.CutPrefix(u.Path, "/api/v4/"); ok {
			segs := strings.Split(rest, "/")
			if len(segs) > 1 && segs[0] == "projects" && segs[1] == strconv.FormatInt(sandboxProject, 10) {
				segs[1] = realProject
			}
			if len(segs) > 3 && segs[2] == "issues" && segs[3] == strconv.FormatInt(sandboxIssue, 10) {
				segs[3] = strconv.FormatInt(s.issue, 10)
			}
			u.Path, u.RawPath = real+strings.Join(segs, "/"), ""
		}
		out.URL = &u
		resp, err := next.RoundTrip(out)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		for k, vs := range resp.Header {
			for i, v := range vs {
				vs[i] = toPlaceholders(v)
			}
			resp.Header[k] = vs
		}
		if b, ok := s.scrubJSON(body, toPlaceholders); ok {
			body = b
		} else {
			body = []byte(toPlaceholders(string(body)))
		}
		resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		resp.Header.Del("Content-Length")
		return resp, nil
	})
}

// scrubJSON rewrites a JSON answer field by field; false when body is not
// JSON.
func (s sandbox) scrubJSON(body []byte, strs func(string) string) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := json.Marshal(s.scrub(v, strs))
	return out, err == nil
}

// number reports whether v is the integer n.
func number(v any, n int64) bool {
	j, ok := v.(json.Number)
	return ok && j.String() == strconv.FormatInt(n, 10)
}

func (s sandbox) scrub(v any, strs func(string) string) any {
	switch v := v.(type) {
	case string:
		return strs(v)
	case []any:
		for i := range v {
			v[i] = s.scrub(v[i], strs)
		}
		return v
	case map[string]any:
		for k, x := range v {
			v[k] = s.scrub(x, strs)
		}
		if _, isUser := v["username"]; isUser {
			s.scrubUser(v)
		}
		if number(v["project_id"], s.project) {
			v["project_id"] = json.Number(strconv.FormatInt(sandboxProject, 10))
		}
		if number(v["iid"], s.issue) {
			v["iid"] = json.Number(strconv.FormatInt(sandboxIssue, 10))
			if _, ok := v["id"]; ok {
				v["id"] = json.Number(strconv.FormatInt(sandboxIssueID, 10))
			}
		}
		if number(v["noteable_iid"], s.issue) {
			v["noteable_iid"] = json.Number(strconv.FormatInt(sandboxIssue, 10))
			v["noteable_id"] = json.Number(strconv.FormatInt(sandboxIssueID, 10))
		}
		return v
	}
	return v
}

// scrubUser replaces a user object's identity: the account becomes
// sandboxAccount and sandboxLogin, anyone else sandboxOtherID and
// sandboxUser; profile and avatar URLs and emails go.
func (s sandbox) scrubUser(u map[string]any) {
	to, id := sandboxUser, json.Number(strconv.Itoa(sandboxOtherID))
	if number(u["id"], s.account) {
		to, id = sandboxLogin, json.Number(strconv.FormatInt(sandboxAccount, 10))
	}
	u["id"], u["username"], u["name"] = id, to, to
	u["web_url"], u["avatar_url"] = "https://"+sandboxHost+"/"+to, "https://"+sandboxHost+"/uploads/-/system/user/avatar/"+id.String()+"/avatar.png"
	delete(u, "email")
	delete(u, "public_email")
	delete(u, "created_at")
}

// The sandbox transport sends the real host, project and issue and returns
// only placeholders: a note answer by the account matches the placeholder
// account, whatever account recorded it.
func TestSandboxTransportMapsIdentifiers(t *testing.T) {
	api, _ := url.Parse("https://git.corp.example/gitlab/api/v4/")
	s := sandbox{api: api, project: 1234567, path: "someone/scratch", issue: 6, account: 7654321, token: "x"}
	var sent, host string
	real := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent, host = r.URL.Path, r.URL.Host
		body := `{"id":99887766,"iid":6,"project_id":1234567,"title":"t","state":"opened",
			"web_url":"https://git.corp.example/someone/scratch/-/issues/6","other":"https://git.corp.example/someone/scratch/-/issues/61",
			"references":{"full":"someone/scratch#6"},
			"author":{"id":7654321,"username":"someone","name":"Some One","state":"active","avatar_url":"https://git.corp.example/uploads/x.png","web_url":"https://git.corp.example/someone","email":"s@corp.example"},
			"closed_by":{"id":555,"username":"stranger","name":"Stranger","web_url":"https://git.corp.example/stranger"}}`
		h := http.Header{"Content-Type": {"application/json"}, "Link": {`<https://git.corp.example/gitlab/api/v4/projects/1234567/issues/6/notes?page=2>; rel="next"`}}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	req, _ := http.NewRequest(http.MethodPut, sandboxAPI+"projects/"+strconv.FormatInt(sandboxProject, 10)+"/issues/"+strconv.FormatInt(sandboxIssue, 10), nil)
	resp, err := s.transport(real).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "/gitlab/api/v4/projects/1234567/issues/6" || host != "git.corp.example" {
		t.Fatalf("sent %s%s, want the real host, project and issue", host, sent)
	}
	b, _ := io.ReadAll(resp.Body)
	got := string(b) + resp.Header.Get("Link")
	for _, leak := range []string{"corp.example", "someone", "scratch", "1234567", "7654321", "99887766", "Some One", "stranger", "s@", "/issues/6\""} {
		if strings.Contains(got, leak) {
			t.Errorf("answer keeps %q: %s", leak, got)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	author, closer := m["author"].(map[string]any), m["closed_by"].(map[string]any)
	if m["iid"] != float64(sandboxIssue) || m["id"] != float64(sandboxIssueID) || m["project_id"] != float64(sandboxProject) ||
		author["id"] != float64(sandboxAccount) || author["username"] != sandboxLogin || closer["id"] != float64(sandboxOtherID) || closer["username"] != sandboxUser ||
		!strings.HasSuffix(m["other"].(string), "/issues/61") || m["references"].(map[string]any)["full"] != sandboxPath+"#4242" ||
		!strings.Contains(resp.Header.Get("Link"), "https://"+sandboxHost+"/gitlab/api/v4/projects/700000001/issues/4242/notes?page=2") {
		t.Fatalf("answer %s link %s", b, resp.Header.Get("Link"))
	}
	other, _ := http.NewRequest(http.MethodPut, sandboxAPI+"projects/700000001/issues/999999", nil)
	if _, err := s.transport(real).RoundTrip(other); err != nil || sent != "/gitlab/api/v4/projects/1234567/issues/999999" {
		t.Fatalf("other issue sent as %q (%v), want unmapped", sent, err)
	}
}
