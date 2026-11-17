package ingress_test

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// TestContactLifecycle consumes every row of the approved contact-lifecycle
// truth table, both flag states, through the public paths: config rows
// through Configuration.ConfigureInbox, routing rows as ingress through the
// engine-bound sink into the memory store, email rows through the email
// header normalizer first, race rows as concurrent ingress.
func TestContactLifecycle(t *testing.T) {
	f := loadLifecycleFixture(t)
	ran := map[string]bool{}
	var mu sync.Mutex
	for _, c := range f.Cases {
		t.Run(c.ID, func(t *testing.T) {
			mu.Lock()
			ran[c.ID] = true
			mu.Unlock()
			switch c.Group {
			case "config":
				configCase(t, c)
			case "race":
				raceCase(t, c)
			case "message_update":
				updateCase(t, c)
			case "lifecycle", "tie_break", "email", "core_check":
				routingCase(t, c)
			default:
				t.Fatalf("unknown group %q", c.Group)
			}
		})
	}
	if len(ran) != 84 {
		t.Fatalf("consumed %d fixture rows, want 84", len(ran))
	}
}

// TestContactLifecycleFirstContact runs the routing rows a first contact can
// be, with the sender unknown to the inbox: its ingress creates the identity
// and selects without reading the candidate group, with the row's outcome.
func TestContactLifecycleFirstContact(t *testing.T) {
	f := loadLifecycleFixture(t)
	ran := 0
	for _, c := range f.Cases {
		switch c.Group {
		case "lifecycle", "tie_break", "email", "core_check":
		default:
			continue
		}
		if !firstContactRow(c) {
			continue
		}
		ran++
		t.Run(c.ID, func(t *testing.T) { firstContactCase(t, c) })
	}
	if ran != 8 {
		t.Fatalf("ran %d first contact rows, want 8", ran)
	}
}

// lcWorld is one routing case seeded into a fresh store: an inbox with the
// case's selection, the sender's and another identity, the candidate
// conversations and, for email rows, the stored messages.
type lcWorld struct {
	*harness
	c      lcCase
	reads  readLog
	inbox  model.Inbox
	sender model.Identity
	other  model.Identity
	convs  map[string]model.ConversationRef
}

// fixtureSelector returns a core_check row's scripted result, translated
// into the seeded inbox.
type fixtureSelector struct{ w **lcWorld }

func (s fixtureSelector) Select(context.Context, policy.SelectionInput) (policy.SelectionResult, error) {
	w := *s.w
	r := w.c.SelectorResult
	res := policy.SelectionResult{CreateKey: r.CreateKey, Reopen: r.Reopen}
	if r.Existing != "" {
		ref := w.convRef(r.Existing)
		res.Existing = &ref
	}
	return res, nil
}

func newWorld(t *testing.T, c lcCase) *lcWorld { return newWorldFor(t, c, true) }

// newWorldFor seeds a case's world; the sender's identity is claimed in it
// only when claimSender is set, else the case's ingress creates it.
func newWorldFor(t *testing.T, c lcCase, claimSender bool) *lcWorld {
	t.Helper()
	typ := chatType
	if c.Group == "email" {
		typ = model.TypeSmtpMail
	}
	w := &lcWorld{c: c, convs: map[string]model.ConversationRef{}}
	h := newHarness(t, typ, withSelector("fixture", fixtureSelector{&w}), withReadLog(&w.reads))
	w.harness = h
	sel := model.Selection{Mode: c.Mode, LockToSingle: c.Flag, LifecycleRules: c.LifecycleRules}
	if c.Group == "core_check" {
		sel.Policy = "fixture"
	}
	w.inbox = h.Configure(sel, dest)
	if claimSender {
		w.sender = w.claimIdentity(w.inbox.Ref, "sender")
	}
	w.other = w.claimIdentity(w.inbox.Ref, "other")
	for _, cand := range c.Candidates {
		owner := w.sender
		if cand.Owner == "other" {
			owner = w.other
		}
		parts := []model.Participant{{Identity: owner.Ref, Role: ingress.RoleContact}}
		if cand.SenderIsParticipant && owner.Ref != w.sender.Ref {
			parts = append(parts, model.Participant{Identity: w.sender.Ref, Role: ingress.RoleParticipant})
		}
		w.seedConversation(w.inbox.Ref, cand.ID, cand.Status, cand.CreatedAt, owner.Ref, parts)
	}
	if c.Email != nil {
		w.seedStored(c.Email.StoredMessages)
	}
	return w
}

