package storereplay_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestImportsStandardLibraryAndPublicContractsOnly pins the package's
// boundary: it lives in the root module beside the verifier it serves, so
// it may import the standard library, the verifier and uma's public
// contract packages, never a backend (those import nothing of it).
func TestImportsStandardLibraryAndPublicContractsOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".") // #nosec G204 -- fixed command and arguments
	cmd.Env = append(scrubbedEnv(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	allowed := map[string]bool{
		"hop.top/uma/adapter/web/webhookauth/storereplay": true, "hop.top/uma/adapter/web/webhookauth": true,
		"hop.top/uma/model": true, "hop.top/uma/ports": true, "hop.top/uma/store": true,
	}
	deps := strings.Fields(string(out))
	if len(deps) < 2 {
		t.Fatalf("go list returned %q; wrong directory?", deps)
	}
	for _, dep := range deps {
		if !allowed[dep] {
			t.Errorf("storereplay depends on %q", dep)
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
