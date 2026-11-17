package main

import "sync"

// decisions remembers what a handler chose before it is sent. A model call
// or a clock makes a rerun choose differently, and a stable operation ID
// only protects a replay that carries the same payload: persist the choice
// under the turn's operation ID, then reuse it on every rerun. This one is
// in memory like the example's store; a durable host keeps it beside its
// uma store, written before the outbound operation is submitted.
type decisions struct {
	mu sync.Mutex
	m  map[string]string
}

func newDecisions() *decisions { return &decisions{m: map[string]string{}} }

// choose returns the recorded text for key, generating and recording it
// first when there is none.
func (d *decisions) choose(key string, generate func() string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.m[key]; ok {
		return s
	}
	s := generate()
	d.m[key] = s
	return s
}
