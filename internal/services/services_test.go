package services_test

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/services"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
)

// rulePolicy decides with decide (nil allows) and records every request.
type rulePolicy struct {
	mu     sync.Mutex
	decide func(policy.Authorization) bool
	checks []policy.Authorization
}

func (p *rulePolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, a)
	if p.decide != nil && !p.decide(a) {
		return policy.Decision{}, nil
	}
	return policy.Decision{Allow: true, PolicyVersion: "pv-1", ReasonCode: "allow_" + a.Action}, nil
}

func (p *rulePolicy) set(decide func(policy.Authorization) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.decide, p.checks = decide, nil
}

func (p *rulePolicy) seen() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.checks)
}

func denyAll(policy.Authorization) bool { return false }

// publicOnly is a conversation member: everything except non-public
// message reads.
func publicOnly(a policy.Authorization) bool {
	return a.Action != model.ActionMessageRead || a.Audience.Visibility == model.Public
}

// svc is a service fixture over the runtime rig: one channel (send
// capable), inbox one with the rig conversation, and inbox two.
type svc struct {
	*rig
	pol    *rulePolicy
	seq    *portstest.SequenceIDs
	inbox2 model.Inbox
}

func newSvc(t *testing.T, opts ...func(*engine.Config)) *svc {
	t.Helper()
	p := &rulePolicy{}
	r := newRig(t, func(c *engine.Config, _ *outFactory) {
		c.Authorizer = p
		for _, o := range opts {
			o(c)
		}
	})
	s := &svc{rig: r, pol: p, seq: portstest.NewSequenceIDs(0x5e1)}
	s.inbox2 = model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: inboxTwo}, Channel: r.Ch.Ref, Name: "two", Revision: "in2"}
	s.put(func(tx store.Tx) error { return tx.PutInbox(r.Ctx, s.inbox2, "") })
	return s
}

func (s *svc) id() model.ID {
	id, err := s.seq.New(s.Ctx)
	if err != nil {
		s.T.Fatal(err)
	}
	return id
}

func (s *svc) put(fn func(store.Tx) error) {
	s.T.Helper()
	if err := s.E.Store().Transact(s.Ctx, tenantA, fn); err != nil {
		s.T.Fatalf("seed: %v", err)
	}
}

func (s *svc) conversation(in model.InboxRef, status string) model.Conversation {
	s.T.Helper()
	c := model.Conversation{
		Ref: model.ConversationRef{Inbox: in, ID: s.id()}, Mode: model.ModePerRoom, Status: status,
		Participants: []model.Participant{{Role: "member"}}, CreatedAt: s.Clock.Now(), Revision: "cv1",
	}
	s.put(func(tx store.Tx) error { return tx.PutConversation(s.Ctx, c, "") })
	return c
}

func (s *svc) message(conv model.ConversationRef, vis model.Visibility) model.Message {
	s.T.Helper()
	m := model.Message{
		Ref: model.MessageRef{Conversation: conv, ID: s.id()}, Audience: model.Audience{Visibility: vis},
		Content:    model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "body"}}},
		External:   []model.ExternalRef{{Channel: s.Ch.Ref, Namespace: routeNSpace, Kind: "message", ID: string(s.id())}},
		RecordedAt: s.Clock.Now(), Revision: "m1",
	}
	s.put(func(tx store.Tx) error { return tx.PutMessage(s.Ctx, m, "") })
	return m
}

func call() model.Call { return model.Call{Correlation: "corr-svc"} }

func cmd(op model.OperationID, rev model.Revision) model.Command {
	return model.Command{Call: call(), Operation: op, ExpectedRevision: rev}
}

func listReq(f model.Filter, limit int, after model.Cursor) model.ListRequest {
	return model.ListRequest{Call: call(), Filter: f, Page: model.PageRequest{Limit: limit, After: after}}
}

// drain pages list to completion and returns every item.
func drain[T any](t *testing.T, list func(model.ListRequest) (model.Page[T], error), f model.Filter, limit int) []T {
	t.Helper()
	var out []T
	var after model.Cursor
	for range 1000 {
		p, err := list(listReq(f, limit, after))
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		out = append(out, p.Items...)
		if p.Complete {
			return out
		}
		after = p.Next
	}
	t.Fatal("list never completed")
	return nil
}

func msgIDs(ms []model.Message) []model.ID {
	out := make([]model.ID, len(ms))
	for i, m := range ms {
		out[i] = m.Ref.ID
	}
	return out
}

// Every read consults policy for the client's principal, per call; a
// denial hides the resource from lists and refuses lookups.
func TestServiceReadAuthorization(t *testing.T) {
	s := newSvc(t)
	msg := s.message(s.Conv.Ref, model.Public)
	type read struct {
		action string
		get    func() error
		list   func() (int, error)
	}
	reads := map[string]read{
		"channels": {model.ActionChannelRead, nil, func() (int, error) {
			p, err := s.C.Configuration().Channels(s.Ctx, listReq(model.Filter{}, 10, ""))
			return len(p.Items), err
		}},
		"inboxes": {model.ActionInboxRead, nil, func() (int, error) {
			p, err := s.C.Configuration().Inboxes(s.Ctx, listReq(model.Filter{}, 10, ""))
			return len(p.Items), err
		}},
		"capabilities": {model.ActionChannelRead, func() error {
			_, err := s.C.Configuration().Capabilities(s.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: s.Ch.Ref})
			return err
		}, nil},
		"conversations": {model.ActionConversationRead, func() error {
			_, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref})
			return err
		}, func() (int, error) {
			p, err := s.C.Conversations().List(s.Ctx, listReq(model.Filter{}, 10, ""))
			return len(p.Items), err
		}},
		"messages": {model.ActionMessageRead, func() error {
			_, err := s.C.Messages().Get(s.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: msg.Ref})
			return err
		}, func() (int, error) {
			p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 10, ""))
			return len(p.Items), err
		}},
	}
	for name, rd := range reads {
		t.Run(name+"/allow", func(t *testing.T) {
			s.pol.set(nil)
			if rd.get != nil {
				if err := rd.get(); err != nil {
					t.Fatal(err)
				}
			}
			if rd.list != nil {
				if n, err := rd.list(); err != nil || n == 0 {
					t.Fatalf("list = %d, %v", n, err)
				}
			}
			for _, a := range s.pol.seen() {
				if a.Principal != principalA() || a.Action != rd.action || a.Resource.Tenant != tenantA {
					t.Fatalf("authorization %+v", a)
				}
			}
			if len(s.pol.seen()) == 0 {
				t.Fatal("policy not consulted")
			}
		})
		t.Run(name+"/deny", func(t *testing.T) {
			s.pol.set(denyAll)
			if rd.get != nil {
				requireCode(t, rd.get(), model.Unauthorized, "")
			}
			if rd.list != nil {
				if n, err := rd.list(); err != nil || n != 0 {
					t.Fatalf("denied list = %d, %v", n, err)
				}
			}
		})
	}
}

