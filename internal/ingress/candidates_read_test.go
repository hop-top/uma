package ingress_test

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// A selection reads its candidate group, except where the group is empty by
// construction: the sender's identity was created by the same unit. These
// tests count the reads through a recording store and compare each skipped
// selection with the same selection over an identity found stored.

// readLog records the candidate groups units read.
type readLog struct {
	mu   sync.Mutex
	keys []store.SelectionKey
}

func (l *readLog) add(k store.SelectionKey) {
	l.mu.Lock()
	l.keys = append(l.keys, k)
	l.mu.Unlock()
}

func (l *readLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.keys)
}

func (l *readLog) all() []store.SelectionKey {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.keys)
}

type readLogStore struct {
	store.Store
	log *readLog
}

func (s readLogStore) Transact(ctx context.Context, tenant model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, tenant, func(tx store.Tx) error { return fn(readLogTx{Tx: tx, log: s.log}) })
}

type readLogTx struct {
	store.Tx
	log *readLog
}

func (t readLogTx) Candidates(ctx context.Context, key store.SelectionKey, limit int) ([]model.Conversation, error) {
	t.log.add(key)
	return t.Tx.Candidates(ctx, key, limit)
}

// withReadLog records the candidate reads of the harness's units in log.
func withReadLog(log *readLog) enginetest.HarnessOption {
	return func(c *engine.Config) { c.Store = readLogStore{Store: c.Store, log: log} }
}

// seedKnown stores an identity of source with its contact and no
// conversation.
func seedKnown(h *harness, in model.InboxRef, source string) model.Identity {
	h.T.Helper()
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: in, ID: h.ID()}, Contact: model.ContactRef{Tenant: tenantA, ID: h.ID()},
		SourceID: source, Namespace: ingressNS, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	h.Tx(func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(h.Ctx, model.IdentityKey{Inbox: in, Namespace: ingressNS, SourceID: source}, id); err != nil {
			return err
		}
		return tx.PutContact(h.Ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, "")
	})
	return id
}

// outcome is what a delivery led to, without IDs.
type outcome struct {
	status       string
	mode         string
	roles        []string
	reused       bool
	reopened     bool
	participants int
	events       []string
}

// deliver accepts env and reports the conversation its message landed in
// and what the delivery changed.
func deliver(h *harness, in model.Inbox, env model.Envelope) (model.Conversation, outcome) {
	h.T.Helper()
	before := map[model.ID]model.Conversation{}
	for _, c := range h.Conversations(in.Ref) {
		before[c.Ref.ID] = c
	}
	evs := len(h.Events())
	h.MustAccept(env)
	var got model.Conversation
	h.View(func(r store.Reader) error {
		m := h.MessageByProviderID(in.Ref, env.MessageID)
		c, err := r.GetConversation(h.Ctx, m.Ref.Conversation)
		got = c
		return err
	})
	prev, reused := before[got.Ref.ID]
	o := outcome{
		status: got.Status, mode: got.Mode, reused: reused, reopened: reused && prev.Status != got.Status,
		participants: len(got.Participants), events: eventKinds(h.Events()[evs:]),
	}
	for _, p := range got.Participants {
		o.roles = append(o.roles, p.Role)
	}
	return got, o
}

// A first contact reads no candidates; a sender found stored does, and
// reads its own group. Both selections create or join the same way.
func TestFirstContactReadsNoCandidates(t *testing.T) {
	for _, lock := range []bool{true, false} {
		t.Run("lock="+map[bool]string{true: "true", false: "false"}[lock], func(t *testing.T) {
			var reads readLog
			h := newHarness(t, chatType, withReadLog(&reads))
			in := h.Configure(perContact(lock), dest)
			known := seedKnown(h, in.Ref, "known")

			_, ko := deliver(h, in, h.Inbound("e-known", "m-known", "known", dest))
			if got := reads.all(); len(got) != 1 || got[0] != ingress.ContactGroup(known.Ref) {
				t.Fatalf("a known sender's selection read %+v, want its own group", got)
			}
			conv, fo := deliver(h, in, h.Inbound("e-new", "m-new", "alice", dest))
			if n := reads.count(); n != 1 {
				t.Fatalf("a first contact's selection made %d reads in all, want it to read nothing", n)
			}
			if !reflect.DeepEqual(fo, ko) || fo.reused || fo.status != model.ConversationOpen || !slices.Equal(fo.roles, []string{ingress.RoleContact}) {
				t.Fatalf("first contact led to %+v, known sender to %+v; want the same creation", fo, ko)
			}
			if !slices.Equal(fo.events, []string{model.EventConversationChanged, model.EventMessageReceived}) {
				t.Fatalf("first contact's events %v", fo.events)
			}
			alice := h.Identities(in.Ref)[len(h.Identities(in.Ref))-1]
			if conv.Owner == nil || *conv.Owner != alice.Ref {
				t.Fatalf("conversation owner %v, want the identity the unit created", conv.Owner)
			}

			// The next message finds the identity stored: it reads its
			// group and joins the conversation.
			again, ao := deliver(h, in, h.Inbound("e-new-2", "m-new-2", "alice", dest))
			if got := reads.all(); len(got) != 2 || got[1] != ingress.ContactGroup(alice.Ref) {
				t.Fatalf("a known sender's selection read %+v, want its own group second", got)
			}
			if again.Ref != conv.Ref || !ao.reused || !slices.Equal(ao.events, []string{model.EventMessageReceived}) {
				t.Fatalf("second message in %v (%+v), want the first one's conversation %v", again.Ref, ao, conv.Ref)
			}
		})
	}
}

