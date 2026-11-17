package engine

import (
	"context"
	"sync"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Commit signals: the engine sees every unit it runs, so it can tell
// waiters in this process when one that appended events has committed,
// without the store's help and on any backend. Units of other processes
// sharing the store go unseen; their waiters find those events by
// rescanning.

// eventCommits hands out, per tenant, a channel closed by the next commit
// of a unit that appended events to the tenant's log. The zero value is
// ready.
type eventCommits struct {
	mu   sync.Mutex
	next map[model.TenantID]chan struct{}
}

func (c *eventCommits) wait(tenant model.TenantID) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.next[tenant]
	if !ok {
		if c.next == nil {
			c.next = map[model.TenantID]chan struct{}{}
		}
		ch = make(chan struct{})
		c.next[tenant] = ch
	}
	return ch
}

func (c *eventCommits) signal(tenant model.TenantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.next[tenant]; ok {
		close(ch)
		delete(c.next, tenant)
	}
}

// signalingStore is the host's store as the engine runs it: a unit that
// appended events signals its commit. A unit whose commit outcome is
// unknown signals too; a waiter woken for nothing rescans and waits again.
type signalingStore struct {
	store.Store
	commits *eventCommits
}

func (s signalingStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	appended := false
	err := s.Store.Transact(ctx, tenant, func(tx store.Tx) error {
		appended = false
		return fn(appendSpy{Tx: tx, appended: &appended})
	})
	if appended && (err == nil || model.IsCode(err, model.OutcomeUnknown)) {
		s.commits.signal(tenant)
	}
	return err
}

// appendSpy notes a successful append within one unit.
type appendSpy struct {
	store.Tx
	appended *bool
}

func (t appendSpy) AppendEvents(ctx context.Context, events []model.Event) error {
	err := t.Tx.AppendEvents(ctx, events)
	if err == nil && len(events) > 0 {
		*t.appended = true
	}
	return err
}

// EventCommits returns a channel closed when a unit of this engine that
// appended events to tenant's log next commits. Take it before reading the
// log, so a commit between the read and the wait is not missed.
func (e *Engine) EventCommits(tenant model.TenantID) <-chan struct{} {
	return e.commits.wait(tenant)
}
