package ingress_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

func interactionsOf(h *harness, in model.InboxRef) []model.Interaction {
	return all(h, store.Reader.Interactions, model.Filter{Inboxes: []model.InboxRef{in}})
}

func tokenOf(h *harness, ref model.InteractionRef) store.ResponseToken {
	h.T.Helper()
	var tok store.ResponseToken
	h.View(func(r store.Reader) error {
		var err error
		tok, err = r.InteractionToken(h.Ctx, ref)
		return err
	})
	return tok
}

func receivedPayload(t *testing.T, ev model.Event) model.InteractionReceivedPayload {
	t.Helper()
	var p model.InteractionReceivedPayload
	if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// An accepted interaction is one interaction record with its token beside
// it, one interaction.received event and a created conversation: no
// message, no message.received.
func TestInteractionAccepted(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	env := h.Interaction("i1", "u1", dest)
	r := h.MustAccept(env)
	if !r.Committed || r.Duplicate || r.ID == "" {
		t.Fatalf("receipt %+v, want committed, not duplicate", r)
	}
	if n := len(h.Messages(in.Ref)); n != 0 {
		t.Fatalf("messages = %d, want none", n)
	}
	convs, ids := h.Conversations(in.Ref), h.Identities(in.Ref)
	if len(convs) != 1 || len(ids) != 1 {
		t.Fatalf("conversations %d identities %d, want 1 1", len(convs), len(ids))
	}
	evs := h.Events()
	requireKinds(t, evs, model.EventConversationChanged, model.EventInteractionReceived)

	recs := interactionsOf(h, in.Ref)
	if len(recs) != 1 {
		t.Fatalf("interactions = %d, want 1", len(recs))
	}
	rec := recs[0]
	switch {
	case rec.Ref.Conversation != convs[0].Ref || rec.Ref.ID == "":
		t.Fatalf("ref %+v, want the created conversation", rec.Ref)
	case rec.Kind != model.InteractionAction || rec.Action != "approve":
		t.Fatalf("kind %q action %q", rec.Kind, rec.Action)
	case rec.Actor.Identity != ids[0].Ref || rec.Actor.Kind != model.ContactKindPerson || rec.Actor.External != env.Sender:
		t.Fatalf("actor %+v, want the resolved person identity", rec.Actor)
	case rec.Source != chatType || rec.Audience != env.Audience:
		t.Fatalf("source %q audience %+v", rec.Source, rec.Audience)
	case !rec.Deadline.Equal(t0.Add(3*time.Second)) || !rec.RespondUntil.Equal(t0.Add(time.Hour)):
		t.Fatalf("deadline %v respond until %v", rec.Deadline, rec.RespondUntil)
	case !rec.OccurredAt.Equal(t0) || !rec.RecordedAt.Equal(t0) || rec.Revision == "":
		t.Fatalf("times/revision %+v", rec)
	case rec.Fields.Name != "com.example.form" || string(rec.Fields.Data) != `{"choice":"yes"}`:
		t.Fatalf("fields %+v", rec.Fields)
	case !slices.Contains(rec.External, model.ProviderInteractionRef(h.Channel.Ref, "i1")):
		t.Fatalf("external %+v, want the provider interaction ref", rec.External)
	case rec.Message != nil || rec.MessageExternal != nil || rec.Redacted:
		t.Fatalf("source message %+v / %+v redacted %v on an interaction naming none", rec.Message, rec.MessageExternal, rec.Redacted)
	}
	if tok := tokenOf(h, rec.Ref); tok.Token != "tok-i1" || !tok.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("token %+v, want the response token with its expiry", tok)
	}

	ev := evs[1]
	if ev.Resource != rec.Ref.Ref() || ev.Audience != env.Audience || ev.Causation != r.ID || ev.Revision != rec.Revision {
		t.Fatalf("event %+v, want about the record under the envelope audience, caused by the ingress", ev)
	}
	p := receivedPayload(t, ev)
	want := model.InteractionReceivedPayload{
		Interaction: rec.Ref, Trigger: model.TriggerProvider, Source: chatType, Kind: model.InteractionAction,
		Actor: ids[0].Ref, ActorKind: model.ContactKindPerson, Action: "approve", ConversationCreated: true,
	}
	if p != want {
		t.Fatalf("payload %+v, want %+v", p, want)
	}
	for _, leak := range []string{"tok-i1", "choice"} {
		if strings.Contains(string(ev.Payload.Data), leak) {
			t.Fatalf("payload %s leaks %q", ev.Payload.Data, leak)
		}
	}
}

