package tombstone

import (
	"go/build"
	"strings"
	"testing"
)

// TestImportsStandardLibraryAndCoreOnly keeps the shared rules as narrow
// as the backends that import them: standard library and uma core
// contracts only.
func TestImportsStandardLibraryAndCoreOnly(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"hop.top/uma/model": true, "hop.top/uma/ports": true, "hop.top/uma/store": true}
	for _, imp := range pkg.Imports {
		first, _, _ := strings.Cut(imp, "/")
		if strings.Contains(first, ".") && !allowed[imp] {
			t.Errorf("tombstone imports %q; only the standard library and uma core packages are allowed", imp)
		}
	}
}