func (w *lcWorld) convRef(name string) model.ConversationRef {
	if ref, ok := w.convs[name]; ok {
		return ref
	}
	return model.ConversationRef{Inbox: w.inbox.Ref, ID: fixtureConvID(name)}
}

func (w *lcWorld) claimIdentity(in model.InboxRef, source string) model.Identity {
	w.T.Helper()
	id := model.Identity{
		Ref: model.IdentityRef{Inbox: in, ID: w.ID()}, Contact: model.ContactRef{Tenant: tenantA, ID: w.ID()},
		SourceID: source, Namespace: ingressNS, Kind: model.ContactKindPerson,
		LinkAssurance: model.LinkAssuranceNone, Revision: "r1",
	}
	w.Tx(func(tx store.Tx) error {
		if _, _, err := tx.ClaimIdentity(w.Ctx, model.IdentityKey{Inbox: in, Namespace: ingressNS, SourceID: source}, id); err != nil {
			return err
		}
		return tx.PutContact(w.Ctx, model.Contact{Ref: id.Contact, Kind: model.ContactKindPerson, Revision: "r1"}, "")
	})
	return id
}

func (w *lcWorld) seedConversation(in model.InboxRef, name, status string, at time.Time, owner model.IdentityRef, parts []model.Participant) {
	w.T.Helper()
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: in, ID: fixtureConvID(name)},
		Mode: model.ModePerContact, Status: status, Owner: &owner, Participants: parts,
		ReplyRoute: w.SenderRef(string(owner.ID)), CreatedAt: at, Revision: "r1",
	}
	w.Tx(func(tx store.Tx) error {
		_, _, err := tx.ClaimConversation(w.Ctx, store.ConversationClaim{
			Selection: ingress.ContactGroup(owner), CreateKey: "seed:" + name, Conversation: conv,
		})
		return err
	})
	w.convs[name] = conv.Ref
}

// seedStored stores the row's messages: in this inbox, or, for "other",
// in an inbox on another mail channel.
func (w *lcWorld) seedStored(stored []lcStored) {
	w.T.Helper()
	var otherCh model.ChannelRef
	var otherIn model.Inbox
	for _, m := range stored {
		ch, conv := w.Channel.Ref, w.convRef(m.Conversation)
		if m.Inbox == "other" {
			if otherIn.Ref.ID == "" {
				otherCh = w.PutChannel(model.TypeSmtpMail).Ref
				otherIn = w.PutInbox(otherCh, perContact(false), false, "elsewhere")
				owner := w.claimIdentity(otherIn.Ref, "sender")
				w.seedConversation(otherIn.Ref, m.Conversation, model.ConversationOpen, t0.Add(-time.Hour), owner.Ref,
					[]model.Participant{{Identity: owner.Ref, Role: ingress.RoleContact}})
			}
			ch, conv = otherCh, w.convs[m.Conversation]
		}
		msg := model.Message{
			Ref:      model.MessageRef{Conversation: conv, ID: w.ID()},
			Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "stored " + m.Direction}}},
			Audience: model.Audience{Visibility: model.Public}, External: []model.ExternalRef{model.ProviderMessageRef(ch, m.MessageID)},
			RecordedAt: t0.Add(-time.Minute), Revision: "r1",
		}
		w.Tx(func(tx store.Tx) error { return tx.PutMessage(w.Ctx, msg, "") })
	}
}

// envelope is the row's new inbound message from the sender. Email rows
// pass the raw headers through the normalizer; subject and extra headers
// have no way in.
func (w *lcWorld) envelope(event string) model.Envelope {
	w.T.Helper()
	c := w.c
	env := w.Inbound(event, "msg-"+event, "sender", dest)
	if c.Event == "auto_message" {
		env.Kind = model.EnvelopeAutomatedMessage
	}
	if e := c.Email; e != nil {
		n := model.EmailHeaders{
			MessageID: e.MessageID, InReplyTo: e.InReplyTo, References: e.References,
			ListID: e.ListID, AutoSubmitted: e.AutoSubmitted,
		}.Normalize(w.Channel.Ref)
		if n.Automated != (c.Event == "auto_message") {
			w.T.Fatalf("normalizer automated = %v for event %s", n.Automated, c.Event)
		}
		env.MessageID, env.References = n.MessageID, n.References
	}
	return env
}

