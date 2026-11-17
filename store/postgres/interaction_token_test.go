package postgres

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// tokenQueries are the only queries that may mention the response token
// column: the insert that stores it, the loader that returns it, and the
// statements that clear it or look for an expired one.
var tokenQueries = map[string]bool{
	"InsertInteraction":              true,
	"GetInteractionToken":            true,
	"RedactInteraction":              true,
	"ClearExpiredInteractionTokens":  true,
	"ExpiredInteractionTokensRemain": true,
}

var queryName = regexp.MustCompile(`(?m)^-- name: (\w+) :(\w+)`)

// TestInteractionTokenColumnNeverRead audits every query in queries/: the
// response token column is selected by GetInteractionToken alone. Other
// statements may write it or filter on it, and no query selects every
// column of the interaction table or returns the column from a write. The
// generated row types agree: only the token loader's carries the token.
func TestInteractionTokenColumnNeverRead(t *testing.T) {
	files, err := filepath.Glob("queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("queries: %v, %d files", err, len(files))
	}
	seen := map[string]bool{}
	for _, file := range files {
		src, err := os.ReadFile(file) // #nosec G304 -- file comes from a glob of this package's own directories
		if err != nil {
			t.Fatal(err)
		}
		marks := queryName.FindAllStringSubmatchIndex(string(src), -1)
		for i, m := range marks {
			end := len(src)
			if i+1 < len(marks) {
				end = marks[i+1][0]
			}
			name, body := string(src[m[2]:m[3]]), strings.ToLower(string(src[m[1]:end]))
			seen[name] = true
			mentionsToken := strings.Contains(body, "response_token")
			if mentionsToken && !tokenQueries[name] {
				t.Errorf("%s: query %s mentions response_token; only %v may", file, name, keys(tokenQueries))
			}
			if !strings.Contains(body, "uma_interactions") {
				continue
			}
			if regexp.MustCompile(`select\s+(\w+\.)?\*|returning\s+(\w+\.)?\*|\w\.\*`).MatchString(body) {
				t.Errorf("%s: query %s selects every interaction column", file, name)
			}
			if mentionsToken && strings.Contains(body, "returning") {
				t.Errorf("%s: query %s returns response_token from a write", file, name)
			}
		}
	}
	for name := range tokenQueries {
		if !seen[name] {
			t.Errorf("audit lists query %s, which does not exist", name)
		}
	}
	generated, err := filepath.Glob("internal/db/*.sql.go")
	if err != nil || len(generated) == 0 {
		t.Fatalf("generated code: %v, %d files", err, len(generated))
	}
	rowType := regexp.MustCompile(`(?s)type (\w+Row) struct \{(.*?)\n\}`)
	for _, file := range generated {
		src, err := os.ReadFile(file) // #nosec G304 -- file comes from a glob of this package's own directories
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range rowType.FindAllStringSubmatch(string(src), -1) {
			if strings.Contains(m[2], "ResponseToken") && m[1] != "GetInteractionTokenRow" {
				t.Errorf("%s: %s carries the response token", file, m[1])
			}
		}
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
