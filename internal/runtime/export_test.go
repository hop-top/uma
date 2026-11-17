package runtime

// TimerWaiter is the default Waiter, for tests in package runtime_test.
type TimerWaiter = timerWaiter

// ReconcileRequestOf is reconcileRequest, for tests in package runtime_test.
var ReconcileRequestOf = reconcileRequest

// Receiver lease timing, for tests in package runtime_test.
const (
	ReceiverLease    = receiverLease
	ReceiverRenew    = receiverRenew
	ReceiverRenewMax = receiverRenewMax
)

// RenewWait is renewWait, for tests in package runtime_test.
var RenewWait = renewWait

// StartOffset is startOffset, for tests in package runtime_test.
var StartOffset = startOffset
