package google

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// healthLog records what the receiver tells the host: degradation codes in
// order ("" is recovery) and the link reports.
type healthLog struct {
	mu    sync.Mutex
	codes []string
	links []bool
}

func (r *healthLog) degraded(c string) { r.mu.Lock(); r.codes = append(r.codes, c); r.mu.Unlock() }
func (r *healthLog) link(c bool)       { r.mu.Lock(); r.links = append(r.links, c); r.mu.Unlock() }
func (r *healthLog) got() []string     { r.mu.Lock(); defer r.mu.Unlock(); return slices.Clone(r.codes) }

func newHealthEnv(t *testing.T) (*env, *healthLog) {
	e := newEnv(t, Config{}, pushConfig)
	r := &healthLog{}
	e.inst.ReportReceiverDegraded(r.degraded)
	e.inst.ReportReceiver(r.link)
	return e, r
}

// failWatch makes users.watch answer status; 0 restores the fake.
func failWatch(e *env, status int) {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	if status == 0 {
		e.g.override = nil
		return
	}
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != "POST" || !strings.HasSuffix(r.URL.Path, "/watch") {
			return false
		}
		googleError(w, status, "denied", "PERMISSION_DENIED")
		return true
	}
}

func round1(t *testing.T, e *env, sink *testSink) {
	t.Helper()
	if _, err := e.inst.round(t.Context(), sink); err != nil {
		t.Fatal(err)
	}
}

// A permanent watch failure degrades at once with a stable code, history
// polling still delivers, and the next successful watch clears it.
func TestPushPermanentFailureDegradesAndRecovers(t *testing.T) {
	for _, c := range []struct {
		status int
		code   string
	}{{http.StatusForbidden, CodePushDenied}, {http.StatusBadRequest, CodePushRejected}} {
		e, r := newHealthEnv(t)
		failWatch(e, c.status)
		sink := &testSink{}
		round1(t, e, sink)
		e.inbound("m1", testPeer, "while push is off")
		round1(t, e, sink)
		if got := r.got(); !slices.Equal(got, []string{c.code}) {
			t.Fatalf("status %d: reports %v, want one %q", c.status, got, c.code)
		}
		if got := sink.ids(); !slices.Equal(got, []string{"m1"}) {
			t.Fatalf("status %d: accepted %v, polling must still deliver", c.status, got)
		}
		failWatch(e, 0)
		round1(t, e, sink)
		if got := r.got(); !slices.Equal(got, []string{c.code, ""}) {
			t.Fatalf("status %d: reports %v, want degraded then cleared", c.status, got)
		}
		round1(t, e, sink)
		if got := r.got(); len(got) != 2 {
			t.Fatalf("status %d: reports %v, a healthy round must report nothing", c.status, got)
		}
	}
}

// A transient failure degrades after three rounds in a row, not before;
// one success in between starts the count again.
func TestPushTransientFailureDegradesAfterThreeRounds(t *testing.T) {
	e, r := newHealthEnv(t)
	failWatch(e, http.StatusServiceUnavailable)
	sink := &testSink{}
	round1(t, e, sink)
	round1(t, e, sink)
	if got := r.got(); len(got) != 0 {
		t.Fatalf("reports %v after two failed rounds, want none", got)
	}
	failWatch(e, 0)
	round1(t, e, sink)
	failWatch(e, http.StatusServiceUnavailable)
	e.clock.Advance(25 * time.Hour) // the watch is due again
	round1(t, e, sink)
	round1(t, e, sink)
	if got := r.got(); len(got) != 0 {
		t.Fatalf("reports %v: a success must reset the count", got)
	}
	round1(t, e, sink)
	if got := r.got(); !slices.Equal(got, []string{CodePushUnavailable}) {
		t.Fatalf("reports %v after three failed rounds, want %q", got, CodePushUnavailable)
	}
}

// A transient failure degrades at once when the live watch would expire
// before the next attempt.
func TestPushTransientFailureDegradesWhenWatchExpires(t *testing.T) {
	e, r := newHealthEnv(t)
	st := receiverState{HistoryID: 1000, WatchedAt: testNow.Add(-6 * 24 * time.Hour), WatchExpires: testNow.Add(2 * time.Minute)}
	if err := e.inst.saveState(t.Context(), st); err != nil {
		t.Fatal(err)
	}
	failWatch(e, http.StatusServiceUnavailable)
	round1(t, e, &testSink{})
	if got := r.got(); !slices.Equal(got, []string{CodePushUnavailable}) {
		t.Fatalf("reports %v, want %q at the first failure", got, CodePushUnavailable)
	}
}

// The link reports connected once a round completed, push or not.
func TestReceiverLinkReportedAfterRound(t *testing.T) {
	e, r := newHealthEnv(t)
	failWatch(e, http.StatusForbidden)
	round1(t, e, &testSink{})
	if len(r.links) != 1 || !r.links[0] {
		t.Fatalf("link reports %v, want one connected", r.links)
	}
}
