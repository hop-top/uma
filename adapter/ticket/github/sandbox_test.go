package github_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Recordings run against a real sandbox issue and account, but cassettes
// and the code under test only ever see these placeholders, in every mode:
// sandboxRepo, issue sandboxIssue and account sandboxAccount. The sandbox
// transport translates between them and the real ones.
const (
	sandboxIssue   int   = 4242
	sandboxAccount int64 = 9100001
	// sandboxLogin replaces the account's login, sandboxUser any other
	// user's.
	sandboxLogin = "uma-sandbox-account"
	sandboxUser  = "uma-sandbox-user"
	sandboxNode  = "NODE_ID"
	// sandboxRepoID replaces the repository's numeric ID, which GitHub's
	// Link header URLs name (/repositories/<id>/...).
	sandboxRepoID = "700000001"
)

// repositoryID matches the repository ID path segment of an API URL.
var repositoryID = regexp.MustCompile(`/repositories/[0-9]+\b`)

// sandbox is the real sandbox a recording reaches, read from
// GITHUB_SANDBOX_REPO, GITHUB_SANDBOX_ISSUE, GITHUB_SANDBOX_ACCOUNT_ID and
// GITHUB_SANDBOX_TOKEN.
type sandbox struct {
	repo    string
	issue   int
	account int64
	token   string
}

// sandboxFromEnv returns the sandbox the environment names, or false when
// a value is missing or malformed.
func sandboxFromEnv() (sandbox, bool) {
	issue, err1 := strconv.Atoi(os.Getenv("GITHUB_SANDBOX_ISSUE"))
	account, err2 := strconv.ParseInt(os.Getenv("GITHUB_SANDBOX_ACCOUNT_ID"), 10, 64)
	s := sandbox{repo: os.Getenv("GITHUB_SANDBOX_REPO"), issue: issue, account: account, token: os.Getenv("GITHUB_SANDBOX_TOKEN")}
	owner, name, ok := strings.Cut(s.repo, "/")
	return s, ok && owner != "" && name != "" && err1 == nil && issue > 0 && err2 == nil && account > 0 && s.token != ""
}

// transport returns a RoundTripper that sends requests naming the
// placeholders to next with the real repository and issue, and returns
// answers with every real identifier replaced by its placeholder: the
// repository name and ID, the issue number, the account ID, user logins
// and node IDs. Other issue numbers pass through unchanged.
func (s sandbox) transport(next http.RoundTripper) http.RoundTripper {
	issue := regexp.MustCompile(`/issues/` + strconv.Itoa(s.issue) + `\b`)
	toPlaceholders := func(v string) string {
		v = strings.ReplaceAll(v, s.repo, sandboxRepo)
		v = repositoryID.ReplaceAllString(v, "/repositories/"+sandboxRepoID)
		return issue.ReplaceAllString(v, "/issues/"+strconv.Itoa(sandboxIssue))
	}
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		out := r.Clone(r.Context())
		u := *r.URL
		prefix := "/repos/" + sandboxRepo + "/"
		if rest, ok := strings.CutPrefix(u.Path, prefix); ok {
			segs := strings.Split(rest, "/")
			if len(segs) > 1 && segs[0] == "issues" && segs[1] == strconv.Itoa(sandboxIssue) {
				segs[1] = strconv.Itoa(s.issue)
			}
			u.Path, u.RawPath = "/repos/"+s.repo+"/"+strings.Join(segs, "/"), ""
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
			if k == "node_id" {
				v[k] = sandboxNode
				continue
			}
			v[k] = s.scrub(x, strs)
		}
		if n, ok := v["number"].(json.Number); ok && n.String() == strconv.Itoa(s.issue) {
			v["number"] = json.Number(strconv.Itoa(sandboxIssue))
			if _, ok := v["id"]; ok {
				v["id"] = json.Number(strconv.Itoa(3_000_000_000 + sandboxIssue))
			}
		}
		if login, ok := v["login"].(string); ok && login != "" {
			s.scrubUser(v, login)
		}
		return v
	}
	return v
}

// avatarHash is the account-specific u= parameter some avatar URLs carry.
var avatarHash = regexp.MustCompile(`u=[0-9a-f]{20,}&?`)

// scrubUser replaces a user object's login, and the account's ID, in all
// its fields: profile and avatar URLs carry both. The avatar hash goes.
func (s sandbox) scrubUser(u map[string]any, login string) {
	to, id := sandboxUser, ""
	if n, ok := u["id"].(json.Number); ok {
		id = n.String()
		if id == strconv.FormatInt(s.account, 10) {
			to = sandboxLogin
			u["id"] = json.Number(strconv.FormatInt(sandboxAccount, 10))
		}
	}
	for k, x := range u {
		str, ok := x.(string)
		if !ok {
			continue
		}
		if k == "login" {
			u[k] = to
			continue
		}
		str = strings.ReplaceAll(str, "/"+login, "/"+to)
		str = strings.TrimRight(avatarHash.ReplaceAllString(str, ""), "?&")
		if to == sandboxLogin && id != "" {
			str = strings.ReplaceAll(str, "/u/"+id, "/u/"+strconv.FormatInt(sandboxAccount, 10))
		}
		u[k] = str
	}
}

