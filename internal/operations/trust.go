package operations

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Trust counters (see docs/decisions/held-operations.md, section 5).
//
// Counters live in 64 channel-scoped protocol records, the buckets
// "uma.trust.00" to "uma.trust.3f" of extension "uma.trust" version 1. A
// trust key lives in the bucket named by the first byte of its SHA-256
// modulo 64; bucket count and hash are a persisted format. A change is a
// read-modify-write of the buckets it touches in one store unit, compare
// and set on each bucket's revision: a unit that loses a race is rerun on
// the winner's state, so concurrent writers never lose a count and a reset
// never resurrects one. A decision's counter move is written in the unit of
// the decision itself.

const (
	trustBucketCount = 64
	trustExtension   = "uma.trust"
	trustVersion     = 1
	// trustBatchMemory is how many batch IDs an entry remembers to answer a
	// replay of a batch.
	trustBatchMemory = 8
	// trustBucketCapacity is the most entries one bucket holds. A write that
	// would exceed it is model.Validation; no entry is ever dropped.
	trustBucketCapacity = 256
	// trustAttempts bounds the reruns of a unit that keeps losing a bucket.
	trustAttempts = 16
)

type trustRecord struct {
	Labels           map[string]string `json:"labels,omitempty"`
	Clean            int64             `json:"clean"`
	Approvals        int64             `json:"approvals"`
	Corrections      int64             `json:"corrections"`
	LastCorrectionAt *time.Time        `json:"last_correction_at,omitempty"`
	UpdatedAt        time.Time         `json:"updated_at"`
	Batches          []string          `json:"batches,omitempty"`
}

type trustBucket struct {
	Seq     uint64                 `json:"seq"`
	Entries map[string]trustRecord `json:"entries"`
}

func trustBucketOf(key string) int {
	sum := sha256.Sum256([]byte(key))
	return int(sum[0]) % trustBucketCount
}

func trustBucketKey(n int) string { return fmt.Sprintf("uma.trust.%02x", n) }

func trustRevision(seq uint64) model.Revision { return model.Revision(fmt.Sprintf("r%d", seq)) }

func (r trustRecord) entry(key string) model.TrustEntry {
	e := model.TrustEntry{
		Key: key, Labels: maps.Clone(r.Labels), Clean: r.Clean, Approvals: r.Approvals,
		Corrections: r.Corrections, UpdatedAt: r.UpdatedAt,
	}
	if r.LastCorrectionAt != nil {
		t := *r.LastCorrectionAt
		e.LastCorrectionAt = &t
	}
	return e
}

