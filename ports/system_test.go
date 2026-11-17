package ports_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
)

var (
	_ ports.Clock = ports.SystemClock{}
	_ ports.IDs   = (*ports.UUIDv7)(nil)
)

func TestSystemClockReadsWallTime(t *testing.T) {
	before := time.Now()
	got := ports.SystemClock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Fatalf("Now = %v, want within [%v, %v]", got, before, after)
	}
}

func TestUUIDv7IsCanonicalVersion7(t *testing.T) {
	ids := ports.NewUUIDv7(nil)
	id, err := ids.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.ParseID(string(id)); err != nil {
		t.Fatalf("%q is not canonical: %v", id, err)
	}
	if id[14] != '7' {
		t.Fatalf("%q: version nibble = %c, want 7", id, id[14])
	}
	if !slices.Contains([]byte("89ab"), id[19]) {
		t.Fatalf("%q: variant nibble = %c, want 8, 9, a or b", id, id[19])
	}
}

func TestUUIDv7CarriesClockMillisecondsInFront(t *testing.T) {
	at := time.UnixMilli(0x0123456789ab)
	id, err := ports.NewUUIDv7(portstest.NewFixedClock(at)).New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := string(id[:13]); got != "01234567-89ab" {
		t.Fatalf("%q: timestamp field = %q, want 01234567-89ab", id, got)
	}
}

func TestUUIDv7NeverRepeatsAndIncreasesWithinOneInstance(t *testing.T) {
	// A frozen clock forces every ID into one millisecond: the counter and
	// its overflow into the next millisecond carry the ordering.
	clock := portstest.NewFixedClock(time.UnixMilli(1_700_000_000_000))
	ids := ports.NewUUIDv7(clock)
	var prev model.ID
	for i := range 3 * 4096 {
		id, err := ids.New(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if id <= prev {
			t.Fatalf("ID %d = %q, not above the previous %q", i, id, prev)
		}
		prev = id
	}
}

func TestUUIDv7ClockStepBackwardsKeepsIncreasing(t *testing.T) {
	clock := portstest.NewFixedClock(time.UnixMilli(1_700_000_000_000))
	ids := ports.NewUUIDv7(clock)
	first, err := ids.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(-time.Hour)
	second, err := ids.New(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second <= first {
		t.Fatalf("after a backwards step got %q, want above %q", second, first)
	}
}

func TestUUIDv7ConcurrentCallsNeverRepeat(t *testing.T) {
	ids := ports.NewUUIDv7(ports.SystemClock{})
	const workers, each = 8, 2000
	out := make([][]model.ID, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				id, err := ids.New(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				out[w] = append(out[w], id)
			}
		}()
	}
	wg.Wait()
	seen := map[model.ID]bool{}
	for _, batch := range out {
		for _, id := range batch {
			if seen[id] {
				t.Fatalf("repeated %q", id)
			}
			seen[id] = true
		}
	}
}

func TestUUIDv7ZeroValueWorks(t *testing.T) {
	var ids ports.UUIDv7
	a, errA := ids.New(context.Background())
	b, errB := ids.New(context.Background())
	if errA != nil || errB != nil || a == b {
		t.Fatalf("zero value New = %q, %v then %q, %v", a, errA, b, errB)
	}
}

func TestUUIDv7ClockOutOfRangeIsPermanentAndLeavesGeneratorUsable(t *testing.T) {
	clock := portstest.NewFixedClock(time.UnixMilli(1 << 48))
	ids := ports.NewUUIDv7(clock)
	_, err := ids.New(context.Background())
	if !model.IsCode(err, model.Permanent) {
		t.Fatalf("New = %v, want permanent", err)
	}
	clock.Set(time.UnixMilli(0x0123456789ab))
	id, err := ids.New(context.Background())
	if err != nil || id[:13] != "01234567-89ab" {
		t.Fatalf("after recovery New = %q, %v", id, err)
	}
}

func TestUUIDv7ClockBeforeEpochCountsAsEpoch(t *testing.T) {
	id, err := ports.NewUUIDv7(portstest.NewFixedClock(time.UnixMilli(-5))).New(context.Background())
	if err != nil || id[:13] != "00000000-0000" {
		t.Fatalf("New = %q, %v, want timestamp zero", id, err)
	}
}
