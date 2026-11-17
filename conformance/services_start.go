package conformance

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// startCases are the outbound-first conversation cases: Conversations.Start
// over email and messaging channels, its replay, selection rules, races
// with the recipient's first inbound message, and refusals.
var startCases = []struct {
	name string
	run  func(*testing.T, *startEnv)
}{
	{"EmailStartThenAnswer", startEmailThenAnswer},
	{"MessagingStartThenAnswer", startMessagingThenAnswer},
	{"ReplayAndConflict", startReplay},
	{"SelectionRules", startSelectionRules},
	{"RoomStart", startRoom},
	{"RaceWithFirstInbound", startRaceWithFirstInbound},
	{"ConcurrentRetries", startConcurrentRetries},
	{"Refusals", startRefusals},
}

// startedConversation checks the conversation a first start created: owned
// by the recipient's identity, routed to the recipient, open, holding the
// one outbound message, with its created and message events.
func startedConversation(t *testing.T, env *startEnv, acc model.Acceptance, in model.InboxRef, to model.ExternalRef) model.Conversation {
	t.Helper()
	if acc.Operation.State != model.Queued || acc.Replayed {
		t.Fatalf("Start = %+v, want a new queued operation", acc)
	}
	msgRef := *acc.Operation.ResultMessage
	if acc.Operation.Result == nil || *acc.Operation.Result != msgRef.Ref() || msgRef.Conversation.Inbox != in {
		t.Fatalf("result %+v / %+v does not name a message of the inbox", acc.Operation.Result, msgRef)
	}
	convs := env.conversations(in)
	ids := env.identities(in)
	if len(convs) != 1 || len(ids) != 1 {
		t.Fatalf("conversations %d, identities %d; want one each", len(convs), len(ids))
	}
	c, id := convs[0], ids[0]
	if c.Ref != msgRef.Conversation || c.Mode != model.ModePerContact || c.Status != model.ConversationOpen ||
		c.ReplyRoute != to || c.Owner == nil || *c.Owner != id.Ref {
		t.Fatalf("conversation %+v, want an open per-contact conversation owned by %v routed to %v", c, id.Ref, to)
	}
	if id.Namespace != to.Namespace || id.SourceID != to.ID || id.Kind != model.ContactKindPerson {
		t.Fatalf("identity %+v, want the recipient's", id)
	}
	msgs := env.messages(c.Ref)
	if len(msgs) != 1 || msgs[0].Ref != msgRef || msgs[0].Origin == nil || msgs[0].Origin.Operation != acc.Operation.Ref ||
		msgs[0].Audience.Visibility != model.Public || msgs[0].Content.Parts[0].Text != "hello" {
		t.Fatalf("messages %+v, want the started message", msgs)
	}
	changed := env.eventsFor(c.Ref.Ref())
	if len(changed) != 1 || changed[0].Kind != model.EventConversationChanged || changed[0].Actor != startActor {
		t.Fatalf("conversation events %+v, want one conversation.changed by the caller", changed)
	}
	var cp model.ConversationChangedPayload
	if err := json.Unmarshal(changed[0].Payload.Data, &cp); err != nil || cp.Trigger != model.TriggerCommand ||
		len(cp.Changes) != 1 || cp.Changes[0] != model.ConversationFieldCreated || cp.Operation == nil || *cp.Operation != acc.Operation.Ref {
		t.Fatalf("conversation.changed payload %+v (%v), want created by the start's command", cp, err)
	}
	created := env.eventsFor(msgRef.Ref())
	var mp model.MessageCreatedPayload
	if len(created) != 1 || json.Unmarshal(created[0].Payload.Data, &mp) != nil || created[0].Kind != model.EventMessageCreated ||
		mp.Origin != model.MessageOriginStart || !mp.ConversationCreated || mp.Operation != acc.Operation.Ref || mp.InReplyTo != nil {
		t.Fatalf("message events %+v payload %+v, want message.created origin start creating the conversation", created, mp)
	}
	return c
}

// delivered runs the start's operation and checks the one send: on the
// recipient's route, answering no message.
func delivered(t *testing.T, env *startEnv, a *startAdapter, acc model.Acceptance, to model.ExternalRef) string {
	t.Helper()
	before := len(a.sent())
	if op := env.deliver(acc.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("delivery = %+v, want provider_accepted", op)
	}
	sends := a.sent()
	if len(sends) != before+1 {
		t.Fatalf("sends = %d, want %d", len(sends), before+1)
	}
	s := sends[len(sends)-1]
	if s.Route != to || s.Target != nil || s.Thread != nil || s.Operation != acc.Operation.Ref || s.Content.Parts[0].Text != "hello" {
		t.Fatalf("send %+v, want the start's content on %v answering nothing", s, to)
	}
	return a.sentID(s)
}

