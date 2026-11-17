package model

import (
	"go/build"
	"strings"
	"testing"
)

// TestModelImportsStandardLibraryOnly pins the package boundary: model imports
// no other uma package and no third-party module, so every adopter can depend
// on it without pulling engine, storage or adapter code.
func TestModelImportsStandardLibraryOnly(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Imports) == 0 {
		t.Fatal("no imports found; wrong directory?")
	}
	for _, imp := range pkg.Imports {
		first, _, _ := strings.Cut(imp, "/")
		if strings.HasPrefix(imp, "hop.top/") || strings.Contains(first, ".") {
			t.Errorf("model imports %q; only the standard library is allowed", imp)
		}
	}
}
