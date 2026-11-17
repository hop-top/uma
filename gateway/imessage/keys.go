package imessage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is what the gateway holds for one Idempotency-Key.
type State string

// Key states. A key with no record is one the gateway never took, or one it
// released because the backend provably sent nothing.
const (
	// StatePending: the key is recorded and the backend call is in flight.
	StatePending State = "pending"
	// StateSent: the backend accepted the message.
	StateSent State = "sent"
	// StateRejected: the backend refused the message for good.
	StateRejected State = "rejected"
	// StateUnknown: the backend call may have sent the message, and its
	// answer was lost. The gateway never sends it again.
	StateUnknown State = "unknown"
)

// DefaultRetention is how long a key is kept after its last change when the
// configuration names no retention.
const DefaultRetention = 30 * 24 * time.Hour

// keysFile is the key log's name in the store directory.
const keysFile = "keys.jsonl"

// Record is one key's entry.
type Record struct {
	Key   string `json:"k"`
	State State  `json:"s,omitempty"`
	// MessageID is the backend's id for a sent message.
	MessageID string `json:"id,omitempty"`
	// At is when the entry was written, in Unix seconds.
	At int64 `json:"t"`
}

// KeyStore remembers Idempotency-Key values in an append-only log, one JSON
// line per change, synced to disk before a write returns. The log is read
// back and compacted when the store opens: entries older than the retention
// are dropped, and a key found pending (the process stopped during the
// backend call) becomes unknown, since the call may have sent the message.
//
// A KeyStore is safe for concurrent use by one process. Two processes must
// not share a directory.
type KeyStore struct {
	mu        sync.Mutex
	f         *os.File
	recs      map[string]Record
	retention time.Duration
	now       func() time.Time
}

// OpenKeyStore opens, or creates, the key log in dir. retention zero is
// DefaultRetention; now nil is time.Now.
func OpenKeyStore(dir string, retention time.Duration, now func() time.Time) (*KeyStore, error) {
	if retention <= 0 {
		retention = DefaultRetention
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("key store: %w", err)
	}
	s := &KeyStore{recs: map[string]Record{}, retention: retention, now: now}
	path := filepath.Join(dir, keysFile)
	if err := s.load(path); err != nil {
		return nil, err
	}
	if err := s.compact(dir, path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- operator-configured directory
	if err != nil {
		return nil, fmt.Errorf("key store: %w", err)
	}
	s.f = f
	return s, nil
}

// load reads the log. A last line that does not parse is a write the
// process did not finish and is ignored; any other is an error.
func (s *KeyStore) load(path string) error {
	data, err := os.ReadFile(path) // #nosec G304 -- operator-configured directory
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 4096), 1<<20)
	var bad error
	for sc.Scan() {
		if bad != nil {
			return bad
		}
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var r Record
		if err := json.Unmarshal(line, &r); err != nil || r.Key == "" {
			bad = errors.New("key store: unreadable entry in " + keysFile)
			continue
		}
		if r.State == "" {
			delete(s.recs, r.Key)
			continue
		}
		s.recs[r.Key] = r
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	cutoff := s.now().Add(-s.retention).Unix()
	for k, r := range s.recs {
		switch {
		case r.At < cutoff:
			delete(s.recs, k)
		case r.State == StatePending:
			r.State = StateUnknown
			s.recs[k] = r
		}
	}
	return nil
}

// compact rewrites the log with the live entries: a temporary file, synced,
// renamed over the log, then the directory synced.
func (s *KeyStore) compact(dir, path string) error {
	tmp, err := os.CreateTemp(dir, keysFile+".*")
	if err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	w := bufio.NewWriter(tmp)
	for _, r := range s.recs {
		line, err := json.Marshal(r)
		if err != nil {
			_ = tmp.Close()
			return fmt.Errorf("key store: %w", err)
		}
		_, _ = w.Write(append(line, '\n'))
	}
	if err := errors.Join(w.Flush(), tmp.Sync(), tmp.Close()); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- operator-configured directory
	if err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("key store: %w", err)
	}
	return nil
}

// Reserve records key as pending, durably, unless the store holds it. It
// returns the record and whether it is new: only a new reservation may act.
func (s *KeyStore) Reserve(key string) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[key]; ok {
		return r, false, nil
	}
	r := Record{Key: key, State: StatePending, At: s.now().Unix()}
	if err := s.append(r); err != nil {
		return Record{}, false, err
	}
	s.recs[key] = r
	return r, true, nil
}

// Settle records the outcome of a reserved key.
func (s *KeyStore) Settle(key string, state State, messageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := Record{Key: key, State: state, MessageID: messageID, At: s.now().Unix()}
	if err := s.append(r); err != nil {
		return err
	}
	s.recs[key] = r
	return nil
}

// Release forgets a reserved key the backend provably sent nothing for, so
// the next callback with it acts again and a lookup answers not_found.
func (s *KeyStore) Release(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.append(Record{Key: key, At: s.now().Unix()}); err != nil {
		return err
	}
	delete(s.recs, key)
	return nil
}

// Get returns the record of key, if the store holds one.
func (s *KeyStore) Get(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[key]
	return r, ok
}

// Close closes the log.
func (s *KeyStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// append writes one entry and syncs it. Caller holds mu.
func (s *KeyStore) append(r Record) error {
	if s.f == nil {
		return errors.New("key store: closed")
	}
	line, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	if _, err := s.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("key store: %w", err)
	}
	return nil
}

var _ io.Closer = (*KeyStore)(nil)
