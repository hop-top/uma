package services_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// A start whose recipient the adapter normalizes to another spelling than
// the caller typed may reuse an identity stored under the typed spelling, by
// one exact point read, when the adapter opts in with
// adapter.LegacyRecipientReuser and the normalized spelling has none.

// legacyFactory is the rig's normalizing adapter with adapter.LegacyRecipientReuser
// added: reuse decides each recipient, and asked records what core asked about.
type legacyFactory struct {
	*normalizingFactory
	reuse func(model.ExternalRef) bool

	mu          sync.Mutex
	askedLegacy []model.ExternalRef
}

func (f *legacyFactory) ReuseLegacyRecipient(r model.ExternalRef) bool {
	f.mu.Lock()
	f.askedLegacy = append(f.askedLegacy, r)
	f.mu.Unlock()
	return f.reuse(r)
}

func (f *legacyFactory) legacyAsked() []model.ExternalRef {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]model.ExternalRef(nil), f.askedLegacy...)
}

var _ adapter.LegacyRecipientReuser = (*legacyFactory)(nil)

func always(model.ExternalRef) bool { return true }

// withLegacyReuse swaps the rig's adapter for one that lowercases the
// recipient and opts in to legacy reuse as reuse decides.
func withLegacyReuse(reuse func(model.ExternalRef) bool) (enginetest.RigOption, **legacyFactory) {
	var lf *legacyFactory
	return func(c *engine.Config, f *outFactory) {
		lf = &legacyFactory{normalizingFactory: &normalizingFactory{outFactory: f, fn: lowerID}, reuse: reuse}
		c.Adapters = []adapter.Factory{lf}
	}, &lf
}

// idReads records the identity point reads of the store's readers and units.
type idReads struct {
	mu    sync.Mutex
	view  []model.IdentityKey
	unit  []model.IdentityKey
	views int
	// lie, when set, makes a reader report an identity under the key.
	lie map[model.IdentityKey]model.Identity
}

func (l *idReads) snapshot() (view, unit []model.IdentityKey) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]model.IdentityKey(nil), l.view...), append([]model.IdentityKey(nil), l.unit...)
}

type readStore struct {
	store.Store
	log *idReads
}

func (s readStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	s.log.mu.Lock()
	s.log.views++
	s.log.mu.Unlock()
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(readReader{Reader: r, log: s.log}) })
}

func (s readStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, t, func(tx store.Tx) error { return fn(readTx{Tx: tx, log: s.log}) })
}

type readReader struct {
	store.Reader
	log *idReads
}

func (r readReader) Identity(ctx context.Context, k model.IdentityKey) (model.Identity, error) {
	r.log.mu.Lock()
	r.log.view = append(r.log.view, k)
	lie, ok := r.log.lie[k]
	r.log.mu.Unlock()
	if ok {
		return lie, nil
	}
	return r.Reader.Identity(ctx, k)
}

type readTx struct {
	store.Tx
	log *idReads
}

func (t readTx) Identity(ctx context.Context, k model.IdentityKey) (model.Identity, error) {
	t.log.mu.Lock()
	t.log.unit = append(t.log.unit, k)
	t.log.mu.Unlock()
	return t.Tx.Identity(ctx, k)
}

// recordReads wraps the rig's store in a recording one.
func recordReads(r *rig) *idReads {
	l := &idReads{}
	r.Store.Set(readStore{Store: r.Store.Inner(), log: l})
	return l
}

func (l *idReads) viewKeys(t *testing.T) []string {
	t.Helper()
	v, _ := l.snapshot()
	out := make([]string, len(v))
	for i, k := range v {
		out[i] = k.SourceID
	}
	return out
}

func (l *idReads) requireNoUnitReads(t *testing.T) {
	t.Helper()
	if _, u := l.snapshot(); len(u) != 0 {
		t.Fatalf("the claiming unit read identities %v, want none", u)
	}
}

