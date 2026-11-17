package main

import (
	"fmt"
	"testing"
)

func TestChooseGeneratesOncePerKey(t *testing.T) {
	d := newDecisions()
	n := 0
	gen := func() string { n++; return fmt.Sprintf("draft %d", n) }
	a, b := d.choose("k", gen), d.choose("k", gen)
	if a != b || n != 1 {
		t.Fatalf("choose = %q, %q after %d generations, want one stable value", a, b, n)
	}
	if c := d.choose("other", gen); c == a {
		t.Fatalf("different key reused %q", c)
	}
}
