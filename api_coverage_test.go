package uma_test

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"hop.top/uma"
)

// coverageFile is the public API coverage matrix: one table row per
// method, keyed by its `Service.Method` name in the first cell.
const coverageFile = "docs/api-coverage.md"

const (
	statusImplemented = "implemented"
	statusPartial     = "partial"
	statusUnsupported = "unsupported"
	storeNone         = "–"
)

// storeValues are the per-store cells a supported method may carry.
var storeValues = map[string]bool{
	"conformance": true, "engine tests": true, "unverified": true, "no store": true,
}

type coverageRow struct {
	line             int
	status           string
	memory, postgres string
}

var methodCell = regexp.MustCompile("^`([A-Z][A-Za-z]*\\.[A-Z][A-Za-z]*)`$")

// readCoverage returns the method rows of the coverage matrix by name. Rows
// whose first cell is not a backticked method name, such as capability
// rows, are left out.
func readCoverage(t *testing.T) map[string]coverageRow {
	t.Helper()
	f, err := os.Open(coverageFile)
	if err != nil {
		t.Fatalf("API coverage matrix: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows := map[string]coverageRow{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		m := methodCell.FindStringSubmatch(cells[0])
		if m == nil {
			continue
		}
		if len(cells) != 5 {
			t.Errorf("%s:%d: %s: want 5 cells (method, status, memory, postgres, notes), got %d", coverageFile, n, m[1], len(cells))
			continue
		}
		if prev, dup := rows[m[1]]; dup {
			t.Errorf("%s:%d: %s already has a row at line %d", coverageFile, n, m[1], prev.line)
			continue
		}
		rows[m[1]] = coverageRow{line: n, status: cells[1], memory: cells[2], postgres: cells[3]}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// publicMethods returns every Engine, Client service and Runtime method by
// name, each reporting whether it has behaviour: Engine methods always do;
// service and runtime methods do when the implemented list says so.
func publicMethods(t *testing.T) map[string]bool {
	t.Helper()
	e := newEngine(t)
	c, err := e.For(principal)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	et := reflect.TypeFor[uma.Engine]()
	for i := 0; i < et.NumMethod(); i++ {
		out["Engine."+et.Method(i).Name] = true
	}
	for _, m := range unimplementedMethods(c, e.Runtime()) {
		out[m.name] = implemented[m.name]
	}
	return out
}

// The coverage matrix has exactly one row per public method, and each
// row's status agrees with the implemented list: a method the engine
// answers is implemented or partial, one it refuses is unsupported with no
// store coverage.
func TestAPICoverageMatrixMatchesEngine(t *testing.T) {
	rows := readCoverage(t)
	methods := publicMethods(t)
	for name, has := range methods {
		row, ok := rows[name]
		if !ok {
			t.Errorf("%s: no row in %s", name, coverageFile)
			continue
		}
		at := coverageFile + ":" + strconv.Itoa(row.line) + ": " + name
		switch row.status {
		case statusImplemented, statusPartial:
			if !has {
				t.Errorf("%s: marked %s, but the method returns model.Unsupported (not in the implemented list)", at, row.status)
			}
			for store, v := range map[string]string{"memory": row.memory, "postgres": row.postgres} {
				if !storeValues[v] {
					t.Errorf("%s: %s cell %q, want one of conformance, engine tests, unverified, no store", at, store, v)
				}
			}
		case statusUnsupported:
			if has {
				t.Errorf("%s: marked unsupported, but the implemented list has it", at)
			}
			if row.memory != storeNone || row.postgres != storeNone {
				t.Errorf("%s: unsupported, store cells must be %s", at, storeNone)
			}
		default:
			t.Errorf("%s: status %q, want implemented, partial or unsupported", at, row.status)
		}
	}
	for name, row := range rows {
		if _, ok := methods[name]; !ok {
			t.Errorf("%s:%d: %s: row for a method the public API lacks", coverageFile, row.line, name)
		}
	}
}

// Every method the matrix marks unsupported says so in its godoc.
func TestUnsupportedMethodsSayUnsupportedInGodoc(t *testing.T) {
	docs := interfaceMethodDocs(t, "uma.go")
	for name, row := range readCoverage(t) {
		if row.status != statusUnsupported {
			continue
		}
		doc, ok := docs[name]
		if !ok {
			t.Errorf("%s: no interface method in uma.go", name)
			continue
		}
		if !strings.Contains(doc, "model.Unsupported") {
			t.Errorf("%s: godoc does not say it returns model.Unsupported today", name)
		}
	}
}

// interfaceMethodDocs returns the doc comment of every method of every
// interface type declared in file, by Interface.Method name.
func interfaceMethodDocs(t *testing.T, file string) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		it, ok := ts.Type.(*ast.InterfaceType)
		if !ok {
			return false
		}
		for _, m := range it.Methods.List {
			for _, id := range m.Names {
				out[ts.Name.Name+"."+id.Name] = m.Doc.Text()
			}
		}
		return false
	})
	return out
}
