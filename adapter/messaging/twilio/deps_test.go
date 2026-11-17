package twilio_test

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

// TestImportsPublicPackagesOnly pins the module boundary: the adapter
// packages import the standard library, this module, public packages of the
// uma root module and twilio-go (for webhook signatures) with what it
// needs. No uma internal package, no other uma module.
func TestImportsPublicPackagesOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}} {{with .Module}}{{.Path}}{{end}}{{end}}", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	thirdParty := []string{"github.com/twilio/twilio-go", "github.com/pkg/errors", "github.com/golang/mock"}
	var uma int
	for line := range strings.Lines(string(out)) {
		pkg, mod, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case pkg == "":
		case mod == "hop.top/uma/adapter/messaging/twilio":
		case mod == "hop.top/uma":
			if slices.Contains(strings.Split(pkg, "/"), "internal") {
				t.Errorf("%s is an internal package", pkg)
			}
			uma++
		case slices.Contains(thirdParty, mod):
		default:
			t.Errorf("%s comes from module %q", pkg, mod)
		}
	}
	if uma == 0 {
		t.Fatal("no uma dependencies listed; wrong directory?")
	}
}

// TestNoInit pins import side effects: no package of the module declares
// an init function.
func TestNoInit(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	more, err := filepath.Glob("*/*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range append(files, more...) {
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
