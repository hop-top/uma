package kit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// kitAllowed lists the only hop.top/kit packages the module may build
// against: interfaces the host implements or constructs, never a backend.
// The blob backends (storage/blob/local, storage/blob/s3 and the AWS SDK
// behind it) appear in tests only; a host using one imports it itself.
var kitAllowed = map[string]bool{
	"hop.top/kit/go/storage/secret": true,
	"hop.top/kit/go/storage/blob":   true,
}

type pkg struct{ path, dir, files string }

// moduleDeps lists the non-standard packages ./... builds against (tests
// excluded), as a consumer resolves them.
func moduleDeps(t *testing.T) []pkg {
	t.Helper()
	// Fixed arguments only.
	cmd := exec.Command("go", "list", "-deps", "-f", // #nosec G204 -- fixed command and arguments
		"{{if not .Standard}}{{.ImportPath}}\t{{.Dir}}\t{{join .GoFiles \",\"}}{{end}}", "./...")
	cmd.Env = append(scrubbedEnv(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	var deps []pkg
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 3 {
			t.Fatalf("go list line %q", line)
		}
		deps = append(deps, pkg{f[0], f[1], f[2]})
	}
	return deps
}

// TestModuleDependsOnKitInterfacesOnly pins the import boundary: besides
// the standard library and uma, the module builds against kit's
// storage/secret and storage/blob packages and nothing else from kit, so
// no backend, provider SDK, bus, telemetry, kv or notify package is linked
// and nothing self-registers.
func TestModuleDependsOnKitInterfacesOnly(t *testing.T) {
	deps := moduleDeps(t)
	seen := map[string]bool{}
	for _, d := range deps {
		seen[d.path] = true
		switch {
		case d.path == "hop.top/uma/contrib/kit" || strings.HasPrefix(d.path, "hop.top/uma/contrib/kit/"):
		case d.path == "hop.top/uma" || strings.HasPrefix(d.path, "hop.top/uma/"):
		case kitAllowed[d.path]:
		default:
			t.Errorf("module depends on %q", d.path)
		}
	}
	for p := range kitAllowed {
		if !seen[p] {
			t.Errorf("module no longer depends on %q; update kitAllowed", p)
		}
	}
	for _, p := range []string{"hop.top/uma/contrib/kit/secret", "hop.top/uma/contrib/kit/blob"} {
		if !seen[p] {
			t.Errorf("go list did not report %s: %v", p, deps)
		}
	}
}

// TestKitPackagesHaveNoSideEffects re-checks, at the pinned kit version,
// that every kit package the module links declares no init and reads no
// environment variable. A kit upgrade that adds either fails here.
func TestKitPackagesHaveNoSideEffects(t *testing.T) {
	n := 0
	for _, d := range moduleDeps(t) {
		if !kitAllowed[d.path] {
			continue
		}
		for _, name := range strings.Split(d.files, ",") {
			n++
			f := parse(t, filepath.Join(d.dir, name))
			checkNoInit(t, d.path+"/"+name, f)
			ast.Inspect(f, func(node ast.Node) bool {
				sel, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); ok && (id.Name == "os" || id.Name == "syscall") {
					switch sel.Sel.Name {
					case "Getenv", "LookupEnv", "Environ", "ExpandEnv":
						t.Errorf("%s/%s reads the environment (%s.%s)", d.path, name, id.Name, sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
	if n == 0 {
		t.Fatal("no kit source files inspected")
	}
}

// TestModuleHasNoInit pins import side effects in this module: no
// non-test file declares an init function.
func TestModuleHasNoInit(t *testing.T) {
	n := 0
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") && path != ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		n++
		checkNoInit(t, path, parse(t, path))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 2 {
		t.Fatalf("inspected %d files", n)
	}
}

func parse(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func checkNoInit(t *testing.T, name string, f *ast.File) {
	t.Helper()
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "init" {
			t.Errorf("%s declares func init", name)
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