// adoptSender takes the identity the ingress created for the sender, when
// the world did not claim it first.
func (w *lcWorld) adoptSender() {
	w.T.Helper()
	if w.sender.Ref.ID != "" {
		return
	}
	w.View(func(r store.Reader) error {
		id, err := r.Identity(w.Ctx, model.IdentityKey{Inbox: w.inbox.Ref, Namespace: ingressNS, SourceID: "sender"})
		if err == nil {
			w.sender = id
		}
		return nil
	})
}

type worldState struct {
	convs  map[model.ID]model.Conversation
	msgs   map[model.ID]model.Message
	events int
}

func (w *lcWorld) state() worldState {
	s := worldState{convs: map[model.ID]model.Conversation{}, msgs: map[model.ID]model.Message{}, events: len(w.Events())}
	for _, c := range w.Conversations(w.inbox.Ref) {
		s.convs[c.Ref.ID] = c
	}
	for _, m := range w.Messages(w.inbox.Ref) {
		s.msgs[m.Ref.ID] = m
	}
	return s
}

// newMessage returns the one message after has and before lacks.
func newMessage(t *testing.T, before, after worldState) model.Message {
	t.Helper()
	var out []model.Message
	for id, m := range after.msgs {
		if _, ok := before.msgs[id]; !ok {
			out = append(out, m)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d new messages, want 1", len(out))
	}
	return out[0]
}

func requireUnchanged(t *testing.T, before, after worldState) {
	t.Helper()
	if len(after.msgs) != len(before.msgs) || len(after.convs) != len(before.convs) || after.events != before.events {
		t.Fatalf("rejected ingress changed state: msgs %d->%d convs %d->%d events %d->%d",
			len(before.msgs), len(after.msgs), len(before.convs), len(after.convs), before.events, after.events)
	}
	for id, c := range before.convs {
		if after.convs[id].Revision != c.Revision {
			t.Fatalf("rejected ingress rewrote conversation %s", id)
		}
	}
}

func routingCase(t *testing.T, c lcCase) { routingCaseOn(t, newWorld(t, c)) }

// firstContactCase runs a routing row whose sender has no conversation of
// its own with the sender unknown to the inbox: the ingress creates its
// identity, selects without reading its candidate group, and the outcome is
// the row's, as for the claimed sender.
func firstContactCase(t *testing.T, c lcCase) {
	w := newWorldFor(t, c, false)
	routingCaseOn(t, w)
	if n := w.reads.count(); n != 0 {
		t.Fatalf("a first contact's selection read %d candidate groups, want none", n)
	}
}

// firstContactRow reports whether a routing row can run as a first contact:
// no candidate is the sender's, whose identity must then exist, or has it as
// a participant.
func firstContactRow(c lcCase) bool {
	for _, cand := range c.Candidates {
		if cand.Owner == "sender" || cand.SenderIsParticipant {
			return false
		}
	}
	return true
}

func routingCaseOn(t *testing.T, w *lcWorld) {
	c := w.c
	before := w.state()
	_, err := w.Accept(w.envelope(c.ID))
	w.adoptSender()
	after := w.state()
	switch c.Expect.Outcome {
	case "conflict", "reject":
		requireCode(t, err, expectedCode(c), "")
		requireUnchanged(t, before, after)
		return
	}
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	msg := newMessage(t, before, after)
	got := after.convs[msg.Ref.Conversation.ID]
	if c.Expect.Outcome == "create" {
		if _, existed := before.convs[got.Ref.ID]; existed {
			t.Fatalf("message joined existing conversation %s, want a new one", got.Ref.ID)
		}
		if got.Status != model.ConversationOpen || got.Owner == nil || *got.Owner != w.sender.Ref {
			t.Fatalf("created conversation %+v", got)
		}
		requireKinds(t, w.Events()[before.events:], model.EventConversationChanged, model.EventMessageReceived)
		return
	}
	// reuse, reuse_reopen, accept (core_check reopen)
	name := c.Expect.Conversation
	if c.Group == "core_check" {
		name = c.SelectorResult.Existing
	}
	want := w.convRef(name)
	prev := before.convs[want.ID]
	if msg.Ref.Conversation != want {
		t.Fatalf("message in %s, want %s (%s)", msg.Ref.Conversation.ID, want.ID, name)
	}
	reopen := c.Expect.Outcome == "reuse_reopen" || c.Group == "core_check"
	wantStatus := prev.Status
	if reopen {
		wantStatus = model.ConversationOpen
	}
	if got.Status != wantStatus {
		t.Fatalf("status %s -> %s, want %s", prev.Status, got.Status, wantStatus)
	}
	added := ingress.IsParticipant(got, w.sender.Ref) && !ingress.IsParticipant(prev, w.sender.Ref)
	if added != c.Expect.AddParticipant {
		t.Fatalf("sender added as participant = %v, want %v", added, c.Expect.AddParticipant)
	}
	if got.ReplyRoute != prev.ReplyRoute {
		t.Fatalf("reply route changed on reuse")
	}
	kinds := []string{model.EventMessageReceived}
	if reopen || added {
		kinds = []string{model.EventConversationChanged, model.EventMessageReceived}
	}
	requireKinds(t, w.Events()[before.events:], kinds...)
}

func requireKinds(t *testing.T, evs []model.Event, want ...string) {
	t.Helper()
	if got := eventKinds(evs); !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

// updateCase sends an edit targeting a message of the row's target
// conversation, or an unknown message when there is none.
func updateCase(t *testing.T, c lcCase) {
	w := newWorld(t, c)
	target := "unknown-target"
	if tc := c.TargetConversation; tc != nil {
		target = "target-" + c.ID
		w.seedConversation(w.inbox.Ref, tc.ID, tc.Status, t0.Add(-time.Hour), w.sender.Ref,
			[]model.Participant{{Identity: w.sender.Ref, Role: ingress.RoleContact}})
		msg := model.Message{
			Ref:      model.MessageRef{Conversation: w.convRef(tc.ID), ID: w.ID()},
			Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "original"}}},
			Audience: model.Audience{Visibility: model.Public}, External: []model.ExternalRef{model.ProviderMessageRef(w.Channel.Ref, target)},
			OccurredAt: t0.Add(-time.Hour), RecordedAt: t0.Add(-time.Hour), Revision: "r1",
		}
		w.Tx(func(tx store.Tx) error { return tx.PutMessage(w.Ctx, msg, "") })
	}
	before := w.state()
	idsBefore := len(w.Identities(w.inbox.Ref))
	env := w.Inbound(c.ID, target, "sender", dest)
	env.Kind, env.SourceRevision, env.OccurredAt = model.EnvelopeMessageEdited, "rev-2", t0
	env.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "edited"}}}
	_, err := w.Accept(env)
	after := w.state()
	if c.Expect.Outcome == "not_found" {
		requireCode(t, err, model.NotFound, "")
		requireUnchanged(t, before, after)
		if n := len(w.Identities(w.inbox.Ref)); n != idsBefore {
			t.Fatalf("quarantined update created identities: %d -> %d", idsBefore, n)
		}
		return
	}
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if len(after.convs) != len(before.convs) || len(after.msgs) != len(before.msgs) {
		t.Fatalf("update created conversations or messages")
	}
	m := w.MessageByProviderID(w.inbox.Ref, target)
	conv := w.convRef(c.Expect.Conversation)
	if m.Ref.Conversation != conv || m.Content.Parts[0].Text != "edited" {
		t.Fatalf("target %+v not edited in %s", m, conv.ID)
	}
	if got, prev := after.convs[conv.ID], before.convs[conv.ID]; got.Status != prev.Status || got.Revision != prev.Revision {
		t.Fatalf("update changed conversation %s: %s -> %s", conv.ID, prev.Status, got.Status)
	}
	requireKinds(t, w.Events()[before.events:], model.EventMessageUpdated)
}

// raceCase runs two concurrent first messages over the row's snapshot:
// both must land in one new conversation.
func raceCase(t *testing.T, c lcCase) {
	w := newWorld(t, c)
	before := w.state()
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := range 2 {
		env := w.envelope(c.ID + "-" + string(rune('a'+i)))
		go func() {
			<-start
			_, err := w.Accept(env)
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}
	after := w.state()
	var created []model.ID
	for id := range after.convs {
		if _, ok := before.convs[id]; !ok {
			created = append(created, id)
		}
	}
	if len(created) != 1 {
		t.Fatalf("%d conversations created, want exactly 1", len(created))
	}
	n := 0
	for _, m := range after.msgs {
		if m.Ref.Conversation.ID == created[0] {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d messages in the new conversation, want 2", n)
	}
}