// An email start creates the recipient's conversation; the answer, whose
// In-Reply-To names the sent Message-ID, lands in it. Without
// lock_to_single the next start opens a new thread.
func startEmailThenAnswer(t *testing.T, env *startEnv) {
	in := env.inbox(env.mail, "mail", startPerContact(false))
	to := env.recipient(env.mail, "ann@client.example")
	acc := env.start(in, to, "hello")
	c := startedConversation(t, env, acc, in, to)
	sentID := delivered(t, env, env.mail, acc, to)
	if err := env.accept(env.mail, env.inbound(env.mail, "mail", to.ID, sentID)); err != nil {
		t.Fatalf("Accept answer: %v", err)
	}
	if n := len(env.messages(c.Ref)); n != 2 {
		t.Fatalf("messages in the started conversation = %d, want 2: the answer threads on the sent Message-ID", n)
	}
	if n := len(env.identities(in)); n != 1 {
		t.Fatalf("identities = %d, want the recipient's one", n)
	}
	env.start(in, to, "hello")
	if n := len(env.conversations(in)); n != 2 {
		t.Fatalf("conversations after a second start = %d, want 2: each email start opens its thread", n)
	}
}

// A messaging start creates the recipient's conversation; the recipient's
// answer and the next start join it under lock_to_single.
func startMessagingThenAnswer(t *testing.T, env *startEnv) {
	in := env.inbox(env.chat, "chat", startPerContact(true))
	to := env.recipient(env.chat, "+15550100")
	acc := env.start(in, to, "hello")
	c := startedConversation(t, env, acc, in, to)
	delivered(t, env, env.chat, acc, to)
	if err := env.accept(env.chat, env.inbound(env.chat, "chat", to.ID, "")); err != nil {
		t.Fatalf("Accept answer: %v", err)
	}
	again := env.start(in, to, "hello")
	if again.Operation.ResultMessage.Conversation != c.Ref {
		t.Fatalf("second start in %v, want the open conversation %v", again.Operation.ResultMessage.Conversation, c.Ref)
	}
	var mp model.MessageCreatedPayload
	evs := env.eventsFor(again.Operation.ResultMessage.Ref())
	if len(evs) != 1 || json.Unmarshal(evs[0].Payload.Data, &mp) != nil || mp.ConversationCreated {
		t.Fatalf("second start events %+v payload %+v, want message.created without conversation_created", evs, mp)
	}
	if n, m := len(env.conversations(in)), len(env.messages(c.Ref)); n != 1 || m != 3 {
		t.Fatalf("conversations %d messages %d, want 1 and 3", n, m)
	}
	delivered(t, env, env.chat, again, to)
}

// A retried start returns the stored operation and writes nothing more;
// the same operation ID with other content conflicts.
func startReplay(t *testing.T, env *startEnv) {
	in := env.inbox(env.chat, "chat", startPerContact(true))
	to := env.recipient(env.chat, "+15550101")
	req := env.startReq("op-start", in, to, "hello")
	first, err := env.client.Conversations().Start(env.ctx, req)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	again, err := env.client.Conversations().Start(env.ctx, req)
	if err != nil || !again.Replayed || again.Operation.Ref != first.Operation.Ref || *again.Operation.ResultMessage != *first.Operation.ResultMessage {
		t.Fatalf("retry = %+v, %v; want the stored operation replayed", again, err)
	}
	if n, m := len(env.conversations(in)), len(env.messages(first.Operation.ResultMessage.Conversation)); n != 1 || m != 1 {
		t.Fatalf("conversations %d messages %d after a retry, want 1 and 1", n, m)
	}
	changed := env.startReq("op-start", in, to, "other")
	if _, err := env.client.Conversations().Start(env.ctx, changed); !model.IsCode(err, model.Conflict) {
		t.Fatalf("other payload = %v, want conflict", err)
	}
	delivered(t, env, env.chat, first, to)
	if _, err := env.client.Conversations().Start(env.ctx, req); err != nil {
		t.Fatalf("retry after delivery: %v", err)
	}
	if n := len(env.chat.sent()); n != 1 {
		t.Fatalf("sends = %d, want 1", n)
	}
}

