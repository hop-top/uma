package widget_test

import (
	"testing"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/adapter/web/widget/widgettest"
)

func TestMemoryLogConformance(t *testing.T) {
	widgettest.RunLogSuite(t, func(_ *testing.T, retain int) widgettest.LogFixture {
		return widgettest.LogFixture{Log: widget.NewMemoryLog(retain)}
	})
}

// Erase deletes a visitor's retained entries and keeps counting, so the
// next entry is not mistaken for one the stream already has.
func TestMemoryLogErase(t *testing.T) {
	l := widget.NewMemoryLog(0)
	a := widget.LogKey{Namespace: widget.NamespaceVisitor, Visitor: "a"}
	b := widget.LogKey{Namespace: widget.NamespaceVisitor, Visitor: "b"}
	for _, k := range []widget.LogKey{a, a, b} {
		if _, err := l.Append(t.Context(), k, widget.Entry{Event: widget.Event{Type: widget.EventMessage}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := l.Erase(a); n != 2 {
		t.Fatalf("Erase = %d, want 2", n)
	}
	if n := l.Erase(widget.LogKey{Namespace: widget.NamespaceVisitor, Visitor: "never"}); n != 0 {
		t.Errorf("Erase of an unknown visitor = %d, want 0", n)
	}
	if got, err := l.Read(t.Context(), a, 0, 0); err != nil || len(got) != 0 {
		t.Fatalf("Read after Erase = %+v, %v; want nothing", got, err)
	}
	if got, err := l.Read(t.Context(), b, 0, 0); err != nil || len(got) != 1 {
		t.Fatalf("other visitor = %+v, %v; want its entry", got, err)
	}
	if seq, err := l.Append(t.Context(), a, widget.Entry{Event: widget.Event{Type: widget.EventMessage}}); err != nil || seq != 3 {
		t.Fatalf("Append after Erase = %d, %v; want 3", seq, err)
	}
}