// Scope holds whatever policy says: another tenant's reference, or a
// reference whose inbox or conversation differs from the record's, is not
// found under allow-all and never reaches policy.
func TestServiceScopeUnderAllowAll(t *testing.T) {
	s := newSvc(t)
	msg := s.message(s.Conv.Ref, model.Public)
	other := s.conversation(s.inbox2.Ref, model.ConversationOpen)
	foreignConv := s.Conv.Ref
	foreignConv.Inbox.Tenant = tenantB
	crossInbox := s.Conv.Ref
	crossInbox.Inbox = s.inbox2.Ref
	foreignMsg := msg.Ref
	foreignMsg.Conversation = foreignConv
	crossMsgInbox := msg.Ref
	crossMsgInbox.Conversation = crossInbox
	crossMsgConv := msg.Ref
	crossMsgConv.Conversation = other.Ref
	foreignCh := s.Ch.Ref
	foreignCh.Tenant = tenantB
	getConv := func(r model.ConversationRef) error {
		_, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: r})
		return err
	}
	getMsg := func(r model.MessageRef) error {
		_, err := s.C.Messages().Get(s.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: r})
		return err
	}
	cases := map[string]func() error{
		"conversation other tenant":  func() error { return getConv(foreignConv) },
		"conversation other inbox":   func() error { return getConv(crossInbox) },
		"message other tenant":       func() error { return getMsg(foreignMsg) },
		"message other inbox":        func() error { return getMsg(crossMsgInbox) },
		"message other conversation": func() error { return getMsg(crossMsgConv) },
		"capabilities other tenant": func() error {
			_, err := s.C.Configuration().Capabilities(s.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: foreignCh})
			return err
		},
		"list filter other tenant": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Inboxes: []model.InboxRef{{Tenant: tenantB, ID: inboxOne}}}, 10, ""))
			return err
		},
		"transition other inbox": func() error {
			_, err := s.C.Conversations().Transition(s.Ctx, model.Transition{Command: cmd("op-t", "cv1"), Conversation: crossInbox, State: model.ConversationClosed})
			return err
		},
		"note other tenant": func() error {
			_, err := s.C.Messages().AddInternalNote(s.Ctx, model.InternalNote{Command: cmd("op-n", ""), Conversation: foreignConv,
				Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "x"}}}})
			return err
		},
		"note other inbox": func() error {
			_, err := s.C.Messages().AddInternalNote(s.Ctx, model.InternalNote{Command: cmd("op-n2", ""), Conversation: crossInbox,
				Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "x"}}}})
			return err
		},
		"configure channel other tenant": func() error {
			_, err := s.C.Configuration().ConfigureChannel(s.Ctx, model.ConfigureChannel{Command: cmd("op-c", "ch1"),
				Channel: model.Channel{Ref: foreignCh, Type: outKey, Configuration: model.Extension{Name: string(outKey) + ".config", Version: 1}}})
			return err
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			s.pol.set(nil)
			requireCode(t, fn(), model.NotFound, "resource not found")
			if n := len(s.pol.seen()); n != 0 {
				t.Fatalf("policy consulted %d times for an out-of-scope reference", n)
			}
		})
	}
	// Lists never cross the filter's inbox.
	s.message(other.Ref, model.Public)
	got := drain(t, func(r model.ListRequest) (model.Page[model.Message], error) { return s.C.Messages().List(s.Ctx, r) },
		model.Filter{Inboxes: []model.InboxRef{s.inbox2.Ref}}, 10)
	for _, m := range got {
		if m.Ref.Conversation.Inbox != s.inbox2.Ref {
			t.Fatalf("inbox-two list returned %v", m.Ref)
		}
	}
	if len(got) != 1 {
		t.Fatalf("inbox-two messages = %d, want 1", len(got))
	}
}

// Another tenant's principal sees none of tenant A's records, even under
// allow-all.
func TestServiceListsStayInTenant(t *testing.T) {
	s := newSvc(t)
	s.message(s.Conv.Ref, model.Public)
	b, err := enginetest.For(s.E, model.Principal{Tenant: tenantB, Actor: actor})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := b.Messages().List(s.Ctx, listReq(model.Filter{}, 10, "")); err != nil || len(p.Items) != 0 || !p.Complete {
		t.Fatalf("tenant B messages = %+v, %v", p, err)
	}
	if p, err := b.Configuration().Channels(s.Ctx, listReq(model.Filter{}, 10, "")); err != nil || len(p.Items) != 0 {
		t.Fatalf("tenant B channels = %+v, %v", p, err)
	}
}

// Internal notes are authorized under their own audience: a member who may
// read the conversation and its public messages never sees a note, by
// lookup or by list; staff do. Unknown visibility is authorized as
// restricted.
func TestMessageVisibilityFiltering(t *testing.T) {
	s := newSvc(t)
	pub := s.message(s.Conv.Ref, model.Public)
	note := s.message(s.Conv.Ref, model.Internal)
	odd := s.message(s.Conv.Ref, "secret")
	s.pol.set(publicOnly)
	if _, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref}); err != nil {
		t.Fatalf("member reads the conversation: %v", err)
	}
	list := func(r model.ListRequest) (model.Page[model.Message], error) { return s.C.Messages().List(s.Ctx, r) }
	conv := s.Conv.Ref
	if got := msgIDs(drain(t, list, model.Filter{Conversation: &conv}, 1)); !slices.Equal(got, []model.ID{pub.Ref.ID}) {
		t.Fatalf("member sees %v, want only the public message", got)
	}
	_, err := s.C.Messages().Get(s.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: note.Ref})
	requireCode(t, err, model.Unauthorized, "")
	var oddAudience model.Visibility
	for _, a := range s.pol.seen() {
		if a.Resource == odd.Ref.Ref() {
			oddAudience = a.Audience.Visibility
		}
	}
	if oddAudience != model.Restricted {
		t.Fatalf("unknown visibility authorized as %q, want restricted", oddAudience)
	}
	s.pol.set(nil)
	if got := drain(t, list, model.Filter{Conversation: &conv}, 2); len(got) != 3 {
		t.Fatalf("staff see %d messages, want 3", len(got))
	}
}

