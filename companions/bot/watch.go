package bot

import (
	"context"
	"sync"

	"hop.top/uma/model"
)

// Watch is one bot's interest in one conversation. The conversation ref
// carries tenant and inbox.
type Watch struct {
	Bot          model.ActorID
	Conversation model.ConversationRef
}

// Watches stores watch state. It belongs to the host, not to uma: it is
// never committed with uma's mutations, so implementations make Add and
// Remove idempotent, and a crash between a watch change and the batch
// acknowledgment repeats the change on redelivery. Implementations are
// safe for concurrent use.
type Watches interface {
	Add(context.Context, Watch) error
	Remove(context.Context, Watch) error
	Has(context.Context, Watch) (bool, error)
}

// MemoryWatches is an in-memory Watches for tests and single-process hosts;
// its state ends with the process.
type MemoryWatches struct {
	mu sync.Mutex
	m  map[Watch]struct{}
}

// NewMemoryWatches returns an empty MemoryWatches.
func NewMemoryWatches() *MemoryWatches { return &MemoryWatches{m: map[Watch]struct{}{}} }

func (w *MemoryWatches) Add(ctx context.Context, k Watch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.m[k] = struct{}{}
	return nil
}

func (w *MemoryWatches) Remove(ctx context.Context, k Watch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.m, k)
	return nil
}

func (w *MemoryWatches) Has(ctx context.Context, k Watch) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.m[k]
	return ok, nil
}

var _ Watches = (*MemoryWatches)(nil)