// The actor kind of the event and the record is the resolved identity's.
func TestInteractionActorKind(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	env := h.Interaction("i1", "bot-1", dest)
	env.SenderKind = model.ContactKindBot
	h.MustAccept(env)
	rec := interactionsOf(h, in.Ref)[0]
	if rec.Actor.Kind != model.ContactKindBot {
		t.Fatalf("actor kind %q, want bot", rec.Actor.Kind)
	}
	evs := h.Events()
	if p := receivedPayload(t, evs[len(evs)-1]); p.ActorKind != model.ContactKindBot {
		t.Fatalf("payload actor kind %q, want bot", p.ActorKind)
	}
	// The kind claimed first wins: the same sender reported as a person.
	again := h.Interaction("i2", "bot-1", dest)
	h.MustAccept(again)
	if recs := interactionsOf(h, in.Ref); len(recs) != 2 || recs[1].Actor.Kind != model.ContactKindBot {
		t.Fatalf("second interaction actor kind %+v, want the first claim's kind", recs)
	}
}

// The response token is stored beside the record only: the raw ingress
// copy never holds it, and the caller's value is not touched.
func TestInteractionTokenOnlyInRecord(t *testing.T) {
	h := newHarness(t, chatType)
	h.Configure(perContact(true), dest)
	env := h.Interaction("i1", "u1", dest)
	r := h.MustAccept(env)
	if env.Interaction.ResponseToken != "tok-i1" {
		t.Fatal("Accept modified the caller's envelope")
	}
	var raw store.IngressRecord
	h.View(func(rd store.Reader) error {
		var err error
		raw, err = rd.Ingress(h.Ctx, r.ID)
		return err
	})
	got := raw.Input.Envelope.Interaction
	if got == nil || got.ID != "i1" || got.Action != "approve" || string(got.Fields.Data) != `{"choice":"yes"}` {
		t.Fatalf("raw copy %+v, want the interaction kept", got)
	}
	if got.ResponseToken != "" {
		t.Fatal("raw ingress copy holds the response token")
	}
}

// A redelivery is a duplicate with the first receipt: one record, one
// event, one conversation.
func TestInteractionDuplicate(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	env := h.Interaction("i1", "u1", dest)
	first := h.MustAccept(env)
	events := len(h.Events())
	again := h.MustAccept(env)
	if !again.Duplicate || again.ID != first.ID {
		t.Fatalf("redelivery %+v, want a duplicate of %+v", again, first)
	}
	if n := len(interactionsOf(h, in.Ref)); n != 1 {
		t.Fatalf("interactions = %d, want 1", n)
	}
	if n := len(h.Events()); n != events {
		t.Fatalf("events %d after a redelivery, want %d", n, events)
	}
	if n := len(h.Conversations(in.Ref)); n != 1 {
		t.Fatalf("conversations = %d, want 1", n)
	}
	// Another provider interaction ID is another interaction.
	h.MustAccept(h.Interaction("i2", "u1", dest))
	if n := len(interactionsOf(h, in.Ref)); n != 2 {
		t.Fatalf("interactions = %d, want 2", n)
	}
}

// A deadline already behind the clock is still accepted and recorded; the
// adapter answers the protocol itself.
func TestInteractionDeadlinePassed(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	env := h.Interaction("i1", "u1", dest)
	env.Interaction.Deadline = t0.Add(-time.Minute)
	env.Interaction.ResponseExpiresAt = t0.Add(-time.Second)
	if r := h.MustAccept(env); !r.Committed {
		t.Fatalf("receipt %+v, want committed", r)
	}
	recs := interactionsOf(h, in.Ref)
	if len(recs) != 1 || !recs[0].Deadline.Equal(t0.Add(-time.Minute)) {
		t.Fatalf("interactions %+v, want the late one recorded", recs)
	}
}

