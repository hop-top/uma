package microsoft

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"hop.top/uma/model"
)

// Receiver state lives in the engine's protocol store when the engine
// supplies one (Dependencies.Protocol), under protocolKey, so a delta round
// resumes where the last receiver stopped, in any process. Without one it
// is kept in the factory for the life of the process; a fresh process then
// starts with a bounded initial sync (Config.InitialSyncWindowMS) and relies
// on ingress deduplication for messages it sees again.
const (
	protocolKey  = "receiver"
	stateSchema  = string(Type) + ".receiver_state"
	stateVersion = 1
)

// Lifecycle work the webhook hands to the receiver.
const (
	lifecycleRenew    = 1 << iota // reauthorize: renew the subscription now
	lifecycleRecreate             // the subscription was removed: create a new one
)

// receiverState is what a receiver persists between rounds.
type receiverState struct {
	// DeltaLink is the link to resume delta tracking from: the last
	// @odata.deltaLink, or an @odata.nextLink while a round is paged.
	DeltaLink string `json:"delta_link,omitempty"`
	// SubscriptionID and SubscriptionExpires describe the live change
	// notification subscription, if any.
	SubscriptionID      string    `json:"subscription_id,omitempty"`
	SubscriptionExpires time.Time `json:"subscription_expires,omitzero"`
	// SubscriptionRenewAt is when that subscription is renewed: its expiry
	// less the renewal margin spread by +-10% when it was made or renewed,
	// so channels do not renew in phase. Zero (a state saved before it
	// existed) means expiry less the plain margin.
	SubscriptionRenewAt time.Time `json:"subscription_renew_at,omitzero"`
	// Folders are the folders watched for provider state, by watch name
	// (a well-known name or a configured ID), each with its own delta link.
	Folders map[string]*folderState `json:"folders,omitempty"`
}

// channelState is the process-local coordination of one channel: wake-ups
// and lifecycle requests from the webhook to the receiver, and the
// fallback receiver state.
type channelState struct {
	wake chan struct{}

	mu        sync.Mutex
	lifecycle int
	mem       receiverState
}

// notify records lifecycle work (renew, recreate; zero for none) and wakes
// the receiver.
func (s *channelState) notify(lifecycle int) {
	s.mu.Lock()
	s.lifecycle |= lifecycle
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// takeLifecycle returns and clears pending lifecycle work.
func (s *channelState) takeLifecycle() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.lifecycle
	s.lifecycle = 0
	return l
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
			return receiverState{}, nil // unreadable state restarts tracking
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
