package google

import (
	"encoding/base64"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// syncBound adds an upper bound to the first inbox synchronization's
// search ("before:<epoch seconds>", the operator Gmail documents next to
// after:), so what the sync lists is the window [after, until) and not
// everything received since. It wraps the xrrtest transport from outside: the bound is
// part of the request Gmail answers and of the recorded fingerprint, and
// replay derives the same query.
type syncBound struct {
	next  http.RoundTripper
	until time.Time
}

var reAfterOperator = regexp.MustCompile(`(^|\s)after:\d+`)

func (b syncBound) RoundTrip(req *http.Request) (*http.Response, error) {
	q := req.URL.Query()
	search := q.Get("q")
	if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/messages") &&
		q.Get("labelIds") == labelInbox && reAfterOperator.MatchString(search) && !strings.Contains(search, "before:") {
		out := req.Clone(req.Context())
		q.Set("q", search+" before:"+strconv.FormatInt(b.until.Unix(), 10))
		out.URL.RawQuery = q.Encode()
		req = out
	}
	return b.next.RoundTrip(req)
}

// historyScope limits the history read to the mailbox's SENT label
// ("labelId=SENT" on users.history.list). Without it the list answers every
// change of the whole mailbox since the cursor, and the receiver fetches each
// message that arrives in the inbox: mail that came in while the scenario ran
// would be read and recorded. SENT holds the message the scenario sent, whose
// label changes are what the round must report. The wrapper is part of the
// request Gmail answers and of the recorded fingerprint, as syncBound is.
type historyScope struct{ next http.RoundTripper }

func (h historyScope) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/history") && req.URL.Query().Get("labelId") == "" {
		out := req.Clone(req.Context())
		q := out.URL.Query()
		q.Set("labelId", "SENT")
		out.URL.RawQuery = q.Encode()
		req = out
	}
	return h.next.RoundTrip(req)
}

// residueIn names the identities still readable in content: as is, URL
// escaped, in the base64 strings it holds (the "raw" fields) and, inside
// those, in their folded, quoted-printable, base64 or encoded-word forms.
// It looks at bytes with its own heuristics, not the hook's parser, so a
// gap in the hook does not hide here.
func residueIn(content []byte, ids []identity) []string {
	var found []string
	for _, id := range ids {
		if id.value != "" && hasView(content, strings.ToLower(id.value), 0) {
			found = append(found, id.name)
		}
	}
	return found
}

var (
	reB64Token   = regexp.MustCompile(`[A-Za-z0-9_\-+/]{24,}={0,2}`)
	reB64Line    = regexp.MustCompile(`^[A-Za-z0-9+/]{4,}={0,2}\r?$`)
	reFold       = regexp.MustCompile(`\r?\n[ \t]+`)
	reSoftBreak  = regexp.MustCompile(`=\r?\n`)
	reQPByte     = regexp.MustCompile(`=([0-9A-Fa-f]{2})`)
	reAtEscapes  = regexp.MustCompile(`(?i)%40|\\u0040|&#0*64;|&#x0*40;|&commat;`)
	b64Encodings = []*base64.Encoding{base64.RawURLEncoding, base64.RawStdEncoding}
)

// hasView reports whether needle (lower case) is readable in b, or in any
// decoding of it, up to three levels deep.
func hasView(b []byte, needle string, depth int) bool {
	if depth > 3 {
		return false
	}
	views := [][]byte{b}
	if u, err := url.PathUnescape(string(b)); err == nil {
		views = append(views, []byte(u))
	}
	views = append(views, reAtEscapes.ReplaceAll(b, []byte("@")))
	unfolded := reFold.ReplaceAll(b, nil)
	views = append(views, unfolded)
	joined := reSoftBreak.ReplaceAll(unfolded, nil)
	views = append(views, reQPByte.ReplaceAllFunc(joined, func(m []byte) []byte {
		n, _ := strconv.ParseUint(string(m[1:]), 16, 8)
		return []byte{byte(n)}
	}))
	if dec, err := wordDecoder.DecodeHeader(string(unfolded)); err == nil {
		views = append(views, []byte(dec))
	}
	for _, v := range views {
		if strings.Contains(strings.ToLower(string(v)), needle) {
			return true
		}
	}
	for _, v := range views[:1] {
		for _, tok := range reB64Token.FindAll(v, -1) {
			if dec, ok := tryB64(string(tok)); ok && hasView(dec, needle, depth+1) {
				return true
			}
		}
	}
	// base64 bodies: runs of lines that look like base64 are one value.
	var run []string
	flush := func() bool {
		defer func() { run = run[:0] }()
		if len(run) == 0 {
			return false
		}
		dec, ok := tryB64(strings.Join(run, ""))
		return ok && hasView(dec, needle, depth+1)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if reB64Line.MatchString(line) {
			run = append(run, strings.TrimRight(line, "\r"))
			continue
		}
		if flush() {
			return true
		}
	}
	return flush()
}

func tryB64(s string) ([]byte, bool) {
	s = strings.TrimRight(s, "=")
	for _, e := range b64Encodings {
		if b, err := e.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// assertNoResidue fails tb for every file under dir in which an identity is
// still readable.
func assertNoResidue(tb testing.TB, dir string, ids []identity) {
	tb.Helper()
	fsys := os.DirFS(dir)
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		if found := residueIn(b, ids); len(found) > 0 {
			tb.Errorf("cassette %s still holds a real %s", path, strings.Join(found, ", "))
		}
		return nil
	})
	if err != nil {
		tb.Fatalf("scan cassettes in %s: %v", dir, err)
	}
}