// Pages are bounded: Limit 0 takes the default, a larger one the maximum,
// and a scan over inaccessible items returns short or empty pages with a
// Next cursor until the query completes.
func TestListBoundsAndShortPages(t *testing.T) {
	s := newSvc(t)
	for range services.MaxPageLimit + 20 {
		s.message(s.Conv.Ref, model.Public)
	}
	p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 0, ""))
	if err != nil || len(p.Items) != services.DefaultPageLimit || p.Complete || p.Next == "" {
		t.Fatalf("default page = %d items, complete %v, %v", len(p.Items), p.Complete, err)
	}
	if p, err = s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 10*services.MaxPageLimit, "")); err != nil || len(p.Items) != services.MaxPageLimit {
		t.Fatalf("capped page = %d items, %v", len(p.Items), err)
	}
	for range 2 * services.MaxScan {
		s.message(s.Conv.Ref, model.Internal)
	}
	s.message(s.Conv.Ref, model.Public)
	s.pol.set(publicOnly)
	var pages, empty, seen int
	var after model.Cursor
	for pages = 1; ; pages++ {
		p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, services.MaxPageLimit, after))
		if err != nil {
			t.Fatal(err)
		}
		seen += len(p.Items)
		if len(p.Items) == 0 {
			empty++
		}
		if p.Complete {
			break
		}
		if p.Next == "" || p.Next == after {
			t.Fatalf("page %d made no progress", pages)
		}
		after = p.Next
	}
	if seen != services.MaxPageLimit+21 || empty == 0 {
		t.Fatalf("member saw %d public messages over %d pages (%d empty)", seen, pages, empty)
	}
}

// A plain list is not sealed: the page that completes it still carries a
// Next, so a caller may observe that records it cannot read exist (as short
// or empty pages with Next). Only search lists seal the completing page.
func TestPlainListCompletingPageCarriesNext(t *testing.T) {
	s := newSvc(t)
	s.message(s.Conv.Ref, model.Public)
	s.message(s.Conv.Ref, model.Internal)
	s.pol.set(publicOnly)
	p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, services.MaxPageLimit, ""))
	if err != nil || !p.Complete || len(p.Items) != 1 || p.Next == "" {
		t.Fatalf("plain page = %d items, complete %v, next %q, %v; want one item, complete, with a Next", len(p.Items), p.Complete, p.Next, err)
	}
}

// A filter field a query does not honour is rejected, never ignored.
func TestListRejectsUnsupportedFilters(t *testing.T) {
	s := newSvc(t)
	conv := s.Conv.Ref
	contact := model.ContactRef{Tenant: tenantA, ID: inboxOne}
	cases := map[string]func() error{
		"channels inboxes": func() error {
			_, err := s.C.Configuration().Channels(s.Ctx, listReq(model.Filter{Inboxes: []model.InboxRef{s.inbox2.Ref}}, 1, ""))
			return err
		},
		"inboxes statuses": func() error {
			_, err := s.C.Configuration().Inboxes(s.Ctx, listReq(model.Filter{Statuses: []string{"open"}}, 1, ""))
			return err
		},
		"conversations conversation": func() error {
			_, err := s.C.Conversations().List(s.Ctx, listReq(model.Filter{Conversation: &conv}, 1, ""))
			return err
		},
		"messages statuses": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Statuses: []string{"open"}}, 1, ""))
			return err
		},
		"messages contact": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Contact: &contact}, 1, ""))
			return err
		},
		"messages kinds": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Kinds: []string{"x"}}, 1, ""))
			return err
		},
		"conversations identity": func() error {
			who := model.IdentityRef(conv)
			_, err := s.C.Conversations().List(s.Ctx, listReq(model.Filter{Identity: &who}, 1, ""))
			return err
		},
		"messages identity": func() error {
			who := model.IdentityRef(conv)
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Identity: &who}, 1, ""))
			return err
		},
		"inboxes contact": func() error {
			_, err := s.C.Configuration().Inboxes(s.Ctx, listReq(model.Filter{Contact: &contact}, 1, ""))
			return err
		},
		"conversations kinds": func() error {
			_, err := s.C.Conversations().List(s.Ctx, listReq(model.Filter{Kinds: []string{"x"}}, 1, ""))
			return err
		},
		"negative limit": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, -1, ""))
			return err
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) { requireCode(t, fn(), model.Validation, "") })
	}
}

// Conversations.List honours inbox and status filters.
func TestConversationListFilters(t *testing.T) {
	s := newSvc(t)
	closed := s.conversation(s.inbox2.Ref, model.ConversationClosed)
	s.conversation(s.inbox2.Ref, model.ConversationOpen)
	list := func(r model.ListRequest) (model.Page[model.Conversation], error) {
		return s.C.Conversations().List(s.Ctx, r)
	}
	got := drain(t, list, model.Filter{Inboxes: []model.InboxRef{s.inbox2.Ref}, Statuses: []string{model.ConversationClosed}}, 1)
	if len(got) != 1 || got[0].Ref != closed.Ref {
		t.Fatalf("closed in inbox two = %+v", got)
	}
	if got := drain(t, list, model.Filter{}, 1); len(got) != 3 {
		t.Fatalf("all conversations = %d, want 3", len(got))
	}
}

