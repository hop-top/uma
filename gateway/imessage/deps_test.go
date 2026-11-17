package imessage

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The gateway and its command import the standard library only (and the
// command the gateway): a reference an operator can read in full and run
// without anything else.
func TestImportsStandardLibraryOnly(t *testing.T) {
	cmd := exec.Command("go", "list", "-buildvcs=false", "-deps", "-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", ".", "../../cmd/imessagegateway")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v", err)
	}
	for line := range strings.Lines(string(out)) {
		switch p := strings.TrimSpace(line); p {
		case "", "hop.top/uma/gateway/imessage", "hop.top/uma/cmd/imessagegateway":
		default:
			t.Errorf("%s is not in the standard library", p)
		}
	}
}
