package conformance

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The A2A task rules fixture lists each rule of the agent protocol model
// with the cases that prove it: the conformance cases of this package and
// the tests of other packages, as "dir:TestName". The cases run in
// RunServiceSuite and RunDurableSuite; this test only keeps the list
// honest, so a renamed or dropped case fails here.

const a2aTasksSchema = "uma.conformance.a2a_tasks/v1"

type a2aTasksFixture struct {
	Schema string `json:"schema"`
	Rules  []struct {
		ID    string   `json:"id"`
		Rule  string   `json:"rule"`
		Cases []string `json:"cases"`
		Tests []string `json:"tests"`
	} `json:"rules"`
}

func TestA2ATasksFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "a2a_tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fx a2aTasksFixture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fx); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fx.Schema != a2aTasksSchema || len(fx.Rules) == 0 {
		t.Fatalf("schema %q with %d rules", fx.Schema, len(fx.Rules))
	}
	known := map[string]bool{}
	for _, c := range agentTaskCases {
		known[c.name] = true
	}
	for _, c := range faultCases {
		known[c.name] = true
	}
	id := regexp.MustCompile(`^A2A-\d{2}$`)
	seen := map[string]bool{}
	cases := map[string]bool{}
	for _, r := range fx.Rules {
		switch {
		case !id.MatchString(r.ID) || seen[r.ID]:
			t.Errorf("rule id %q is malformed or repeated", r.ID)
		case strings.TrimSpace(r.Rule) == "":
			t.Errorf("%s: empty rule", r.ID)
		case len(r.Cases)+len(r.Tests) == 0:
			t.Errorf("%s: no proof", r.ID)
		}
		seen[r.ID] = true
		for _, c := range r.Cases {
			cases[c] = true
			if !known[c] {
				t.Errorf("%s: no conformance case %q", r.ID, c)
			}
		}
		for _, ref := range r.Tests {
			dir, name, ok := strings.Cut(ref, ":")
			if !ok || !hasTest(t, filepath.Join("..", filepath.FromSlash(dir)), name) {
				t.Errorf("%s: no test %q", r.ID, ref)
			}
		}
	}
	// Every case of the agent group proves some rule.
	for _, c := range agentTaskCases {
		if !cases[c.name] {
			t.Errorf("case %q proves no rule of the fixture", c.name)
		}
	}
}

// hasTest reports whether a test file of dir declares func name.
func hasTest(t *testing.T, dir, name string) bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		file, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range file.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}
