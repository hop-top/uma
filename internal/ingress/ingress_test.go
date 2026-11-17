package ingress_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/ingress"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Two goroutines delivering a contact's first messages at once resolve to
// one identity, one contact and one conversation.
func TestIngressConcurrentFirstContact(t *testing.T) {
	for _, lock := range []bool{false, true} {
		h := newHarness(t, chatType)
		in := h.Configure(perContact(lock), dest)
		const n = 8
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, n)
		for i := range n {
			env := h.Inbound("e"+string(rune('a'+i)), "m"+string(rune('a'+i)), "u1", dest)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := h.Accept(env)
				errs <- err
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatalf("lock=%v Accept: %v", lock, err)
			}
		}
		if ids, cs, convs, msgs := len(h.Identities(in.Ref)), len(h.Contacts()), h.Conversations(in.Ref), h.Messages(in.Ref); ids != 1 || cs != 1 || len(convs) != 1 || len(msgs) != n {
			t.Fatalf("lock=%v: identities %d contacts %d conversations %d messages %d; want 1 1 1 %d", lock, ids, cs, len(convs), len(msgs), n)
		}
	}
}

// Equal sender IDs in two inboxes of one account stay distinct identities,
// contacts and conversations.
func TestIngressEqualSenderIDsInTwoInboxes(t *testing.T) {
	h := newHarness(t, chatType)
	a := h.Configure(perContact(false), "sales")
	b := h.Configure(perContact(false), "support")
	h.MustAccept(h.Inbound("e1", "m1", "u1", "sales"))
	h.MustAccept(h.Inbound("e2", "m2", "u1", "support"))
	ia, ib := h.Identities(a.Ref), h.Identities(b.Ref)
	if len(ia) != 1 || len(ib) != 1 || ia[0].Ref == ib[0].Ref || ia[0].Contact == ib[0].Contact {
		t.Fatalf("identities %+v / %+v; want one distinct identity and contact per inbox", ia, ib)
	}
	if ca, cb := h.Conversations(a.Ref), h.Conversations(b.Ref); len(ca) != 1 || len(cb) != 1 {
		t.Fatalf("conversations %d / %d, want 1 / 1", len(ca), len(cb))
	}
	if n := len(h.Contacts()); n != 2 {
		t.Fatalf("contacts = %d, want 2: ingress never links", n)
	}
}

// Two senders in one thread share a conversation; one sender in two
// threads gets two.
func TestIngressThreadMembership(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(model.Selection{Mode: model.ModePerThread}, dest)
	t1, t2 := h.Ref(model.RefKindThread, "t1", "room-1"), h.Ref(model.RefKindThread, "t2", "room-1")
	h.MustAccept(h.Inbound("e1", "m1", "alice", dest, t1))
	h.MustAccept(h.Inbound("e2", "m2", "bob", dest, t1))
	h.MustAccept(h.Inbound("e3", "m3", "alice", dest, t2))
	convs := h.Conversations(in.Ref)
	if len(convs) != 2 {
		t.Fatalf("conversations = %d, want 2", len(convs))
	}
	byThread := map[string]model.Conversation{}
	for _, c := range convs {
		if c.Owner != nil || len(c.External) != 1 || c.ReplyRoute != c.External[0] {
			t.Fatalf("thread conversation %+v: want no owner, thread ref as external and route", c)
		}
		byThread[c.External[0].ID] = c
	}
	if n := len(byThread["t1"].Participants); n != 2 {
		t.Fatalf("t1 participants = %d, want alice and bob", n)
	}
	if n := len(byThread["t2"].Participants); n != 1 {
		t.Fatalf("t2 participants = %d, want alice", n)
	}
	// Same thread ID under another room is another thread.
	h.MustAccept(h.Inbound("e4", "m4", "alice", dest, h.Ref(model.RefKindThread, "t1", "room-2")))
	if n := len(h.Conversations(in.Ref)); n != 3 {
		t.Fatalf("conversations = %d, want 3", n)
	}
}

