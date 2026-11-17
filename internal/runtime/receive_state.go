package runtime

import (
	"sync"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/model"
)

// ReceiverPhase is what a Receive call is doing, as ReceiverStatus reports
// it for the call this process runs.
type ReceiverPhase string

// Receiver phases.
const (
	// ReceiverRunning: a connection is being opened or is open. It is not
	// proof the provider link is up; see ReceiverLink.
	ReceiverRunning ReceiverPhase = "running"
	// ReceiverRetrying: the last connection failed and the call waits to
	// open the next one.
	ReceiverRetrying ReceiverPhase = "retrying"
	// ReceiverEnded: the call stopped and is releasing its lease.
	ReceiverEnded ReceiverPhase = "ended"
)

// ReceiverLink is what the receive adapter reports of its provider link
// while the phase is running.
type ReceiverLink string

// Receiver links.
const (
	// ReceiverConnecting: the adapter reports it is not connected yet, or
	// is re-establishing its link.
	ReceiverConnecting ReceiverLink = "connecting"
	// ReceiverConnected: the adapter reports its provider link is up.
	ReceiverConnected ReceiverLink = "connected"
	// ReceiverDegraded: the adapter reports it still receives but part of
	// what it relies on is failing (ReceiverStatus.Detail has the code).
	// Reports of connecting and connected resume when it recovers.
	ReceiverDegraded ReceiverLink = "degraded"
)

// receiverState is the state of one Receive call this process runs. It is
// memory only and advisory: the supervisor writes it and nothing reads it
// back.
type receiverState struct {
	owner string

	mu   sync.Mutex
	st   ReceiverStatus // Owner and Until are the lease's, not kept here
	conn *receiverConn  // the connection whose reports count, nil between
	// base is the connecting or connected link the adapter last reported;
	// detail, when set, is the degradation code that shows the link as
	// ReceiverDegraded over it.
	base   ReceiverLink
	detail string
}

// receiverConn is one connection's report channel; it stops counting when
// the connection ends.
type receiverConn struct{ s *receiverState }

func newReceiverState(owner string, now time.Time) *receiverState {
	return &receiverState{owner: owner, st: ReceiverStatus{Phase: ReceiverRunning, Since: now}}
}

// snapshot is the state as ReceiverStatus reports it.
func (s *receiverState) snapshot() ReceiverStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

// running starts a connection attempt.
func (s *receiverState) running(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Phase != ReceiverRunning {
		s.st.Phase, s.st.Since = ReceiverRunning, now
	}
	s.st.RetryAt = time.Time{}
}

// bind hands the instance its report functions, when it has them, and
// starts the link at connecting. A ReportReceiver or
// ReportReceiverDegraded that panics is treated as absent: the signal is
// advisory and must not break the call.
func (s *receiverState) bind(inst adapter.Instance) {
	rep, hasLink := inst.(adapter.ReceiverReporter)
	deg, hasDeg := inst.(adapter.ReceiverDegradationReporter)
	if !hasLink && !hasDeg {
		return
	}
	c := &receiverConn{s: s}
	s.mu.Lock()
	s.conn = c
	s.base, s.detail = ReceiverConnecting, ""
	s.st.Link, s.st.Detail = ReceiverConnecting, ""
	s.mu.Unlock()
	defer func() {
		if recover() != nil {
			s.unbind()
		}
	}()
	if hasLink {
		rep.ReportReceiver(c.report)
	}
	if hasDeg {
		deg.ReportReceiverDegraded(c.degrade)
	}
}

// refresh sets the status link from the reported base and degradation.
// The caller holds s.mu.
func (s *receiverState) refresh() {
	s.st.Link, s.st.Detail = s.base, ""
	if s.detail != "" {
		s.st.Link, s.st.Detail = ReceiverDegraded, s.detail
	}
}

// degrade is the function the adapter calls to report a degradation code,
// or "" for recovery.
func (c *receiverConn) degrade(code string) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.conn != c {
		return
	}
	c.s.detail = code
	c.s.refresh()
}

// report is the function the adapter calls.
func (c *receiverConn) report(connected bool) {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.conn != c {
		return
	}
	c.s.base = ReceiverConnecting
	if connected {
		c.s.base = ReceiverConnected
	}
	c.s.refresh()
}

// unbind ends the connection's reports and clears the link.
func (s *receiverState) unbind() {
	s.mu.Lock()
	s.conn, s.st.Link, s.st.Detail, s.base, s.detail = nil, "", "", "", ""
	s.mu.Unlock()
}

// accepted records the connection's first accepted ingress: the failures
// before it no longer count.
func (s *receiverState) accepted() {
	s.mu.Lock()
	s.st.Failures, s.st.Reason = 0, ""
	s.mu.Unlock()
}

// retrying records a failure and the wait before the next connection.
func (s *receiverState) retrying(failures int, err error, now time.Time, wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Phase, s.st.Since = ReceiverRetrying, now
	s.st.Failures, s.st.Reason = failures, codeOf(err)
	s.st.RetryAt = now.Add(wait)
}

// ended records the end of the call with err.
func (s *receiverState) ended(failures int, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn, s.st.Link, s.st.Detail, s.base, s.detail = nil, "", "", "", ""
	s.st.Phase, s.st.Since = ReceiverEnded, now
	s.st.Failures, s.st.RetryAt = failures, time.Time{}
	if c := codeOf(err); c != "" {
		s.st.Reason = c
	}
}

// codeOf is the code of a typed failure, "" for anything else.
func codeOf(err error) model.ErrorCode {
	if me, ok := err.(*model.Error); ok {
		return me.Code
	}
	return ""
}

// receiverStates holds the state of the Receive calls this runtime runs,
// by channel.
type receiverStates struct {
	mu sync.Mutex
	by map[model.ChannelRef]*receiverState
}

func (r *receiverStates) add(ref model.ChannelRef, s *receiverState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by == nil {
		r.by = map[model.ChannelRef]*receiverState{}
	}
	r.by[ref] = s
}

// remove drops s, unless a later call of the channel replaced it.
func (r *receiverStates) remove(ref model.ChannelRef, s *receiverState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by[ref] == s {
		delete(r.by, ref)
	}
}

// ownedBy returns the state of the call of ref holding the lease of owner.
func (r *receiverStates) ownedBy(ref model.ChannelRef, owner string) (*receiverState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.by[ref]
	return s, s != nil && s.owner == owner
}

// get returns the state of the call of ref this runtime runs.
func (r *receiverStates) get(ref model.ChannelRef) (*receiverState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.by[ref]
	return s, s != nil
}
