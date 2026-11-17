package operations

import (
	"time"

	"hop.top/uma/store"
)

// Internals that tests in package operations_test exercise directly.
var (
	ReplyFingerprint = replyFingerprint
	ReplyAudience    = replyAudience
)

// DefaultReplyParent is the default-parent selection of a reply that names
// no InReplyTo.
var DefaultReplyParent = defaultReplyParent

// Unit rerun bounds.
const (
	MaxUnitAttempts = maxUnitAttempts
	UnitPauseBase   = unitPauseBase
	UnitPauseMax    = unitPauseMax
)

// SetUnitJitter replaces the jitter of pauses between unit reruns until
// the returned restore runs. Tests using it must not run in parallel.
func SetUnitJitter(fn func(max time.Duration) time.Duration) (restore func()) {
	prev := unitJitter
	unitJitter = fn
	return func() { unitJitter = prev }
}

// ResolveOutcome is the Outcome of evidence appended in order.
func ResolveOutcome(evs ...store.AttemptEvidence) Outcome {
	r := newResolver()
	for _, ev := range evs {
		r.add(ev)
	}
	return r.outcome()
}

// UnregisterApplier removes the Applier of an intent name.
func UnregisterApplier(intent string) { appliers.Delete(intent) }

// HeldRefs and ExpireRefs split ExpireHeld's scan from its units, so a test
// can sweep from a scan another sweep has already outdated.
var (
	HeldRefs   = heldRefs
	ExpireRefs = expireRefs
)

// UnregisterValidator removes the Validator of an intent name.
func UnregisterValidator(intent string) { validators.Delete(intent) }

// Trust storage format and bounds.
var TrustBucketOf = trustBucketOf

const (
	TrustBucketCapacity = trustBucketCapacity
	TrustBatchMemory    = trustBatchMemory
)

// TrustBucketKey is the protocol key of bucket n.
func TrustBucketKey(n int) string { return trustBucketKey(n) }
