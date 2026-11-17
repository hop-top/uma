package httpmail_test

import (
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestNoInternalImports pins the adapter boundary: the package reaches uma
// through its public packages only.
func TestNoInternalImports(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for line := range strings.Lines(string(out)) {
		pkg := strings.TrimSpace(line)
		if strings.HasPrefix(pkg, "hop.top/uma") && slices.Contains(strings.Split(pkg, "/"), "internal") {
			t.Errorf("imports internal package %s", pkg)
		}
	}
}

// TestNoInit pins that importing the package registers nothing.
func TestNoInit(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := gotoken.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
				t.Errorf("%s declares init", fset.Position(fn.Pos()))
			}
		}
	}
}