// Built-in selection: reuse the open conversation, never reopen a resolved
// one, start after a closed one; without lock_to_single a resolved one is
// not eligible.
func startSelectionRules(t *testing.T, env *startEnv) {
	transition := func(c model.ConversationRef, state string) {
		t.Helper()
		cur, err := env.client.Conversations().Get(env.ctx, model.Lookup[model.ConversationRef]{Call: model.Call{Correlation: "c"}, Ref: c})
		if err != nil {
			t.Fatal(err)
		}
		cmd := env.cmd()
		cmd.ExpectedRevision = cur.Revision
		if _, err := env.client.Conversations().Transition(env.ctx, model.Transition{Command: cmd, Conversation: c, State: state}); err != nil {
			t.Fatalf("Transition %s: %v", state, err)
		}
	}
	for _, lock := range []bool{true, false} {
		in := env.inbox(env.chat, fmt.Sprintf("rules-%v", lock), startPerContact(lock))
		to := env.recipient(env.chat, fmt.Sprintf("+1555020%d", map[bool]int{true: 1, false: 2}[lock]))
		if err := env.accept(env.chat, env.inbound(env.chat, fmt.Sprintf("rules-%v", lock), to.ID, "")); err != nil {
			t.Fatalf("Accept: %v", err)
		}
		first := env.conversations(in)[0].Ref
		if got := env.start(in, to, "hello").Operation.ResultMessage.Conversation; got != first {
			t.Fatalf("lock=%v: start on an open conversation went to %v, want %v", lock, got, first)
		}
		transition(first, model.ConversationResolved)
		got := env.start(in, to, "hello").Operation.ResultMessage.Conversation
		cur := env.conversations(in)
		switch {
		case lock && (got != first || len(cur) != 1 || cur[0].Status != model.ConversationResolved):
			t.Fatalf("lock=true resolved: start in %v (%d conversations, status %s), want the resolved one reused, not reopened", got, len(cur), cur[0].Status)
		case !lock && (got == first || len(cur) != 2):
			t.Fatalf("lock=false resolved: start in %v (%d conversations), want a new conversation", got, len(cur))
		}
		transition(got, model.ConversationClosed)
		if after := env.start(in, to, "hello").Operation.ResultMessage.Conversation; after == got {
			t.Fatalf("lock=%v: start after close reused the closed conversation", lock)
		}
	}
}

// A per-room start opens the room's conversation, routed to the room; a
// message in the room joins it.
func startRoom(t *testing.T, env *startEnv) {
	in := env.inbox(env.chat, "rooms", model.Selection{Mode: model.ModePerRoom})
	room := model.ExternalRef{Channel: env.chans[env.chat], Namespace: startNS, Kind: model.RefKindRoom, ID: "general"}
	acc := env.start(in, room, "hello")
	convs := env.conversations(in)
	if len(convs) != 1 || convs[0].ReplyRoute != room || convs[0].Owner != nil || len(convs[0].External) != 1 || convs[0].External[0] != room {
		t.Fatalf("conversations %+v, want one routed to the room", convs)
	}
	if n := len(env.identities(in)); n != 0 {
		t.Fatalf("identities = %d, want none: a room is no identity", n)
	}
	delivered(t, env, env.chat, acc, room)
	e := env.inbound(env.chat, "rooms", "member-1", "")
	e.References = []model.ExternalRef{room}
	if err := env.accept(env.chat, e); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if n, m := len(env.conversations(in)), len(env.messages(convs[0].Ref)); n != 1 || m != 2 {
		t.Fatalf("conversations %d messages %d, want the room message in the started conversation", n, m)
	}
}

// Starts racing the recipient's first inbound messages converge on one
// identity and one conversation under lock_to_single.
func startRaceWithFirstInbound(t *testing.T, env *startEnv) {
	in := env.inbox(env.chat, "race", startPerContact(true))
	to := env.recipient(env.chat, "+15550300")
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, 2*n)
	for i := range n {
		inbound := env.inbound(env.chat, "race", to.ID, "")
		req := env.startReq(model.OperationID(fmt.Sprintf("op-race-%d", i)), in, to, "hello")
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- env.accept(env.chat, inbound)
		}()
		go func() {
			defer wg.Done()
			_, err := env.client.Conversations().Start(env.ctx, req)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !model.IsCode(err, model.Transient) {
			t.Fatalf("racing call: %v", err)
		}
	}
	convs, ids := env.conversations(in), env.identities(in)
	if len(convs) != 1 || len(ids) != 1 {
		t.Fatalf("conversations %d identities %d, want one each", len(convs), len(ids))
	}
}

