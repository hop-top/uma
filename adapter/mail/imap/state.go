package imap

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"hop.top/uma/model"
)

// Receiver state lives in the engine's protocol store when the engine
// supplies one (Dependencies.Protocol), under protocolKey, so a receiver
// resumes where the last one stopped, in any process. Without one it is kept
// in the factory for the life of the process.
const (
	protocolKey  = "receiver"
	stateSchema  = string(Type) + ".receiver_state"
	stateVersion = 1
)

// cursor is the receiver position in the mailbox.
type cursor struct {
	// Mailbox and UIDValidity identify the UID space LastUID belongs to. A
	// different UIDVALIDITY voids every stored UID.
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uid_validity"`
	// LastUID is the highest UID handed to the sink and accepted, or skipped
	// as gone from the mailbox or undeliverable (both reported); messages
	// with greater UIDs are new.
	LastUID uint32 `json:"last_uid"`
	// LastDate is the INTERNALDATE of the last accepted message.
	LastDate time.Time `json:"last_date,omitzero"`
	// Since, while set, makes the next pass a catch-up: only messages with
	// an INTERNALDATE at or after it are delivered. It bounds the first
	// synchronization (the initial sync window) and the resynchronization
	// after a UIDVALIDITY change, and is cleared once that pass completes.
	Since time.Time `json:"since,omitzero"`
	// Gaps counts the UIDVALIDITY changes seen; GapAt is the latest.
	Gaps  int       `json:"gaps,omitempty"`
	GapAt time.Time `json:"gap_at,omitzero"`
}

// memState is the factory-local cursor of one channel.
type memState struct {
	mu  sync.Mutex
	cur cursor
	// led is what the receiver follows (see state_sync.go).
	led memLedger
	// imp is the import ledger (see state_import.go).
	imp memLedger
}

// loadCursor returns the stored cursor; the zero cursor when there is none
// or it is unreadable (the receiver then starts a fresh synchronization).
func (i *Instance) loadCursor(ctx context.Context) (cursor, error) {
	if i.deps.Protocol == nil {
		i.mem.mu.Lock()
		defer i.mem.mu.Unlock()
		return i.mem.cur, nil
	}
	ext, _, err := i.deps.Protocol.Load(ctx, protocolKey)
	switch {
	case model.IsCode(err, model.NotFound):
		return cursor{}, nil
	case err != nil:
		return cursor{}, err
	}
	var c cursor
	if ext.Name != stateSchema || len(ext.Data) == 0 || json.Unmarshal(ext.Data, &c) != nil {
		return cursor{}, nil
	}
	return c, nil
}

// saveCursor persists c. The engine's receiver lease makes this receiver the
// only writer; a compare-and-swap conflict is reported as such.
func (i *Instance) saveCursor(ctx context.Context, c cursor) error {
	if i.deps.Protocol == nil {
		i.mem.mu.Lock()
		i.mem.cur = c
		i.mem.mu.Unlock()
		return nil
	}
	_, rev, err := i.deps.Protocol.Load(ctx, protocolKey)
	if err != nil && !model.IsCode(err, model.NotFound) {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return i.deps.Protocol.CompareAndSwap(ctx, protocolKey, rev, model.Extension{Name: stateSchema, Version: stateVersion, Data: data})
}