func TestIngressRoomAndWorkItemModes(t *testing.T) {
	for _, tc := range []struct{ mode, kind string }{
		{model.ModePerRoom, model.RefKindRoom}, {model.ModePerWorkItem, model.RefKindWorkItem},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			h := newHarness(t, chatType)
			in := h.Configure(model.Selection{Mode: tc.mode}, dest)
			scope := h.Ref(tc.kind, "x1", "")
			h.MustAccept(h.Inbound("e1", "m1", "alice", dest, scope))
			h.MustAccept(h.Inbound("e2", "m2", "bob", dest, scope))
			convs := h.Conversations(in.Ref)
			if len(convs) != 1 || len(convs[0].Participants) != 2 {
				t.Fatalf("conversations %+v; want one shared by both senders", convs)
			}
			// A resolved work item or room keeps its binding: no new conversation, no reopen.
			c := convs[0]
			prev := c.Revision
			c.Status, c.Revision = model.ConversationResolved, "r-resolved"
			h.Tx(func(tx store.Tx) error { return tx.PutConversation(h.Ctx, c, prev) })
			h.MustAccept(h.Inbound("e3", "m3", "carol", dest, scope))
			convs = h.Conversations(in.Ref)
			if len(convs) != 1 || convs[0].Status != model.ConversationResolved {
				t.Fatalf("after resolution: %+v; want the same resolved conversation", convs)
			}
			_, err := h.Accept(h.Inbound("e4", "m4", "alice", dest))
			requireCode(t, err, model.Validation, "requires a "+tc.kind+" reference")
		})
	}
}

func TestIngressDuplicateReusesReceipt(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	env := h.Inbound("e1", "m1", "u1", dest)
	first := h.MustAccept(env)
	if !first.Committed || first.Durable || first.Duplicate || first.ID == "" {
		t.Fatalf("first receipt %+v; want committed, not durable (memory store), new", first)
	}
	evs := len(h.Events())
	again := h.MustAccept(env)
	if !again.Duplicate || again.ID != first.ID || !again.Committed {
		t.Fatalf("redelivery receipt %+v; want duplicate of %s", again, first.ID)
	}
	if n := len(h.Messages(in.Ref)); n != 1 {
		t.Fatalf("messages = %d, want 1", n)
	}
	if n := len(h.Events()); n != evs {
		t.Fatalf("redelivery appended %d events", n-evs)
	}
	// Same event ID with another payload is still the same event identity.
	env.Content.Parts[0].Text = "replayed differently"
	if r := h.MustAccept(env); !r.Duplicate {
		t.Fatalf("receipt %+v, want duplicate", r)
	}
}

// Distinct revisions of one message are distinct events, even without
// provider event IDs; a redelivered revision is a duplicate.
func TestIngressRevisionsAreNotDeduplicated(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	orig := h.Inbound("", "m1", "u1", dest)
	orig.SourceRevision = "1"
	h.MustAccept(orig)
	edit := func(rev string, at time.Time) model.Envelope {
		env := h.Inbound("", "m1", "u1", dest)
		env.Kind, env.SourceRevision, env.OccurredAt = model.EnvelopeMessageEdited, rev, at
		env.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "rev " + rev}}}
		return env
	}
	r2 := h.MustAccept(edit("2", t0.Add(time.Minute)))
	r3 := h.MustAccept(edit("3", t0.Add(2*time.Minute)))
	if r2.Duplicate || r3.Duplicate || r2.ID == r3.ID {
		t.Fatalf("revision receipts %+v %+v; want two new events", r2, r3)
	}
	if r := h.MustAccept(edit("3", t0.Add(2*time.Minute))); !r.Duplicate || r.ID != r3.ID {
		t.Fatalf("redelivered revision receipt %+v, want duplicate of %s", r, r3.ID)
	}
	m := h.MessageByProviderID(in.Ref, "m1")
	if m.Content.Parts[0].Text != "rev 3" {
		t.Fatalf("content %q, want rev 3", m.Content.Parts[0].Text)
	}
	updated := 0
	for _, ev := range h.Events() {
		if ev.Kind == model.EventMessageUpdated {
			updated++
		}
	}
	if updated != 2 {
		t.Fatalf("message.updated events = %d, want 2", updated)
	}
}

