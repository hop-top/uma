package conformance

import (
	"errors"
	"reflect"
	"strconv"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// recipientLogCases cover AppendRecipientLog and RecipientLogEntries:
// gapless sequences per log, retention of the latest entries, paging after
// a cursor, rollback, validation, snapshots and tenant scope.
var recipientLogCases = []storeCase{
	{"AppendAssignsGaplessSequencesPerLog", func(t *testing.T, e *env) {
		a := e.recipientLog("visitor-a")
		b := e.recipientLog("visitor-b")
		c := a
		c.Namespace = "conformance.chat.user"
		for i, k := range []store.RecipientLogKey{a, a, b, a, c, b} {
			want := map[int]uint64{0: 1, 1: 2, 2: 1, 3: 3, 4: 1, 5: 2}[i]
			if got := e.appendRecipient(t, k, i, 10); got != want {
				t.Fatalf("append %d to %+v = seq %d, want %d", i, k, got, want)
			}
		}
		e.wantRecipientLog(t, a, 0, recipientEntry(1, 0), recipientEntry(2, 1), recipientEntry(3, 3))
		e.wantRecipientLog(t, b, 0, recipientEntry(1, 2), recipientEntry(2, 5))
		e.wantRecipientLog(t, c, 0, recipientEntry(1, 4))
	}},
	{"RetainKeepsTheLatestEntries", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		for i := range 5 {
			e.appendRecipient(t, k, i, 3)
		}
		e.wantRecipientLog(t, k, 0, recipientEntry(3, 2), recipientEntry(4, 3), recipientEntry(5, 4))
		e.wantRecipientLog(t, k, 2, recipientEntry(3, 2), recipientEntry(4, 3), recipientEntry(5, 4))
		e.wantRecipientLog(t, k, 4, recipientEntry(5, 4))
		// A larger retain never brings trimmed entries back; a smaller one
		// trims down to it.
		e.appendRecipient(t, k, 5, 10)
		e.wantRecipientLog(t, k, 0, recipientEntry(3, 2), recipientEntry(4, 3), recipientEntry(5, 4), recipientEntry(6, 5))
		e.appendRecipient(t, k, 6, 1)
		e.wantRecipientLog(t, k, 0, recipientEntry(7, 6))
	}},
	{"EntriesPageAfterTheCursor", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		for i := range 4 {
			e.appendRecipient(t, k, i, 10)
		}
		e.mustView(t, func(r store.Reader) error {
			got, err := r.RecipientLogEntries(e.ctx, k, 1, 2)
			if err == nil && !reflect.DeepEqual(got, []store.RecipientLogEntry{recipientEntry(2, 1), recipientEntry(3, 2)}) {
				t.Errorf("entries after 1, limit 2 = %+v", got)
			}
			return err
		})
		e.wantRecipientLog(t, k, 4)
		e.wantRecipientLog(t, k, 1<<63)
		e.wantRecipientLog(t, e.recipientLog("never-appended"), 0)
		unstored := k
		unstored.Channel.ID = e.id(t)
		e.wantRecipientLog(t, unstored, 0)
		for _, limit := range []int{0, -1} {
			wantCode(t, e.viewErr(func(r store.Reader) error {
				_, err := r.RecipientLogEntries(e.ctx, k, 0, limit)
				return err
			}), model.Validation, "RecipientLogEntries at limit ", limit)
		}
	}},
	{"RollbackStoresNothing", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		errRollback := errors.New("roll back")
		err := e.txErr(func(tx store.Tx) error {
			if _, err := tx.AppendRecipientLog(e.ctx, k, recipientValue(0), 10); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("Transact = %v, want the callback error", err)
		}
		e.wantRecipientLog(t, k, 0)
		if got := e.appendRecipient(t, k, 1, 10); got != 1 {
			t.Fatalf("append after a rolled back append = seq %d, want 1", got)
		}
		// Appends in one unit read their own writes.
		e.mustTx(t, func(tx store.Tx) error {
			for want := uint64(2); want <= 3; want++ {
				got, err := tx.AppendRecipientLog(e.ctx, k, recipientValue(int(want)), 10)
				if err != nil {
					return err
				}
				if got != want {
					t.Errorf("append in one unit = seq %d, want %d", got, want)
				}
			}
			got, err := tx.RecipientLogEntries(e.ctx, k, 0, 10)
			if err == nil && len(got) != 3 {
				t.Errorf("entries inside the unit = %+v, want 3", got)
			}
			return err
		})
	}},
	{"AppendValidates", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		for name, c := range map[string]struct {
			key    store.RecipientLogKey
			value  model.Extension
			retain int
		}{
			"no namespace":      {store.RecipientLogKey{Channel: k.Channel, Recipient: "v"}, recipientValue(0), 1},
			"no recipient":      {store.RecipientLogKey{Channel: k.Channel, Namespace: k.Namespace}, recipientValue(0), 1},
			"NUL recipient":     {store.RecipientLogKey{Channel: k.Channel, Namespace: k.Namespace, Recipient: "a\x00b"}, recipientValue(0), 1},
			"no channel":        {store.RecipientLogKey{Namespace: k.Namespace, Recipient: "v"}, recipientValue(0), 1},
			"no value name":     {k, model.Extension{Version: 1, Data: []byte(`{}`)}, 1},
			"invalid JSON data": {k, model.Extension{Name: "conformance.entry", Version: 1, Data: []byte(`{`)}, 1},
			"zero retain":       {k, recipientValue(0), 0},
			"negative retain":   {k, recipientValue(0), -1},
		} {
			err := e.txErr(func(tx store.Tx) error {
				_, err := tx.AppendRecipientLog(e.ctx, c.key, c.value, c.retain)
				return err
			})
			if model.CodeOf(err) != model.Validation {
				t.Errorf("%s: AppendRecipientLog = %v, want validation", name, err)
			}
		}
		e.wantRecipientLog(t, k, 0)
		unstored := k
		unstored.Channel.ID = e.id(t)
		wantCode(t, e.txErr(func(tx store.Tx) error {
			_, err := tx.AppendRecipientLog(e.ctx, unstored, recipientValue(0), 1)
			return err
		}), model.NotFound)
	}},
	{"EntriesAreSnapshots", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		v := recipientValue(0)
		e.mustTx(t, func(tx store.Tx) error {
			_, err := tx.AppendRecipientLog(e.ctx, k, v, 10)
			v.Data[2] = 'X'
			return err
		})
		e.mustView(t, func(r store.Reader) error {
			got, err := r.RecipientLogEntries(e.ctx, k, 0, 10)
			if err == nil && len(got) == 1 {
				got[0].Value.Data[2] = 'Y'
			}
			return err
		})
		e.wantRecipientLog(t, k, 0, recipientEntry(1, 0))
	}},
	{"LogsAreTenantScoped", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		e.appendRecipient(t, k, 0, 10)
		e.appendRecipient(t, k, 1, 10)
		// The foreign tenant's unit never reaches tenantA's log.
		e.wantForeign(t, func(r store.Reader) error { _, err := r.RecipientLogEntries(e.ctx, k, 0, 10); return err })
		wantCode(t, e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			_, err := tx.AppendRecipientLog(e.ctx, k, recipientValue(9), 10)
			return err
		}), model.NotFound)
		// The same channel ID, namespace and recipient in tenantB is
		// another log with its own sequences.
		other := k
		other.Channel.Tenant = tenantB
		if err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			if err := tx.PutChannel(e.ctx, e.channelRecord(other.Channel, "r1"), ""); err != nil {
				return err
			}
			seq, err := tx.AppendRecipientLog(e.ctx, other, recipientValue(7), 10)
			if err == nil && seq != 1 {
				t.Errorf("first append in tenantB = seq %d, want 1", seq)
			}
			return err
		}); err != nil {
			t.Fatalf("tenantB: %v", err)
		}
		if err := e.s.View(e.ctx, tenantB, func(r store.Reader) error {
			got, err := r.RecipientLogEntries(e.ctx, other, 0, 10)
			if err == nil && !reflect.DeepEqual(got, []store.RecipientLogEntry{recipientEntry(1, 7)}) {
				t.Errorf("tenantB entries = %+v", got)
			}
			return err
		}); err != nil {
			t.Fatalf("tenantB view: %v", err)
		}
		e.wantRecipientLog(t, k, 0, recipientEntry(1, 0), recipientEntry(2, 1))
	}},
}

