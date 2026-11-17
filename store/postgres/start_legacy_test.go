package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"hop.top/uma"
	"hop.top/uma/adapter"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// The legacy-recipient lookup of Conversations.Start on PostgreSQL: a start
// the adapter normalizes to another spelling than typed reuses an identity
// stored under the typed spelling, by exact point reads before the claiming
// unit. The memory store's counterparts are in internal/services.

// legacyText is the contention text channel with adapter.RecipientNormalizer
// (lowercase).
type legacyText struct{ *textChannel }

func (legacyText) NormalizeRecipient(r model.ExternalRef) (model.ExternalRef, error) {
	r.ID = strings.ToLower(r.ID)
	return r, nil
}

// optIn adds the opt-in to legacyText.
type optIn struct{ legacyText }

func (optIn) ReuseLegacyRecipient(model.ExternalRef) bool { return true }

var (
	_ adapter.RecipientNormalizer   = legacyText{}
	_ adapter.LegacyRecipientReuser = optIn{}
)

// identityReads records the identity point reads of readers and units.
type identityReads struct {
	store.Store
	mu         sync.Mutex
	view, unit []string
}

func (s *identityReads) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(readerLog{Reader: r, s: s}) })
}

func (s *identityReads) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, t, func(tx store.Tx) error { return fn(txLog{Tx: tx, s: s}) })
}

type readerLog struct {
	store.Reader
	s *identityReads
}

func (r readerLog) Identity(ctx context.Context, k model.IdentityKey) (model.Identity, error) {
	r.s.mu.Lock()
	r.s.view = append(r.s.view, k.SourceID)
	r.s.mu.Unlock()
	return r.Reader.Identity(ctx, k)
}

type txLog struct {
	store.Tx
	s *identityReads
}

func (t txLog) Identity(ctx context.Context, k model.IdentityKey) (model.Identity, error) {
	t.s.mu.Lock()
	t.s.unit = append(t.s.unit, k.SourceID)
	t.s.mu.Unlock()
	return t.Tx.Identity(ctx, k)
}

func (s *identityReads) reads() (view, unit []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.view...), append([]string(nil), s.unit...)
}

func (s *identityReads) reset() {
	s.mu.Lock()
	s.view, s.unit = nil, nil
	s.mu.Unlock()
}

// legacyRig is a text rig on PostgreSQL whose adapter normalizes recipients
// to lowercase, with the opt-in when optedIn, and records identity reads.
type legacyRig struct {
	*textRig
	log *identityReads
	n   int
}

func newLegacyRig(t *testing.T, optedIn bool) *legacyRig {
	t.Helper()
	l := &legacyRig{log: &identityReads{}}
	l.textRig = textRigWith(t, newDatabase(t, true).Config(), int32(2*contentionCallers), func(c *uma.Config, text *textChannel) {
		var f adapter.Factory = legacyText{textChannel: text}
		if optedIn {
			f = optIn{legacyText{textChannel: text}}
		}
		c.Adapters = []adapter.Factory{f}
		l.log.Store = c.Store
		c.Store = l.log
	})
	return l
}

