package model

// IsDeliveryState reports whether s is a state an operation can hold.
func IsDeliveryState(s DeliveryState) bool {
	switch s {
	case Queued, RetryPending, ProviderAccepted, Delivered, Failed, Unknown, Rejected, Committed, DeadLettered, Held, Expired:
		return true
	}
	return false
}
