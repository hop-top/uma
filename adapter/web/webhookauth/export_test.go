package webhookauth

// SpyEqual replaces the constant-time comparison with one that reports
// both operands to spy, and returns the function that restores it.
func SpyEqual(spy func(a, b []byte)) (restore func()) {
	orig := equalFixed
	equalFixed = func(a, b []byte) int {
		spy(a, b)
		return orig(a, b)
	}
	return func() { equalFixed = orig }
}

// QueueLen is the size of the store's expiry queue, live and stale items.
func QueueLen(s *MemoryReplayStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}