// Paging while records are inserted concurrently returns every record that
// existed when paging began exactly once, in recording order, and never a
// duplicate.
func TestListStableUnderConcurrentInserts(t *testing.T) {
	s := newSvc(t)
	var before []model.ID
	for range 40 {
		before = append(before, s.message(s.Conv.Ref, model.Public).Ref.ID)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			m := model.Message{
				Ref: model.MessageRef{Conversation: s.Conv.Ref, ID: s.id()}, Audience: model.Audience{Visibility: model.Public},
				RecordedAt: s.Clock.Now(), Revision: "m1",
			}
			if err := s.E.Store().Transact(s.Ctx, tenantA, func(tx store.Tx) error { return tx.PutMessage(s.Ctx, m, "") }); err != nil {
				t.Error(err)
				return
			}
		}
	})
	var got []model.ID
	var after model.Cursor
	for len(got) < 60 {
		p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 7, after))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, msgIDs(p.Items)...)
		after = p.Next
		if p.Complete {
			break
		}
	}
	close(stop)
	wg.Wait()
	seen := map[model.ID]bool{}
	for _, id := range got {
		if seen[id] {
			t.Fatalf("message %s paged twice", id)
		}
		seen[id] = true
	}
	if !slices.Equal(got[:len(before)], before) {
		t.Fatal("pre-existing messages not returned first, once, in recording order")
	}
	// Recording order holds across the whole run.
	all := msgIDs(drain(t, func(r model.ListRequest) (model.Page[model.Message], error) { return s.C.Messages().List(s.Ctx, r) }, model.Filter{}, services.MaxPageLimit))
	if !slices.Equal(all[:len(got)], got) {
		t.Fatal("paged order differs from recording order")
	}
}

// Each page reauthorizes: a policy change applies from the next page on.
func TestListReauthorizesEachPage(t *testing.T) {
	s := newSvc(t)
	for range 4 {
		s.message(s.Conv.Ref, model.Public)
	}
	p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 2, ""))
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("first page = %d, %v", len(p.Items), err)
	}
	s.pol.set(denyAll)
	p, err = s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 2, p.Next))
	if err != nil || len(p.Items) != 0 {
		t.Fatalf("page after revocation = %d items, %v", len(p.Items), err)
	}
}

// Returned items are snapshots: mutating them changes nothing stored.
func TestServiceResultsAreClones(t *testing.T) {
	s := newSvc(t)
	m := s.message(s.Conv.Ref, model.Public)
	p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 10, ""))
	if err != nil {
		t.Fatal(err)
	}
	p.Items[0].Content.Parts[0].Text = "mutated"
	p.Items[0].External[0].ID = "mutated"
	got, err := s.C.Messages().Get(s.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: m.Ref})
	if err != nil {
		t.Fatal(err)
	}
	got.Content.Parts[0].Text = "mutated again"
	again, _ := s.C.Messages().Get(s.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: m.Ref})
	if again.Content.Parts[0].Text != "body" || again.External[0].ID != m.External[0].ID {
		t.Fatalf("stored message changed through a result: %+v", again)
	}
	c, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref})
	if err != nil {
		t.Fatal(err)
	}
	c.ReplyRoute.ID = "mutated"
	again2, _ := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref})
	if again2.ReplyRoute.ID != s.Conv.ReplyRoute.ID {
		t.Fatal("stored conversation changed through a result")
	}
}

// Cursors are bound to tenant, actor, query, filter and schema: an altered
// or transplanted cursor is a validation error, and one from another store
// schema has expired.
func TestCursorBinding(t *testing.T) {
	s := newSvc(t)
	for range 3 {
		s.message(s.Conv.Ref, model.Public)
	}
	conv := s.Conv.Ref
	p, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, ""))
	if err != nil || p.Next == "" {
		t.Fatalf("first page: %+v, %v", p, err)
	}
	next := string(p.Next)
	flip := func(i int) model.Cursor {
		b := []byte(next)
		if b[i] == 'A' {
			b[i] = 'B'
		} else {
			b[i] = 'A'
		}
		return model.Cursor(b)
	}
	other, err := enginetest.For(s.E, model.Principal{Tenant: tenantA, Actor: "actor-2"})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func() error{
		"flipped tail": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, flip(len(next)-1)))
			return err
		},
		"flipped position": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, flip(len(services.CursorPrefix)+8)))
			return err
		},
		"truncated": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, model.Cursor(next[:len(next)-4])))
			return err
		},
		"store cursor": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, "mem.1"))
			return err
		},
		"other filter": func() error {
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Conversation: &conv}, 1, p.Next))
			return err
		},
		"other query": func() error {
			_, err := s.C.Conversations().List(s.Ctx, listReq(model.Filter{}, 1, p.Next))
			return err
		},
		"other actor": func() error {
			_, err := other.Messages().List(s.Ctx, listReq(model.Filter{}, 1, p.Next))
			return err
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) { requireCode(t, fn(), model.Validation, "cursor") })
	}
	// Any single-character change is rejected, non-canonical encodings of
	// the same bytes included.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for i := len(services.CursorPrefix); i < len(next); i++ {
		for _, ch := range []byte(alphabet) {
			if ch == next[i] {
				continue
			}
			b := []byte(next)
			b[i] = ch
			_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, model.Cursor(b)))
			if model.CodeOf(err) != model.Validation {
				t.Fatalf("cursor altered at %d to %q accepted: %v", i, ch, err)
			}
		}
	}
	// The same cursor with another page size continues the query.
	if _, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 5, p.Next)); err != nil {
		t.Fatalf("resize: %v", err)
	}
	s.Store.Set(schemaStore{Store: s.Store.Inner(), schema: s.E.Store().Properties().SchemaVersion + 1})
	_, err = s.C.Messages().List(s.Ctx, listReq(model.Filter{}, 1, p.Next))
	requireCode(t, err, model.CursorExpired, "restart")
}

type schemaStore struct {
	store.Store
	schema uint32
}

func (s schemaStore) Properties() store.Properties {
	p := s.Store.Properties()
	p.SchemaVersion = s.schema
	return p
}

