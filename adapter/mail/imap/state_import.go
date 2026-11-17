package imap

import (
	"context"
	"encoding/json"
	"time"

	"hop.top/uma/model"
)

// Imported mail. A history read hands the sink the same envelopes the
// receiver delivers and follows nothing itself: the ledger (state_sync.go)
// has one writer, the receiver. The reader instead records what it imported
// in a second ledger, the import ledger, under its own protocol key (or in
// the factory without a store), and the receiver adopts it at the start of
// each round: every message there is followed from then on as if the
// receiver had delivered it, by the same Message-ID, mailbox and UID, so a
// flag, move or deletion is the same conversation.state report.
//
// Both sides write the import ledger, the reader to add and the receiver to
// release what it adopted, each as a compare-and-swap loop over a fresh read,
// so neither loses the other's write. The receiver never overwrites an
// entry it already follows: an imported and a received copy of one message
// (the same UIDVALIDITY and UID) are one entry.
//
// Bounds: the import ledger and the ledger each hold at most trackedLimit
// messages per mailbox, the highest UIDs (the newest mail), so importing
// years of mail follows the newest trackedLimit of it and the state sync
// costs what it costs for received mail: one UID SEARCH of at most
// trackedLimit UIDs per round, flags fetched incrementally (CONDSTORE) or
// at most every Config.StateSyncIntervalMS. Adopting new messages makes the
// next comparison of their mailbox a full one (once, one FETCH of flags of
// at most trackedLimit UIDs), because the flags the reader saw may predate
// the mod-sequence the ledger holds.
const (
	importKey     = "mailstate.import"
	importSchema  = string(Type) + ".mailbox_import"
	importRetries = 8
)

func decodeLedger(raw []byte) *ledger {
	l := &ledger{}
	if len(raw) == 0 || json.Unmarshal(raw, l) != nil {
		return &ledger{}
	}
	return l
}

// loadImports returns the import ledger, empty when there is none.
func (i *Instance) loadImports(ctx context.Context) (*ledger, error) {
	if i.deps.Protocol == nil {
		i.mem.imp.mu.Lock()
		defer i.mem.imp.mu.Unlock()
		return decodeLedger(i.mem.imp.raw), nil
	}
	ext, _, err := i.deps.Protocol.Load(ctx, importKey)
	switch {
	case model.IsCode(err, model.NotFound):
		return &ledger{}, nil
	case err != nil:
		return nil, err
	case ext.Name != importSchema:
		return &ledger{}, nil
	}
	return decodeLedger(ext.Data), nil
}

// updateImports applies fn to the import ledger and stores it when fn marked
// it dirty, retrying over a fresh read when another writer got there first.
func (i *Instance) updateImports(ctx context.Context, fn func(*ledger)) error {
	if i.deps.Protocol == nil {
		i.mem.imp.mu.Lock()
		defer i.mem.imp.mu.Unlock()
		l := decodeLedger(i.mem.imp.raw)
		fn(l)
		if !l.dirty {
			return nil
		}
		data, err := json.Marshal(l)
		if err != nil {
			return err
		}
		i.mem.imp.raw = data
		return nil
	}
	for range importRetries {
		var raw []byte
		ext, rev, err := i.deps.Protocol.Load(ctx, importKey)
		switch {
		case model.IsCode(err, model.NotFound):
		case err != nil:
			return err
		case ext.Name == importSchema:
			raw = ext.Data
		}
		l := decodeLedger(raw)
		fn(l)
		if !l.dirty {
			return nil
		}
		data, err := json.Marshal(l)
		if err != nil {
			return err
		}
		err = i.deps.Protocol.CompareAndSwap(ctx, importKey, rev, model.Extension{Name: importSchema, Version: ledgerVersion, Data: data})
		if !model.IsCode(err, model.Conflict) {
			return err
		}
	}
	return model.NewError(model.Conflict, PurposeIMAP+": import ledger is contended")
}

// followImported records messages a history read imported from mailbox under
// uidValidity, for the receiver to adopt.
func (i *Instance) followImported(ctx context.Context, mailbox string, uidValidity uint32, msgs map[uint32]entry) error {
	if len(msgs) == 0 {
		return nil
	}
	return i.updateImports(ctx, func(l *ledger) {
		f := l.box(mailbox, uidValidity)
		for u, e := range msgs {
			f.set(u, e)
		}
		l.dirty = true
	})
}

// adoptImported moves the messages the import ledger holds into led, the
// receiver's, and releases them from the import ledger once led is stored.
// Where led already follows the mailbox under another UIDVALIDITY the
// imported messages are dropped (the sync voids one of the two anyway).
func (i *Instance) adoptImported(ctx context.Context, led *ledger) error {
	imp, err := i.loadImports(ctx)
	if err != nil || len(imp.Mailboxes) == 0 {
		return err
	}
	for box, p := range imp.Mailboxes {
		if f := led.Mailboxes[box]; f != nil && len(f.Messages) > 0 && f.UIDValidity != p.UIDValidity {
			continue
		}
		f := led.box(box, p.UIDValidity)
		adopted := false
		for _, u := range p.uids() {
			if _, ok := f.Messages[u]; !ok {
				f.set(u, p.Messages[u])
				adopted = true
			}
		}
		if adopted {
			// Compare every flag once: see the bounds above.
			f.ModSeq, f.SyncedAt = 0, time.Time{}
			led.dirty = true
		}
	}
	if err := i.saveLedger(ctx, led); err != nil {
		return err
	}
	return i.updateImports(ctx, func(l *ledger) {
		for box, p := range imp.Mailboxes {
			f := l.Mailboxes[box]
			if f == nil || f.UIDValidity != p.UIDValidity {
				continue
			}
			for u, e := range p.Messages {
				if cur, ok := f.Messages[u]; ok && cur == e {
					delete(f.Messages, u)
					l.dirty = true
				}
			}
			if len(f.Messages) == 0 {
				delete(l.Mailboxes, box)
			}
		}
	})
}
