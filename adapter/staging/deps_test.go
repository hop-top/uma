package staging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportsStandardLibraryAndPublicContractsOnly pins the package's
// boundary: it is shared by adapters in several modules, so it may import
// the standard library and uma's public contract packages, nothing else.
// A provider SDK or another adapter would follow every importer.
func TestImportsStandardLibraryAndPublicContractsOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".") // #nosec G204 -- fixed command and arguments
	cmd.Env = append(scrubbedEnv(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		"hop.top/uma/adapter/staging": true, "hop.top/uma/adapter": true,
		"hop.top/uma/model": true, "hop.top/uma/ports": true,
	}
	deps := strings.Fields(string(out))
	if len(deps) < 2 {
		t.Fatalf("go list returned %q; wrong directory?", deps)
	}
	for _, dep := range deps {
		if !allowed[dep] {
			t.Errorf("staging depends on %q", dep)
		}
	}
}

// TestNoInit pins that importing the package registers nothing.
func TestNoInit(t *testing.T) {
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

func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}