// snapSelector records what it is handed and creates under a fixed key, or
// joins the newest candidate that is not closed.
type snapSelector struct{ seen *[]policy.SelectionInput }

func (s snapSelector) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	*s.seen = append(*s.seen, in)
	var live []model.Conversation
	for _, c := range in.Candidates {
		if c.Status != model.ConversationClosed {
			live = append(live, c)
		}
	}
	if len(live) == 0 {
		return policy.SelectionResult{CreateKey: "snap"}, nil
	}
	ref := live[len(live)-1].Ref
	return policy.SelectionResult{Existing: &ref, AddParticipant: true}, nil
}

// A custom selector over a first contact is handed what the read would have
// returned: an empty, non-nil candidate list, as for an identity found
// stored with no conversation, and nothing else differs.
func TestFirstContactSelectorSnapshot(t *testing.T) {
	var seen []policy.SelectionInput
	var reads readLog
	h := newHarness(t, chatType, withReadLog(&reads), withSelector("snap", snapSelector{&seen}))
	lock := false
	in := h.Configure(model.Selection{
		Mode: model.ModePerContact, LockToSingle: &lock, LifecycleRules: model.RuleSetContactLifecycleV1, Policy: "snap",
	}, dest)
	seedKnown(h, in.Ref, "known")

	_, ko := deliver(h, in, h.Inbound("e-known", "m-known", "known", dest))
	if reads.count() != 1 {
		t.Fatalf("the known sender's selection made %d reads, want 1", reads.count())
	}
	_, fo := deliver(h, in, h.Inbound("e-new", "m-new", "alice", dest))
	if reads.count() != 1 {
		t.Fatalf("the first contact's selection read %v", reads.all()[1:])
	}
	if len(seen) != 2 {
		t.Fatalf("selector called %d times, want 2", len(seen))
	}
	known, first := seen[0], seen[1]
	if known.Candidates == nil || len(known.Candidates) != 0 {
		t.Fatalf("known sender's candidates = %#v, want empty and not nil", known.Candidates)
	}
	if first.Candidates == nil || len(first.Candidates) != 0 {
		t.Fatalf("first contact's candidates = %#v, want empty and not nil", first.Candidates)
	}
	if first.Event != known.Event || first.Matches != nil || known.Matches != nil || first.Operation != known.Operation ||
		!reflect.DeepEqual(first.Inbox, known.Inbox) || first.Sender == nil || first.Sender.Kind != known.Sender.Kind {
		t.Fatalf("first contact's input %+v differs from the known sender's %+v", first, known)
	}
	if !reflect.DeepEqual(fo, ko) || fo.reused {
		t.Fatalf("first contact led to %+v, known sender to %+v", fo, ko)
	}
}

// Mail threading hands the selector header matches; a first contact replying
// into a thread of another sender joins or succeeds it as a known sender
// without conversation would.
func TestFirstContactMailThreading(t *testing.T) {
	for _, status := range []string{model.ConversationOpen, model.ConversationResolved, model.ConversationClosed} {
		t.Run(status, func(t *testing.T) {
			run := func(known bool) (outcome, int, model.ConversationRef, model.Conversation) {
				var reads readLog
				h := newHarness(t, model.TypeSmtpMail, withReadLog(&reads))
				in := h.Configure(perContact(true), dest)
				env := func(event, from, msgID, inReplyTo string) model.Envelope {
					e := h.Inbound(event, "msg-"+event, from, dest)
					n := model.EmailHeaders{MessageID: msgID, InReplyTo: inReplyTo, References: inReplyTo}.Normalize(h.Channel.Ref)
					e.MessageID, e.References = n.MessageID, n.References
					return e
				}
				root, _ := deliver(h, in, env("e-root", "bob", "<root@a.example>", ""))
				setStatus(h, root, status)
				if known {
					seedKnown(h, in.Ref, "alice")
				}
				before := reads.count()
				got, o := deliver(h, in, env("e-reply", "alice", "<reply@a.example>", "<root@a.example>"))
				return o, reads.count() - before, root.Ref, got
			}
			ko, kr, _, _ := run(true)
			fo, fr, root, got := run(false)
			if kr != 1 || fr != 0 {
				t.Fatalf("selection reads: known sender %d, first contact %d; want 1 and 0", kr, fr)
			}
			if !reflect.DeepEqual(fo, ko) {
				t.Fatalf("first contact led to %+v, known sender to %+v", fo, ko)
			}
			if joined := got.Ref == root; joined != (status != model.ConversationClosed) {
				t.Fatalf("reply to a %s thread joined the root = %v", status, joined)
			}
		})
	}
}

