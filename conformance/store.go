// Package conformance holds shared test suites that every implementation of a
// uma provider interface must pass. A suite exercises only the exported
// interface, so maintained and third-party backends run the same cases.
//
// Run the store suite from a backend's tests:
//
//	func TestConformance(t *testing.T) {
//		conformance.RunStoreSuite(t, func(t *testing.T) conformance.StoreFixture {
//			clock := portstest.NewFixedClock(time.Unix(1_700_000_000, 0))
//			return conformance.StoreFixture{
//				Store:   mybackend.New(clock),
//				Now:     clock.Now,
//				Advance: func(d time.Duration) { clock.Advance(d) },
//			}
//		})
//	}
//
// A new backend runs RunStoreSuite, RunDurableLogicalSuite (or RunDurableSuite)
// and RunServiceSuite. RunStoreSuite alone rejects a backend that holds a lock
// from the start of a unit's callback to its commit, that ignores a filter
// field a read documents, or that resumes a cursor wrongly: its Filters,
// Cursors and Overlap groups pin the filter matrix, the cursor rules and unit
// overlap of the store package. The durable suites add concurrency and fault
// cases, and RunServiceSuite checks that the engine's authorized queries
// compose with the store's filters and cursors.
//
// A durable backend also runs RunDurableSuite with a RestartFunc that reopens
// its persisted state; only that suite backs Properties().Durable. A store
// that cannot restart, such as the memory store, runs RunDurableLogicalSuite:
// the concurrency, publication and replay cases without the restart cases.
// Passing it is logical evidence, never durability evidence.
//
//	func TestDurable(t *testing.T) {
//		conformance.RunDurableSuite(t, newFixture, func(t *testing.T, fx conformance.StoreFixture) conformance.StoreFixture {
//			next := fx
//			next.Store = mybackend.Open(dataDirOf(fx.Store)) // same state, new handle
//			return next
//		})
//	}
//
// A provider adapter runs RunAdapterSuite with an AdapterHarness: the
// factory under test, the scripted provider side behind it and the
// capabilities it declares. The suite composes an engine over the memory
// store, or a store the harness supplies, and runs only the cases those
// capabilities imply.
//
// A ports.BlobStore runs RunBlobStoreSuite with a BlobFactory, and
// RunAttachmentSuite composes the Attachments service over a store and a
// blob store, so either side can be swapped for the backend under test.
//
// AttestationVerifier and ScriptedVerifier are identity verifier doubles:
// an honest attestation verifier, and one that answers whatever a case
// scripts, for testing that core checks verifier output before any policy
// decision.
//
// The suites grow with the interface.
package conformance

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// StoreFixture is one fresh, empty store under test and control of the clock
// it judges lease expiry by.
type StoreFixture struct {
	// Store is the backend under test. It must hold no data.
	Store store.Store
	// Now reports the store's lease clock. Required.
	Now func() time.Time
	// Advance moves the store's lease clock forward; it is also the clock
	// operations are written by. When nil, cases that need lease expiry or
	// distinct operation write times are skipped.
	Advance func(time.Duration)
	// SubscriptionRows counts the subscription rows the backend holds, in
	// any tenant. Optional: cases that must show a call left no
	// subscription row skip that check when it is nil, and rely on the
	// store opening no write unit.
	SubscriptionRows func(context.Context) (int, error)
}

// StoreFactory returns a fresh fixture for one subtest. It may register
// cleanup with t.Cleanup.
type StoreFactory func(t *testing.T) StoreFixture

// RunStoreSuite runs the shared store cases against factory: transaction
// atomicity and rollback, handle lifetime, tenant isolation, revision
// conflicts, snapshots, streams, work items, attachment records and protocol
// state, channel and inbox keys, labels and the conversation label
// selector, atomic claims, identity links, operation fencing, work and leases,
// the event log with checkpoints, retention purges, orphan-contact purges,
// staging intents, redaction and link tombstones, export records, agent
// tasks and their message history, work quarantine and in-flight caps,
// recipient logs, interaction records with their response claims,
// redaction and expired-token sweep, and the conversation workflow filters
// (assignee, snooze judged at the store clock), and per-actor read markers.
//
// The Filters, Cursors and Overlap groups run for every paged read and filter
// field: each read narrows by the fields the store package lists for it and
// rejects the rest, each cursor resumes and survives commits, and overlapping
// units commit once or fail with a conflict.
func RunStoreSuite(t *testing.T, factory StoreFactory) {
	t.Helper()
	groups := []struct {
		name  string
		cases []storeCase
	}{
		{"Transactions", transactionCases},
		{"Records", recordCases},
		{"Resources", slices.Concat(resourceCases, streamsOfCases, inboundMessagesCases)},
		{"References", referenceCases},
		{"Holders", holderCases},
		{"Inboxes", inboxCases},
		{"Keys", keyCases},
		{"Labels", labelCases},
		{"Claims", claimCases},
		{"Links", linkCases},
		{"Ledger", ledgerCases},
		{"OperationList", operationListCases},
		{"OperationUpdatedAt", operationUpdatedAtCases},
		{"Events", slices.Concat(eventCases, eventGetStoreCases, eventFilterCases)},
		{"Retention", slices.Concat(retentionCases, contactPurgeCases)},
		{"Staging", stagingCases},
		{"Redaction", slices.Concat(redactionCases, redactionExactCases, linkTombstoneCases)},
		{"Exports", exportCases},
		{"Tasks", taskCases},
		{"Work", workCases},
		{"Search", slices.Concat(searchCases, searchAttachmentCases, searchLabelCases)},
		{"RecipientLogs", slices.Concat(recipientLogCases, recipientLogEraseCases)},
		{"Interactions", slices.Concat(interactionRecordCases, interactionClaimCases, interactionPrivacyCases, interactionLinkCases)},
		{"Filters", slices.Concat(filterMatrixCases, subscriptionFilterCases)},
		{"Cursors", cursorMatrixCases},
		{"Overlap", unitOverlapCases},
		{"ConversationWorkflow", slices.Concat(workflowCases, conversationLinkFilterCases)},
		{"ReadMarkers", readMarkerCases},
	}
	for _, g := range groups {
		t.Run(g.name, func(t *testing.T) {
			for _, c := range g.cases {
				t.Run(c.name, func(t *testing.T) {
					fx := factory(t)
					if fx.Store == nil || fx.Now == nil {
						t.Fatal("StoreFixture needs Store and Now")
					}
					c.run(t, newEnv(t, fx))
				})
			}
		})
	}
}

