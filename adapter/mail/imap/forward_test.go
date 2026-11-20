package imap

import (
	"strings"
	"testing"
)

// imap.mail ships no forward effect: a host refuses the verb offline.
func TestExtensionsShipNoForward(t *testing.T) {
	for _, d := range (&Factory{}).Extensions() {
		if strings.HasSuffix(d.Name, ".forward") {
			t.Errorf("Extensions ships %s", d.Name)
		}
	}
}
