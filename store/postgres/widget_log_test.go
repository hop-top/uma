package postgres_test

import (
	"slices"
	"sync"
	"testing"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/adapter/web/widget/storelog"
	"hop.top/uma/adapter/web/widget/widgettest"
	"hop.top/uma/model"
	"hop.top/uma/store"
	"hop.top/uma/store/postgres"
)

func putWidgetChannel(t *testing.T, st store.Store, ref model.ChannelRef) {
	t.Helper()
	if err := st.Transact(t.Context(), ref.Tenant, func(tx store.Tx) error {
		return tx.PutChannel(t.Context(), model.Channel{Ref: ref, Type: widget.Type, Revision: "r1"}, "")
	}); err != nil {
		t.Fatalf("PutChannel: %v", err)
	}
}

func newWidgetLog(t *testing.T, st store.Store, retain int) *storelog.Log {
	t.Helper()
	l, err := storelog.New(st, storelog.Options{Retain: retain})
	if err != nil {
		t.Fatalf("storelog.New: %v", err)
	}
	return l
}

// TestWidgetLogConformance runs the shared widget.Log suite on the
// store-backed log over PostgreSQL.
func TestWidgetLogConformance(t *testing.T) {
	widgettest.RunLogSuite(t, func(t *testing.T, retain int) widgettest.LogFixture {
		st := fixture(t).Store
		return widgettest.LogFixture{
			Log:     newWidgetLog(t, st, retain),
			Channel: func(t *testing.T, ref model.ChannelRef) { putWidgetChannel(t, st, ref) },
		}
	})
}

// TestWidgetLogSurvivesRestart: entries appended before a crash are read
// by the restarted host's log, which continues the sequence and the
// retention window.
func TestWidgetLogSurvivesRestart(t *testing.T) {
	st := fixture(t).Store.(*postgres.Store)
	ch := model.ChannelRef{Tenant: widgettest.TenantA, ID: widgettest.Channel}
	putWidgetChannel(t, st, ch)
	key := widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor, Visitor: "v"}
	before := newWidgetLog(t, st, 3)
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := before.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: id}}); err != nil {
			t.Fatal(err)
		}
	}
	after := newWidgetLog(t, postgres.RestartTestStore(t, st), 3)
	got, err := after.Read(t.Context(), key, 1, 0)
	if err != nil || len(got) != 2 || got[0].Seq != 2 || got[1].Event.Message != "m3" {
		t.Fatalf("Read after restart = %+v, %v; want m2 and m3", got, err)
	}
	if seq, err := after.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage, Message: "m4"}}); err != nil || seq != 4 {
		t.Fatalf("Append after restart = %d, %v; want 4", seq, err)
	}
	if got, err := after.Read(t.Context(), key, 0, 0); err != nil || got[0].Seq != 2 || len(got) != 3 {
		t.Fatalf("Read after the next append = %+v, %v; want seqs 2..4", got, err)
	}
}

// TestWidgetLogAcrossProcesses: workers in two processes, each with its
// own pool and Log, append to one visitor; every sequence is assigned
// once and the log holds them all.
func TestWidgetLogAcrossProcesses(t *testing.T) {
	pool := postgres.NewTestDatabase(t, true)
	open := func() store.Store {
		st, err := postgres.Open(t.Context(), postgres.NewTestPool(t, pool), postgres.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	stores := []store.Store{open(), open()}
	ch := model.ChannelRef{Tenant: widgettest.TenantA, ID: widgettest.Channel}
	putWidgetChannel(t, stores[0], ch)
	key := widget.LogKey{Channel: ch, Namespace: widget.NamespaceVisitor, Visitor: "v"}
	const perWorker = 15
	var (
		mu   sync.Mutex
		seqs []uint64
		wg   sync.WaitGroup
	)
	for i := range 6 {
		l := newWidgetLog(t, stores[i%2], 1000)
		wg.Go(func() {
			for n := 0; n < perWorker; {
				seq, err := l.Append(t.Context(), key, widget.Entry{Event: widget.Event{Type: widget.EventMessage}})
				switch {
				case err == nil:
					mu.Lock()
					seqs = append(seqs, seq)
					mu.Unlock()
					n++
				case model.CodeOf(err) != model.Transient:
					t.Errorf("Append: %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
	slices.Sort(seqs)
	got, err := newWidgetLog(t, stores[1], 1000).Read(t.Context(), key, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != 6*perWorker || len(got) != len(seqs) {
		t.Fatalf("returned %d seqs, stored %d; want %d", len(seqs), len(got), 6*perWorker)
	}
	var want uint64
	for i := range seqs {
		want++
		if seqs[i] != want || got[i].Seq != want {
			t.Fatalf("returned seq %d, stored %d; want %d", seqs[i], got[i].Seq, want)
		}
	}
}
