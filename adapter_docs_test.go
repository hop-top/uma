package uma_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The adapter pages describe what each adapter declares, so they are checked
// against the declaration itself. Adapters live in separate modules the root
// module cannot import; the test reads every `Describe` method of an
// adapter.Factory from the source instead, resolving the constants and
// variables its descriptor names, and compares what it finds with
// docs/adapters.

const (
	adaptersDocs = "docs/adapters"
	adapterTree  = "adapter"
	modelTree    = "model"
)

// declared is what one adapter's descriptor declares.
type declared struct {
	key, maturity string
	caps, events  []string
	where         string
}

// pkgDecls are the package-level declarations of one directory.
type pkgDecls struct {
	values map[string]ast.Expr
	funcs  map[string]*ast.FuncDecl
}

func parseDir(t *testing.T, dir string) (map[string]*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = f
	}
	return files, fset
}

func collect(files map[string]*ast.File) pkgDecls {
	d := pkgDecls{values: map[string]ast.Expr{}, funcs: map[string]*ast.FuncDecl{}}
	for _, f := range files {
		for _, decl := range f.Decls {
			switch x := decl.(type) {
			case *ast.FuncDecl:
				if x.Recv == nil {
					d.funcs[x.Name.Name] = x
				}
			case *ast.GenDecl:
				for _, spec := range x.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) == len(vs.Values) {
						for i, n := range vs.Names {
							d.values[n.Name] = vs.Values[i]
						}
					}
				}
			}
		}
	}
	return d
}

// resolver resolves descriptor expressions to strings within one package,
// reaching package model for `model.X` selectors.
type resolver struct {
	pkg, model pkgDecls
}

func (r resolver) str(e ast.Expr) (string, error) {
	switch x := e.(type) {
	case *ast.BasicLit:
		return strconv.Unquote(x.Value)
	case *ast.Ident:
		if v, ok := r.pkg.values[x.Name]; ok {
			return r.str(v)
		}
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && id.Name == "model" {
			if v, ok := r.model.values[x.Sel.Name]; ok {
				return r.str(v)
			}
		}
	case *ast.CallExpr: // a conversion such as string(Type)
		if len(x.Args) == 1 {
			return r.str(x.Args[0])
		}
	case *ast.BinaryExpr:
		a, err := r.str(x.X)
		if err != nil {
			return "", err
		}
		b, err := r.str(x.Y)
		return a + b, err
	}
	return "", fmt.Errorf("cannot resolve %T %v", e, e)
}

func (r resolver) strs(e ast.Expr) ([]string, error) {
	switch x := e.(type) {
	case *ast.CompositeLit:
		var out []string
		for _, el := range x.Elts {
			s, err := r.str(el)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case *ast.Ident:
		if v, ok := r.pkg.values[x.Name]; ok {
			return r.strs(v)
		}
	case *ast.CallExpr:
		if id, ok := x.Fun.(*ast.Ident); ok && r.pkg.funcs[id.Name] != nil {
			return r.fromFunc(r.pkg.funcs[id.Name])
		}
		if len(x.Args) > 0 { // append([]T(nil), list...), slices.Clone(list)
			return r.strs(x.Args[len(x.Args)-1])
		}
	}
	return nil, fmt.Errorf("cannot resolve list %T", e)
}

func (r resolver) fromFunc(fn *ast.FuncDecl) ([]string, error) {
	for _, st := range fn.Body.List {
		if ret, ok := st.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
			return r.strs(ret.Results[0])
		}
	}
	return nil, fmt.Errorf("%s returns no list", fn.Name.Name)
}