// Equal filters in another order bind the same cursor.
func TestCursorFilterIsCanonical(t *testing.T) {
	s := newSvc(t)
	a := services.CanonicalFilter(model.Filter{Inboxes: []model.InboxRef{s.inbox2.Ref, s.Conv.Ref.Inbox}, Statuses: []string{"b", "a"}})
	b := services.CanonicalFilter(model.Filter{Inboxes: []model.InboxRef{s.Conv.Ref.Inbox, s.inbox2.Ref}, Statuses: []string{"a", "b"}})
	if string(a) != string(b) {
		t.Fatal("filter encoding depends on set order")
	}
}

// Transition changes local status only, at the expected revision, in the
// ledger, with a conversation.changed event.
func TestConversationTransition(t *testing.T) {
	s := newSvc(t)
	tr := func(op model.OperationID, rev model.Revision, state string) (model.Acceptance, error) {
		return s.C.Conversations().Transition(s.Ctx, model.Transition{Command: cmd(op, rev), Conversation: s.Conv.Ref, State: state})
	}
	a, err := tr("op-1", s.Conv.Revision, model.ConversationResolved)
	if err != nil {
		t.Fatal(err)
	}
	if a.Operation.State != model.Committed || a.Replayed || a.Operation.Result == nil || *a.Operation.Result != s.Conv.Ref.Ref() {
		t.Fatalf("acceptance %+v", a)
	}
	if got := s.Op(a.Operation.Ref); got.State != model.Committed {
		t.Fatalf("Operations.Get = %+v", got)
	}
	cur, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref})
	if err != nil || cur.Status != model.ConversationResolved || cur.Revision == s.Conv.Revision {
		t.Fatalf("after transition %+v, %v", cur, err)
	}
	evs := s.Events()
	if len(evs) != 1 || evs[0].Kind != model.EventConversationChanged || evs[0].Revision != cur.Revision {
		t.Fatalf("events %+v", evs)
	}
	var payload services.CommandConversationChanged
	if err := json.Unmarshal(evs[0].Payload.Data, &payload); err != nil || payload.Trigger != model.TriggerCommand ||
		payload.Operation != a.Operation.Ref || payload.Status.From != model.ConversationOpen ||
		payload.Status.To != model.ConversationResolved || payload.PreviousRevision != s.Conv.Revision {
		t.Fatalf("payload %+v, %v", payload, err)
	}
	// Replay after the revision moved on returns the stored operation.
	if again, err := tr("op-1", s.Conv.Revision, model.ConversationResolved); err != nil || !again.Replayed || again.Operation.Ref != a.Operation.Ref {
		t.Fatalf("replay %+v, %v", again, err)
	}
	_, err = tr("op-1", s.Conv.Revision, model.ConversationPending)
	requireCode(t, err, model.Conflict, "different payload")
	_, err = tr("op-2", s.Conv.Revision, model.ConversationOpen)
	requireCode(t, err, model.Conflict, "revision")
	// Current status: committed, no change, no event.
	same, err := tr("op-3", cur.Revision, model.ConversationResolved)
	if err != nil || same.Operation.State != model.Committed || len(s.Events()) != 1 {
		t.Fatalf("same-status transition %+v, %v, %d events", same, err, len(s.Events()))
	}
	if _, err := tr("op-4", cur.Revision, model.ConversationClosed); err != nil {
		t.Fatal(err)
	}
	closed, _ := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: s.Conv.Ref})
	_, err = tr("op-5", closed.Revision, model.ConversationOpen)
	requireCode(t, err, model.Conflict, "terminal")
	_, err = tr("op-6", closed.Revision, "archived")
	requireCode(t, err, model.Validation, "state")
	_, err = tr("op-7", "", model.ConversationOpen)
	requireCode(t, err, model.Validation, "expected_revision")
	s.pol.set(denyAll)
	_, err = tr("op-8", closed.Revision, model.ConversationClosed)
	requireCode(t, err, model.Unauthorized, "")
}

// An internal note is a separate committed operation: an internal message
// under the policy reference and a message.created event, readable only
// under its audience and never a public reply.
func TestAddInternalNote(t *testing.T) {
	s := newSvc(t)
	req := model.InternalNote{
		Command: cmd("op-note", ""), Conversation: s.Conv.Ref, PolicyRef: "staff",
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "customer is VIP"}}},
	}
	a, err := s.C.Messages().AddInternalNote(s.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if a.Operation.State != model.Committed || a.Operation.ResultMessage == nil || a.Replayed {
		t.Fatalf("acceptance %+v", a)
	}
	ref := *a.Operation.ResultMessage
	if *a.Operation.Result != ref.Ref() || ref.Conversation != s.Conv.Ref {
		t.Fatalf("result %+v", a.Operation)
	}
	var note model.Message
	s.view(func(r store.Reader) (err error) { note, err = r.GetMessage(s.Ctx, ref); return err })
	if note.Audience != (model.Audience{Visibility: model.Internal, PolicyRef: "staff"}) || note.Origin == nil ||
		note.Origin.Operation != a.Operation.Ref || note.Origin.Actor != actor || note.Content.Parts[0].Text != "customer is VIP" {
		t.Fatalf("stored note %+v", note)
	}
	evs := s.Events()
	if len(evs) != 1 || evs[0].Kind != model.EventMessageCreated || evs[0].Audience.Visibility != model.Internal {
		t.Fatalf("events %+v", evs)
	}
	var payload services.CommandMessageCreated
	if err := json.Unmarshal(evs[0].Payload.Data, &payload); err != nil || payload.Origin != model.MessageOriginNote ||
		payload.Operation != a.Operation.Ref || payload.Message != ref || payload.Source != "" {
		t.Fatalf("payload %+v, %v", payload, err)
	}
	if n := len(s.Claim("w")); n != 0 {
		t.Fatalf("a note queued %d provider work items", n)
	}
	// The policy saw the note audience on the conversation.
	var noted bool
	for _, c := range s.pol.seen() {
		noted = noted || (c.Action == model.ActionMessageNote && c.Resource == s.Conv.Ref.Ref() && c.Audience == note.Audience)
	}
	if !noted {
		t.Fatalf("authorizations %+v", s.pol.seen())
	}
	if again, err := s.C.Messages().AddInternalNote(s.Ctx, req); err != nil || !again.Replayed || *again.Operation.ResultMessage != ref {
		t.Fatalf("replay %+v, %v", again, err)
	}
	changed := req
	changed.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "other"}}}
	_, err = s.C.Messages().AddInternalNote(s.Ctx, changed)
	requireCode(t, err, model.Conflict, "different payload")
	stale := req
	stale.Operation, stale.ExpectedRevision = "op-stale", "not-current"
	_, err = s.C.Messages().AddInternalNote(s.Ctx, stale)
	requireCode(t, err, model.Conflict, "revision")
	s.pol.set(publicOnly)
	list := func(r model.ListRequest) (model.Page[model.Message], error) { return s.C.Messages().List(s.Ctx, r) }
	if got := drain(t, list, model.Filter{}, 5); len(got) != 0 {
		t.Fatalf("member lists %d messages, want none", len(got))
	}
	s.pol.set(denyAll)
	denied := req
	denied.Operation = "op-denied"
	_, err = s.C.Messages().AddInternalNote(s.Ctx, denied)
	requireCode(t, err, model.Unauthorized, "")
	s.pol.set(nil)
	if got := drain(t, list, model.Filter{}, 5); len(got) != 1 {
		t.Fatalf("notes stored = %d, want 1", len(got))
	}
}