// An interaction is scoped as a message: per-contact by its actor, other
// modes by the references.
func TestInteractionSelection(t *testing.T) {
	t.Run("per contact by actor", func(t *testing.T) {
		h := newHarness(t, chatType)
		in := h.Configure(perContact(true), dest)
		h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
		h.MustAccept(h.Interaction("i1", "u1", dest))
		h.MustAccept(h.Interaction("i2", "u2", dest))
		convs := h.Conversations(in.Ref)
		if len(convs) != 2 {
			t.Fatalf("conversations = %d, want one per contact", len(convs))
		}
		recs := interactionsOf(h, in.Ref)
		if len(recs) != 2 || recs[0].Ref.Conversation == recs[1].Ref.Conversation {
			t.Fatalf("interactions %+v, want one per contact conversation", recs)
		}
		if msgs := h.Messages(in.Ref); len(msgs) != 1 || msgs[0].Ref.Conversation != recs[0].Ref.Conversation {
			t.Fatalf("the interaction did not join its contact's conversation: %+v", msgs)
		}
	})
	t.Run("per room by reference", func(t *testing.T) {
		h := newHarness(t, chatType)
		in := h.Configure(model.Selection{Mode: model.ModePerRoom}, dest)
		r1, r2 := h.Ref(model.RefKindRoom, "r1", ""), h.Ref(model.RefKindRoom, "r2", "")
		h.MustAccept(h.Inbound("e1", "m1", "alice", dest, r1))
		h.MustAccept(h.Interaction("i1", "bob", dest, r1))
		h.MustAccept(h.Interaction("i2", "bob", dest, r2))
		recs := interactionsOf(h, in.Ref)
		if len(recs) != 2 || len(h.Conversations(in.Ref)) != 2 {
			t.Fatalf("interactions %d conversations %d, want 2 2", len(recs), len(h.Conversations(in.Ref)))
		}
		if msgs := h.Messages(in.Ref); recs[0].Ref.Conversation != msgs[0].Ref.Conversation {
			t.Fatal("interaction in room r1 did not join the room's conversation")
		}
	})
	t.Run("per thread without its reference", func(t *testing.T) {
		h := newHarness(t, chatType)
		in := h.Configure(model.Selection{Mode: model.ModePerThread}, dest)
		_, err := h.Accept(h.Interaction("i1", "u1", dest, h.Ref(model.RefKindRoom, "r1", "")))
		requireCode(t, err, model.Validation, "thread")
		if len(interactionsOf(h, in.Ref)) != 0 || len(h.Conversations(in.Ref)) != 0 || len(h.Events()) != 0 {
			t.Fatal("rejected interaction left state")
		}
	})
	t.Run("per agent context", func(t *testing.T) {
		h := newHarness(t, chatType)
		in := h.Configure(model.Selection{Mode: model.ModePerAgentContext}, dest)
		_, err := h.Accept(h.Interaction("i0", "u1", dest))
		requireCode(t, err, model.Validation, "context")
		ctxRef := h.Ref(model.RefKindContext, "c1", "")
		h.MustAccept(h.Interaction("i1", "u1", dest, ctxRef))
		h.MustAccept(h.Interaction("i2", "u2", dest, ctxRef))
		convs := h.Conversations(in.Ref)
		if len(convs) != 1 || convs[0].Owner == nil {
			t.Fatalf("conversations %+v, want one owned by the first sender", convs)
		}
		if n := len(interactionsOf(h, in.Ref)); n != 2 {
			t.Fatalf("interactions = %d, want 2", n)
		}
	})
}

// The built-in contact selector treats an interaction like the contact's
// message: it reopens a resolved conversation. The selector sees the
// interaction event class.
func TestInteractionSelectionEventClass(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	h.MustAccept(h.Inbound("e1", "m1", "u1", dest))
	conv := h.Conversations(in.Ref)[0]
	setStatus(h, conv, model.ConversationResolved)
	h.MustAccept(h.Interaction("i1", "u1", dest))
	if got := h.Conversations(in.Ref); len(got) != 1 || got[0].Status != model.ConversationOpen {
		t.Fatalf("conversations %+v, want the resolved one reopened", got)
	}

	var seen []policy.EventClass
	sel := recordingSelector{seen: &seen}
	h2 := newHarness(t, chatType, withSelector("rec", sel))
	h2.Configure(model.Selection{Mode: model.ModePerContact, LockToSingle: new(bool), LifecycleRules: model.RuleSetContactLifecycleV1, Policy: "rec"}, dest)
	h2.MustAccept(h2.Interaction("i1", "u1", dest))
	if len(seen) != 1 || seen[0] != policy.EventInteraction {
		t.Fatalf("selector event classes %v, want [interaction]", seen)
	}
}

