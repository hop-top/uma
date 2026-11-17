package google

import (
	"sync"
	"sync/atomic"
	"time"

	"hop.top/uma/model"
)

// Codes the receiver reports through adapter.ReceiverDegradationReporter
// while push notifications are off and only polling covers the mailbox.
const (
	// CodePushDenied: users.watch was refused for lack of permission, such
	// as the Pub/Sub topic not granting Gmail publish rights.
	CodePushDenied = string(Type) + ".push_denied"
	// CodePushRejected: users.watch was refused for another permanent
	// reason, such as a topic name Gmail does not accept.
	CodePushRejected = string(Type) + ".push_rejected"
	// CodePushUnavailable: users.watch kept failing for a transient
	// reason, or would expire before the next attempt.
	CodePushUnavailable = string(Type) + ".push_unavailable"
)

// pushFailureRounds is how many consecutive rounds a transient failure
// (throttling, 5xx, no answer) of the users.watch push registration may last before the receiver
// reports itself degraded. A permanent failure degrades at once.
const pushFailureRounds = 3

// pushHealth tracks whether the users.watch push registration works and reports it. It is
// safe for concurrent use; the zero value reports nothing until bound.
type pushHealth struct {
	mu       sync.Mutex
	report   func(string)
	failures int
	code     string // the code reported now, "" when healthy
}

// bind stores the supervisor's report function.
func (h *pushHealth) bind(report func(string)) {
	h.mu.Lock()
	h.report = report
	h.mu.Unlock()
}

// ok records that the users.watch push registration works, clearing a degradation.
func (h *pushHealth) ok() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures = 0
	h.set("")
}

// fail records a failed attempt with code. A permanent failure degrades at
// once; a transient one after pushFailureRounds in a row, or when imminent:
// the live registration expires before the next attempt.
func (h *pushHealth) fail(code string, permanent, imminent bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures++
	if permanent || imminent || h.failures >= pushFailureRounds {
		h.set(code)
	}
}

// set reports code when it differs from the last one. The caller holds mu.
func (h *pushHealth) set(code string) {
	if h.code == code {
		return
	}
	h.code = code
	if h.report != nil {
		h.report(code)
	}
}

// permanentFailure reports a failure no retry fixes: anything but
// throttling, a server fault, or no answer.
func permanentFailure(err error) bool {
	switch model.CodeOf(err) {
	case "", model.Transient, model.RateLimited, model.Unavailable, model.DeadlineExpired, model.OutcomeUnknown:
		return false
	}
	return true
}

// expiresBefore reports a live registration that expires before at.
func expiresBefore(expires, at time.Time) bool { return !expires.IsZero() && !expires.After(at) }

// linkReport holds the supervisor's report function for the receiver link.
type linkReport struct{ f atomic.Pointer[func(bool)] }

func (l *linkReport) bind(f func(bool)) { l.f.Store(&f) }

// set tells the host whether polling works; it is advisory.
func (l *linkReport) set(connected bool) {
	if f := l.f.Load(); f != nil {
		(*f)(connected)
	}
}