func (s *svc) view(fn func(store.Reader) error) {
	s.T.Helper()
	if err := s.E.Store().View(s.Ctx, tenantA, fn); err != nil {
		s.T.Fatalf("View: %v", err)
	}
}

func (s *svc) channels() []model.Channel {
	var out []model.Channel
	s.view(func(r store.Reader) error {
		p, err := r.Channels(s.Ctx, model.ListRequest{Page: model.PageRequest{Limit: 100}})
		out = p.Items
		return err
	})
	return out
}

// ConfigureChannel creates and updates channels through the ledger, opens
// no adapter instance, and validates the type and configuration.
func TestConfigureChannel(t *testing.T) {
	s := newSvc(t)
	cfg := model.Extension{Name: string(outKey) + ".config", Version: 1}
	req := model.ConfigureChannel{
		Command: cmd("op-ch", ""),
		Channel: model.Channel{Ref: model.ChannelRef{Tenant: tenantA}, Type: outKey, Configuration: cfg, CredentialRef: "cred-2"},
	}
	before := len(s.channels())
	a, err := s.C.Configuration().ConfigureChannel(s.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := a.Operation.Result.AsChannel()
	if err != nil || a.Operation.State != model.Committed {
		t.Fatalf("acceptance %+v, %v", a, err)
	}
	again, err := s.C.Configuration().ConfigureChannel(s.Ctx, req)
	if err != nil || !again.Replayed || *again.Operation.Result != ref.Ref() || len(s.channels()) != before+1 {
		t.Fatalf("replay %+v, %v; channels %d", again, err, len(s.channels()))
	}
	if s.F.Opens != 0 {
		t.Fatalf("configuring opened %d adapter instances", s.F.Opens)
	}
	var stored model.Channel
	s.view(func(r store.Reader) (err error) { stored, err = r.GetChannel(s.Ctx, ref); return err })
	upd := model.ConfigureChannel{Command: cmd("op-upd", stored.Revision), Channel: stored}
	upd.Channel.CredentialRef = "cred-3"
	if _, err := s.C.Configuration().ConfigureChannel(s.Ctx, upd); err != nil {
		t.Fatalf("update: %v", err)
	}
	stale := upd
	stale.Operation = "op-stale"
	_, err = s.C.Configuration().ConfigureChannel(s.Ctx, stale)
	requireCode(t, err, model.Conflict, "")
	cases := map[string]struct {
		mut  func(*model.ConfigureChannel)
		code model.ErrorCode
		msg  string
	}{
		"unregistered type":  {func(r *model.ConfigureChannel) { r.Channel.Type = "com.example.other.chat" }, model.Unsupported, "not registered"},
		"schema mismatch":    {func(r *model.ConfigureChannel) { r.Channel.Configuration.Name = "com.example.chat.other" }, model.SchemaMismatch, ""},
		"bad schema name":    {func(r *model.ConfigureChannel) { r.Channel.Configuration.Name = "bad" }, model.Validation, "channel.configuration"},
		"revision on create": {func(r *model.ConfigureChannel) { r.ExpectedRevision = "r" }, model.Validation, "expected_revision"},
		"missing channel": {func(r *model.ConfigureChannel) {
			r.Channel.Ref.ID, r.ExpectedRevision = s.id(), "r"
		}, model.NotFound, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := req
			r.Operation = model.OperationID("op-" + name[:3])
			tc.mut(&r)
			_, err := s.C.Configuration().ConfigureChannel(s.Ctx, r)
			requireCode(t, err, tc.code, tc.msg)
		})
	}
	s.pol.set(denyAll)
	denied := req
	denied.Operation = "op-denied"
	_, err = s.C.Configuration().ConfigureChannel(s.Ctx, denied)
	requireCode(t, err, model.Unauthorized, "")
	if len(s.channels()) != before+1 {
		t.Fatal("a denied configuration stored a channel")
	}
}

// A channel's type is fixed once created.
func TestConfigureChannelKeepsType(t *testing.T) {
	h := newHarness(t, chatType)
	cfg := func(typ model.TypeKey) model.Extension {
		return model.Extension{Name: string(typ) + ".ingress-test", Version: 1}
	}
	a, err := h.Client.Configuration().ConfigureChannel(h.Ctx, model.ConfigureChannel{
		Command: cmd("op-1", ""), Channel: model.Channel{Ref: model.ChannelRef{Tenant: tenantA}, Type: chatType, Configuration: cfg(chatType)},
	})
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := a.Operation.Result.AsChannel()
	var ch model.Channel
	h.View(func(r store.Reader) (err error) { ch, err = r.GetChannel(h.Ctx, ref); return err })
	ch.Type, ch.Configuration = model.TypeSmtpMail, cfg(model.TypeSmtpMail)
	_, err = h.Client.Configuration().ConfigureChannel(h.Ctx, model.ConfigureChannel{Command: cmd("op-2", ch.Revision), Channel: ch})
	requireCode(t, err, model.Validation, "never changes type")
}

