package model

import "testing"

// A held operation on a label (a definition create, update or delete) is
// shown under the label read right.
func TestReadActionCoversLabels(t *testing.T) {
	if a, ok := ReadAction(KindLabel); !ok || a != ActionLabelRead {
		t.Fatalf("ReadAction(label) = %q, %v", a, ok)
	}
}
