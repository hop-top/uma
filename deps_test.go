package uma

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

// corePackages are the packages every adopter imports.
var corePackages = []string{".", "./model", "./adapter", "./store", "./policy", "./ports"}

// testSupport are internal packages only tests import.
var testSupport = map[string]bool{"internal/enginetest": true}

// internalPackages returns the engine implementation packages under
// internal/, with or without the test-support ones.
func internalPackages(t *testing.T, withTestSupport bool) []string {
	t.Helper()
	dirs, err := filepath.Glob("internal/*")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		if !withTestSupport && testSupport[filepath.ToSlash(d)] {
			continue
		}
		out = append(out, "./"+filepath.ToSlash(d))
	}
	if len(out) == 0 {
		t.Fatal("no internal packages found")
	}
	return out
}

// TestCoreDependsOnStandardLibraryOnly pins the import boundary: the core
// packages and the engine implementation under internal/ transitively
// import only the standard library and each other, so importing uma never
// pulls hop.top/kit, heo or any provider SDK.
func TestCoreDependsOnStandardLibraryOnly(t *testing.T) {
	// Fixed arguments only: core packages and directories under internal/.
	args := append([]string{"list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}"}, corePackages...)
	args = append(args, internalPackages(t, false)...)
	cmd := exec.Command("go", args...) // #nosec G204 -- fixed command and arguments
	cmd.Env = append(scrubbedEnv(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		"hop.top/uma": true, "hop.top/uma/model": true, "hop.top/uma/adapter": true,
		"hop.top/uma/store": true, "hop.top/uma/policy": true, "hop.top/uma/ports": true,
	}
	deps := strings.Fields(string(out))
	if len(deps) < len(allowed) {
		t.Fatalf("go list returned %d packages: %q", len(deps), deps)
	}
	for _, dep := range deps {
		if strings.Contains(dep, "hop.top/kit") || strings.Contains(dep, "heo") {
			t.Errorf("core depends on %q", dep)
		}
		internal := strings.HasPrefix(dep, "hop.top/uma/internal/") &&
			!testSupport[strings.TrimPrefix(dep, "hop.top/uma/")]
		if !allowed[dep] && !internal {
			t.Errorf("core depends on non-core package %q", dep)
		}
	}
}

// TestCoreHasNoInit pins import side effects: no core or internal package
// declares an init function, so importing uma registers and starts nothing.
func TestCoreHasNoInit(t *testing.T) {
	for _, dir := range append(slices.Clone(corePackages), internalPackages(t, true)...) {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, name := range files {
			if strings.HasSuffix(name, "_test.go") {
				continue
			}
			n++
			f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range f.Decls {
				if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
					t.Errorf("%s declares func init", name)
				}
			}
		}
		if n == 0 {
			t.Errorf("%s: no Go files found", dir)
		}
	}
}

// scrubbedEnv drops GIT_* variables that git hooks export, so the child go
// command never inspects the caller's repository state.
func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return env
}
