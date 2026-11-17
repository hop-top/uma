package host_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// goList runs go list on this package with fixed arguments, outside any
// workspace, as a consumer resolves it.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, append(args, ".")...)...) // #nosec G204 -- fixed command and arguments
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// allowedImports is every non-standard package host imports directly.
var allowedImports = []string{"hop.top/uma", "hop.top/uma/model", "hop.top/uma/companions/bot"}

// TestUsesPublicAPIOnly pins the companion boundary: the package imports the
// standard library and public uma packages only, never the engine
// implementation, and pulls in no third-party code.
func TestUsesPublicAPIOnly(t *testing.T) {
	imports := goList(t, "-f", `{{join .Imports " "}}`)
	if len(imports) == 0 {
		t.Fatal("go list returned no imports")
	}
	var uma int
	for _, p := range imports {
		if !strings.Contains(strings.Split(p, "/")[0], ".") {
			continue // standard library
		}
		if !slices.Contains(allowedImports, p) {
			t.Errorf("imports %q, outside the allow-list %v", p, allowedImports)
		}
		if slices.Contains(strings.Split(p, "/"), "internal") {
			t.Errorf("imports internal package %q", p)
		}
		uma++
	}
	if uma == 0 {
		t.Fatal("no uma imports listed; wrong directory?")
	}
	for _, p := range goList(t, "-deps", "-f", `{{if not .Standard}}{{.ImportPath}}{{end}}`) {
		if p != "hop.top/uma" && !strings.HasPrefix(p, "hop.top/uma/") {
			t.Errorf("depends on non-uma package %q", p)
		}
	}
}
