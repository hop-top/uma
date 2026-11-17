package discord

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

// socketModule is the one third-party module the adapter imports: the
// WebSocket client of the gateway receiver. It has no dependencies of
// its own.
const socketModule = "github.com/coder/websocket"

// TestImportsPublicPackagesOnly pins the module boundary: the adapter's
// transitive imports are the standard library, this module, public
// packages of the uma root module and socketModule.
func TestImportsPublicPackagesOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}} {{with .Module}}{{.Path}}{{end}}{{end}}", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var umaDeps int
	for line := range strings.Lines(string(out)) {
		pkg, mod, _ := strings.Cut(strings.TrimSpace(line), " ")
		switch {
		case pkg == "", mod == "hop.top/uma/adapter/chat/discord", mod == socketModule:
		case mod != "hop.top/uma":
			t.Errorf("%s comes from module %q", pkg, mod)
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

// TestNoInitRegistration pins that the package registers nothing at
// import.
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