// An interaction by a new actor selects as a message does, without reading.
func TestFirstContactInteraction(t *testing.T) {
	var reads readLog
	h := newHarness(t, chatType, withReadLog(&reads))
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Interaction("i1", "alice", dest))
	if n := reads.count(); n != 0 {
		t.Fatalf("a new actor's interaction read %d candidate groups, want none", n)
	}
	convs := h.Conversations(in.Ref)
	if len(convs) != 1 || convs[0].Owner == nil || convs[0].Status != model.ConversationOpen {
		t.Fatalf("conversations %+v, want one owned by the actor", convs)
	}
	h.MustAccept(h.Interaction("i2", "alice", dest))
	if n := reads.count(); n != 1 {
		t.Fatalf("the known actor's interaction made %d reads in all, want 1", n)
	}
	if got := h.Conversations(in.Ref); len(got) != 1 {
		t.Fatalf("conversations %d, want the first one reused", len(got))
	}
}

// Where the candidate group is not the sender's alone, a sender whose
// identity the unit creates still reads it, and joins what others started.
func TestNewSenderStillReadsSharedGroups(t *testing.T) {
	ctxRef := func(h *harness) model.ExternalRef { return h.Ref(model.RefKindContext, "ctx-1", "") }
	for _, tc := range []struct {
		name string
		sel  model.Selection
		ref  func(*harness) model.ExternalRef
	}{
		{"per_room", model.Selection{Mode: model.ModePerRoom}, func(h *harness) model.ExternalRef { return h.Ref(model.RefKindRoom, "r1", "") }},
		{"per_thread", model.Selection{Mode: model.ModePerThread}, func(h *harness) model.ExternalRef { return h.Ref(model.RefKindThread, "t1", "r1") }},
		{"per_work_item", model.Selection{Mode: model.ModePerWorkItem}, func(h *harness) model.ExternalRef { return h.Ref(model.RefKindWorkItem, "w1", "") }},
		{"per_agent_context", model.Selection{Mode: model.ModePerAgentContext}, ctxRef},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reads readLog
			h := newHarness(t, chatType, withReadLog(&reads))
			in := h.Configure(tc.sel, dest)
			ref := tc.ref(h)
			first, _ := deliver(h, in, h.Inbound("e1", "m1", "alice", dest, ref))
			if n := reads.count(); n != 1 {
				t.Fatalf("alice's first message made %d reads, want 1: its group is not hers alone", n)
			}
			// bob's identity is created by his unit; the group holds alice's
			// conversation, which he joins.
			second, o := deliver(h, in, h.Inbound("e2", "m2", "bob", dest, ref))
			if n := reads.count(); n != 2 {
				t.Fatalf("bob's first message made %d reads in all, want 2", n)
			}
			if second.Ref != first.Ref || !o.reused {
				t.Fatalf("bob's message in %v, want alice's conversation %v", second.Ref, first.Ref)
			}
			if got := h.Conversations(in.Ref); len(got) != 1 || len(got[0].Participants) < 1 {
				t.Fatalf("conversations %+v, want one", got)
			}
		})
	}
}

// Concurrent first messages of one new sender: one unit creates the
// identity, the others find it stored and read its group; all land in one
// conversation.
func TestConcurrentFirstContactsOneConversation(t *testing.T) {
	var reads readLog
	h := newHarness(t, chatType, withReadLog(&reads))
	in := h.Configure(perContact(true), dest)
	const n = 6
	start := make(chan struct{})
	errs := make(chan error, n)
	for i := range n {
		env := h.Inbound("e"+string(rune('a'+i)), "m"+string(rune('a'+i)), "alice", dest)
		go func() {
			<-start
			_, err := h.Accept(env)
			errs <- err
		}()
	}
	close(start)
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}
	if got := h.Conversations(in.Ref); len(got) != 1 {
		t.Fatalf("%d conversations, want 1", len(got))
	}
	if got := h.Identities(in.Ref); len(got) != 1 {
		t.Fatalf("%d identities, want 1", len(got))
	}
	if msgs := h.Messages(in.Ref); len(msgs) != n {
		t.Fatalf("%d messages, want %d", len(msgs), n)
	}
	if got := reads.count(); got < n-1 {
		t.Fatalf("%d reads for %d racing first contacts, want one from each unit that found the identity", got, n)
	}
}