type storeCase struct {
	name string
	run  func(*testing.T, *env)
}

const (
	tenantA model.TenantID = "conformance-tenant-a"
	tenantB model.TenantID = "conformance-tenant-b"
)

// env is a fixture seeded with one channel and inbox in tenantA.
type env struct {
	fx      StoreFixture
	s       store.Store
	ctx     context.Context
	ids     *portstest.SequenceIDs
	channel model.ChannelRef
	inbox   model.InboxRef
}

func newEnv(t *testing.T, fx StoreFixture) *env {
	t.Helper()
	e := &env{fx: fx, s: fx.Store, ctx: t.Context(), ids: portstest.NewSequenceIDs(0xc0f)}
	e.channel = model.ChannelRef{Tenant: tenantA, ID: e.id(t)}
	e.inbox = model.InboxRef{Tenant: tenantA, ID: e.id(t)}
	e.mustTx(t, func(tx store.Tx) error {
		if err := tx.PutChannel(e.ctx, e.channelRecord(e.channel, "r1"), ""); err != nil {
			return err
		}
		return tx.PutInbox(e.ctx, model.Inbox{
			Ref: e.inbox, Channel: e.channel, Name: "support",
			Destinations: []string{"support@example.test"}, Revision: "r1",
		}, "")
	})
	return e
}

func (e *env) id(t *testing.T) model.ID {
	t.Helper()
	id, err := e.ids.New(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) channelRecord(ref model.ChannelRef, rev model.Revision) model.Channel {
	return model.Channel{
		Ref: ref, Type: "conformance.chat",
		Configuration: model.Extension{Name: "conformance.chat.config", Version: 1, Data: []byte(`{"k":"v"}`)},
		Revision:      rev,
	}
}

// mustTx runs fn in tenantA and fails the test on error.
func (e *env) mustTx(t *testing.T, fn func(store.Tx) error) {
	t.Helper()
	if err := e.s.Transact(e.ctx, tenantA, fn); err != nil {
		t.Fatalf("Transact: %v", err)
	}
}

// mustView runs fn in tenantA and fails the test on error.
func (e *env) mustView(t *testing.T, fn func(store.Reader) error) {
	t.Helper()
	if err := e.s.View(e.ctx, tenantA, fn); err != nil {
		t.Fatalf("View: %v", err)
	}
}

// txErr runs fn in tenantA and returns the error fn's last store call
// reported, or Transact's own error.
func (e *env) txErr(fn func(store.Tx) error) error {
	return e.s.Transact(e.ctx, tenantA, fn)
}

// retryTx runs fn in tenantA, retrying only model.Transient serialization
// failures, as the engine would for a side-effect-free unit.
func (e *env) retryTx(fn func(store.Tx) error) error {
	var err error
	for range 200 {
		if err = e.s.Transact(e.ctx, tenantA, fn); !model.IsCode(err, model.Transient) {
			return err
		}
	}
	return err
}

// wantCode requires err to be a *model.Error with code. A call that sits in
// a loop or a table passes the input it is checking, so a failure names the
// input rather than the one line every input shares.
func wantCode(t *testing.T, err error, code model.ErrorCode, input ...any) {
	t.Helper()
	var me *model.Error
	if !errors.As(err, &me) {
		t.Fatalf("%serror = %v (%T), want *model.Error with code %q", inputPrefix(input), err, err, code)
	}
	if me.Code != code {
		t.Fatalf("%scode = %q (%q), want %q", inputPrefix(input), me.Code, me.SafeMessage, code)
	}
}

// page is a first-page request.
func page(limit int) model.PageRequest { return model.PageRequest{Limit: limit} }

func list(limit int) model.ListRequest { return model.ListRequest{Page: page(limit)} }