// ConfigureInbox rejects destinations already bound on the channel and
// replays a repeated create.
func TestConfigureInboxDestinationsAndReplay(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), "a")
	_, err := h.ConfigureOn(h.Channel.Ref, perContact(false), "b", "a")
	requireCode(t, err, model.Conflict, "already bound")
	_, err = h.ConfigureOn(h.Channel.Ref, perContact(false), "c", "c")
	requireCode(t, err, model.Validation, "distinct")
	_, err = h.ConfigureOn(h.Channel.Ref, perContact(false), "")
	requireCode(t, err, model.Validation, "empty")
	req := model.ConfigureInbox{
		Command: cmd("op-in", ""),
		Inbox:   model.Inbox{Ref: model.InboxRef{Tenant: tenantA}, Channel: h.Channel.Ref, Name: "x", Destinations: []string{"x"}, Selection: perContact(true)},
	}
	first, err := h.Client.Configuration().ConfigureInbox(h.Ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := h.Client.Configuration().ConfigureInbox(h.Ctx, req)
	if err != nil || !again.Replayed || *again.Operation.Result != *first.Operation.Result {
		t.Fatalf("replay %+v, %v", again, err)
	}
	if n := len(all(h, store.Reader.Inboxes, model.Filter{})); n != 2 {
		t.Fatalf("inboxes = %d, want 2", n)
	}
}

// A host-set key names one channel or inbox in the tenant: lookups by key
// return it, a key another record holds conflicts, and the key is part of
// the configuration payload.
func TestConfigurationKeys(t *testing.T) {
	s := newSvc(t)
	cfgn := s.C.Configuration()
	cfg := model.Extension{Name: string(outKey) + ".config", Version: 1}
	channel := func(op, key string) model.ConfigureChannel {
		return model.ConfigureChannel{
			Command: cmd(model.OperationID(op), ""),
			Channel: model.Channel{Ref: model.ChannelRef{Tenant: tenantA}, Type: outKey, Configuration: cfg, Key: key},
		}
	}
	a, err := cfgn.ConfigureChannel(s.Ctx, channel("op-ch1", "mail"))
	if err != nil {
		t.Fatal(err)
	}
	chRef, _ := a.Operation.Result.AsChannel()
	got, err := cfgn.ChannelByKey(s.Ctx, model.LookupKey{Call: call(), Key: "mail"})
	if err != nil || got.Ref != chRef || got.Key != "mail" {
		t.Fatalf("ChannelByKey = %+v, %v; want %s", got, err, chRef.ID)
	}
	_, err = cfgn.ConfigureChannel(s.Ctx, channel("op-ch2", "mail"))
	requireCode(t, err, model.Conflict, "channel.key")
	changed := channel("op-ch1", "post")
	_, err = cfgn.ConfigureChannel(s.Ctx, changed)
	requireCode(t, err, model.Conflict, "different payload")
	_, err = cfgn.ConfigureChannel(s.Ctx, channel("op-ch3", " bad"))
	requireCode(t, err, model.Validation, "channel.key")

	inbox := func(op, key string) model.ConfigureInbox {
		return model.ConfigureInbox{
			Command: cmd(model.OperationID(op), ""),
			Inbox:   model.Inbox{Ref: model.InboxRef{Tenant: tenantA}, Channel: s.Ch.Ref, Name: op, Key: key, Selection: perContact(false)},
		}
	}
	b, err := cfgn.ConfigureInbox(s.Ctx, inbox("op-in1", "front"))
	if err != nil {
		t.Fatal(err)
	}
	inRef, _ := b.Operation.Result.AsInbox()
	gotIn, err := cfgn.InboxByKey(s.Ctx, model.LookupKey{Call: call(), Key: "front"})
	if err != nil || gotIn.Ref != inRef || gotIn.Key != "front" {
		t.Fatalf("InboxByKey = %+v, %v; want %s", gotIn, err, inRef.ID)
	}
	_, err = cfgn.ConfigureInbox(s.Ctx, inbox("op-in2", "front"))
	requireCode(t, err, model.Conflict, "inbox.key")

	_, err = cfgn.ChannelByKey(s.Ctx, model.LookupKey{Call: call(), Key: "missing"})
	requireCode(t, err, model.NotFound, "")
	_, err = cfgn.InboxByKey(s.Ctx, model.LookupKey{Call: call(), Key: ""})
	requireCode(t, err, model.Validation, "key")
	s.pol.set(denyAll)
	_, err = cfgn.ChannelByKey(s.Ctx, model.LookupKey{Call: call(), Key: "mail"})
	requireCode(t, err, model.Unauthorized, "")
	_, err = cfgn.InboxByKey(s.Ctx, model.LookupKey{Call: call(), Key: "front"})
	requireCode(t, err, model.Unauthorized, "")
}

// Capabilities reports each declared capability as the instance and its
// permission probe report it now, never an undeclared one.
func TestCapabilities(t *testing.T) {
	s := newSvc(t)
	s.F.Withdraw(model.CapabilitySend, model.Unauthorized)
	got, err := s.C.Configuration().Capabilities(s.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: s.Ch.Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != model.CapabilitySend || got[0].Available || got[0].Reason != model.Unauthorized {
		t.Fatalf("capabilities %+v", got)
	}
	if s.F.Opens != 1 || s.F.Closes != 1 {
		t.Fatalf("opens %d closes %d, want one instance opened and closed", s.F.Opens, s.F.Closes)
	}
	s.F.FailOpen(model.NewError(model.Unavailable, "provider down"))
	_, err = s.C.Configuration().Capabilities(s.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: s.Ch.Ref})
	requireCode(t, err, model.Unavailable, "")
	missing := model.ChannelRef{Tenant: tenantA, ID: s.id()}
	_, err = s.C.Configuration().Capabilities(s.Ctx, model.Lookup[model.ChannelRef]{Call: call(), Ref: missing})
	requireCode(t, err, model.NotFound, "")
}

type probeInstance struct{ *outInstance }

func (probeInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilitySend, Available: true},
		{Name: model.CapabilityProbePermissions, Available: true},
	}, nil
}

func (probeInstance) Probe(context.Context) ([]model.CapabilityStatus, error) {
	return []model.CapabilityStatus{
		{Name: model.CapabilitySend, Available: false, Reason: model.Unauthorized},
		{Name: model.CapabilityEdit, Available: true},
	}, nil
}

