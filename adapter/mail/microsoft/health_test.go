package microsoft

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// healthLog records what the receiver tells the host.
type healthLog struct {
	mu    sync.Mutex
	codes []string
	links []bool
}

func (r *healthLog) degraded(c string) { r.mu.Lock(); r.codes = append(r.codes, c); r.mu.Unlock() }
func (r *healthLog) link(c bool)       { r.mu.Lock(); r.links = append(r.links, c); r.mu.Unlock() }
func (r *healthLog) got() []string     { r.mu.Lock(); defer r.mu.Unlock(); return slices.Clone(r.codes) }

func newHealthEnv(t *testing.T) (*env, *healthLog) {
	e := newEnv(t, Config{NotificationURL: hookURL})
	r := &healthLog{}
	e.inst.ReportReceiverDegraded(r.degraded)
	e.inst.ReportReceiver(r.link)
	return e, r
}

// failSubscriptions makes creating and renewing a subscription answer
// status; 0 restores the fake.
func failSubscriptions(e *env, status int) {
	e.g.mu.Lock()
	defer e.g.mu.Unlock()
	if status == 0 {
		e.g.override = nil
		return
	}
	e.g.override = func(w http.ResponseWriter, r *http.Request) bool {
		if (r.Method != "POST" && r.Method != "PATCH") || !strings.HasPrefix(r.URL.Path, "/v1.0/subscriptions") {
			return false
		}
		graphError(w, status, "ValidationError")
		return true
	}
}

// A permanent failure (Graph rejects the notification URL) degrades at once,
// delta polling still delivers, and a later success clears it.
func TestSubscriptionRejectedDegradesAndRecovers(t *testing.T) {
	e, r := newHealthEnv(t)
	failSubscriptions(e, http.StatusBadRequest)
	sink := &testSink{}
	if err := round(t, e, sink); err != nil {
		t.Fatal(err)
	}
	e.inbound("m1", "peer@example.test", "while push is off")
	if err := round(t, e, sink); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{CodeSubscriptionRejected}) {
		t.Fatalf("reports %v, want one %q", got, CodeSubscriptionRejected)
	}
	if got := sink.ids(); !slices.Equal(got, []string{"m1"}) {
		t.Fatalf("accepted %v, polling must still deliver", got)
	}
	failSubscriptions(e, 0)
	if err := round(t, e, sink); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{CodeSubscriptionRejected, ""}) {
		t.Fatalf("reports %v, want degraded then cleared", got)
	}
}

// A missing notification secret is its own code.
func TestClientStateMissingDegrades(t *testing.T) {
	e, r := newHealthEnv(t)
	e.creds.mu.Lock()
	e.creds.secret = nil
	e.creds.mu.Unlock()
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{CodeClientStateMissing}) {
		t.Fatalf("reports %v, want %q", got, CodeClientStateMissing)
	}
	e.creds.mu.Lock()
	e.creds.secret = []byte("notification-secret")
	e.creds.mu.Unlock()
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{CodeClientStateMissing, ""}) {
		t.Fatalf("reports %v, want cleared once the secret is back", got)
	}
}

// Transient failures degrade after three rounds in a row.
func TestSubscriptionTransientDegradesAfterThreeRounds(t *testing.T) {
	e, r := newHealthEnv(t)
	failSubscriptions(e, http.StatusServiceUnavailable)
	for range 2 {
		if err := round(t, e, &testSink{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := r.got(); len(got) != 0 {
		t.Fatalf("reports %v after two failed rounds, want none", got)
	}
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{CodeSubscriptionUnavailable}) {
		t.Fatalf("reports %v, want %q", got, CodeSubscriptionUnavailable)
	}
}

// A transient renewal failure degrades at once when the subscription would
// expire before the next attempt.
func TestSubscriptionTransientDegradesWhenItExpires(t *testing.T) {
	e, r := newHealthEnv(t)
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	st, err := e.inst.loadState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	failSubscriptions(e, http.StatusServiceUnavailable)
	e.clock.Advance(st.SubscriptionExpires.Add(-2 * time.Minute).Sub(e.clock.Now()))
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if got := r.got(); !slices.Equal(got, []string{CodeSubscriptionUnavailable}) {
		t.Fatalf("reports %v, want %q at the first failure", got, CodeSubscriptionUnavailable)
	}
}

func TestReceiverLinkReportedAfterRound(t *testing.T) {
	e, r := newHealthEnv(t)
	failSubscriptions(e, http.StatusBadRequest)
	if err := round(t, e, &testSink{}); err != nil {
		t.Fatal(err)
	}
	if len(r.links) != 1 || !r.links[0] {
		t.Fatalf("link reports %v, want one connected", r.links)
	}
}