// descriptors returns the descriptor of every adapter.Factory under adapter/.
func descriptors(t *testing.T) []declared {
	t.Helper()
	modelFiles, _ := parseDir(t, modelTree)
	model := collect(modelFiles)
	var out []declared
	err := filepath.WalkDir(adapterTree, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		files, fset := parseDir(t, path)
		pkg := collect(files)
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || fn.Name.Name != "Describe" || fn.Body == nil {
					continue
				}
				lit := descriptorLiteral(fn)
				if lit == nil {
					continue
				}
				got, err := readDescriptor(resolver{pkg, model}, lit)
				if err != nil {
					t.Fatalf("%s: unsupported descriptor shape: %v", fset.Position(fn.Pos()), err)
				}
				got.where = path
				out = append(out, got)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) == 0 {
		t.Fatal("no adapter descriptors found under adapter/")
	}
	return out
}

func descriptorLiteral(fn *ast.FuncDecl) *ast.CompositeLit {
	for _, st := range fn.Body.List {
		ret, ok := st.(*ast.ReturnStmt)
		if !ok || len(ret.Results) != 1 {
			continue
		}
		lit, ok := ret.Results[0].(*ast.CompositeLit)
		if !ok {
			continue
		}
		if sel, ok := lit.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Descriptor" {
			return lit
		}
	}
	return nil
}

func readDescriptor(r resolver, lit *ast.CompositeLit) (declared, error) {
	var d declared
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		var err error
		switch kv.Key.(*ast.Ident).Name {
		case "Type":
			d.key, err = r.str(kv.Value)
		case "Maturity":
			d.maturity, err = r.str(kv.Value)
		case "Capabilities":
			d.caps, err = r.strs(kv.Value)
		case "Events":
			d.events, err = r.strs(kv.Value)
		}
		if err != nil {
			return d, err
		}
	}
	if d.key == "" {
		return d, fmt.Errorf("descriptor has no type key")
	}
	return d, nil
}

var codeSpan = regexp.MustCompile("`([^`]+)`")

// codes returns the backticked tokens of a table cell; `none` and an empty
// cell are the empty list.
func codes(cell string) []string {
	var out []string
	for _, m := range codeSpan.FindAllStringSubmatch(cell, -1) {
		out = append(out, m[1])
	}
	return out
}

func cells(line string) []string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- fixed documentation paths
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

// TestEveryAdapterHasAPage checks, per registered descriptor, that
// docs/adapters/<key>.md exists and states the key, capabilities, events and
// maturity the descriptor declares.
func TestEveryAdapterHasAPage(t *testing.T) {
	for _, d := range descriptors(t) {
		t.Run(d.key, func(t *testing.T) {
			page := filepath.Join(adaptersDocs, d.key+".md")
			if _, err := os.Stat(page); err != nil {
				t.Fatalf("adapter %s (%s) has no page: want %s", d.key, d.where, page)
			}
			lines := readLines(t, page)
			if lines[0] != "# "+d.key {
				t.Errorf("%s: first line %q, want %q", page, lines[0], "# "+d.key)
			}
			rows := map[string][]string{}
			for _, l := range lines {
				if c := cells(l); strings.HasPrefix(l, "| ") && len(c) == 2 {
					rows[c[0]] = codes(c[1])
					if c[1] == "none" {
						rows[c[0]] = nil
					}
				}
			}
			if got := rows["Key"]; !slices.Equal(got, []string{d.key}) {
				t.Errorf("%s: Key row %v, want %q", page, got, d.key)
			}
			if got := rows["Capabilities"]; !sameSet(got, d.caps) {
				t.Errorf("%s: Capabilities row %v, descriptor declares %v", page, got, d.caps)
			}
			if got := rows["Events"]; !sameSet(got, d.events) {
				t.Errorf("%s: Events row %v, descriptor declares %v", page, got, d.events)
			}
			if !slices.ContainsFunc(lines, func(l string) bool { return l == "| Maturity | "+d.maturity+" |" }) {
				t.Errorf("%s: no Maturity row %q", page, d.maturity)
			}
		})
	}
}

