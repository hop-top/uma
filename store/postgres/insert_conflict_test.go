package postgres

import (
	"context"
	"fmt"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Units writing new rows of one small table at once. Every new row of such
// a table, and every row keyed by a time-ordered ID, lands on the same
// index leaf page. A write that first looked its key up left a predicate
// lock on that page, so two units interleaving lookups and inserts formed
// a read/write cycle and one of them failed with model.Transient although
// they wrote different rows. Writes insert first and read only when the
// insert finds the key held; the tests interleave two units statement by
// statement and require both to commit.

const insertTenant model.TenantID = "postgres-insert-conflicts"

// steppedUnit is a unit running in its own goroutine that executes the
// steps it is handed, one at a time, until commit.
type steppedUnit struct {
	steps chan func(store.Tx) error
	done  chan error
	ran   chan error
}

func startUnit(ctx context.Context, st *Store, tenant model.TenantID) *steppedUnit {
	u := &steppedUnit{steps: make(chan func(store.Tx) error), done: make(chan error, 1), ran: make(chan error)}
	go func() {
		u.ran <- st.Transact(ctx, tenant, func(tx store.Tx) error {
			var first error
			for f := range u.steps {
				err := f(tx)
				if first == nil {
					first = err
				}
				u.done <- err
			}
			return first
		})
	}()
	return u
}

// step runs f in the unit and returns its result.
func (u *steppedUnit) step(f func(store.Tx) error) error {
	u.steps <- f
	return <-u.done
}

// commit ends the unit and returns the outcome of its Transact.
func (u *steppedUnit) commit() error {
	close(u.steps)
	return <-u.ran
}

// interleave runs two units, a and b, alternating their steps a[0], b[0],
// a[1], b[1], ..., then commits a and b, and fails on any error.
func interleave(t *testing.T, st *Store, a, b []func(store.Tx) error) {
	t.Helper()
	interleaveIn(t, st, insertTenant, a, insertTenant, b)
}

// interleaveIn is interleave with unit a in tenant ta and b in tb.
func interleaveIn(t *testing.T, st *Store, ta model.TenantID, a []func(store.Tx) error, tb model.TenantID, b []func(store.Tx) error) {
	t.Helper()
	ua, ub := startUnit(t.Context(), st, ta), startUnit(t.Context(), st, tb)
	for i := range a {
		if err := ua.step(a[i]); err != nil {
			t.Errorf("unit a step %d: %v", i, err)
		}
		if err := ub.step(b[i]); err != nil {
			t.Errorf("unit b step %d: %v", i, err)
		}
	}
	if err := ua.commit(); err != nil {
		t.Errorf("unit a commit: %v", err)
	}
	if err := ub.commit(); err != nil {
		t.Errorf("unit b commit: %v", err)
	}
}

// insertRig is a store with a channel, an inbox and two conversations.
type insertRig struct {
	st     *Store
	tenant model.TenantID
	ch     model.ChannelRef
	in     model.InboxRef
	convs  [2]model.ConversationRef
	ids    int
}

func newInsertRig(t *testing.T) *insertRig {
	t.Helper()
	st, err := Open(t.Context(), newDatabase(t, true), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return newInsertRigIn(t, st, insertTenant)
}

// newInsertRigIn is an insert rig of tenant on st.
func newInsertRigIn(t *testing.T, st *Store, tenant model.TenantID) *insertRig {
	t.Helper()
	ctx := t.Context()
	r := &insertRig{
		st: st, tenant: tenant,
		ch: model.ChannelRef{Tenant: tenant, ID: benchID(0xa0, 1)},
		in: model.InboxRef{Tenant: tenant, ID: benchID(0xa0, 2)},
	}
	if err := st.Transact(ctx, tenant, func(tx store.Tx) error {
		if err := firstErr(
			tx.PutChannel(ctx, model.Channel{
				Ref: r.ch, Type: benchSendType, Configuration: model.Extension{Name: string(benchSendType) + ".config", Version: 1}, Revision: "ch1",
			}, ""),
			tx.PutInbox(ctx, model.Inbox{Ref: r.in, Channel: r.ch, Name: "insert", Revision: "in1"}, ""),
		); err != nil {
			return err
		}
		for i := range r.convs {
			r.convs[i] = model.ConversationRef{Inbox: r.in, ID: benchID(0xa1, i)}
			if err := tx.PutConversation(ctx, model.Conversation{
				Ref: r.convs[i], Mode: model.ModePerRoom, Status: model.ConversationOpen, CreatedAt: time.Now(), Revision: "cv1",
			}, ""); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return r
}

// id returns a new time-ordered ID: each sorts after the ones before it.
func (r *insertRig) id() model.ID {
	r.ids++
	return benchID(0xa2, r.ids)
}

func (r *insertRig) work() store.Work {
	return store.Work{ID: r.id(), Kind: "insert.work", Resource: model.OperationRef{Tenant: r.tenant, ID: r.id()}.Ref()}
}

func (r *insertRig) enqueue(w store.Work) func(store.Tx) error {
	return func(tx store.Tx) error { return tx.EnqueueWork(context.Background(), w) }
}

func (r *insertRig) operation(caller model.ActorID, key model.OperationID, fp byte) store.OperationRecord {
	return store.OperationRecord{
		Operation: model.Operation{Ref: model.OperationRef{Tenant: r.tenant, ID: r.id()}, State: model.Queued, Revision: "op1"},
		Key: store.OperationKey{
			Tenant: r.tenant, Caller: caller, Kind: model.ActionMessageReply, Target: r.convs[0].Ref(), ID: key,
		},
		FingerprintVersion: 1, Fingerprint: []byte{fp},
	}
}

func (r *insertRig) claim(rec store.OperationRecord) func(store.Tx) error {
	return func(tx store.Tx) error {
		_, created, err := tx.ClaimOperation(context.Background(), rec)
		if err == nil && !created {
			return fmt.Errorf("claim of %s replayed, want created", rec.Key.ID)
		}
		return err
	}
}

// message is a new message of conversation conv. An indexed message holds
// a provider reference and an attachment, so its write fills the reference
// and attachment indexes too.
func (r *insertRig) message(conv int, indexed bool) model.Message {
	ref := model.MessageRef{Conversation: r.convs[conv], ID: r.id()}
	m := model.Message{
		Ref:        ref,
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
		Audience:   model.Audience{Visibility: model.Public},
		RecordedAt: time.Now().UTC(),
		Revision:   "m1",
	}
	if indexed {
		m.Content.Parts = append(m.Content.Parts, model.Part{Kind: model.PartAttachment, Attachment: &model.AttachmentRef{Inbox: r.in, ID: r.id()}})
		m.External = []model.ExternalRef{model.ProviderMessageRef(r.ch, "provider-"+string(ref.ID))}
	}
	return m
}

func (r *insertRig) put(m model.Message) func(store.Tx) error {
	return func(tx store.Tx) error { return tx.PutMessage(context.Background(), m, "") }
}

// TestInterleavedEnqueuesBothCommit: units enqueueing different work at
// once both commit.
func TestInterleavedEnqueuesBothCommit(t *testing.T) {
	r := newInsertRig(t)
	interleave(t, r.st,
		[]func(store.Tx) error{r.enqueue(r.work()), r.enqueue(r.work())},
		[]func(store.Tx) error{r.enqueue(r.work()), r.enqueue(r.work())},
	)
}

// TestInterleavedOperationClaimsBothCommit: units claiming different
// operation keys at once both commit.
func TestInterleavedOperationClaimsBothCommit(t *testing.T) {
	r := newInsertRig(t)
	interleave(t, r.st,
		[]func(store.Tx) error{r.claim(r.operation("a", "a-1", 1)), r.claim(r.operation("a", "a-2", 1))},
		[]func(store.Tx) error{r.claim(r.operation("b", "b-1", 1)), r.claim(r.operation("b", "b-2", 1))},
	)
}

// TestInterleavedNewMessagesBothCommit: units writing new messages at
// once both commit, in one conversation or in two, also when one fills the
// reference and attachment indexes.
func TestInterleavedNewMessagesBothCommit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		b       int
		indexed bool
	}{
		{"one conversation", 0, false},
		{"two conversations", 1, false},
		{"one with references and attachments", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newInsertRig(t)
			interleave(t, r.st,
				[]func(store.Tx) error{r.put(r.message(0, false)), r.put(r.message(0, false))},
				[]func(store.Tx) error{r.put(r.message(tc.b, tc.indexed)), r.put(r.message(tc.b, tc.indexed))},
			)
		})
	}
}

// TestEnqueueHeldWorkIDConflicts: enqueueing an ID already enqueued is
// model.Conflict and leaves the unit usable.
func TestEnqueueHeldWorkIDConflicts(t *testing.T) {
	r := newInsertRig(t)
	ctx := t.Context()
	held, next := r.work(), r.work()
	if err := r.st.Transact(ctx, insertTenant, r.enqueue(held)); err != nil {
		t.Fatal(err)
	}
	if err := r.st.Transact(ctx, insertTenant, func(tx store.Tx) error {
		again := held
		again.Kind = "insert.other"
		if err := tx.EnqueueWork(ctx, again); !model.IsCode(err, model.Conflict) {
			t.Errorf("enqueue of a held ID = %v, want conflict", err)
		}
		return tx.EnqueueWork(ctx, next)
	}); err != nil {
		t.Fatalf("unit after the conflict: %v", err)
	}
	got := claimAll(t, r.st)
	if len(got) != 2 || got[0].ID != held.ID || got[0].Kind != held.Kind || got[1].ID != next.ID {
		t.Fatalf("claimed %+v, want %s as first enqueued, then %s", got, held.ID, next.ID)
	}
}

func claimAll(t *testing.T, st *Store) []store.Work {
	t.Helper()
	var got []store.Work
	if err := st.Transact(t.Context(), insertTenant, func(tx store.Tx) error {
		var err error
		got, err = tx.ClaimWork(t.Context(), store.WorkClaim{
			Kinds: []string{"insert.work", "insert.other"}, Owner: "w", Until: time.Now().Add(time.Minute), Limit: 10,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestClaimOperationOfHeldKeyOrReference: a claimed key replays under an
// equal fingerprint and conflicts under another; a claimed operation
// reference under another key conflicts. None of them spoils the unit.
func TestClaimOperationOfHeldKeyOrReference(t *testing.T) {
	r := newInsertRig(t)
	ctx := t.Context()
	first := r.operation("caller", "held", 1)
	if err := r.st.Transact(ctx, insertTenant, r.claim(first)); err != nil {
		t.Fatal(err)
	}
	next := r.operation("caller", "next", 1)
	if err := r.st.Transact(ctx, insertTenant, func(tx store.Tx) error {
		replay := r.operation("caller", "held", 1)
		got, created, err := tx.ClaimOperation(ctx, replay)
		if err != nil || created || got.Operation.Ref != first.Operation.Ref || got.Operation.Revision != first.Operation.Revision {
			t.Errorf("equal claim = %v created=%v ref=%v, want the stored claim replayed", err, created, got.Operation.Ref)
		}
		if _, _, err := tx.ClaimOperation(ctx, r.operation("caller", "held", 2)); !model.IsCode(err, model.Conflict) {
			t.Errorf("claim under another fingerprint = %v, want conflict", err)
		}
		reused := r.operation("caller", "other-key", 1)
		reused.Operation.Ref = first.Operation.Ref
		if _, _, err := tx.ClaimOperation(ctx, reused); !model.IsCode(err, model.Conflict) {
			t.Errorf("claim of a held reference under another key = %v, want conflict", err)
		}
		return r.claim(next)(tx)
	}); err != nil {
		t.Fatalf("unit after the replay and conflicts: %v", err)
	}
	if err := r.st.View(ctx, insertTenant, func(rd store.Reader) error {
		got, err := rd.OperationByKey(ctx, next.Key)
		if err == nil && got.Operation.Ref != next.Operation.Ref {
			t.Errorf("claim after the conflicts stored %v, want %v", got.Operation.Ref, next.Operation.Ref)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPutNewMessageOfHeldID: a new message whose reference is held is
// model.Conflict and leaves the unit usable; one whose ID a message of
// another conversation holds is model.Conflict too.
func TestPutNewMessageOfHeldID(t *testing.T) {
	r := newInsertRig(t)
	ctx := t.Context()
	held := r.message(0, true)
	if err := r.st.Transact(ctx, insertTenant, r.put(held)); err != nil {
		t.Fatal(err)
	}
	next := r.message(0, true)
	if err := r.st.Transact(ctx, insertTenant, func(tx store.Tx) error {
		again := held
		again.Revision = "m2"
		again.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "again"}}}
		if err := tx.PutMessage(ctx, again, ""); !model.IsCode(err, model.Conflict) {
			t.Errorf("new message under a held reference = %v, want conflict", err)
		}
		return tx.PutMessage(ctx, next, "")
	}); err != nil {
		t.Fatalf("unit after the conflict: %v", err)
	}
	moved := held
	moved.Ref.Conversation = r.convs[1]
	moved.External = nil
	if err := r.st.Transact(ctx, insertTenant, r.put(moved)); !model.IsCode(err, model.Conflict) {
		t.Errorf("new message under an ID another conversation holds = %v, want conflict", err)
	}
	if err := r.st.View(ctx, insertTenant, func(rd store.Reader) error {
		got, err := rd.GetMessage(ctx, held.Ref)
		if err == nil && (got.Revision != held.Revision || got.Content.Parts[0].Text != "hello") {
			t.Errorf("held message = %+v, want it unchanged", got)
		}
		if err != nil {
			return err
		}
		refs, err := rd.MessagesByReference(ctx, r.in, held.External[0], 10)
		if err == nil && (len(refs) != 1 || refs[0].Ref != held.Ref) {
			t.Errorf("held reference finds %d messages, want only the held one", len(refs))
		}
		if err != nil {
			return err
		}
		_, err = rd.GetMessage(ctx, next.Ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