// With a resolvable source message, an actor who owns or participates in
// its conversation joins it: no selection, no status change. Any other
// actor is selected by policy.
func TestInteractionSourceConversation(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(model.Selection{Mode: model.ModePerRoom}, dest)
	r1, r2 := h.Ref(model.RefKindRoom, "r1", ""), h.Ref(model.RefKindRoom, "r2", "")
	h.MustAccept(h.Inbound("e1", "m1", "alice", dest, r1))
	home := h.Conversations(in.Ref)[0]
	setStatus(h, home, model.ConversationResolved)
	src := model.ProviderMessageRef(h.Channel.Ref, "m1")
	events := len(h.Events())

	// A participant, naming another room: joins the source conversation.
	env := h.Interaction("i1", "alice", dest, r2)
	env.Interaction.Message = &src
	h.MustAccept(env)
	recs := interactionsOf(h, in.Ref)
	if len(recs) != 1 || recs[0].Ref.Conversation != home.Ref {
		t.Fatalf("participant interaction %+v, want the source conversation %v", recs, home.Ref)
	}
	srcMsg := h.MessageByProviderID(in.Ref, "m1")
	if recs[0].Message == nil || *recs[0].Message != srcMsg.Ref || recs[0].MessageExternal == nil || *recs[0].MessageExternal != src {
		t.Fatalf("source %+v / %+v, want the resolved local message and the provider's name", recs[0].Message, recs[0].MessageExternal)
	}
	if got := h.Conversations(in.Ref); len(got) != 1 || got[0].Status != model.ConversationResolved {
		t.Fatalf("conversations %+v, want the resolved one untouched", got)
	}
	requireKinds(t, h.Events()[events:], model.EventInteractionReceived)
	if p := receivedPayload(t, h.Events()[events]); p.Message == nil || *p.Message != srcMsg.Ref || p.ConversationCreated {
		t.Fatalf("payload %+v, want the resolved message, no creation", p)
	}

	// A stranger: selected by policy (room r2), source still recorded.
	env = h.Interaction("i2", "mallory", dest, r2)
	env.Interaction.Message = &src
	h.MustAccept(env)
	recs = interactionsOf(h, in.Ref)
	if len(recs) != 2 || recs[1].Ref.Conversation == home.Ref {
		t.Fatalf("stranger interaction %+v, want the room r2 conversation", recs)
	}
	if recs[1].Message == nil || *recs[1].Message != srcMsg.Ref {
		t.Fatalf("stranger source %+v, want the resolved message", recs[1].Message)
	}
	if len(h.Conversations(in.Ref)) != 2 {
		t.Fatal("stranger did not get a conversation by selection")
	}

	// An unknown source: selection, nothing resolved, the name kept.
	unknown := model.ProviderMessageRef(h.Channel.Ref, "nope")
	env = h.Interaction("i3", "alice", dest, r1)
	env.Interaction.Message = &unknown
	h.MustAccept(env)
	last := interactionsOf(h, in.Ref)[2]
	if last.Message != nil || last.MessageExternal == nil || *last.MessageExternal != unknown {
		t.Fatalf("unknown source %+v / %+v, want only the provider's name", last.Message, last.MessageExternal)
	}
}

// A sender-chosen message ID never resolves a source: a forged one could
// steer the interaction into another conversation.
func TestInteractionSourceNeedsProviderAssignedIDs(t *testing.T) {
	h := newHarness(t, "smtp.mail")
	in := h.Configure(model.Selection{Mode: model.ModePerRoom}, dest)
	r1, r2 := h.Ref(model.RefKindRoom, "r1", ""), h.Ref(model.RefKindRoom, "r2", "")
	h.MustAccept(h.Inbound("e1", "<m1@a.example>", "alice", dest, r1))
	src := model.ProviderMessageRef(h.Channel.Ref, "<m1@a.example>")
	env := h.Interaction("i1", "alice", dest, r2)
	env.Interaction.Message = &src
	h.MustAccept(env)
	recs := interactionsOf(h, in.Ref)
	if len(recs) != 1 || recs[0].Message != nil || recs[0].MessageExternal == nil {
		t.Fatalf("interactions %+v, want the source unresolved", recs)
	}
	if home := h.Conversations(in.Ref); len(home) != 2 {
		t.Fatalf("conversations = %d, want room r2 selected by policy", len(home))
	}
}