// seed stores what a start before the adapter normalized left behind: the
// identity claimed as typed, its provisional contact and, when asked, an open
// conversation routed to the typed spelling.
func (l *legacyRig) seed(t *testing.T, typed string, withConversation bool) (model.Identity, model.Conversation) {
	t.Helper()
	ctx := t.Context()
	l.n++
	id := model.Identity{
		Ref:      model.IdentityRef{Inbox: l.inbox, ID: benchID(0x91, l.n)},
		Contact:  model.ContactRef{Tenant: contentionTenant, ID: benchID(0x92, l.n)},
		SourceID: typed, Namespace: textContentionNS, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	owner := id.Ref
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: l.inbox, ID: benchID(0x93, l.n)},
		Mode: model.ModePerContact, Status: model.ConversationOpen, Owner: &owner,
		ReplyRoute: l.number(typed), CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Revision: "c1",
		Participants: []model.Participant{{Identity: id.Ref, Role: "contact"}},
	}
	err := l.uc.Store.Transact(ctx, contentionTenant, func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(ctx, model.IdentityKey{Inbox: l.inbox, Namespace: id.Namespace, SourceID: typed}, id); err != nil {
			return err
		}
		if err := tx.PutContact(ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, ""); err != nil {
			return err
		}
		if !withConversation {
			return nil
		}
		_, _, err := tx.ClaimConversation(ctx, store.ConversationClaim{
			Selection: store.SelectionKey{Inbox: l.inbox, Mode: model.ModePerContact, Key: "identity:" + string(id.Ref.ID)},
			CreateKey: "legacy", Conversation: conv,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed %q: %v", typed, err)
	}
	return id, conv
}

// startTo starts a conversation with typed under op and returns the
// conversation its message landed in.
func (l *legacyRig) startTo(t *testing.T, op, typed string) (model.Acceptance, model.ConversationRef) {
	t.Helper()
	acc, err := l.admin.Conversations().Start(t.Context(), model.StartConversation{
		Command: contentionCommand(op), Inbox: l.inbox, Recipient: l.number(typed),
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	})
	if err != nil {
		t.Fatalf("Start %s to %q: %v", op, typed, err)
	}
	if acc.Operation.ResultMessage == nil {
		t.Fatalf("Start %s: no result message in %+v", op, acc.Operation)
	}
	return acc, acc.Operation.ResultMessage.Conversation
}

func (l *legacyRig) identities(t *testing.T) map[string]model.IdentityRef {
	t.Helper()
	p, err := l.admin.Contacts().Identities(t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "list"}, Filter: model.Filter{Inboxes: []model.InboxRef{l.inbox}}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.IdentityRef{}
	for _, i := range p.Items {
		out[i.SourceID] = i.Ref
	}
	if len(out) != len(p.Items) {
		t.Fatalf("identities %+v repeat a source", p.Items)
	}
	return out
}

func (l *legacyRig) conversations(t *testing.T) []model.Conversation {
	t.Helper()
	p, err := l.admin.Conversations().List(t.Context(), model.ListRequest{
		Call: model.Call{Correlation: "list"}, Filter: model.Filter{Inboxes: []model.InboxRef{l.inbox}}, Page: model.PageRequest{Limit: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p.Items
}

func TestStartTypedAsALegacyIdentityReusesItOnPostgres(t *testing.T) {
	l := newLegacyRig(t, true)
	legacy, conv := l.seed(t, "Alice@Example.com", true)
	_, got := l.startTo(t, "op-1", "Alice@Example.com")
	if got != conv.Ref {
		t.Fatalf("start landed in %v, want the legacy conversation %v", got, conv.Ref)
	}
	if ids := l.identities(t); len(ids) != 1 || ids["Alice@Example.com"] != legacy.Ref {
		t.Fatalf("identities %v, want only the legacy one", ids)
	}
	if convs := l.conversations(t); len(convs) != 1 {
		t.Fatalf("conversations = %d, want the legacy one", len(convs))
	}
}

func TestStartTypedAsALegacyIdentityWithoutConversationOnPostgres(t *testing.T) {
	l := newLegacyRig(t, true)
	legacy, _ := l.seed(t, "Alice@Example.com", false)
	_, got := l.startTo(t, "op-1", "Alice@Example.com")
	if ids := l.identities(t); len(ids) != 1 || ids["Alice@Example.com"] != legacy.Ref {
		t.Fatalf("identities %v, want only the legacy one", ids)
	}
	convs := l.conversations(t)
	if len(convs) != 1 || convs[0].Ref != got || convs[0].Owner == nil || *convs[0].Owner != legacy.Ref || convs[0].ReplyRoute.ID != "alice@example.com" {
		t.Fatalf("conversations %+v, want one owned by the legacy identity, routed to alice@example.com", convs)
	}
}

func TestStartInAnotherCaseThanTheLegacyIdentityIsFreshOnPostgres(t *testing.T) {
	l := newLegacyRig(t, true)
	_, conv := l.seed(t, "Alice@Example.com", true)
	_, got := l.startTo(t, "op-1", "ALICE@example.com")
	if got == conv.Ref {
		t.Fatal("a start typed in another case landed in the legacy conversation")
	}
	ids := l.identities(t)
	if _, ok := ids["Alice@Example.com"]; !ok || len(ids) != 2 {
		t.Fatalf("identities %v, want the legacy one and a fresh alice@example.com", ids)
	}
	if _, ok := ids["alice@example.com"]; !ok {
		t.Fatalf("identities %v, want a fresh alice@example.com", ids)
	}
}

func TestStartWithoutALegacyIdentityBehavesAsTodayOnPostgres(t *testing.T) {
	l := newLegacyRig(t, true)
	_, got := l.startTo(t, "op-1", "Bob@Example.com")
	ids := l.identities(t)
	if _, ok := ids["bob@example.com"]; !ok || len(ids) != 1 {
		t.Fatalf("identities %v, want only bob@example.com", ids)
	}
	if convs := l.conversations(t); len(convs) != 1 || convs[0].Ref != got || convs[0].ReplyRoute.ID != "bob@example.com" {
		t.Fatalf("conversations %+v, want one routed to bob@example.com", convs)
	}
}

func TestStartPrefersTheNormalizedIdentityOnPostgres(t *testing.T) {
	l := newLegacyRig(t, true)
	l.seed(t, "Alice@Example.com", true)
	_, normConv := l.seed(t, "alice@example.com", true)
	_, got := l.startTo(t, "op-1", "Alice@Example.com")
	if got != normConv.Ref {
		t.Fatalf("start landed in %v, want the normalized identity's conversation %v", got, normConv.Ref)
	}
	if v, _ := l.log.reads(); len(v) != 1 || v[0] != "alice@example.com" {
		t.Fatalf("pre-unit identity reads %v, want only the normalized key", v)
	}
}

func TestStartReplayWithALegacyIdentityOnPostgres(t *testing.T) {
	l := newLegacyRig(t, true)
	_, conv := l.seed(t, "Alice@Example.com", true)
	first, got := l.startTo(t, "op-1", "Alice@Example.com")
	if got != conv.Ref {
		t.Fatalf("start landed in %v, want the legacy conversation", got)
	}
	for _, typed := range []string{"Alice@Example.com", "alice@example.com", "ALICE@EXAMPLE.COM"} {
		again, _ := l.startTo(t, "op-1", typed)
		if !again.Replayed || again.Operation.Ref != first.Operation.Ref {
			t.Fatalf("replay as %q = %+v, want the first operation replayed", typed, again)
		}
	}
	if ids := l.identities(t); len(ids) != 1 {
		t.Fatalf("identities %v, want 1", ids)
	}
}

// Concurrent starts typed with the legacy spelling converge on the one
// legacy identity and one conversation, whether it had one or not.
func TestConcurrentStartsTypedAsALegacyIdentityConvergeOnPostgres(t *testing.T) {
	for name, withConv := range map[string]bool{"with conversation": true, "without conversation": false} {
		t.Run(name, func(t *testing.T) {
			l := newLegacyRig(t, true)
			legacy, _ := l.seed(t, "Alice@Example.com", withConv)
			const n = 2 * contentionCallers
			var wg sync.WaitGroup
			refs := make([]model.ConversationRef, n)
			errs := make([]error, n)
			for i := range n {
				wg.Add(1)
				go func() {
					defer wg.Done()
					acc, err := l.admin.Conversations().Start(t.Context(), model.StartConversation{
						Command: contentionCommand(fmt.Sprintf("op-%d", i)), Inbox: l.inbox, Recipient: l.number("Alice@Example.com"),
						Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
					})
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
			if ids := l.identities(t); len(ids) != 1 || ids["Alice@Example.com"] != legacy.Ref {
				t.Fatalf("identities %v, want only the legacy one", ids)
			}
			if convs := l.conversations(t); len(convs) != 1 {
				t.Fatalf("conversations = %d, want 1", len(convs))
			}
		})
	}
}

// An adapter that normalizes without the opt-in keeps today's behaviour.
func TestStartWithoutTheOptInIgnoresLegacyIdentitiesOnPostgres(t *testing.T) {
	l := newLegacyRig(t, false)
	_, conv := l.seed(t, "Alice@Example.com", true)
	l.log.reset()
	_, got := l.startTo(t, "op-1", "Alice@Example.com")
	if got == conv.Ref {
		t.Fatal("an adapter without the opt-in landed in the legacy conversation")
	}
	if ids := l.identities(t); len(ids) != 2 {
		t.Fatalf("identities %v, want the legacy one and a fresh alice@example.com", ids)
	}
	if v, u := l.log.reads(); len(v)+len(u) != 0 {
		t.Fatalf("identity reads %v %v, want none", v, u)
	}
}

// The lookup is bounded and kept out of the claiming unit; typed equal to
// normalized reads nothing.
func TestStartLegacyLookupReadsOnPostgres(t *testing.T) {
	for _, tt := range []struct {
		name  string
		typed string
		seed  []string
		want  []string
	}{
		{"typed equals normalized", "alice@example.com", []string{"alice@example.com"}, nil},
		{"normalized stored", "Alice@Example.com", []string{"alice@example.com"}, []string{"alice@example.com"}},
		{"nothing stored", "Alice@Example.com", nil, []string{"alice@example.com", "Alice@Example.com"}},
		{"legacy stored", "Alice@Example.com", []string{"Alice@Example.com"}, []string{"alice@example.com", "Alice@Example.com"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			l := newLegacyRig(t, true)
			for _, s := range tt.seed {
				l.seed(t, s, true)
			}
			l.log.reset()
			l.startTo(t, "op-1", tt.typed)
			v, u := l.log.reads()
			if fmt.Sprint(v) != fmt.Sprint(tt.want) {
				t.Fatalf("pre-unit identity reads %v, want %v", v, tt.want)
			}
			if len(u) != 0 {
				t.Fatalf("the claiming unit read identities %v, want none", u)
			}
		})
	}
}