// The sandbox transport sends the real repository and issue and returns
// only placeholders: a comment answer by the account matches the
// placeholder account, whatever account recorded it.
func TestSandboxTransportMapsIdentifiers(t *testing.T) {
	s := sandbox{repo: "someone/scratch", issue: 6, account: 1234567, token: "x"}
	var sent string
	real := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = r.URL.Path
		body := `{"id":2000000001,"node_id":"IC_kwDOreal","number":6,"comments":6,
			"url":"https://api.github.com/repos/someone/scratch/issues/6","html_url":"https://github.com/someone/scratch/issues/6#issuecomment-1",
			"other":"https://api.github.com/repos/someone/scratch/issues/61",
			"user":{"login":"someone","id":1234567,"node_id":"U_real","avatar_url":"https://avatars.githubusercontent.com/u/1234567?u=0123456789abcdef0123456789abcdef01234567&v=4","html_url":"https://github.com/someone"},
			"closed_by":{"login":"stranger","id":7654321,"html_url":"https://github.com/stranger"}}`
		h := http.Header{"Content-Type": {"application/json"}, "Link": {`<https://api.github.com/repos/someone/scratch/issues/6/comments?page=2>; rel="next"`}}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	req, _ := http.NewRequest(http.MethodPatch, "https://api.github.com/repos/"+sandboxRepo+"/issues/"+strconv.Itoa(sandboxIssue), nil)
	resp, err := s.transport(real).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if sent != "/repos/someone/scratch/issues/6" {
		t.Fatalf("sent path %q, want the real issue", sent)
	}
	b, _ := io.ReadAll(resp.Body)
	got := string(b) + resp.Header.Get("Link")
	for _, leak := range []string{"someone", "scratch", "/issues/6\"", "1234567", "IC_kwDOreal", "U_real", "0123456789abcdef"} {
		if strings.Contains(got, leak) {
			t.Errorf("answer keeps %q: %s", leak, got)
		}
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	user := m["user"].(map[string]any)
	if m["number"] != float64(sandboxIssue) || m["comments"] != float64(6) || user["id"] != float64(sandboxAccount) ||
		!strings.HasSuffix(m["other"].(string), "/issues/61") || m["closed_by"].(map[string]any)["id"] != float64(7654321) ||
		!strings.Contains(resp.Header.Get("Link"), "/repos/example/uma-sandbox/issues/4242/comments") {
		t.Fatalf("answer %s link %s", b, resp.Header.Get("Link"))
	}
	other, _ := http.NewRequest(http.MethodPatch, "https://api.github.com/repos/"+sandboxRepo+"/issues/999999", nil)
	if _, err := s.transport(real).RoundTrip(other); err != nil || sent != "/repos/someone/scratch/issues/999999" {
		t.Fatalf("other issue sent as %q (%v), want unmapped", sent, err)
	}
}

// Comment and reaction paths name the repository, not the issue: the real
// repository is sent, comment and reaction IDs pass through, and a comment
// answer's issue URL and links map back to the placeholders.
func TestSandboxTransportMapsCommentPaths(t *testing.T) {
	s := sandbox{repo: "someone/scratch", issue: 6, account: 1234567, token: "x"}
	var sent []string
	real := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sent = append(sent, r.Method+" "+r.URL.Path)
		body := `{"id":2000000001,"issue_url":"https://api.github.com/repos/someone/scratch/issues/6","reactions":{"url":"https://api.github.com/repos/someone/scratch/issues/comments/2000000001/reactions"}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	for _, path := range []string{"issues/comments/2000000001", "issues/comments/2000000001/reactions", "issues/comments/2000000001/reactions/55", "issues/" + strconv.Itoa(sandboxIssue) + "/reactions"} {
		req, _ := http.NewRequest(http.MethodDelete, "https://api.github.com/repos/"+sandboxRepo+"/"+path, nil)
		resp, err := s.transport(real).RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		for _, leak := range []string{"someone", "scratch", "/issues/6\""} {
			if strings.Contains(string(b), leak) {
				t.Errorf("%s: answer keeps %q: %s", path, leak, b)
			}
		}
	}
	want := []string{
		"DELETE /repos/someone/scratch/issues/comments/2000000001",
		"DELETE /repos/someone/scratch/issues/comments/2000000001/reactions",
		"DELETE /repos/someone/scratch/issues/comments/2000000001/reactions/55",
		"DELETE /repos/someone/scratch/issues/6/reactions",
	}
	if strings.Join(sent, "\n") != strings.Join(want, "\n") {
		t.Fatalf("sent %q, want %q", sent, want)
	}
}

// GitHub's Link header names the repository by ID: the ID is replaced, the
// page parameters kept.
func TestSandboxTransportMapsRepositoryID(t *testing.T) {
	s := sandbox{repo: "someone/scratch", issue: 6, account: 1234567, token: "x"}
	real := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		h := http.Header{"Content-Type": {"application/json"}, "Link": {`<https://api.github.com/repositories/987654321/issues/6/comments?per_page=2&page=2>; rel="next"`}}
		return &http.Response{StatusCode: http.StatusOK, Header: h, Body: io.NopCloser(strings.NewReader(`[]`)), Request: r}, nil
	})
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+sandboxRepo+"/issues/"+strconv.Itoa(sandboxIssue)+"/comments", nil)
	resp, err := s.transport(real).RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	want := `<https://api.github.com/repositories/` + sandboxRepoID + `/issues/4242/comments?per_page=2&page=2>; rel="next"`
	if got := resp.Header.Get("Link"); got != want {
		t.Fatalf("Link = %s, want %s", got, want)
	}
}
