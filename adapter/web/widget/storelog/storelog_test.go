package storelog_test

import (
	"testing"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/adapter/web/widget/storelog"
	"hop.top/uma/adapter/web/widget/widgettest"
	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

// putChannel stores the channel ref in st.
func putChannel(t *testing.T, st store.Store, ref model.ChannelRef) {
	t.Helper()
	if err := st.Transact(t.Context(), ref.Tenant, func(tx store.Tx) error {
		return tx.PutChannel(t.Context(), model.Channel{Ref: ref, Type: widget.Type, Revision: "r1"}, "")
	}); err != nil {
		t.Fatalf("PutChannel: %v", err)
	}
}

func newLog(t *testing.T, st store.Store, retain int) *storelog.Log {
	t.Helper()
	l, err := storelog.New(st, storelog.Options{Retain: retain})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func TestConformanceOnMemoryStore(t *testing.T) {
	widgettest.RunLogSuite(t, func(t *testing.T, retain int) widgettest.LogFixture {
		st := memory.New(memory.Options{})
		return widgettest.LogFixture{
			Log:     newLog(t, st, retain),
			Channel: func(t *testing.T, ref model.ChannelRef) { putChannel(t, st, ref) },
		}
	})
}

// TestEntriesOutliveTheLog: a new Log on the same store, as a restarted
// host builds, reads what the old one appended and continues its sequence.
func TestEntriesOutliveTheLog(t *testing.T) {
	st := memory.New(memory.Options{})
	ch := model.ChannelRef{Tenant: widgettest.TenantA, ID: widgettest.Channel}
	putChannel(t, st, ch)
	key := widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor, Visitor: "v"}
	first := newLog(t, st, 0)
	for range 2 {
		if _, err := first.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: "m"}}); err != nil {
			t.Fatal(err)
		}
	}
	next := newLog(t, st, 0)
	got, err := next.Read(t.Context(), key, 0, 0)
	if err != nil || len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("Read after restart = %+v, %v; want seqs 1 and 2", got, err)
	}
	if seq, err := next.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage}}); err != nil || seq != 3 {
		t.Fatalf("Append after restart = %d, %v; want 3", seq, err)
	}
}

func TestDefaultRetain(t *testing.T) {
	st := memory.New(memory.Options{})
	ch := model.ChannelRef{Tenant: widgettest.TenantA, ID: widgettest.Channel}
	putChannel(t, st, ch)
	key := widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor, Visitor: "v"}
	l := newLog(t, st, 0)
	for range widget.DefaultRetain + 1 {
		if _, err := l.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventSignal, Kind: "typing"}}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := l.Read(t.Context(), key, 0, 0)
	if err != nil || len(got) != widget.DefaultRetain || got[0].Seq != 2 {
		t.Fatalf("Read = %d entries from seq %d, %v; want DefaultRetain from 2", len(got), got[0].Seq, err)
	}
}

// TestRefusals: an unstored channel and a malformed key refuse the event
// for good; nothing is stored.
func TestRefusals(t *testing.T) {
	st := memory.New(memory.Options{})
	l := newLog(t, st, 0)
	ch := model.ChannelRef{Tenant: widgettest.TenantA, ID: widgettest.Channel}
	e := widget.Entry{Event: widget.Event{Type: widget.EventMessage}}
	if _, err := l.Append(t.Context(), widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor, Visitor: "v"}, e); model.CodeOf(err) != model.NotFound {
		t.Errorf("Append on an unstored channel = %v, want not_found", err)
	}
	putChannel(t, st, ch)
	if _, err := l.Append(t.Context(), widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor}, e); model.CodeOf(err) != model.Validation {
		t.Errorf("Append without a visitor = %v, want validation", err)
	}
	if _, err := storelog.New(nil, storelog.Options{}); model.CodeOf(err) != model.Validation {
		t.Errorf("New(nil) = %v, want validation", err)
	}
}

// TestErasedLogReadsEmptyAndKeepsCounting: a visitor's log erased through
// the store (what a privacy cascade on the visitor's contact does, see
// store.RecipientLogKey) reads empty, and a reconnecting stream resuming
// from an old cursor finds the gap on the next entry instead of missing it.
func TestErasedLogReadsEmptyAndKeepsCounting(t *testing.T) {
	st := memory.New(memory.Options{})
	ch := model.ChannelRef{Tenant: widgettest.TenantA, ID: widgettest.Channel}
	putChannel(t, st, ch)
	key := widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor, Visitor: "v"}
	l := newLog(t, st, 0)
	for range 2 {
		if _, err := l.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: "m"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Transact(t.Context(), ch.Tenant, func(tx store.Tx) error {
		n, err := tx.EraseRecipientLog(t.Context(), store.RecipientLogKey{Channel: ch, Namespace: key.Namespace, Recipient: key.Visitor})
		if err == nil && n != 2 {
			t.Errorf("EraseRecipientLog = %d, want 2", n)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Read(t.Context(), key, 0, 0); err != nil || len(got) != 0 {
		t.Fatalf("Read after erase = %+v, %v; want nothing", got, err)
	}
	if seq, err := l.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: "m"}}); err != nil || seq != 3 {
		t.Fatalf("Append after erase = %d, %v; want 3", seq, err)
	}
	if got, err := l.Read(t.Context(), key, 1, 0); err != nil || len(got) != 1 || got[0].Seq != 3 {
		t.Fatalf("Read from cursor 1 = %+v, %v; want seq 3 only", got, err)
	}
}
