package fakeprovider_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	umaModule  = "hop.top/uma"
	thisModule = "hop.top/uma/examples/fakeprovider"
)

// TestImportsPublicPackagesOnly pins the external-module boundary: the fake
// provider's transitive imports are the standard library, this module, and
// public packages of the uma root module. No internal package, no other uma
// module, no third-party code.
func TestImportsPublicPackagesOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}} {{with .Module}}{{.Path}}{{end}}{{end}}", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var umaDeps int
	for line := range strings.Lines(string(out)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		pkg, mod, _ := strings.Cut(line, " ")
		switch {
		case mod == thisModule:
		case mod != umaModule:
			t.Errorf("%s comes from module %q; only the uma root module is allowed", pkg, mod)
		case slices.Contains(strings.Split(pkg, "/"), "internal"):
			t.Errorf("%s is an internal package", pkg)
		default:
			umaDeps++
		}
	}
	if umaDeps == 0 {
		t.Fatal("no uma dependencies listed; wrong directory?")
	}
}

// TestNoInitRegistration pins that the module registers nothing at import:
// no package declares an init function.
func TestNoInitRegistration(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
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
