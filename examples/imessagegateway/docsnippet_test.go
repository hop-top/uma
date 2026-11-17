package imessagegateway_test

import (
	"os"
	"strings"
	"testing"
)

// relayPage is the adapter page whose "Set up an iMessage channel" section
// shows the host wiring this module's end-to-end tests run.
const relayPage = "../../docs/adapters/relay.messages.md"

// The Go blocks of the page's iMessage section are the code the tests run:
// each block's lines, ignoring indentation, appear in that order in
// host_test.go or e2e_test.go. A change to either side that the other does
// not follow fails here, so the page cannot drift from compiling code.
func TestSetupSnippetsAreTheTestedCode(t *testing.T) {
	page, err := os.ReadFile(relayPage)
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(page), "\n## Set up an iMessage channel\n")
	if !ok {
		t.Fatal("no \"Set up an iMessage channel\" section")
	}
	if next := strings.Index(section, "\n## "); next >= 0 {
		section = section[:next]
	}
	var sources []string
	for _, f := range []string{"host_test.go", "e2e_test.go"} {
		b, err := os.ReadFile(f) // #nosec G304 -- fixed file names
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, string(b))
	}
	blocks := goBlocks(section)
	if len(blocks) < 2 {
		t.Fatalf("%d Go blocks in the section, want the wiring and the mount", len(blocks))
	}
	for i, b := range blocks {
		found := false
		for _, src := range sources {
			if containsLines(src, b) {
				found = true
			}
		}
		if !found {
			t.Errorf("Go block %d of the section is not the tested code:\n%s", i+1, b)
		}
	}
}

// goBlocks returns the bodies of the ```go fences in md.
func goBlocks(md string) []string {
	var out []string
	for {
		_, rest, ok := strings.Cut(md, "```go\n")
		if !ok {
			return out
		}
		body, after, ok := strings.Cut(rest, "\n```")
		if !ok {
			return out
		}
		out = append(out, body)
		md = after
	}
}

// containsLines reports whether the non-blank lines of block appear in src
// as consecutive non-blank lines, ignoring leading and trailing space.
func containsLines(src, block string) bool {
	norm := func(s string) []string {
		var out []string
		for l := range strings.Lines(s) {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	s, b := norm(src), norm(block)
	for i := 0; i+len(b) <= len(s); i++ {
		match := true
		for j := range b {
			if s[i+j] != b[j] {
				match = false
				break
			}
		}
		if match && len(b) > 0 {
			return true
		}
	}
	return false
}
