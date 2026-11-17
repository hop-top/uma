package google

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"hop.top/uma/model"
)

// Receiver state lives in the engine's protocol store when the engine
// supplies one (Dependencies.Protocol), under protocolKey, so a history
// round resumes where the last receiver stopped, in any process. Without
// one it is kept in the factory for the life of the process; a fresh
// process then starts with a bounded full synchronization and relies on
// ingress deduplication for messages it sees again.
const (
	protocolKey  = "receiver"
	stateSchema  = string(Type) + ".receiver_state"
	stateVersion = 1
)

// receiverState is what a receiver persists between rounds.
type receiverState struct {
	// HistoryID is the mailbox history record every change up to which has
	// been handed to the sink; history.list resumes after it.
	HistoryID int64s `json:"history_id,omitempty"`
	// WatchExpires and WatchedAt describe the live users.watch, if any.
	WatchExpires time.Time `json:"watch_expires,omitzero"`
	WatchedAt    time.Time `json:"watched_at,omitzero"`
	// WatchRenewAt is when that watch is renewed: the renewal interval
	// after WatchedAt, spread by +-10% when the watch was made so channels
	// do not renew in phase. Zero (a state saved before it existed) means
	// WatchedAt plus the plain interval.
	WatchRenewAt time.Time `json:"watch_renew_at,omitzero"`
	// FullResyncs counts full synchronizations forced by a historyId
	// Gmail no longer holds (history.list answered 404); LastFullResync is
	// when the latest one ran. Messages changed in the gap and outside the
	// initial sync window are not received.
	FullResyncs    int       `json:"full_resyncs,omitempty"`
	LastFullResync time.Time `json:"last_full_resync,omitzero"`
}

// channelState is the process-local coordination of one channel: wake-ups
// from the push handler to the receiver and the fallback receiver state.
type channelState struct {
	wake chan struct{}

	mu  sync.Mutex
	mem receiverState
}

// notify wakes the receiver.
func (s *channelState) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// loadState returns the receiver state.
func (i *instance) loadState(ctx context.Context) (receiverState, error) {
	if i.deps.Protocol == nil {
		i.state.mu.Lock()
		defer i.state.mu.Unlock()
		return i.state.mem, nil
	}
	ext, _, err := i.deps.Protocol.Load(ctx, protocolKey)
	switch {
	case model.IsCode(err, model.NotFound):
		return receiverState{}, nil
	case err != nil:
		return receiverState{}, err
	}
	var st receiverState
	if len(ext.Data) > 0 {
		if err := json.Unmarshal(ext.Data, &st); err != nil {
			return receiverState{}, nil // unreadable state restarts with a full sync
		}
	}
	return st, nil
}

// saveState persists st. The engine's receiver lease makes this receiver
// the only writer; a compare-and-swap conflict is reported as such.
func (i *instance) saveState(ctx context.Context, st receiverState) error {
	if i.deps.Protocol == nil {
		i.state.mu.Lock()
		i.state.mem = st
		i.state.mu.Unlock()
		return nil
	}
	_, rev, err := i.deps.Protocol.Load(ctx, protocolKey)
	if err != nil && !model.IsCode(err, model.NotFound) {
		return err
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return i.deps.Protocol.CompareAndSwap(ctx, protocolKey, rev, model.Extension{Name: stateSchema, Version: stateVersion, Data: data})
}