// seedLegacy stores what a start before the adapter normalized left behind:
// the identity claimed as typed, its provisional contact and a conversation
// routed to the typed spelling.
func seedLegacy(t *testing.T, r *rig, in model.InboxRef, typed string) (model.Identity, model.Conversation) {
	t.Helper()
	n := len(claimed(t, r, in)) + 1
	id := model.Identity{
		Ref:      model.IdentityRef{Inbox: in, ID: model.ID(fmt.Sprintf("00000009-0000-7000-8000-%012d", n))},
		Contact:  model.ContactRef{Tenant: tenantA, ID: model.ID(fmt.Sprintf("0000000a-0000-7000-8000-%012d", n))},
		SourceID: typed, Namespace: routeNSpace, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	owner := id.Ref
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: in, ID: model.ID(fmt.Sprintf("0000000b-0000-7000-8000-%012d", n))},
		Mode: model.ModePerContact, Status: model.ConversationOpen, Owner: &owner,
		ReplyRoute: model.ExternalRef{Channel: r.Ch.Ref, Namespace: routeNSpace, Kind: "user", ID: typed},
		CreatedAt:  r.Clock.Now(), Revision: "c1",
		Participants: []model.Participant{{Identity: id.Ref, Role: "contact"}},
	}
	err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(r.Ctx, model.IdentityKey{Inbox: in, Namespace: routeNSpace, SourceID: typed}, id); err != nil {
			return err
		}
		if err := tx.PutContact(r.Ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, ""); err != nil {
			return err
		}
		_, _, err := tx.ClaimConversation(r.Ctx, store.ConversationClaim{
			Selection: store.SelectionKey{Inbox: in, Mode: model.ModePerContact, Key: "identity:" + string(id.Ref.ID)},
			CreateKey: "legacy", Conversation: conv,
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, conv
}

// seedLegacyIdentity is seedLegacy without a conversation.
func seedLegacyIdentity(t *testing.T, r *rig, in model.InboxRef, typed string) model.Identity {
	t.Helper()
	n := len(claimed(t, r, in)) + 1
	id := model.Identity{
		Ref:      model.IdentityRef{Inbox: in, ID: model.ID(fmt.Sprintf("00000009-0000-7000-8000-%012d", n))},
		Contact:  model.ContactRef{Tenant: tenantA, ID: model.ID(fmt.Sprintf("0000000a-0000-7000-8000-%012d", n))},
		SourceID: typed, Namespace: routeNSpace, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(r.Ctx, model.IdentityKey{Inbox: in, Namespace: routeNSpace, SourceID: typed}, id); err != nil {
			return err
		}
		return tx.PutContact(r.Ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, "")
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func sources(ids []model.Identity) map[string]bool {
	out := map[string]bool{}
	for _, i := range ids {
		out[i.SourceID] = true
	}
	return out
}

func landedIn(t *testing.T, acc model.Acceptance) model.ConversationRef {
	t.Helper()
	if acc.Operation.ResultMessage == nil {
		t.Fatalf("operation %+v names no result message", acc.Operation)
	}
	return acc.Operation.ResultMessage.Conversation
}

// A start typed with the spelling a legacy identity was claimed under lands
// on that identity and its conversation: no second identity, no second
// conversation. The provider call goes to the route the conversation
// stores, as every send on it did before.
func TestStartTypedAsTheLegacyIdentityReusesIt(t *testing.T) {
	opt, _ := withLegacyReuse(always)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	legacy, conv := seedLegacy(t, r, in, "Alice@Example.com")

	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := landedIn(t, acc); got != conv.Ref {
		t.Fatalf("start landed in %v, want the legacy conversation %v", got, conv.Ref)
	}
	if ids := claimed(t, r, in); len(ids) != 1 || ids[0].Ref != legacy.Ref {
		t.Fatalf("identities %+v, want only the legacy one", ids)
	}
	if n := len(conversationsOf(t, r, in)); n != 1 {
		t.Fatalf("conversations = %d, want the legacy one", n)
	}
	r.Step("w")
	enginetest.RequireState(t, r.Op(acc.Operation.Ref), model.ProviderAccepted, "")
	if sends := r.F.ProviderSends(); len(sends) != 1 || sends[0].Route.ID != "Alice@Example.com" {
		t.Fatalf("provider sends %+v, want one to the conversation's stored route", sends)
	}
}

// A legacy identity with no conversation to join gets a new one, routed to
// the normalized spelling the adapter can send to.
func TestStartTypedAsTheLegacyIdentityOpensItsConversationOnTheNormalizedRoute(t *testing.T) {
	opt, _ := withLegacyReuse(always)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	legacy := seedLegacyIdentity(t, r, in, "Alice@Example.com")

	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if ids := claimed(t, r, in); len(ids) != 1 || ids[0].Ref != legacy.Ref {
		t.Fatalf("identities %+v, want only the legacy one", ids)
	}
	convs := conversationsOf(t, r, in)
	if len(convs) != 1 || convs[0].Ref != landedIn(t, acc) {
		t.Fatalf("conversations %+v, want one, the start's", convs)
	}
	if convs[0].Owner == nil || *convs[0].Owner != legacy.Ref || convs[0].ReplyRoute.ID != "alice@example.com" {
		t.Fatalf("conversation %+v, want owned by the legacy identity and routed to alice@example.com", convs[0])
	}
	r.Step("w")
	if sends := r.F.ProviderSends(); len(sends) != 1 || sends[0].Route.ID != "alice@example.com" {
		t.Fatalf("provider sends %+v, want one to the normalized route", sends)
	}
}

// The lookup is exact: a start typed in any other case than the legacy
// identity's behaves as it does without one, a fresh normalized identity.
// This is the documented partial limit.
func TestStartTypedInAnotherCaseThanTheLegacyIdentityIsFresh(t *testing.T) {
	for _, typed := range []string{"alice@example.com", "ALICE@example.com", "alice@EXAMPLE.COM"} {
		t.Run(typed, func(t *testing.T) {
			opt, _ := withLegacyReuse(always)
			r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
			legacy, conv := seedLegacy(t, r, in, "Alice@Example.com")
			acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, typed)))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := landedIn(t, acc); got == conv.Ref {
				t.Fatalf("start typed %q landed in the legacy conversation", typed)
			}
			got := sources(claimed(t, r, in))
			if len(got) != 2 || !got["Alice@Example.com"] || !got["alice@example.com"] {
				t.Fatalf("identities %v, want the legacy one and a fresh alice@example.com", got)
			}
			for _, i := range claimed(t, r, in) {
				if i.SourceID == "alice@example.com" && i.Ref == legacy.Ref {
					t.Fatal("the normalized identity is the legacy one")
				}
			}
		})
	}
}