// TestAdapterPagesHaveAnAdapter checks the reverse: every page names a
// registered adapter.
func TestAdapterPagesHaveAnAdapter(t *testing.T) {
	keys := map[string]bool{}
	for _, d := range descriptors(t) {
		keys[d.key] = true
	}
	pages, err := filepath.Glob(filepath.Join(adaptersDocs, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		base := strings.TrimSuffix(filepath.Base(p), ".md")
		if base != "README" && !keys[base] {
			t.Errorf("%s documents no registered adapter", p)
		}
	}
}

// TestAdapterMatrix checks the capability matrix and the declared-events
// table of docs/adapters/README.md against the descriptors, and that the
// main README links to it.
func TestAdapterMatrix(t *testing.T) {
	want := map[string]declared{}
	for _, d := range descriptors(t) {
		want[d.key] = d
	}
	var header []string
	matrix, events := map[string][]string{}, map[string][]string{}
	for _, l := range readLines(t, filepath.Join(adaptersDocs, "README.md")) {
		if !strings.HasPrefix(l, "|") {
			continue
		}
		c := cells(l)
		switch {
		case c[0] == "Adapter" && len(c) > 5 && c[1] == "`receive`":
			header = c
		case header != nil && strings.HasPrefix(c[0], "[`") && len(c) == len(header):
			key := codes(c[0])[0]
			for i, cell := range c[1:] {
				if cell == "yes" {
					matrix[key] = append(matrix[key], codes(header[i+1])[0])
				}
			}
			if _, ok := matrix[key]; !ok {
				matrix[key] = nil
			}
		case strings.HasPrefix(c[0], "`") && len(c) == 7: // the identity and events table
			events[codes(c[0])[0]] = codes(c[2])
		}
	}
	wantMatrixColumns(t, header)
	for key, d := range want {
		got, ok := matrix[key]
		if !ok {
			t.Errorf("capability matrix has no row for %s", key)
		} else if builtin := builtinOnly(t, d.caps); !sameSet(got, builtin) {
			t.Errorf("capability matrix row %s lists %v, descriptor declares the builtin %v", key, got, builtin)
		}
		if got, ok := events[key]; !ok {
			t.Errorf("events table has no row for %s", key)
		} else if !sameSet(got, d.events) {
			t.Errorf("events table row %s lists %v, descriptor declares %v", key, got, d.events)
		}
	}
	for key := range matrix {
		if _, ok := want[key]; !ok {
			t.Errorf("capability matrix row %s is no registered adapter", key)
		}
	}
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "docs/adapters/README.md") {
		t.Error("README.md does not link docs/adapters/README.md")
	}
}

// unmatrixed are the builtin capabilities the matrix has no column for:
// no adapter declares them and none is being built. The row check above
// fails once an adapter declares one, so its column lands with it.
var unmatrixed = []string{"probe_permissions"}

// wantMatrixColumns requires the matrix header to name each builtin
// capability of package model once, except those in unmatrixed, and
// nothing else: a capability no adapter declares yet still has its column,
// so the row that first declares it changes one cell.
func wantMatrixColumns(t *testing.T, header []string) {
	t.Helper()
	if header == nil {
		t.Fatal("capability matrix header not found")
	}
	builtin := builtinCapabilities(t)
	var columns []string
	for _, h := range header[1:] {
		c := codes(h)
		if len(c) != 1 || slices.Contains(columns, c[0]) {
			t.Errorf("capability matrix column %q: want one capability, named once", h)
			continue
		}
		columns = append(columns, c[0])
	}
	for _, c := range builtin {
		switch {
		case slices.Contains(unmatrixed, c) && slices.Contains(columns, c):
			t.Errorf("capability matrix has a column for %s; drop it from unmatrixed", c)
		case !slices.Contains(unmatrixed, c) && !slices.Contains(columns, c):
			t.Errorf("capability matrix has no %s column", c)
		}
	}
	for _, c := range columns {
		if !slices.Contains(builtin, c) {
			t.Errorf("capability matrix column %s is no builtin capability", c)
		}
	}
}

// builtinOnly returns the builtin capabilities among caps: the matrix has a
// column for each of them, and none for the namespaced capabilities an
// adapter declares for its own extensions, which its page lists.
func builtinOnly(t *testing.T, caps []string) []string {
	t.Helper()
	builtin := builtinCapabilities(t)
	var out []string
	for _, c := range caps {
		if slices.Contains(builtin, c) {
			out = append(out, c)
		}
	}
	return out
}

