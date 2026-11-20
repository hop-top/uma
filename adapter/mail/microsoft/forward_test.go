package microsoft

import (
	"strings"
	"testing"
)

// A forward needs the original attached as message/rfc822; Graph has no
// way to attach a raw message to a draft, so the adapter ships no forward
// effect and a host refuses the verb offline instead of sending a
// lossy copy.
func TestExtensionsShipNoForward(t *testing.T) {
	for _, d := range NewFactory(Options{}).Extensions() {
		if strings.HasSuffix(d.Name, ".forward") {
			t.Errorf("Extensions ships %s", d.Name)
		}
	}
}