// With no legacy identity a start behaves exactly as without the opt-in.
func TestStartWithoutLegacyIdentityBehavesAsToday(t *testing.T) {
	opt, _ := withLegacyReuse(always)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Bob@Example.com")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ids := claimed(t, r, in)
	if len(ids) != 1 || ids[0].SourceID != "bob@example.com" {
		t.Fatalf("identities %+v, want one claimed as bob@example.com", ids)
	}
	convs := conversationsOf(t, r, in)
	if len(convs) != 1 || convs[0].ReplyRoute.ID != "bob@example.com" || convs[0].Ref != landedIn(t, acc) {
		t.Fatalf("conversations %+v, want one routed to bob@example.com", convs)
	}
}

// When the normalized spelling has an identity too, it wins: no ambiguity
// error, and the legacy identity is left alone.
func TestStartPrefersTheNormalizedIdentityOverTheLegacyOne(t *testing.T) {
	opt, _ := withLegacyReuse(always)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	_, legacyConv := seedLegacy(t, r, in, "Alice@Example.com")
	_, normConv := seedLegacy(t, r, in, "alice@example.com")
	log := recordReads(r)

	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := landedIn(t, acc); got != normConv.Ref {
		t.Fatalf("start landed in %v, want the normalized identity's conversation %v (legacy %v)", got, normConv.Ref, legacyConv.Ref)
	}
	if ids := claimed(t, r, in); len(ids) != 2 {
		t.Fatalf("identities %+v, want the two seeded", ids)
	}
	if keys := log.viewKeys(t); len(keys) != 1 || keys[0] != "alice@example.com" {
		t.Fatalf("pre-unit reads %v, want only the normalized key", keys)
	}
}

