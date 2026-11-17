package portstest

import (
	"context"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
)

func TestSequenceIDs(t *testing.T) {
	ctx := context.Background()
	ids := NewSequenceIDs(0x2a)
	want := []model.ID{
		"0000002a-0000-7000-8000-000000000001",
		"0000002a-0000-7000-8000-000000000002",
		"0000002a-0000-7000-8000-000000000003",
	}
	for _, w := range want {
		got, err := ids.New(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Fatalf("New = %q, want %q", got, w)
		}
		if _, err := model.ParseID(string(got)); err != nil {
			t.Fatalf("%q is not a canonical ID: %v", got, err)
		}
	}
	other, _ := NewSequenceIDs(0x2b).New(ctx)
	if other != "0000002b-0000-7000-8000-000000000001" {
		t.Fatalf("seeds do not separate instances: %q", other)
	}
	var zero SequenceIDs
	if id, err := zero.New(ctx); err != nil || id != "00000000-0000-7000-8000-000000000001" {
		t.Fatalf("zero value New = %q, %v", id, err)
	}
}

func TestSequenceIDsConcurrent(t *testing.T) {
	ids := NewSequenceIDs(1)
	const n = 200
	seen := make(chan model.ID, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := ids.New(context.Background())
			if err != nil {
				t.Error(err)
			}
			seen <- id
		}()
	}
	wg.Wait()
	close(seen)
	unique := map[model.ID]bool{}
	for id := range seen {
		if unique[id] {
			t.Fatalf("duplicate %q", id)
		}
		unique[id] = true
	}
	if len(unique) != n {
		t.Fatalf("got %d IDs, want %d", len(unique), n)
	}
}

func TestSequenceIDsExhausted(t *testing.T) {
	ids := NewSequenceIDs(1)
	ids.n.Store(maxSequence - 1)
	last, err := ids.New(context.Background())
	if err != nil || last != "00000001-0000-7000-8000-ffffffffffff" {
		t.Fatalf("last = %q, %v", last, err)
	}
	if _, err := ids.New(context.Background()); model.CodeOf(err) != model.Permanent {
		t.Fatalf("exhausted generator error = %v", err)
	}
}

func TestFixedClock(t *testing.T) {
	start := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	c := NewFixedClock(start)
	first := c.Now()
	time.Sleep(time.Millisecond)
	if !first.Equal(start) || !c.Now().Equal(first) {
		t.Fatal("fixed clock moved on its own")
	}
	if got := c.Advance(time.Minute); !got.Equal(start.Add(time.Minute)) || !c.Now().Equal(got) {
		t.Fatalf("Advance = %v", got)
	}
	later := start.Add(time.Hour)
	c.Set(later)
	if !c.Now().Equal(later) {
		t.Fatalf("Set did not take: %v", c.Now())
	}
	var zero FixedClock
	if !zero.Now().IsZero() {
		t.Fatal("zero FixedClock not at zero time")
	}
}

var (
	_ ports.IDs   = (*SequenceIDs)(nil)
	_ ports.Clock = (*FixedClock)(nil)
)