func TestCapabilitiesMergeProbe(t *testing.T) {
	d := receiveDescriptor(thirdPartyKey)
	d.Capabilities = []model.Capability{model.CapabilitySend, model.CapabilityProbePermissions}
	statuses, err := services.CurrentCapabilities(t.Context(), d, probeInstance{&outInstance{F: &outFactory{}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Available || statuses[0].Reason != model.Unauthorized || !statuses[1].Available {
		t.Fatalf("statuses %+v", statuses)
	}
}

// statusReport is an instance reporting a fixed status list.
type statusReport struct {
	*outInstance
	statuses []model.CapabilityStatus
}

func (r statusReport) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	return r.statuses, nil
}

// A declared capability the instance does not report is listed
// unavailable, never assumed available; an invalid report fails.
func TestCapabilitiesFailClosed(t *testing.T) {
	d := receiveDescriptor(thirdPartyKey)
	d.Capabilities = []model.Capability{model.CapabilitySend, model.CapabilityReconcile}
	inst := statusReport{&outInstance{F: &outFactory{}}, []model.CapabilityStatus{{Name: model.CapabilitySend, Available: true}}}
	statuses, err := services.CurrentCapabilities(t.Context(), d, inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || !statuses[0].Available || statuses[1].Available || statuses[1].Reason != model.Unsupported {
		t.Fatalf("statuses %+v, want send available and unreported reconcile unsupported", statuses)
	}
	inst.statuses = append(inst.statuses, model.CapabilityStatus{Name: model.CapabilitySend, Reason: model.Disabled})
	_, err = services.CurrentCapabilities(t.Context(), d, inst)
	requireCode(t, err, model.Permanent, "invalid capability statuses")
}

// looseStore models a backend that resolves conversations and messages by
// ID alone, ignoring the inbox and conversation of the reference.
type looseStore struct {
	store.Store
	convs map[model.ID]model.Conversation
	msgs  map[model.ID]model.Message
}

func (s looseStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Store.View(ctx, t, func(r store.Reader) error { return fn(looseReader{r, s}) })
}

func (s looseStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, t, func(tx store.Tx) error { return fn(looseTx{tx, s}) })
}

type looseReader struct {
	store.Reader
	s looseStore
}

func (r looseReader) GetConversation(ctx context.Context, ref model.ConversationRef) (model.Conversation, error) {
	if c, ok := r.s.convs[ref.ID]; ok {
		return c.Clone(), nil
	}
	return r.Reader.GetConversation(ctx, ref)
}

func (r looseReader) GetMessage(ctx context.Context, ref model.MessageRef) (model.Message, error) {
	if m, ok := r.s.msgs[ref.ID]; ok {
		return m.Clone(), nil
	}
	return r.Reader.GetMessage(ctx, ref)
}

// Messages ignores the filter: every message of the tenant, paged.
func (r looseReader) Messages(ctx context.Context, req model.ListRequest) (model.Page[model.Message], error) {
	req.Filter = model.Filter{}
	return r.Reader.Messages(ctx, req)
}

type looseTx struct {
	store.Tx
	s looseStore
}

func (t looseTx) GetConversation(ctx context.Context, ref model.ConversationRef) (model.Conversation, error) {
	return looseReader{t.Tx, t.s}.GetConversation(ctx, ref)
}

// Every service compares the record it loaded with the reference the
// request named: a backend matching on ID alone still exposes or mutates
// nothing across inboxes or conversations.
func TestServicesCheckLoadedScope(t *testing.T) {
	s := newSvc(t)
	other := s.conversation(s.inbox2.Ref, model.ConversationOpen)
	sibling := s.conversation(s.Conv.Ref.Inbox, model.ConversationOpen)
	m := s.message(sibling.Ref, model.Public)
	s.Store.Set(looseStore{Store: s.Store.Inner(),
		convs: map[model.ID]model.Conversation{other.Ref.ID: other},
		msgs:  map[model.ID]model.Message{m.Ref.ID: m}})
	inOne := model.ConversationRef{Inbox: s.Conv.Ref.Inbox, ID: other.Ref.ID}
	text := model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "x"}}}
	cases := map[string]func() error{
		"conversation get": func() error {
			_, err := s.C.Conversations().Get(s.Ctx, model.Lookup[model.ConversationRef]{Call: call(), Ref: inOne})
			return err
		},
		"transition": func() error {
			_, err := s.C.Conversations().Transition(s.Ctx, model.Transition{Command: cmd("op-t", other.Revision), Conversation: inOne, State: model.ConversationClosed})
			return err
		},
		"note": func() error {
			_, err := s.C.Messages().AddInternalNote(s.Ctx, model.InternalNote{Command: cmd("op-n", ""), Conversation: inOne, Content: text})
			return err
		},
		"message get": func() error {
			_, err := s.C.Messages().Get(s.Ctx, model.Lookup[model.MessageRef]{Call: call(), Ref: model.MessageRef{Conversation: s.Conv.Ref, ID: m.Ref.ID}})
			return err
		},
	}
	for name, fn := range cases {
		t.Run(name, func(t *testing.T) {
			s.pol.set(nil)
			requireCode(t, fn(), model.NotFound, "resource not found")
		})
	}
	if n := len(s.Events()); n != 0 {
		t.Fatalf("out-of-scope commands appended %d events", n)
	}
	// A list never exposes an item outside its filter, nor runs a foreign
	// filter, whatever the backend returned.
	inbox2 := s.inbox2.Ref
	list := func(r model.ListRequest) (model.Page[model.Message], error) { return s.C.Messages().List(s.Ctx, r) }
	if got := drain(t, list, model.Filter{Inboxes: []model.InboxRef{inbox2}}, 10); len(got) != 0 {
		t.Fatalf("inbox-two list exposed %d messages of inbox one", len(got))
	}
	_, err := s.C.Messages().List(s.Ctx, listReq(model.Filter{Inboxes: []model.InboxRef{{Tenant: tenantB, ID: inboxOne}}}, 10, ""))
	requireCode(t, err, model.NotFound, "resource not found")
}