// recipientLog is a log of recipient on the env's channel.
func (e *env) recipientLog(recipient string) store.RecipientLogKey {
	return store.RecipientLogKey{Channel: e.channel, Namespace: "conformance.chat.visitor", Recipient: recipient}
}

// recipientValue is the value of entry n of a case.
func recipientValue(n int) model.Extension {
	return model.Extension{Name: "conformance.entry", Version: 1, Data: []byte(`{"n":` + strconv.Itoa(n) + `}`)}
}

func recipientEntry(seq uint64, n int) store.RecipientLogEntry {
	return store.RecipientLogEntry{Seq: seq, Value: recipientValue(n)}
}

// appendRecipient appends value n to k in its own unit and returns the
// sequence.
func (e *env) appendRecipient(t *testing.T, k store.RecipientLogKey, n, retain int) uint64 {
	t.Helper()
	var seq uint64
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		seq, err = tx.AppendRecipientLog(e.ctx, k, recipientValue(n), retain)
		return err
	})
	return seq
}

// wantRecipientLog requires the entries of k after the cursor to be want.
func (e *env) wantRecipientLog(t *testing.T, k store.RecipientLogKey, after uint64, want ...store.RecipientLogEntry) {
	t.Helper()
	e.mustView(t, func(r store.Reader) error {
		got, err := r.RecipientLogEntries(e.ctx, k, after, 100)
		if err == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("entries of %s after %d = %+v, want %+v", k.Recipient, after, got, want)
		}
		return err
	})
}