// An update before its message is quarantined without a trace and applies
// on redelivery; an older edit after a newer one keeps the newer content.
func TestIngressReordering(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	edit := h.Inbound("e-edit", "m1", "u1", dest)
	edit.Kind, edit.SourceRevision, edit.OccurredAt = model.EnvelopeMessageEdited, "2", t0.Add(time.Minute)
	edit.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "edited"}}}
	r := &queueReceiver{Queue: []model.Ingress{{Envelope: edit}}}
	requireCode(t, r.Run(h.Ctx, h.Sink), model.NotFound, "target message not found")
	if len(r.Queue) != 1 || len(h.Identities(in.Ref)) != 0 || len(h.Events()) != 0 {
		t.Fatalf("quarantined update left state or left the queue")
	}
	r.Queue = append([]model.Ingress{{Envelope: h.Inbound("e-orig", "m1", "u1", dest)}}, r.Queue...)
	if err := r.Run(h.Ctx, h.Sink); err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if got := h.MessageByProviderID(in.Ref, "m1").Content.Parts[0].Text; got != "edited" {
		t.Fatalf("content %q after in-order redelivery, want edited", got)
	}
	stale := h.Inbound("e-stale", "m1", "u1", dest)
	stale.Kind, stale.SourceRevision, stale.OccurredAt = model.EnvelopeMessageEdited, "1b", t0.Add(30*time.Second)
	stale.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "stale"}}}
	evs := len(h.Events())
	if rc := h.MustAccept(stale); !rc.Committed || rc.Duplicate {
		t.Fatalf("stale receipt %+v; want committed evidence", rc)
	}
	if got := h.MessageByProviderID(in.Ref, "m1").Content.Parts[0].Text; got != "edited" {
		t.Fatalf("stale edit overwrote newer content: %q", got)
	}
	if n := len(h.Events()); n != evs {
		t.Fatalf("stale edit emitted %d events", n-evs)
	}
	var rec store.IngressRecord
	h.View(func(r store.Reader) error {
		var err error
		rec, err = r.Ingress(h.Ctx, h.MustAccept(stale).ID)
		return err
	})
	if rec.Input.Envelope.Content.Parts[0].Text != "stale" {
		t.Fatalf("stale input not retained as evidence")
	}
}

func TestIngressDeleteAndReaction(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	react := h.Inbound("e2", "m1", "u2", dest)
	react.Kind = model.EnvelopeReactionAdded
	react.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "+1"}}}
	h.MustAccept(react)
	del := h.Inbound("e3", "m1", "u1", dest)
	del.Kind = model.EnvelopeMessageDeleted
	h.MustAccept(del)
	m := h.MessageByProviderID(in.Ref, "m1")
	if !m.Deleted || m.Redacted || len(m.Content.Parts) != 0 || m.LastEdit != nil {
		t.Fatalf("deleted message %+v", m)
	}
	// A deleted message takes no further edit or deletion.
	edit := h.Inbound("e3b", "m1", "u1", dest)
	edit.Kind, edit.OccurredAt = model.EnvelopeMessageEdited, edit.OccurredAt.Add(time.Hour)
	h.MustAccept(edit)
	again := h.Inbound("e3c", "m1", "u1", dest)
	again.Kind = model.EnvelopeMessageDeleted
	h.MustAccept(again)
	if got := h.MessageByProviderID(in.Ref, "m1"); got.Revision != m.Revision || len(got.Content.Parts) != 0 {
		t.Fatalf("deleted message changed: %+v", got)
	}
	evs := h.Events()
	var changes []string
	for _, ev := range evs {
		if ev.Kind != model.EventMessageUpdated {
			continue
		}
		var p ingress.MessageUpdatedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		changes = append(changes, p.Change)
		if p.Change == ingress.ChangeReactionAdded && (p.Reaction == nil || p.Reaction.Emoji != "+1" ||
			p.Reaction.By == nil || p.Reaction.By.Inbox != in.Ref || p.Reaction.Count != 0) {
			t.Fatalf("reaction payload %+v", p)
		}
	}
	if len(changes) != 2 || changes[0] != ingress.ChangeReactionAdded || changes[1] != ingress.ChangeDeleted {
		t.Fatalf("message.updated changes %v", changes)
	}
	bad := h.Inbound("e4", "m1", "u2", dest)
	bad.Kind = model.EnvelopeReactionAdded
	bad.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "two words"}}}
	_, err := h.Accept(bad)
	requireCode(t, err, model.Validation, "emoji")
}