// Malformed, foreign and flattened interactions are rejected before any
// write, and nothing is left behind.
func TestInteractionRejects(t *testing.T) {
	h := newHarness(t, chatType)
	in := h.Configure(perContact(true), dest)
	foreign := model.ChannelRef{Tenant: tenantA, ID: "00000009-0000-7000-8000-000000000009"}
	ownOther := func(r model.ExternalRef) model.ExternalRef { r.Channel = foreign; return r }
	mut := map[string]func(*model.Envelope){
		"foreign bot": func(e *model.Envelope) {
			b := ownOther(e.Interaction.Actor)
			e.Interaction.Bot = &b
		},
		"foreign source": func(e *model.Envelope) {
			m := ownOther(e.Interaction.Actor)
			e.Interaction.Message = &m
		},
		"foreign context": func(e *model.Envelope) {
			c := ownOther(e.Interaction.Actor)
			e.Interaction.Context, e.References = []model.ExternalRef{c}, []model.ExternalRef{c}
		},
		"foreign actor":       func(e *model.Envelope) { e.Interaction.Actor = ownOther(e.Interaction.Actor) },
		"actor is not sender": func(e *model.Envelope) { e.Interaction.Actor = h.SenderRef("someone-else") },
		"event id differs":    func(e *model.Envelope) { e.EventID = "other" },
		"no event id":         func(e *model.Envelope) { e.EventID = "" },
		"message id":          func(e *model.Envelope) { e.MessageID = "m1" },
		"text content":        func(e *model.Envelope) { e.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "hi"}}} },
		"system actor": func(e *model.Envelope) {
			e.SystemActor, e.Sender = true, model.ExternalRef{}
		},
		"context not in references": func(e *model.Envelope) {
			e.Interaction.Context = []model.ExternalRef{h.Ref(model.RefKindRoom, "r9", "")}
		},
		"options kind": func(e *model.Envelope) { e.Interaction.Kind = model.InteractionOptions },
		"unknown kind": func(e *model.Envelope) { e.Interaction.Kind = "click" },
		"no action":    func(e *model.Envelope) { e.Interaction.Action = "" },
		"no deadline":  func(e *model.Envelope) { e.Interaction.Deadline = time.Time{} },
		"token no expiry": func(e *model.Envelope) {
			e.Interaction.ResponseExpiresAt = time.Time{}
		},
		"no audience":     func(e *model.Envelope) { e.Audience = model.Audience{} },
		"undeclared kind": func(e *model.Envelope) { e.SenderKind = model.ContactKindAgent + "x" },
		"work item":       func(e *model.Envelope) { e.WorkItem = &model.WorkItemState{} },
		"delivery":        func(e *model.Envelope) { e.Delivery = &model.DeliveryReport{State: model.Delivered} },
		"task":            func(e *model.Envelope) { e.Task = &model.TaskBinding{Open: true} },
		"no interaction":  func(e *model.Envelope) { e.Interaction = nil },
	}
	for name, m := range mut {
		t.Run(name, func(t *testing.T) {
			env := h.Interaction("i1", "u1", dest)
			m(&env)
			_, err := h.Accept(env)
			requireCode(t, err, model.Validation, "")
			if err != nil && strings.Contains(err.Error(), "tok-i1") {
				t.Fatalf("error %q echoes the token", err)
			}
		})
	}
	// A text-only envelope of kind interaction is not flattened into a message.
	flat := h.Inbound("e2", "", "u1", dest)
	flat.Kind = model.EnvelopeInteraction
	_, err := h.Accept(flat)
	requireCode(t, err, model.Validation, "")
	// Any other kind carrying an interaction is refused too.
	other := h.Inbound("e3", "m3", "u1", dest)
	other.Interaction = h.Interaction("i3", "u1", dest).Interaction
	_, err = h.Accept(other)
	requireCode(t, err, model.Validation, "interaction")

	if len(h.Contacts()) != 0 || len(h.Events()) != 0 || len(interactionsOf(h, in.Ref)) != 0 || len(h.Conversations(in.Ref)) != 0 {
		t.Fatal("rejected interaction left state")
	}
	// The record the rejects share is accepted once unmodified.
	h.MustAccept(h.Interaction("i1", "u1", dest))
}

// A denied channel.receive stops an interaction like any other ingress,
// and nothing is written.
func TestInteractionDeniedByPolicy(t *testing.T) {
	h, p := receiveHarness(t)
	in := h.Configure(perContact(true), dest)
	before := len(h.Events())
	p.denied.Store(true)
	_, err := h.Accept(h.Interaction("i1", "u1", dest))
	requireCode(t, err, model.Unauthorized, "denied by policy")
	if len(interactionsOf(h, in.Ref)) != 0 || len(h.Conversations(in.Ref)) != 0 || len(h.Events()) != before || len(h.Contacts()) != 0 {
		t.Fatal("denied interaction left state")
	}
	p.denied.Store(false)
	if r := h.MustAccept(h.Interaction("i1", "u1", dest)); r.Duplicate {
		t.Fatal("interaction accepted after the grant returned was a duplicate")
	}
}

// recordingSelector notes the event class it is asked about and always
// creates.
type recordingSelector struct{ seen *[]policy.EventClass }

func (s recordingSelector) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	*s.seen = append(*s.seen, in.Event)
	return policy.SelectionResult{CreateKey: "k"}, nil
}

// setStatus moves a stored conversation to status.
func setStatus(h *harness, c model.Conversation, status string) {
	h.T.Helper()
	prev := c.Revision
	c.Status, c.Revision = status, model.Revision(h.ID())
	h.Tx(func(tx store.Tx) error { return tx.PutConversation(h.Ctx, c, prev) })
}
