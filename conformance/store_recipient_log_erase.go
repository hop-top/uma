package conformance

import (
	"errors"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// recipientLogEraseCases cover EraseRecipientLog: the entries go and the
// sequence stays, other logs are untouched, an absent log is not an error,
// the delete rolls back with its unit, and keys are validated and tenant
// scoped.
var recipientLogEraseCases = []storeCase{
	{"EraseDeletesEntriesAndKeepsTheSequence", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		for i := range 4 {
			e.appendRecipient(t, k, i, 3) // retains 3 of 4
		}
		if got := e.eraseRecipient(t, k); got != 3 {
			t.Fatalf("EraseRecipientLog = %d, want the 3 retained entries", got)
		}
		e.wantRecipientLog(t, k, 0)
		// The next append continues after the erased entries: a reader
		// holding the old cursor finds a gap, never a reused sequence.
		if got := e.appendRecipient(t, k, 4, 3); got != 5 {
			t.Fatalf("append after erase = seq %d, want 5", got)
		}
		e.wantRecipientLog(t, k, 0, recipientEntry(5, 4))
		e.wantRecipientLog(t, k, 3, recipientEntry(5, 4))
	}},
	{"EraseTouchesOnlyTheNamedLog", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		otherRecipient := e.recipientLog("visitor-2")
		otherNamespace := k
		otherNamespace.Namespace = "conformance.chat.user"
		for _, l := range []store.RecipientLogKey{k, otherRecipient, otherNamespace} {
			e.appendRecipient(t, l, 0, 10)
			e.appendRecipient(t, l, 1, 10)
		}
		e.eraseRecipient(t, k)
		e.wantRecipientLog(t, k, 0)
		e.wantRecipientLog(t, otherRecipient, 0, recipientEntry(1, 0), recipientEntry(2, 1))
		e.wantRecipientLog(t, otherNamespace, 0, recipientEntry(1, 0), recipientEntry(2, 1))
	}},
	{"EraseOfAnAbsentLogIsNotAnError", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		if got := e.eraseRecipient(t, k); got != 0 {
			t.Errorf("erase of a log never appended to = %d, want 0", got)
		}
		e.appendRecipient(t, k, 0, 10)
		if got := e.eraseRecipient(t, k); got != 1 {
			t.Errorf("first erase = %d, want 1", got)
		}
		if got := e.eraseRecipient(t, k); got != 0 {
			t.Errorf("repeated erase = %d, want 0", got)
		}
		unstored := k
		unstored.Channel.ID = e.id(t)
		if got := e.eraseRecipient(t, unstored); got != 0 {
			t.Errorf("erase on a channel that is not stored = %d, want 0", got)
		}
	}},
	{"EraseRollsBackWithItsUnit", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		e.appendRecipient(t, k, 0, 10)
		e.appendRecipient(t, k, 1, 10)
		errRollback := errors.New("roll back")
		err := e.txErr(func(tx store.Tx) error {
			if _, err := tx.EraseRecipientLog(e.ctx, k); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("Transact = %v, want the callback error", err)
		}
		e.wantRecipientLog(t, k, 0, recipientEntry(1, 0), recipientEntry(2, 1))
		// Inside one unit an erase sees earlier appends and later reads and
		// appends see the erase.
		e.mustTx(t, func(tx store.Tx) error {
			if _, err := tx.AppendRecipientLog(e.ctx, k, recipientValue(2), 10); err != nil {
				return err
			}
			n, err := tx.EraseRecipientLog(e.ctx, k)
			if err != nil {
				return err
			}
			if n != 3 {
				t.Errorf("erase in the unit = %d, want the 3 entries it can read", n)
			}
			got, err := tx.RecipientLogEntries(e.ctx, k, 0, 10)
			if err == nil && len(got) != 0 {
				t.Errorf("entries after the erase in the unit = %+v", got)
			}
			seq, err := tx.AppendRecipientLog(e.ctx, k, recipientValue(3), 10)
			if err == nil && seq != 4 {
				t.Errorf("append after the erase in the unit = seq %d, want 4", seq)
			}
			return err
		})
		e.wantRecipientLog(t, k, 0, recipientEntry(4, 3))
	}},
	{"EraseValidatesTheKey", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		e.appendRecipient(t, k, 0, 10)
		for name, key := range map[string]store.RecipientLogKey{
			"no namespace":  {Channel: k.Channel, Recipient: "v"},
			"no recipient":  {Channel: k.Channel, Namespace: k.Namespace},
			"NUL recipient": {Channel: k.Channel, Namespace: k.Namespace, Recipient: "a\x00b"},
			"no channel":    {Namespace: k.Namespace, Recipient: "v"},
		} {
			err := e.txErr(func(tx store.Tx) error {
				_, err := tx.EraseRecipientLog(e.ctx, key)
				return err
			})
			if model.CodeOf(err) != model.Validation {
				t.Errorf("%s: EraseRecipientLog = %v, want validation", name, err)
			}
		}
		e.wantRecipientLog(t, k, 0, recipientEntry(1, 0))
	}},
	{"EraseIsTenantScoped", func(t *testing.T, e *env) {
		k := e.recipientLog("visitor")
		e.appendRecipient(t, k, 0, 10)
		// The foreign tenant's unit cannot reach tenantA's log.
		wantCode(t, e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			_, err := tx.EraseRecipientLog(e.ctx, k)
			return err
		}), model.NotFound)
		e.wantRecipientLog(t, k, 0, recipientEntry(1, 0))
		// The same coordinates in tenantB are another log: erasing it
		// leaves tenantA's.
		other := k
		other.Channel.Tenant = tenantB
		if err := e.s.Transact(e.ctx, tenantB, func(tx store.Tx) error {
			if err := tx.PutChannel(e.ctx, e.channelRecord(other.Channel, "r1"), ""); err != nil {
				return err
			}
			if _, err := tx.AppendRecipientLog(e.ctx, other, recipientValue(7), 10); err != nil {
				return err
			}
			n, err := tx.EraseRecipientLog(e.ctx, other)
			if err == nil && n != 1 {
				t.Errorf("erase in tenantB = %d, want 1", n)
			}
			return err
		}); err != nil {
			t.Fatalf("tenantB: %v", err)
		}
		e.wantRecipientLog(t, k, 0, recipientEntry(1, 0))
	}},
}

// eraseRecipient erases k in its own unit and returns the number of
// entries deleted.
func (e *env) eraseRecipient(t *testing.T, k store.RecipientLogKey) int {
	t.Helper()
	var n int
	e.mustTx(t, func(tx store.Tx) error {
		var err error
		n, err = tx.EraseRecipientLog(e.ctx, k)
		return err
	})
	return n
}