// builtinCapabilities reads every Capability* constant of package model.
func builtinCapabilities(t *testing.T) []string {
	t.Helper()
	modelFiles, _ := parseDir(t, modelTree)
	r := resolver{model: collect(modelFiles)}
	var builtin []string
	for name, v := range r.model.values {
		if strings.HasPrefix(name, "Capability") {
			c, err := r.str(v)
			if err != nil {
				t.Fatalf("model.%s: %v", name, err)
			}
			builtin = append(builtin, c)
		}
	}
	slices.Sort(builtin)
	return builtin
}

// undeclaredReasons are the reason classes a page's "Not declared" table
// may give, as docs/adapters/README.md defines them.
var undeclaredReasons = []string{"provider", "not built", "engine", "by design"}

const undeclaredHeading = "## Not declared"

// TestAdapterPagesExplainUndeclared checks that every page has a "Not
// declared" table with one row per builtin capability the descriptor
// omits, each with a reason class and a sentence, and no row for a
// capability the descriptor declares; and that the matrix page defines
// the reason classes.
func TestAdapterPagesExplainUndeclared(t *testing.T) {
	builtin := builtinCapabilities(t)
	for _, d := range descriptors(t) {
		t.Run(d.key, func(t *testing.T) {
			page := filepath.Join(adaptersDocs, d.key+".md")
			rows, found := undeclaredTable(readLines(t, page))
			if !found {
				t.Fatalf("%s: no %q section", page, undeclaredHeading)
			}
			var want []string
			for _, c := range builtin {
				if !slices.Contains(d.caps, c) {
					want = append(want, c)
				}
			}
			var got []string
			for c, cells := range rows {
				got = append(got, c)
				if !slices.Contains(undeclaredReasons, cells[0]) {
					t.Errorf("%s: %s reason %q, want one of %v", page, c, cells[0], undeclaredReasons)
				}
				if strings.TrimSpace(cells[1]) == "" {
					t.Errorf("%s: %s has no explanation", page, c)
				}
				if slices.Contains(d.caps, c) {
					t.Errorf("%s: %s is declared, so it has no place in the table", page, c)
				}
			}
			if !sameSet(got, want) {
				slices.Sort(got)
				t.Errorf("%s: table explains %v, descriptor omits %v", page, got, want)
			}
		})
	}
	readme := string(mustRead(t, filepath.Join(adaptersDocs, "README.md")))
	if !strings.Contains(readme, "## Why a capability is not declared") {
		t.Error("docs/adapters/README.md: no section defining the reason classes")
	}
	for _, r := range undeclaredReasons {
		if !strings.Contains(readme, "| `"+r+"` |") {
			t.Errorf("docs/adapters/README.md: reason class %q not defined", r)
		}
	}
}

// undeclaredTable returns the rows of the page's "Not declared" table:
// capability to [reason, explanation]. The table is the first one after
// the heading and ends at the next blank line after its header.
func undeclaredTable(lines []string) (map[string][2]string, bool) {
	rows := map[string][2]string{}
	in, header := false, false
	for _, l := range lines {
		switch {
		case l == undeclaredHeading:
			in = true
		case !in:
		case strings.HasPrefix(l, "## "):
			return rows, true
		case !strings.HasPrefix(l, "|"):
			if header {
				return rows, true
			}
		default:
			c := cells(l)
			if len(c) != 3 || c[0] == "Capability" || strings.HasPrefix(c[0], "---") {
				header = header || c[0] == "Capability"
				continue
			}
			header = true
			if names := codes(c[0]); len(names) == 1 {
				rows[names[0]] = [2]string{strings.Trim(c[1], "`"), c[2]}
			}
		}
	}
	return rows, in
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- fixed documentation paths
	if err != nil {
		t.Fatal(err)
	}
	return b
}
