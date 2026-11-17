package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// goList runs go list on this module as a consumer resolves it.
func goList(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...) // #nosec G204 -- fixed command, test-chosen arguments
	cmd.Env = append(scrubbedEnv(), "GOWORK=off", "GOFLAGS=-buildvcs=false")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// TestPublicAPIOnly pins what the host is written against: uma's public
// packages and the public store and adapter packages, never an internal
// package of a uma module. Nothing it pulls in, directly or not, is
// hop.top/kit or heo.
func TestPublicAPIOnly(t *testing.T) {
	imports := goList(t, "-f", "{{join .Imports \"\\n\"}}\n{{join .TestImports \"\\n\"}}", ".")
	for _, imp := range imports {
		if strings.HasPrefix(imp, "hop.top/uma") && slices.Contains(strings.Split(imp, "/"), "internal") {
			t.Errorf("host imports internal package %q", imp)
		}
	}
	for _, want := range []string{"hop.top/uma", "hop.top/uma/store/postgres", "hop.top/uma/adapter/web/widget", "hop.top/uma/adapter/web/widget/storelog", "hop.top/uma/adapter/mail/smtp"} {
		if !slices.Contains(imports, want) {
			t.Errorf("host does not import %s; imports %q", want, imports)
		}
	}
	for _, dep := range goList(t, "-deps", "-test", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".") {
		segs := strings.Split(dep, "/")
		if strings.HasPrefix(dep, "hop.top/kit") || slices.Contains(segs, "heo") {
			t.Errorf("host depends on %q", dep)
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