// Concurrent retries of one email start, whose selection gives each start
// its own conversation, commit one operation and one conversation: a
// retry that lost the claim keeps none of its own selection.
func startConcurrentRetries(t *testing.T, env *startEnv) {
	in := env.inbox(env.mail, "mail", startPerContact(false))
	req := env.startReq("op-retried", in, env.recipient(env.mail, "bob@client.example"), "hello")
	const n = 8
	var wg sync.WaitGroup
	accs := make(chan model.Acceptance, n)
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			acc, err := env.client.Conversations().Start(env.ctx, req)
			if err != nil {
				errs <- err
				return
			}
			accs <- acc
		}()
	}
	wg.Wait()
	close(accs)
	close(errs)
	for err := range errs {
		if !model.IsCode(err, model.Transient) {
			t.Fatalf("retry: %v", err)
		}
	}
	var ops []model.OperationRef
	for acc := range accs {
		if len(ops) > 0 && acc.Operation.Ref != ops[0] {
			t.Fatalf("retries returned operations %v and %v", ops[0], acc.Operation.Ref)
		}
		ops = append(ops, acc.Operation.Ref)
	}
	if len(ops) == 0 {
		t.Fatal("no retry succeeded")
	}
	convs := env.conversations(in)
	if len(convs) != 1 || len(env.messages(convs[0].Ref)) != 1 {
		t.Fatalf("conversations %+v, want one holding the one message", convs)
	}
}

// Refused starts write nothing: no identity, conversation, operation or
// send. The decisions asked are conversation.start on the inbox.
func startRefusals(t *testing.T, env *startEnv) {
	in := env.inbox(env.chat, "chat", startPerContact(true))
	to := env.recipient(env.chat, "+15550400")
	env.pol.set(func(a policy.Authorization) bool { return a.Action == model.ActionConversationStart })
	if _, err := env.client.Conversations().Start(env.ctx, env.startReq("op-denied", in, to, "hello")); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("denied start = %v, want unauthorized", err)
	}
	asked := env.pol.take()
	if len(asked) != 1 || asked[0].Action != model.ActionConversationStart || asked[0].Resource != in.Ref() {
		t.Fatalf("decisions %+v, want conversation.start on the inbox", asked)
	}
	env.pol.set(nil)
	work := env.inbox(env.chat, "work", model.Selection{Mode: model.ModePerWorkItem})
	disabled := func() model.InboxRef {
		acc, err := env.client.Configuration().ConfigureInbox(env.ctx, model.ConfigureInbox{
			Command: env.cmd(),
			Inbox: model.Inbox{
				Ref: model.InboxRef{Tenant: serviceTenant}, Channel: env.chans[env.chat], Name: "off", Destinations: []string{"off"},
				Selection: startPerContact(true), Disabled: true,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ref, _ := acc.Operation.Result.AsInbox()
		return ref
	}()
	foreign := in
	foreign.Tenant = "conformance-other"
	for i, c := range []struct {
		name string
		in   model.InboxRef
		to   model.ExternalRef
		code model.ErrorCode
	}{
		{"undeclared namespace", in, model.ExternalRef{Channel: to.Channel, Namespace: "com.example.other", Kind: startRouteKind, ID: to.ID}, model.Validation},
		{"other channel", in, env.recipient(env.mail, "ann@client.example"), model.Validation},
		{"room recipient per contact", in, model.ExternalRef{Channel: to.Channel, Namespace: startNS, Kind: model.RefKindRoom, ID: "r"}, model.Validation},
		{"per work item", work, to, model.Unsupported},
		{"disabled inbox", disabled, to, model.Disabled},
		{"foreign inbox", foreign, model.ExternalRef{Channel: model.ChannelRef{Tenant: foreign.Tenant, ID: to.Channel.ID}, Namespace: startNS, Kind: startRouteKind, ID: to.ID}, model.NotFound},
	} {
		if _, err := env.client.Conversations().Start(env.ctx, env.startReq(model.OperationID(fmt.Sprintf("op-refused-%d", i)), c.in, c.to, "hello")); !model.IsCode(err, c.code) {
			t.Errorf("%s: Start = %v, want %s", c.name, err, c.code)
		}
	}
	for _, ref := range []model.InboxRef{in, work, disabled} {
		if c, i := len(env.conversations(ref)), len(env.identities(ref)); c != 0 || i != 0 {
			t.Errorf("inbox %s: refused starts left %d conversations, %d identities", ref.ID, c, i)
		}
	}
	if n := len(env.chat.sent()); n != 0 {
		t.Fatalf("refused starts sent %d messages", n)
	}
	acc := env.start(in, to, "hello")
	env.pol.take()
	delivered(t, env, env.chat, acc, to)
	var execution []policy.Authorization
	for _, a := range env.pol.take() {
		if a.Action == model.ActionConversationStart {
			execution = append(execution, a)
		}
	}
	if len(execution) != 1 || execution[0].Resource != in.Ref() || execution[0].Principal.Actor != startActor {
		t.Fatalf("execution decisions %+v, want conversation.start on the inbox as the originating actor", execution)
	}
}
