package memory

import (
	"go/build"
	"strings"
	"testing"
)

// TestImportsStandardLibraryAndCoreOnly pins the package boundary: the memory
// store imports the standard library, the uma core contracts and the
// store's own shared tombstone, search and interaction rules, nothing else.
func TestImportsStandardLibraryAndCoreOnly(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"hop.top/uma/model": true, "hop.top/uma/ports": true, "hop.top/uma/store": true,
		"hop.top/uma/store/internal/tombstone": true, "hop.top/uma/store/internal/search": true,
		"hop.top/uma/store/internal/interactions": true,
	}
	for _, imp := range pkg.Imports {
		first, _, _ := strings.Cut(imp, "/")
		if strings.Contains(first, ".") && !allowed[imp] {
			t.Errorf("memory imports %q; only the standard library and uma core packages are allowed", imp)
		}
	}
}