// The start's fingerprint stays keyed on the normalized recipient: the same
// operation key under any spelling, the legacy one included, is the one
// operation, replayed with nothing second written.
func TestStartReplayWithALegacyIdentityIsTheSameOperation(t *testing.T) {
	opt, _ := withLegacyReuse(always)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	_, conv := seedLegacy(t, r, in, "Alice@Example.com")
	first, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, id := range []string{"Alice@Example.com", "alice@example.com", "ALICE@EXAMPLE.COM"} {
		again, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, id)))
		if err != nil {
			t.Fatalf("replay as %q: %v", id, err)
		}
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay as %q = %+v, want the first operation replayed", id, again)
		}
	}
	if n := len(r.Claim("w")); n != 1 {
		t.Fatalf("work items = %d, want the first start's one", n)
	}
	if got := landedIn(t, first); got != conv.Ref {
		t.Fatalf("start landed in %v, want the legacy conversation", got)
	}
	if n := len(claimed(t, r, in)); n != 1 {
		t.Fatalf("identities = %d, want 1", n)
	}
}

// Starts typed with the legacy spelling, concurrently, converge on the one
// legacy identity and one conversation.
func TestConcurrentStartsTypedAsTheLegacyIdentityConverge(t *testing.T) {
	for name, withConv := range map[string]bool{"conversation": true, "no conversation": false} {
		t.Run(name, func(t *testing.T) {
			opt, _ := withLegacyReuse(always)
			r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
			var legacy model.Identity
			if withConv {
				legacy, _ = seedLegacy(t, r, in, "Alice@Example.com")
			} else {
				legacy = seedLegacyIdentity(t, r, in, "Alice@Example.com")
			}
			const n = 8
			var wg sync.WaitGroup
			refs := make([]model.ConversationRef, n)
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					acc, err := r.C.Conversations().Start(r.Ctx, startReq(model.OperationID(fmt.Sprintf("op-%d", i)), in, recipient(r, "Alice@Example.com")))
					errs[i] = err
					if err == nil && acc.Operation.ResultMessage != nil {
						refs[i] = acc.Operation.ResultMessage.Conversation
					}
				}()
			}
			wg.Wait()
			for i, err := range errs {
				if err != nil {
					t.Fatalf("start %d: %v", i, err)
				}
				if refs[i] != refs[0] {
					t.Fatalf("start %d landed in %v, start 0 in %v", i, refs[i], refs[0])
				}
			}
			if ids := claimed(t, r, in); len(ids) != 1 || ids[0].Ref != legacy.Ref {
				t.Fatalf("identities %+v, want only the legacy one", ids)
			}
			if convs := conversationsOf(t, r, in); len(convs) != 1 {
				t.Fatalf("conversations = %d, want 1", len(convs))
			}
		})
	}
}

// An adapter that does not opt in keeps today's behaviour: its normalized
// spelling is claimed fresh whatever legacy identity exists, and nothing is
// read before the claiming unit.
func TestStartWithoutTheOptInIgnoresLegacyIdentities(t *testing.T) {
	opt, _ := withNormalizer(lowerID)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	_, conv := seedLegacy(t, r, in, "Alice@Example.com")
	log := recordReads(r)
	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := landedIn(t, acc); got == conv.Ref {
		t.Fatal("a normalizing adapter without the opt-in landed in the legacy conversation")
	}
	got := sources(claimed(t, r, in))
	if len(got) != 2 || !got["alice@example.com"] {
		t.Fatalf("identities %v, want the legacy one and a fresh alice@example.com", got)
	}
	if v, u := log.snapshot(); len(v)+len(u) != 0 {
		t.Fatalf("identity reads %v %v, want none", v, u)
	}
}

// An adapter that opts in and declines a recipient reads nothing for it.
func TestStartLegacyReuseDeclinedReadsNothing(t *testing.T) {
	opt, lf := withLegacyReuse(func(model.ExternalRef) bool { return false })
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	seedLegacy(t, r, in, "Alice@Example.com")
	log := recordReads(r)
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com"))); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if asked := (*lf).legacyAsked(); len(asked) != 1 || asked[0].ID != "Alice@Example.com" {
		t.Fatalf("adapter asked about %+v, want the recipient as typed, once", asked)
	}
	if v, u := log.snapshot(); len(v)+len(u) != 0 {
		t.Fatalf("identity reads %v %v, want none", v, u)
	}
	if n := len(claimed(t, r, in)); n != 2 {
		t.Fatalf("identities = %d, want the legacy one and a fresh one", n)
	}
}