// A system actor needs no contact: system messages in sender-independent
// modes and status events create no identity or contact.
func TestIngressSystemActor(t *testing.T) {
	h := newHarness(t, chatType)
	room := h.Configure(model.Selection{Mode: model.ModePerRoom}, "room")
	contact := h.Configure(perContact(false), dest)
	sys := h.Inbound("e1", "m1", "", "room", h.Ref(model.RefKindRoom, "r1", ""))
	sys.Sender, sys.SystemActor = model.ExternalRef{}, true
	h.MustAccept(sys)
	msgs := h.Messages(room.Ref)
	if len(msgs) != 1 || msgs[0].Author.Kind != model.AuthorKindSystem || msgs[0].Author.Identity != (model.IdentityRef{}) {
		t.Fatalf("system message %+v", msgs)
	}
	status := model.Envelope{EventID: "e2", Destination: dest, Kind: model.EnvelopeStatus, SystemActor: true}
	if r := h.MustAccept(status); !r.Committed {
		t.Fatalf("status receipt %+v", r)
	}
	if len(h.Identities(room.Ref))+len(h.Identities(contact.Ref))+len(h.Contacts()) != 0 {
		t.Fatal("system actor manufactured an identity or contact")
	}
	if len(h.Conversations(contact.Ref)) != 0 || len(h.Messages(contact.Ref)) != 0 {
		t.Fatal("status event created a conversation or message")
	}
	perContactSys := sys
	perContactSys.EventID, perContactSys.Destination = "e3", dest
	_, err := h.Accept(perContactSys)
	requireCode(t, err, model.Validation, "requires a sender")
	withSender := sys
	withSender.EventID, withSender.Sender = "e4", h.SenderRef("u1")
	_, err = h.Accept(withSender)
	requireCode(t, err, model.Validation, "system actor carries no sender")
}

func TestIngressDestinationResolution(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), "a", "shared")
	// ConfigureInbox refuses a bound destination; seed the ambiguity directly.
	h.PutInbox(h.Channel.Ref, perContact(false), false, "b", "shared")
	h.PutInbox(h.Channel.Ref, perContact(false), true, "off")
	h.PutInbox(h.Channel.Ref, model.Selection{Mode: model.ModePerAgentContext}, false, "ctx")
	cases := map[string]struct {
		dest string
		code model.ErrorCode
	}{
		"unbound":       {"nowhere", model.NotFound},
		"ambiguous":     {"shared", model.Conflict},
		"disabled":      {"off", model.Disabled},
		"agent context": {"ctx", model.Validation},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.Accept(h.Inbound("e-"+name, "m-"+name, "u1", tc.dest))
			requireCode(t, err, tc.code, "")
		})
	}
	if len(h.Contacts()) != 0 || len(h.Events()) != 0 {
		t.Fatal("rejected destinations left state")
	}
	// No event identity was claimed: once the destination is bound, the
	// same event is new.
	h.Configure(perContact(false), "nowhere")
	if r := h.MustAccept(h.Inbound("e-unbound", "m-unbound", "u1", "nowhere")); r.Duplicate {
		t.Fatal("rejected ingress left its event identity claimed")
	}
}

// The sink accepts only references of its bound channel and sender
// namespaces its adapter declares.
func TestIngressBindingChecks(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), dest)
	foreign := model.ChannelRef{Tenant: tenantA, ID: h.ID()}
	mut := map[string]func(*model.Envelope){
		"foreign sender channel": func(e *model.Envelope) { e.Sender.Channel = foreign },
		"foreign reference": func(e *model.Envelope) {
			e.References = []model.ExternalRef{{Channel: foreign, Namespace: ingressNS, Kind: "room", ID: "r"}}
		},
		"undeclared namespace": func(e *model.Envelope) { e.Sender.Namespace = "org.other.user" },
		"unknown kind":         func(e *model.Envelope) { e.Kind = "message.created" },
		"no destination":       func(e *model.Envelope) { e.Destination = "" },
		"no identity":          func(e *model.Envelope) { e.EventID, e.MessageID = "", "" },
		"no sender":            func(e *model.Envelope) { e.Sender = model.ExternalRef{} },
		"unknown visibility":   func(e *model.Envelope) { e.Audience.Visibility = "secret" },
		"empty content":        func(e *model.Envelope) { e.Content = model.Content{} },
		"edit without target":  func(e *model.Envelope) { e.Kind, e.MessageID = model.EnvelopeMessageEdited, "" },
	}
	for name, m := range mut {
		t.Run(name, func(t *testing.T) {
			env := h.Inbound("e1", "m1", "u1", dest)
			m(&env)
			_, err := h.Accept(env)
			requireCode(t, err, model.Validation, "")
		})
	}
	interaction := h.Inbound("e2", "m2", "u1", dest)
	interaction.Interaction = &model.InteractionInput{ID: "i1"}
	_, err := h.Accept(interaction)
	requireCode(t, err, model.Validation, "only an interaction carries one")
	bound := h.Inbound("e3", "m3", "u1", dest)
	bound.Task = &model.TaskBinding{Open: true}
	_, err = h.Accept(bound)
	requireCode(t, err, model.Validation, "per-agent-context")
	kind := h.Inbound("e4", "", "u1", dest)
	kind.Kind, kind.Content = model.EnvelopeInteraction, model.Content{}
	_, err = h.Accept(kind)
	requireCode(t, err, model.Validation, "interaction")
	if len(h.Contacts()) != 0 {
		t.Fatal("rejected ingress left state")
	}

	_, err = ingress.Bind(h.Ctx, h.E, model.Principal{Tenant: tenantB, Actor: serviceAct}, h.Channel.Ref)
	requireCode(t, err, model.NotFound, "")
	_, err = ingress.Bind(h.Ctx, h.E, h.Service(), foreign)
	requireCode(t, err, model.NotFound, "")
	unregistered := h.PutChannel("com.example.unregistered.chat")
	_, err = ingress.Bind(h.Ctx, h.E, h.Service(), unregistered.Ref)
	requireCode(t, err, model.Unsupported, "")
}

