package main

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

const (
	umaModule  = "hop.top/uma"
	thisModule = "hop.top/uma/examples"
	// ownInternal holds this module's shared example code (the chat page and
	// the contract suite). It is the examples' own, not uma's.
	ownInternal = thisModule + "/internal/"
)

// TestImportsPublicPackagesOnly pins the adopter boundary: the demo, its
// tests included, imports no internal uma package, and everything it
// builds on comes from the standard library or the uma root module. This
// module's own internal packages (shared page, contract suite) are the
// examples', not uma's, and are allowed; their imports are held to the same
// rules.
func TestImportsPublicPackagesOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-test",
		"-f", "{{if not .Standard}}{{.ImportPath}}\t{{with .Module}}{{.Path}}{{end}}\t{{join .Imports \",\"}}{{end}}", ".")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	var direct []string
	for line := range strings.Lines(string(out)) {
		fields := strings.Split(strings.TrimSuffix(line, "\n"), "\t")
		if len(fields) != 3 {
			continue
		}
		pkg, mod := fields[0], fields[1]
		switch mod {
		case umaModule:
		case thisModule:
			direct = append(direct, strings.Split(fields[2], ",")...)
		default:
			t.Errorf("%s comes from module %q; only the standard library and %s are allowed", pkg, mod, umaModule)
		}
	}
	for _, imp := range direct {
		if strings.HasPrefix(imp, ownInternal) {
			continue
		}
		if strings.HasPrefix(imp, umaModule+"/") && slices.Contains(strings.Split(imp, "/"), "internal") {
			t.Errorf("imports internal package %s", imp)
		}
	}
	for _, want := range []string{"hop.top/uma", "hop.top/uma/adapter/web/widget", "hop.top/uma/companions/bot", "hop.top/uma/store/memory"} {
		if !slices.Contains(direct, want) {
			t.Errorf("does not import %s", want)
		}
	}
}