// The reads are bounded and kept out of the claiming unit. Typed equal to
// normalized costs nothing: not a read, not a snapshot, and the adapter is
// not asked.
func TestStartLegacyLookupReadCounts(t *testing.T) {
	type tc struct {
		name     string
		typed    string
		seed     []string
		wantKeys []string
		wantView int
	}
	for _, tt := range []tc{
		{"typed equals normalized, no identity", "alice@example.com", nil, nil, 1},
		{"typed equals normalized, identity stored", "alice@example.com", []string{"alice@example.com"}, nil, 1},
		{"normalized stored", "Alice@Example.com", []string{"alice@example.com"}, []string{"alice@example.com"}, 2},
		{"nothing stored", "Alice@Example.com", nil, []string{"alice@example.com", "Alice@Example.com"}, 2},
		{"legacy stored", "Alice@Example.com", []string{"Alice@Example.com"}, []string{"alice@example.com", "Alice@Example.com"}, 2},
		{"another case stored", "Alice@Example.com", []string{"ALICE@Example.com"}, []string{"alice@example.com", "Alice@Example.com"}, 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opt, lf := withLegacyReuse(always)
			r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
			for _, s := range tt.seed {
				seedLegacy(t, r, in, s)
			}
			log := recordReads(r)
			if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, tt.typed))); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if got := log.viewKeys(t); fmt.Sprint(got) != fmt.Sprint(tt.wantKeys) {
				t.Fatalf("pre-unit identity reads %v, want %v", got, tt.wantKeys)
			}
			log.requireNoUnitReads(t)
			log.mu.Lock()
			views := log.views
			log.mu.Unlock()
			if views != tt.wantView {
				t.Fatalf("read units = %d, want %d", views, tt.wantView)
			}
			asked := (*lf).legacyAsked()
			if tt.typed == "alice@example.com" && len(asked) != 0 {
				t.Fatalf("adapter asked about %+v although nothing changed", asked)
			}
		})
	}
}

// A probe that saw an identity the claiming unit no longer finds, because
// it was removed in between, must not leave a new identity under the typed
// spelling: the start is refused for a retry and nothing is written.
func TestStartLegacyHintGoneBeforeTheUnitWritesNothing(t *testing.T) {
	opt, _ := withLegacyReuse(always)
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, opt)
	log := recordReads(r)
	typed := model.IdentityKey{Inbox: in, Namespace: routeNSpace, SourceID: "Alice@Example.com"}
	log.lie = map[model.IdentityKey]model.Identity{typed: {SourceID: typed.SourceID, Namespace: typed.Namespace}}

	_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com")))
	requireCode(t, err, model.Conflict, "recipient identity")
	if n := len(r.Claim("w")); n != 0 {
		t.Fatalf("a refused start queued %d work items", n)
	}
	log.lie = nil
	if n := len(claimed(t, r, in)) + len(conversationsOf(t, r, in)); n != 0 {
		t.Fatalf("a refused start wrote %d identities and conversations", n)
	}

	// The operation key stays free: once the hint is true or absent it starts.
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "Alice@Example.com"))); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := sources(claimed(t, r, in)); len(got) != 1 || !got["alice@example.com"] {
		t.Fatalf("identities %v, want only alice@example.com", got)
	}
}

// Scoped (non per-contact) starts claim no identity: nothing is read and
// the adapter is not asked.
func TestStartLegacyReuseIsPerContactOnly(t *testing.T) {
	opt, lf := withLegacyReuse(always)
	r, in := newStartRig(t, model.Selection{Mode: model.ModePerRoom}, enginetest.WithStart, opt)
	log := recordReads(r)
	to := recipient(r, "ROOM-1")
	to.Kind = model.RefKindRoom
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, to)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if v, u := log.snapshot(); len(v)+len(u) != 0 {
		t.Fatalf("identity reads %v %v, want none", v, u)
	}
	if asked := (*lf).legacyAsked(); len(asked) != 0 {
		t.Fatalf("adapter asked about %+v, want nothing", asked)
	}
}