// Domain events are appended in the ingress commit, caused by the ingress
// record and attributed to the service principal.
func TestIngressOutbox(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	r := h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	evs := h.Events()
	requireKinds(t, evs, model.EventConversationChanged, model.EventMessageReceived)
	for _, ev := range evs {
		if ev.Causation != r.ID || ev.Actor != serviceAct || ev.Inbox != in.Ref.ID || ev.Correlation != "corr-e1" ||
			ev.Payload.Version != model.PayloadVersion1 || ev.Version != model.EventEnvelopeVersion {
			t.Fatalf("event envelope %+v", ev)
		}
	}
	var changed ingress.ConversationChangedPayload
	var received ingress.MessageReceivedPayload
	if err := json.Unmarshal(evs[0].Payload.Data, &changed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(evs[1].Payload.Data, &received); err != nil {
		t.Fatal(err)
	}
	if len(changed.Changes) != 1 || changed.Changes[0] != ingress.ChangeCreated || changed.Trigger != model.TriggerProvider {
		t.Fatalf("conversation.changed payload %+v", changed)
	}
	if !received.ConversationCreated || received.Author == nil || received.Source != chatType ||
		received.Message.Conversation != changed.Conversation || evs[1].Payload.Name != model.SchemaMessageReceived {
		t.Fatalf("message.received payload %+v", received)
	}
	var raw map[string]any
	if err := json.Unmarshal(evs[1].Payload.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, leaked := raw["content"]; leaked {
		t.Fatal("payload carries message content")
	}
	// Reopen emits the status change.
	c := h.Conversations(in.Ref)[0]
	prev := c.Revision
	c.Status, c.Revision = model.ConversationResolved, "r-resolved"
	h.Tx(func(tx store.Tx) error { return tx.PutConversation(h.Ctx, c, prev) })
	h.MustAccept(h.Inbound("e2", "m2", "u1", dest))
	evs = h.Events()[2:]
	requireKinds(t, evs, model.EventConversationChanged, model.EventMessageReceived)
	if err := json.Unmarshal(evs[0].Payload.Data, &changed); err != nil {
		t.Fatal(err)
	}
	if changed.Status == nil || changed.Status.From != model.ConversationResolved || changed.Status.To != model.ConversationOpen ||
		changed.PreviousRevision != "r-resolved" {
		t.Fatalf("reopen payload %+v", changed)
	}
}

// durableStore reports a durable backend over the memory store.
type durableStore struct{ store.Store }

func (d durableStore) Properties() store.Properties {
	p := d.Store.Properties()
	p.Durable = true
	return p
}

func TestIngressReceiptReportsDurability(t *testing.T) {
	h := newHarness(t, chatType, func(c *engine.Config) { c.Store = durableStore{c.Store} })
	h.Configure(perContact(false), dest)
	if r := h.MustAccept(h.Inbound("e1", "m1", "u1", dest)); !r.Durable || !r.Committed {
		t.Fatalf("receipt %+v; want committed and durable", r)
	}
}

func TestIngressAfterClose(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(false), dest)
	if err := h.E.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := h.Accept(h.Inbound("e1", "m1", "u1", dest))
	requireCode(t, err, model.Unavailable, "engine closed")
	_, err = ingress.Bind(h.Ctx, h.E, h.Service(), h.Channel.Ref)
	requireCode(t, err, model.Unavailable, "engine closed")
}

// An adapter receiver delivers through the engine-bound sink; Run stops on
// the first rejection and the ingress stays queued.
func TestIngressReceiverRun(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(false), dest)
	inst, err := ingressAdapter{Typ: chatType}.Open(h.Ctx, h.Channel, adapter.Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	rcv := inst.(*queueReceiver)
	rcv.Queue = []model.Ingress{
		{Envelope: h.Inbound("e1", "m1", "u1", dest)},
		{Envelope: h.Inbound("e2", "m2", "u2", dest)},
		{Envelope: h.Inbound("e3", "m3", "u3", "nowhere")},
	}
	requireCode(t, rcv.Run(h.Ctx, h.Sink), model.NotFound, "")
	if len(rcv.Queue) != 1 || len(h.Conversations(in.Ref)) != 2 {
		t.Fatalf("queue %d conversations %d; want 1 queued, 2 committed", len(rcv.Queue), len(h.Conversations(in.Ref)))
	}
}

// Header matches are inbox-wide, independent of sender: a CC'd party
// replying to another sender's mail joins that conversation as a
// participant, with the reply route unchanged; a msg-id stored in two
// conversations stays a conflict.
func TestIngressEmailReplyFromAnotherSender(t *testing.T) {
	h := newHarness(t, model.TypeSmtpMail)
	in := h.Configure(perContact(false), dest)
	mail := func(event, from string, hdr model.EmailHeaders) model.Envelope {
		n := hdr.Normalize(h.Channel.Ref)
		env := h.Inbound(event, n.MessageID, from, dest)
		env.References = n.References
		return env
	}
	h.MustAccept(mail("e1", "alice", model.EmailHeaders{MessageID: "<a1@a.example>"}))
	first := h.Conversations(in.Ref)
	if len(first) != 1 {
		t.Fatalf("conversations = %d, want 1", len(first))
	}
	h.MustAccept(mail("e2", "bob", model.EmailHeaders{MessageID: "<b1@b.example>", InReplyTo: "<a1@a.example>"}))
	convs := h.Conversations(in.Ref)
	if len(convs) != 1 {
		t.Fatalf("conversations = %d after bob's reply, want 1", len(convs))
	}
	c := convs[0]
	bob := h.MessageByProviderID(in.Ref, "<b1@b.example>")
	if bob.Ref.Conversation != c.Ref || len(c.Participants) != 2 || !ingress.IsParticipant(c, bob.Author.Identity) {
		t.Fatalf("bob's reply %+v not joined to %+v as participant", bob.Ref, c)
	}
	if c.ReplyRoute != first[0].ReplyRoute || *c.Owner != *first[0].Owner {
		t.Fatal("reply route or owner changed on a CC reply")
	}

	// Same msg-id stored in two conversations: conflict, nothing written.
	h.MustAccept(mail("e3", "carol", model.EmailHeaders{MessageID: "<c1@c.example>"}))
	var dup model.Conversation
	for _, x := range h.Conversations(in.Ref) {
		if x.Ref != c.Ref {
			dup = x
		}
	}
	msg := model.Message{
		Ref:      model.MessageRef{Conversation: dup.Ref, ID: h.ID()},
		Content:  model.Content{Parts: []model.Part{{Kind: "text", Text: "copy"}}},
		Audience: model.Audience{Visibility: model.Public}, External: []model.ExternalRef{model.ProviderMessageRef(h.Channel.Ref, "<a1@a.example>")},
		RecordedAt: t0, Revision: "r1",
	}
	h.Tx(func(tx store.Tx) error { return tx.PutMessage(h.Ctx, msg, "") })
	before := len(h.Messages(in.Ref))
	_, err := h.Accept(mail("e4", "bob", model.EmailHeaders{MessageID: "<b2@b.example>", InReplyTo: "<a1@a.example>"}))
	requireCode(t, err, model.Conflict, "")
	if n := len(h.Messages(in.Ref)); n != before {
		t.Fatalf("conflicting reply stored a message")
	}
}
