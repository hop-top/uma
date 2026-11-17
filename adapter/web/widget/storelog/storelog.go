// Package storelog is a widget.Log kept in a uma store.Store, so what a
// worker delivered to a visitor survives a host restart and a reconnecting
// browser resumes from its cursor on any host sharing the store.
//
// Each visitor log is a store recipient log (store.RecipientLogKey): the
// channel, the visitor's identity namespace and the visitor ID, in the
// channel's tenant. Append stores an entry and trims the log to the latest
// Retain entries in one unit of work, so sequences are 1, 2, 3 without gaps
// per visitor and a stream whose cursor falls behind the oldest kept entry
// receives a reset event, as with widget.MemoryLog. Concurrent appends to
// one visitor from several workers or processes serialize in the store;
// Append retries a unit that lost that race a few times before reporting
// it as model.Transient, which stores nothing.
//
// Privacy erasure reaches these logs: a visitor's log is addressed by the
// channel, the identity namespace and the visitor ID, which are the
// coordinates of the visitor's identity, so Privacy.Redact with Cascade on
// the visitor's contact (or identity) deletes the log in the erasure's own
// unit (store.Tx.EraseRecipientLog). The log keeps counting: a stream
// behind the erased entries receives a reset event.
//
// Watch wakes event streams in this process only. A stream served by
// another process sees an append at its next read, at the latest after the
// Factory's heartbeat interval: the stream reads the log again after every
// heartbeat.
//
// Durability is the store's: over hop.top/uma/store/postgres entries
// survive a restart; over the memory store they do not, which suits tests.
// The host runs the store's migrations as usual; the log needs no schema of
// its own.
package storelog

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"hop.top/uma/adapter/web/widget"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// appendAttempts bounds the units Append runs when concurrent appends to
// one visitor make a unit fail with model.Transient.
const appendAttempts = 5

// Entry value schema: an entry without its sequence, as JSON.
const (
	entryName    = string(widget.Type) + ".entry"
	entryVersion = 1
)

// Options configures a Log.
type Options struct {
	// Retain is the number of entries kept per visitor; zero or less keeps
	// widget.DefaultRetain.
	Retain int
}

// Log is a widget.Log in a store.Store. It is safe for concurrent use.
type Log struct {
	st     store.Store
	retain int

	mu       sync.Mutex
	watchers map[widget.LogKey]map[chan struct{}]struct{}
}

// New returns a Log keeping visitor logs in st.
func New(st store.Store, o Options) (*Log, error) {
	if st == nil {
		return nil, model.NewError(model.Validation, "storelog: store: required")
	}
	retain := o.Retain
	if retain <= 0 {
		retain = widget.DefaultRetain
	}
	return &Log{st: st, retain: retain, watchers: map[widget.LogKey]map[chan struct{}]struct{}{}}, nil
}

// storedEntry is an entry without its sequence, as the store value holds
// it.
type storedEntry struct {
	Event       widget.Event         `json:"event"`
	Session     string               `json:"session,omitempty"`
	Operation   model.OperationRef   `json:"operation"`
	ExpiresAt   *time.Time           `json:"expires_at,omitempty"`
	Interaction model.InteractionRef `json:"interaction,omitzero"`
}

func recipientKey(key widget.LogKey) store.RecipientLogKey {
	return store.RecipientLogKey{Channel: key.Channel, Namespace: key.Namespace, Recipient: key.Visitor}
}

// Append stores e under the next sequence of key and wakes the key's
// watchers in this process. Store errors keep their meaning: a
// model.Transient, Unavailable or RateLimited error stored nothing,
// model.OutcomeUnknown may have stored the entry, and any other
// *model.Error refuses it.
func (l *Log) Append(ctx context.Context, key widget.LogKey, e widget.Entry) (uint64, error) {
	s := storedEntry{Event: e.Event, Session: e.Session, Operation: e.Operation, Interaction: e.Interaction}
	if !e.ExpiresAt.IsZero() {
		s.ExpiresAt = &e.ExpiresAt
	}
	data, err := json.Marshal(s)
	if err != nil {
		return 0, model.NewError(model.Permanent, "storelog: entry does not encode")
	}
	value := model.Extension{Name: entryName, Version: entryVersion, Data: data}
	rk := recipientKey(key)
	var seq uint64
	for attempt := 1; ; attempt++ {
		err = l.st.Transact(ctx, key.Channel.Tenant, func(tx store.Tx) error {
			var err error
			seq, err = tx.AppendRecipientLog(ctx, rk, value, l.retain)
			return err
		})
		if model.CodeOf(err) != model.Transient || attempt == appendAttempts || ctx.Err() != nil {
			break
		}
	}
	if err != nil {
		return 0, err
	}
	l.wake(key)
	return seq, nil
}

// Read returns the retained entries of key after the cursor, oldest first,
// at most limit; zero or less reads up to Retain entries.
func (l *Log) Read(ctx context.Context, key widget.LogKey, after uint64, limit int) ([]widget.Entry, error) {
	if limit <= 0 {
		limit = l.retain
	}
	var rows []store.RecipientLogEntry
	err := l.st.View(ctx, key.Channel.Tenant, func(r store.Reader) error {
		var err error
		rows, err = r.RecipientLogEntries(ctx, recipientKey(key), after, limit)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]widget.Entry, 0, len(rows))
	for _, row := range rows {
		if row.Value.Name != entryName || row.Value.Version != entryVersion {
			return nil, model.NewError(model.Permanent, "storelog: entry has an unknown schema")
		}
		var s storedEntry
		if err := json.Unmarshal(row.Value.Data, &s); err != nil {
			return nil, model.NewError(model.Permanent, "storelog: entry does not decode")
		}
		e := widget.Entry{Seq: row.Seq, Event: s.Event, Session: s.Session, Operation: s.Operation, Interaction: s.Interaction}
		if s.ExpiresAt != nil {
			e.ExpiresAt = *s.ExpiresAt
		}
		out = append(out, e)
	}
	return out, nil
}

// Watch registers a watcher of key in this process.
func (l *Log) Watch(key widget.LogKey) (<-chan struct{}, func()) {
	w := make(chan struct{}, 1)
	l.mu.Lock()
	if l.watchers[key] == nil {
		l.watchers[key] = map[chan struct{}]struct{}{}
	}
	l.watchers[key][w] = struct{}{}
	l.mu.Unlock()
	var once sync.Once
	return w, func() {
		once.Do(func() {
			l.mu.Lock()
			delete(l.watchers[key], w)
			if len(l.watchers[key]) == 0 {
				delete(l.watchers, key)
			}
			l.mu.Unlock()
		})
	}
}

func (l *Log) wake(key widget.LogKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for w := range l.watchers[key] {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

var _ widget.Log = (*Log)(nil)