func loadTrustBucket(ctx context.Context, r store.Reader, ch model.ChannelRef, n int) (trustBucket, model.Revision, error) {
	st, err := r.Protocol(ctx, ch, trustBucketKey(n))
	if model.IsCode(err, model.NotFound) {
		return trustBucket{Entries: map[string]trustRecord{}}, "", nil
	}
	if err != nil {
		return trustBucket{}, "", err
	}
	var b trustBucket
	if st.Value.Name != trustExtension || st.Value.Version != trustVersion {
		return b, "", &model.Error{Code: model.SchemaMismatch, SafeMessage: "trust state is not recognized", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(st.Value.Data, &b); err != nil {
		return b, "", &model.Error{Code: model.Permanent, SafeMessage: "trust state is unreadable", Retry: model.NoRetry}
	}
	if b.Entries == nil {
		b.Entries = map[string]trustRecord{}
	}
	return b, st.Revision, nil
}

// trustWrite applies mutate to every key, in as few bucket writes as there
// are buckets, inside tx. mutate reports whether it changed the record; a
// bucket none changed is not written. An error aborts the unit, so a set is
// all or none.
func trustWrite(ctx context.Context, tx store.Tx, ch model.ChannelRef, keys []string, mutate func(key string, entries map[string]trustRecord) (bool, error)) error {
	byBucket := map[int][]string{}
	for _, k := range keys {
		n := trustBucketOf(k)
		byBucket[n] = append(byBucket[n], k)
	}
	for _, n := range slices.Sorted(maps.Keys(byBucket)) {
		b, expected, err := loadTrustBucket(ctx, tx, ch, n)
		if err != nil {
			return err
		}
		changed := false
		for _, k := range byBucket[n] {
			c, err := mutate(k, b.Entries)
			if err != nil {
				return err
			}
			changed = changed || c
		}
		if !changed {
			continue
		}
		b.Seq++
		raw, err := json.Marshal(b)
		if err != nil {
			return model.NewError(model.Validation, "trust state cannot be stored")
		}
		err = tx.PutProtocol(ctx, store.ProtocolState{
			Channel: ch, Key: trustBucketKey(n),
			Value:    model.Extension{Name: trustExtension, Version: trustVersion, Data: raw},
			Revision: trustRevision(b.Seq),
		}, expected)
		if err != nil {
			return err
		}
	}
	return nil
}

// trustRecordFor returns the record of key for update, created when
// absent; creating past the bucket capacity is model.Validation.
func trustRecordFor(entries map[string]trustRecord, key string) (trustRecord, error) {
	r, ok := entries[key]
	if !ok && len(entries) >= trustBucketCapacity {
		return r, model.NewError(model.Validation, "trust bucket is full")
	}
	return r, nil
}

func cleanApproval(now time.Time, labels map[string]string, batch string) func(string, map[string]trustRecord) (bool, error) {
	return func(key string, entries map[string]trustRecord) (bool, error) {
		r, err := trustRecordFor(entries, key)
		if err != nil {
			return false, err
		}
		if batch != "" && slices.Contains(r.Batches, batch) {
			return false, nil
		}
		r.Clean++
		r.Approvals++
		r.UpdatedAt = now
		if labels != nil {
			r.Labels = maps.Clone(labels)
		}
		if batch != "" {
			r.Batches = append(slices.Clone(r.Batches), batch)
			if n := len(r.Batches); n > trustBatchMemory {
				r.Batches = r.Batches[n-trustBatchMemory:]
			}
		}
		entries[key] = r
		return true, nil
	}
}

func correction(now time.Time, labels map[string]string) func(string, map[string]trustRecord) (bool, error) {
	return func(key string, entries map[string]trustRecord) (bool, error) {
		r, err := trustRecordFor(entries, key)
		if err != nil {
			return false, err
		}
		r.Clean = 0
		r.Corrections++
		t := now
		r.LastCorrectionAt = &t
		r.UpdatedAt = now
		if labels != nil {
			r.Labels = maps.Clone(labels)
		}
		entries[key] = r
		return true, nil
	}
}

// reset zeroes the consecutive count of an existing key and keeps its
// totals. A key never counted has nothing to reset and is not created.
func reset(now time.Time) func(string, map[string]trustRecord) (bool, error) {
	return func(key string, entries map[string]trustRecord) (bool, error) {
		r, ok := entries[key]
		if !ok || r.Clean == 0 {
			return false, nil
		}
		r.Clean = 0
		r.UpdatedAt = now
		entries[key] = r
		return true, nil
	}
}

// channelOfTarget resolves the channel whose trust counters a decision of
// an operation on target moves: the channel of the target's inbox. A target
// that belongs to no inbox has none and its decisions count nowhere.
func channelOfTarget(ctx context.Context, r store.Reader, target model.Ref) (model.ChannelRef, bool, error) {
	var inbox model.InboxRef
	switch {
	case target.Kind == model.KindChannel:
		return model.ChannelRef{Tenant: target.Tenant, ID: target.ID}, true, nil
	case target.Kind == model.KindInbox:
		inbox = model.InboxRef{Tenant: target.Tenant, ID: target.ID}
	case target.Inbox != "":
		inbox = model.InboxRef{Tenant: target.Tenant, ID: target.Inbox}
	default:
		return model.ChannelRef{}, false, nil
	}
	in, err := r.GetInbox(ctx, inbox)
	if model.IsCode(err, model.NotFound) {
		return model.ChannelRef{}, false, nil
	}
	if err != nil {
		return model.ChannelRef{}, false, err
	}
	return in.Channel, true, nil
}

// recordDecision writes a decision's trust counters. It runs inside the
// unit of the decision, after the operation row is written and before the
// unit commits, so a decision and its counter move commit together or not
// at all. It never runs for a replay.
//
//   - approve as proposed: one clean approval of the operation's trust key,
//     the operation being its own batch unless the decision names one;
//   - reject: a correction of the operation's trust key;
//   - edit: a correction of the keys the edit changed
//     (Decision.CorrectedKeys), by default the operation's own; keys the
//     edit left alone are not touched;
//   - expiry: nothing.
//
// An operation held with no trust key counts nowhere, and neither does one
// whose target belongs to no channel.
func recordDecision(ctx context.Context, tx store.Tx, d Decision) error {
	var keys []string
	switch d.Decision {
	case model.HeldApproved:
		if d.TrustKey == "" {
			return nil
		}
		keys = []string{d.TrustKey}
	case model.HeldRejected:
		if d.TrustKey == "" {
			return nil
		}
		keys = []string{d.TrustKey}
	case model.HeldEdited:
		keys = d.CorrectedKeys
		if keys == nil && d.TrustKey != "" {
			keys = []string{d.TrustKey}
		}
	default:
		return nil
	}
	if len(keys) == 0 {
		return nil
	}
	ch, ok, err := channelOfTarget(ctx, tx, d.Key.Target)
	if err != nil || !ok {
		return err
	}
	labelled := func(key string) map[string]string {
		if key == d.TrustKey {
			return d.TrustLabels
		}
		return nil
	}
	if d.Decision == model.HeldApproved {
		batch := d.Batch
		if batch == "" {
			batch = string(d.Operation.ID)
		}
		err = trustWrite(ctx, tx, ch, keys, cleanApproval(d.At, labelled(d.TrustKey), batch))
	} else {
		err = trustWrite(ctx, tx, ch, keys, func(key string, entries map[string]trustRecord) (bool, error) {
			return correction(d.At, labelled(key))(key, entries)
		})
	}
	if model.IsCode(err, model.Conflict) {
		// A lost compare-and-set on a bucket: rerun the whole decision on
		// the winner's state, as for a serialization failure.
		return model.NewError(model.Transient, "trust counter changed; retry the unit")
	}
	return err
}

// trustUnit runs fn in a unit on ch's tenant, rerunning it when it loses a
// bucket to a concurrent writer.
func (s Service) trustUnit(ctx context.Context, ch model.ChannelRef, fn func(tx store.Tx) error) error {
	e := s.c.Engine()
	var err error
	for n := range trustAttempts {
		if n > 0 {
			if werr := pauseUnit(ctx, n); werr != nil {
				return werr
			}
		}
		err = Transact(ctx, e, ch.Tenant, fn)
		if !model.IsCode(err, model.Conflict) || ctx.Err() != nil {
			break
		}
	}
	if model.IsCode(err, model.NotFound) {
		return engine.ErrNotFound()
	}
	return err
}

func (s Service) authorizeTrust(ctx context.Context, action string, ch model.ChannelRef) error {
	if err := ch.Validate(); err != nil {
		return err
	}
	res := ch.Ref()
	return s.c.Authorize(ctx, policy.Authorization{Action: action, Resource: res}, res)
}

func validateTrustKeys(keys []string) error {
	for _, k := range keys {
		if err := model.ValidateTrustKey(k); err != nil {
			return err
		}
	}
	return nil
}

// trustWrite is the shared path of the trust writes: closed check, argument
// check, authorization (model.ActionTrustReset on the channel), then one
// unit.
func (s Service) trustWrite(ctx context.Context, ch model.ChannelRef, keys []string, mutate func(now time.Time) func(string, map[string]trustRecord) (bool, error)) error {
	return s.trustWriteAs(ctx, model.ActionTrustReset, ch, keys, mutate)
}

func (s Service) trustWriteAs(ctx context.Context, action string, ch model.ChannelRef, keys []string, mutate func(now time.Time) func(string, map[string]trustRecord) (bool, error)) error {
	e := s.c.Engine()
	if e.IsClosed() {
		return engine.ErrClosed()
	}
	if err := validateTrustKeys(keys); err != nil {
		return err
	}
	if err := s.authorizeTrust(ctx, action, ch); err != nil {
		return err
	}
	done, err := e.Begin()
	if err != nil {
		return err
	}
	defer done()
	now := e.Clock().Now().UTC()
	return s.trustUnit(ctx, ch, func(tx store.Tx) error {
		return trustWrite(ctx, tx, ch, keys, mutate(now))
	})
}

// RecordCleanApproval counts one clean approval of key on channel for batch:
// Clean and Approvals move by one, and the counter remembers the last 8
// batch IDs so that a batch moves it at most once per key. A replay of the
// batch changes nothing. labels, when not nil, replace the key's labels.
// Authorized for model.ActionTrustReset on the channel, as every write.
func (s Service) RecordCleanApproval(ctx context.Context, ch model.ChannelRef, key string, labels map[string]string, batch string) (model.TrustEntry, error) {
	if err := firstErr(model.ValidateTrustBatch(batch), model.ValidateTrustLabels(labels)); err != nil {
		return model.TrustEntry{}, err
	}
	err := s.trustWrite(ctx, ch, []string{key}, func(now time.Time) func(string, map[string]trustRecord) (bool, error) {
		return cleanApproval(now, labels, batch)
	})
	if err != nil {
		return model.TrustEntry{}, err
	}
	return s.Trust(ctx, ch, key)
}

// RecordCorrection counts a correction of key on channel: Clean to zero,
// Corrections up by one, LastCorrectionAt now.
func (s Service) RecordCorrection(ctx context.Context, ch model.ChannelRef, key string, labels map[string]string) (model.TrustEntry, error) {
	if err := model.ValidateTrustLabels(labels); err != nil {
		return model.TrustEntry{}, err
	}
	err := s.trustWrite(ctx, ch, []string{key}, func(now time.Time) func(string, map[string]trustRecord) (bool, error) {
		return correction(now, labels)
	})
	if err != nil {
		return model.TrustEntry{}, err
	}
	return s.Trust(ctx, ch, key)
}

// importTrust merges imp into a record: each total is the larger of stored
// and imported, LastCorrectionAt the later. Clean is raised only while the
// stored LastCorrectionAt is not later than the imported one: a stored
// correction after the imported history ended the streak it describes, so
// the imported Clean is stale. A merge that changes nothing is not written.
func importTrust(now time.Time, imp model.ImportTrust) func(string, map[string]trustRecord) (bool, error) {
	approvals := max(imp.Approvals, imp.Clean)
	corrections := imp.Corrections
	if corrections == 0 && imp.LastCorrectionAt != nil {
		corrections = 1
	}
	return func(key string, entries map[string]trustRecord) (bool, error) {
		r, err := trustRecordFor(entries, key)
		if err != nil {
			return false, err
		}
		_, exists := entries[key]
		next := r
		if r.LastCorrectionAt == nil || imp.LastCorrectionAt != nil && !r.LastCorrectionAt.After(*imp.LastCorrectionAt) {
			next.Clean = max(r.Clean, imp.Clean)
		}
		next.Approvals = max(r.Approvals, approvals)
		next.Corrections = max(r.Corrections, corrections)
		if imp.LastCorrectionAt != nil && (r.LastCorrectionAt == nil || imp.LastCorrectionAt.After(*r.LastCorrectionAt)) {
			t := imp.LastCorrectionAt.UTC()
			next.LastCorrectionAt = &t
		}
		if imp.Labels != nil && !maps.Equal(imp.Labels, r.Labels) {
			next.Labels = maps.Clone(imp.Labels)
		}
		if exists && next.Clean == r.Clean && next.Approvals == r.Approvals && next.Corrections == r.Corrections &&
			next.LastCorrectionAt == r.LastCorrectionAt && maps.Equal(next.Labels, r.Labels) {
			return false, nil
		}
		next.UpdatedAt = now
		entries[key] = next
		return true, nil
	}
}

// ImportTrust seeds the counters of imp.Key from an earlier approval
// history; see model.ImportTrust for the merge. Authorized for
// model.ActionTrustWrite on the channel. Returns the entry as stored.
func (s Service) ImportTrust(ctx context.Context, imp model.ImportTrust) (model.TrustEntry, error) {
	if err := imp.Validate(); err != nil {
		return model.TrustEntry{}, err
	}
	err := s.trustWriteAs(ctx, model.ActionTrustWrite, imp.Channel, []string{imp.Key}, func(now time.Time) func(string, map[string]trustRecord) (bool, error) {
		return importTrust(now, imp)
	})
	if err != nil {
		return model.TrustEntry{}, err
	}
	// Read back without trust.read: the write was authorized.
	return s.readTrust(ctx, imp.Channel, imp.Key)
}

// ResetTrust sets Clean of key to zero and keeps its totals. A key never
// counted is left absent.
func (s Service) ResetTrust(ctx context.Context, ch model.ChannelRef, key string) error {
	return s.ResetTrustSet(ctx, ch, []string{key})
}

// ResetTrustSet resets every key of the set in one unit: all or none.
func (s Service) ResetTrustSet(ctx context.Context, ch model.ChannelRef, keys []string) error {
	return s.trustWrite(ctx, ch, slices.Compact(slices.Sorted(slices.Values(keys))), reset)
}

// Trust returns the counter of key on channel, or model.NotFound. It is
// authorized for model.ActionTrustRead on the channel.
func (s Service) Trust(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
	if s.c.Engine().IsClosed() {
		return model.TrustEntry{}, engine.ErrClosed()
	}
	if err := model.ValidateTrustKey(key); err != nil {
		return model.TrustEntry{}, err
	}
	if err := s.authorizeTrust(ctx, model.ActionTrustRead, ch); err != nil {
		return model.TrustEntry{}, err
	}
	return s.readTrust(ctx, ch, key)
}

func (s Service) readTrust(ctx context.Context, ch model.ChannelRef, key string) (model.TrustEntry, error) {
	var out model.TrustEntry
	err := s.c.Engine().Store().View(ctx, ch.Tenant, func(r store.Reader) error {
		b, _, err := loadTrustBucket(ctx, r, ch, trustBucketOf(key))
		if err != nil {
			return err
		}
		rec, ok := b.Entries[key]
		if !ok {
			return engine.ErrNotFound()
		}
		out = rec.entry(key)
		return nil
	})
	return out, err
}

// Trusts lists the counters of channel whose labels match the filter,
// ordered by key. It reads all 64 buckets. Authorized for
// model.ActionTrustRead on the channel.
func (s Service) Trusts(ctx context.Context, ch model.ChannelRef, f model.TrustFilter) ([]model.TrustEntry, error) {
	if s.c.Engine().IsClosed() {
		return nil, engine.ErrClosed()
	}
	if err := s.authorizeTrust(ctx, model.ActionTrustRead, ch); err != nil {
		return nil, err
	}
	var out []model.TrustEntry
	err := s.c.Engine().Store().View(ctx, ch.Tenant, func(r store.Reader) error {
		out = nil
		for n := range trustBucketCount {
			b, _, err := loadTrustBucket(ctx, r, ch, n)
			if err != nil {
				return err
			}
			for k, rec := range b.Entries {
				if f.Matches(rec.Labels) {
					out = append(out, rec.entry(k))
				}
			}
		}
		return nil
	})
	slices.SortFunc(out, func(a, b model.TrustEntry) int {
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}
		return 0
	})
	return out, err
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
