package ingress_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// stateChatType is a chat channel that declares the provider state
// capabilities, so it may report conversation state.
const stateChatType model.TypeKey = "com.example.ingress.statechat"

type stateChat struct{ enginetest.IngressAdapter }

func (s stateChat) Describe() adapter.Descriptor {
	d := s.IngressAdapter.Describe()
	d.Capabilities = append(slices.Clone(d.Capabilities), enginetest.StateCapabilities...)
	return d
}

func withStateChat() enginetest.HarnessOption {
	return func(c *engine.Config) {
		c.Adapters = append(c.Adapters, stateChat{enginetest.IngressAdapter{
			Typ: stateChatType, Exchange: enginetest.ChatExchange, SenderKinds: enginetest.ChatSenderKinds,
		}})
	}
}

// threadWorld is a per-thread inbox on a state-reporting chat channel with
// one conversation in thread t1.
type threadWorld struct {
	*harness
	inbox  model.Inbox
	thread model.ExternalRef
}

func newThreadWorld(t *testing.T) *threadWorld {
	t.Helper()
	h := newHarness(t, stateChatType, withStateChat())
	w := &threadWorld{harness: h, thread: h.Ref(model.RefKindThread, "t1", "room-1")}
	w.inbox = h.Configure(model.Selection{Mode: model.ModePerThread}, dest)
	h.MustAccept(h.Inbound("e-1", "m-1", "alice", dest, w.thread))
	return w
}

func (w *threadWorld) conv() model.Conversation {
	w.T.Helper()
	cs := w.Conversations(w.inbox.Ref)
	if len(cs) != 1 {
		w.T.Fatalf("conversations = %d, want 1", len(cs))
	}
	return cs[0]
}

// stateOf is the conversation's provider state on the harness channel.
func stateOf(t *testing.T, c model.Conversation, ch model.ChannelRef) (model.ProviderConversationState, bool) {
	t.Helper()
	for _, s := range c.ProviderState {
		if s.Channel == ch {
			return s, true
		}
	}
	return model.ProviderConversationState{}, false
}

// stateReport is a conversation.state envelope about conv, observed at at.
func stateReport(event string, conv model.ExternalRef, at time.Time, complete bool, set func(*model.ProviderConversationState)) model.Envelope {
	st := model.ProviderConversationState{ObservedAt: at, Complete: complete}
	if set != nil {
		set(&st)
	}
	return model.Envelope{
		EventID: event, Destination: dest, Kind: model.EnvelopeConversationState, SystemActor: true,
		Assurance: "verified", OccurredAt: at,
		State: &model.ProviderStateReport{Conversation: conv, State: st},
	}
}

func boolp(b bool) *bool { return &b }

// stateChanges returns the conversation.changed payloads that report
// provider_state, with the event that carried each.
func stateChanges(t *testing.T, h *harness) []stateChange {
	t.Helper()
	var out []stateChange
	for _, ev := range h.Events() {
		if ev.Kind != model.EventConversationChanged {
			continue
		}
		var p model.ConversationChangedPayload
		if err := json.Unmarshal(ev.Payload.Data, &p); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(p.Changes, model.ConversationFieldProviderState) {
			out = append(out, stateChange{p, ev})
		}
	}
	return out
}

type stateChange struct {
	model.ConversationChangedPayload
	event model.Event
}

func readMarkerAbsent(t *testing.T, h *harness, c model.Conversation) {
	t.Helper()
	h.View(func(r store.Reader) error {
		if m, err := r.ReadMarker(h.Ctx, c.Ref, enginetest.Actor); !model.IsCode(err, model.NotFound) {
			t.Fatalf("read marker = %+v, %v; want none: provider read state never touches it", m, err)
		}
		return nil
	})
}

// A report sets the channel's provider state on the resolved conversation
// and emits conversation.changed provider_state, provider-triggered, under
// a new revision. Local status and labels stay, and the per-actor read
// marker is never touched by a provider read state.
func TestStateReportApplies(t *testing.T) {
	w := newThreadWorld(t)
	before := w.conv()
	at := t0.Add(time.Minute)
	rcpt := w.MustAccept(stateReport("s-1", w.thread, at, true, func(s *model.ProviderConversationState) {
		s.Archived, s.Read = true, boolp(true)
		s.Folder = &model.Folder{ID: "f-arch", Name: "Archive", Role: model.RoleArchive}
		s.Flags = []string{model.FlagStar}
	}))
	if !rcpt.Committed || rcpt.Operation != nil {
		t.Fatalf("receipt %+v, want committed with no operation", rcpt)
	}
	got := w.conv()
	st, ok := stateOf(t, got, w.Channel.Ref)
	if !ok || !st.Archived || st.Read == nil || !*st.Read || st.Folder == nil || st.Folder.ID != "f-arch" ||
		!slices.Equal(st.Flags, []string{model.FlagStar}) || !st.Complete || !st.ObservedAt.Equal(at) || st.Trashed {
		t.Fatalf("provider state = %+v", st)
	}
	if got.Revision == before.Revision || got.Status != before.Status || !slices.Equal(got.Labels, before.Labels) {
		t.Fatalf("conversation %+v: want a new revision and local status and labels kept (%+v)", got, before)
	}
	cs := stateChanges(t, w.harness)
	if len(cs) != 1 {
		t.Fatalf("provider_state events = %d, want 1", len(cs))
	}
	c := cs[0]
	if c.Trigger != model.TriggerProvider || c.Conversation != got.Ref || c.PreviousRevision != before.Revision ||
		c.Operation != nil || !slices.Equal(c.Changes, []string{model.ConversationFieldProviderState}) ||
		c.event.Revision != got.Revision || c.event.Audience.Visibility != model.Internal {
		t.Fatalf("event %+v", c)
	}
	readMarkerAbsent(t, w.harness, got)
}

// A report naming the state the conversation already holds, redelivered
// under another event ID, changes nothing and emits nothing.
func TestStateReportRepeatIsNoOp(t *testing.T) {
	w := newThreadWorld(t)
	set := func(s *model.ProviderConversationState) { s.Archived = true }
	w.MustAccept(stateReport("s-1", w.thread, t0.Add(time.Minute), true, set))
	before := w.conv()
	w.MustAccept(stateReport("s-2", w.thread, t0.Add(2*time.Minute), true, set))
	if got := w.conv(); got.Revision != before.Revision {
		t.Fatalf("revision %s, want %s: a repeated state wrote", got.Revision, before.Revision)
	}
	if n := len(stateChanges(t, w.harness)); n != 1 {
		t.Fatalf("provider_state events = %d, want 1", n)
	}
}

// A report observed before, or when, the held state was observed is stale:
// ignored, whatever it says.
func TestStaleStateReportIgnored(t *testing.T) {
	w := newThreadWorld(t)
	w.MustAccept(stateReport("s-1", w.thread, t0.Add(10*time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = true }))
	before := w.conv()
	for i, at := range []time.Time{t0.Add(5 * time.Minute), t0.Add(10 * time.Minute)} {
		w.MustAccept(stateReport("s-stale"+string(rune('a'+i)), w.thread, at, true, func(s *model.ProviderConversationState) { s.Trashed = true }))
	}
	got := w.conv()
	st, _ := stateOf(t, got, w.Channel.Ref)
	if got.Revision != before.Revision || !st.Archived || st.Trashed || !st.ObservedAt.Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("after stale reports: %+v (revision %s, was %s)", st, got.Revision, before.Revision)
	}
	if n := len(stateChanges(t, w.harness)); n != 1 {
		t.Fatalf("provider_state events = %d, want 1", n)
	}
}

// A report about a conversation uma does not hold is accepted and recorded
// as nothing: no conversation, message, identity, contact or event. The
// ingress receipt is the evidence, and a redelivery returns it.
func TestStateReportUnknownConversationDropped(t *testing.T) {
	w := newThreadWorld(t)
	evs, msgs := len(w.Events()), len(w.Messages(w.inbox.Ref))
	unknown := w.Ref(model.RefKindThread, "t-none", "room-1")
	env := stateReport("s-1", unknown, t0.Add(time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = true })
	rcpt := w.MustAccept(env)
	if !rcpt.Committed || rcpt.ID == "" || rcpt.Conversation != nil || rcpt.Operation != nil {
		t.Fatalf("receipt %+v, want committed without a conversation", rcpt)
	}
	if n := len(w.Events()); n != evs {
		t.Fatalf("events %d, want %d", n, evs)
	}
	if n := len(w.Conversations(w.inbox.Ref)); n != 1 {
		t.Fatalf("conversations = %d: a state report created one", n)
	}
	if n, m := len(w.Messages(w.inbox.Ref)), len(w.Identities(w.inbox.Ref)); n != msgs || m != 1 {
		t.Fatalf("messages %d identities %d: a state report wrote", n, m)
	}
	if st, held := stateOf(t, w.conv(), w.Channel.Ref); held {
		t.Fatalf("another conversation took the state: %+v", st)
	}
	if again := w.MustAccept(env); !again.Duplicate || again.ID != rcpt.ID {
		t.Fatalf("redelivery %+v, want a duplicate of %s", again, rcpt.ID)
	}
}

// A report of one message of a thread is a partial view: its fields are
// merged into what is held, Complete drops while the view is partial and
// returns with a report over every item. A partial report that changes
// nothing keeps a complete view complete.
func TestPartialStateReportMergesAndTracksComplete(t *testing.T) {
	w := newThreadWorld(t)
	w.MustAccept(stateReport("s-1", w.thread, t0.Add(1*time.Minute), true, func(s *model.ProviderConversationState) {
		s.Archived, s.Read = true, boolp(true)
		s.Folder = &model.Folder{ID: "f-arch", Role: model.RoleArchive}
	}))
	if st, _ := stateOf(t, w.conv(), w.Channel.Ref); !st.Complete {
		t.Fatalf("state %+v, want complete", st)
	}

	// One message reports itself unarchived: its fields apply, the rest of
	// the held state stays, and the view is no longer complete.
	w.MustAccept(stateReport("s-2", w.thread, t0.Add(2*time.Minute), false, func(s *model.ProviderConversationState) {
		s.Archived = false
	}))
	st, _ := stateOf(t, w.conv(), w.Channel.Ref)
	if st.Archived || st.Complete || st.Read == nil || !*st.Read || st.Folder == nil || st.Folder.ID != "f-arch" ||
		!st.ObservedAt.Equal(t0.Add(2*time.Minute)) {
		t.Fatalf("after a partial report: %+v, want unarchived, incomplete, read and folder kept", st)
	}

	// The same partial view again changes nothing.
	rev := w.conv().Revision
	w.MustAccept(stateReport("s-3", w.thread, t0.Add(3*time.Minute), false, func(s *model.ProviderConversationState) {
		s.Archived = false
	}))
	if got := w.conv().Revision; got != rev {
		t.Fatalf("repeated partial report wrote: %s, was %s", got, rev)
	}

	// A report over every item is complete again, and replaces the view.
	w.MustAccept(stateReport("s-4", w.thread, t0.Add(4*time.Minute), true, func(s *model.ProviderConversationState) {
		s.Read = boolp(false)
	}))
	st, _ = stateOf(t, w.conv(), w.Channel.Ref)
	if !st.Complete || st.Archived || st.Read == nil || *st.Read || st.Folder != nil {
		t.Fatalf("after a complete report: %+v, want a complete view replacing the held one", st)
	}
	if n := len(stateChanges(t, w.harness)); n != 3 {
		t.Fatalf("provider_state events = %d, want 3 (the repeated partial report emits none)", n)
	}

	// A partial report over a complete view that it does not change keeps it complete.
	w.MustAccept(stateReport("s-5", w.thread, t0.Add(5*time.Minute), false, func(s *model.ProviderConversationState) {
		s.Read = boolp(false)
	}))
	if st, _ = stateOf(t, w.conv(), w.Channel.Ref); !st.Complete {
		t.Fatalf("an unchanged partial report broke completeness: %+v", st)
	}
}

// A first partial report records the view it has, incomplete.
func TestFirstPartialStateReportIsIncomplete(t *testing.T) {
	w := newThreadWorld(t)
	w.MustAccept(stateReport("s-1", w.thread, t0.Add(time.Minute), false, func(s *model.ProviderConversationState) { s.Trashed = true }))
	st, ok := stateOf(t, w.conv(), w.Channel.Ref)
	if !ok || !st.Trashed || st.Complete {
		t.Fatalf("state %+v, want a trashed, incomplete view", st)
	}
}

// A conversation reached through a message reference, as a mailbox
// conversation held per contact is, resolves too.
func TestStateReportResolvesByMessageReference(t *testing.T) {
	h := newHarness(t, stateChatType, withStateChat())
	in := h.Configure(perContact(true), dest)
	thread := h.Ref(model.RefKindThread, "mail-t1", "")
	h.MustAccept(h.Inbound("e-1", "m-1", "alice", dest, thread))
	h.MustAccept(h.Inbound("e-2", "m-2", "alice", dest, thread))
	h.MustAccept(stateReport("s-1", thread, t0.Add(time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = true }))
	cs := h.Conversations(in.Ref)
	if len(cs) != 1 {
		t.Fatalf("conversations = %d", len(cs))
	}
	if st, ok := stateOf(t, cs[0], h.Channel.Ref); !ok || !st.Archived {
		t.Fatalf("state %+v, want archived", st)
	}
}

// The envelope is checked against the binding before any storage access.
func TestStateReportChecks(t *testing.T) {
	w := newThreadWorld(t)
	ok := func() model.Envelope {
		return stateReport("s-1", w.thread, t0.Add(time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = true })
	}
	for _, tc := range []struct {
		name string
		edit func(*model.Envelope)
		code model.ErrorCode
		msg  string
	}{
		{"no state", func(e *model.Envelope) { e.State = nil }, model.Validation, "envelope.state"},
		{"no event id", func(e *model.Envelope) { e.EventID = "" }, model.Validation, "event_id"},
		{"message id", func(e *model.Envelope) { e.MessageID = "m-1" }, model.Validation, "message_id"},
		{"content", func(e *model.Envelope) {
			e.Content = model.Content{Parts: []model.Part{{Kind: "text", Text: "x"}}}
		}, model.Validation, "content"},
		{"task", func(e *model.Envelope) { e.Task = &model.TaskBinding{} }, model.Validation, "task"},
		{"sender attributes", func(e *model.Envelope) { e.SenderAttributes = []model.Attribute{attrPhone} }, model.Validation, "carries no sender"},
		{"foreign conversation", func(e *model.Envelope) {
			e.State.Conversation.Channel = model.ChannelRef{Tenant: tenantA, ID: w.ID()}
		}, model.Validation, "bound channel"},
		{"no observation time", func(e *model.Envelope) { e.State.State.ObservedAt = time.Time{} }, model.Validation, "observed_at"},
		{"folder and labels", func(e *model.Envelope) {
			e.State.State.Folder = &model.Folder{ID: "f"}
			e.State.State.Labels = []model.ProviderLabel{{ID: "l"}}
		}, model.Validation, "labels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := ok()
			tc.edit(&env)
			_, err := w.Accept(env)
			requireCode(t, err, tc.code, tc.msg)
		})
	}
	t.Run("state on another kind", func(t *testing.T) {
		env := w.Inbound("e-9", "m-9", "alice", dest, w.thread)
		env.State = ok().State
		_, err := w.Accept(env)
		requireCode(t, err, model.Validation, "envelope.state")
	})
	t.Run("adapter declares no state capability", func(t *testing.T) {
		h := newHarness(t, chatType)
		h.Configure(model.Selection{Mode: model.ModePerThread}, dest)
		_, err := h.Accept(stateReport("s-1", h.Ref(model.RefKindThread, "t1", "room-1"), t0, true, nil))
		requireCode(t, err, model.Unsupported, "state")
	})
	if n := len(stateChanges(t, w.harness)); n != 0 {
		t.Fatalf("a rejected report emitted %d events", n)
	}
}

// stateDesk is a per-work-item desk whose adapter changes provider state;
// its conversation is the one of issue 7.
func newStateDesk(t *testing.T) (*desk, model.Conversation) {
	t.Helper()
	d := newDeskWith(t, &enginetest.OutFactory{
		State: true, StateCaps: enginetest.StateCapabilities, StateLimits: enginetest.StateLimitsOf(model.ScopeAccount),
	})
	d.MustAccept(d.comment("e-1", "c-1", "customer", "7"))
	cs := d.Conversations(d.inbox.Ref)
	if len(cs) != 1 {
		t.Fatalf("conversations = %d", len(cs))
	}
	return d, cs[0]
}

func (d *desk) archive(c model.Conversation, id model.OperationID) model.Acceptance {
	d.T.Helper()
	a, err := d.Client.Conversations().Archive(d.Ctx, model.ArchiveConversation{
		Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: id},
		Conversation: c.Ref,
	})
	if err != nil {
		d.T.Fatalf("Archive: %v", err)
	}
	return a
}

func (d *desk) setRead(c model.Conversation, id model.OperationID, read bool) model.Acceptance {
	d.T.Helper()
	a, err := d.Client.Conversations().SetRead(d.Ctx, model.SetConversationRead{
		Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: id},
		Conversation: c.Ref, Read: read,
	})
	if err != nil {
		d.T.Fatalf("SetRead: %v", err)
	}
	return a
}

func (d *desk) stateCalls() int {
	n := 0
	for _, c := range d.f.MutationCalls() {
		if c.State != nil {
			n++
		}
	}
	return n
}

func (d *desk) workPending() int {
	d.T.Helper()
	var n int
	d.View(func(r store.Reader) error {
		depth, err := r.WorkDepth(d.Ctx, operations.WorkKindOperation)
		n = depth.Pending
		return err
	})
	return n
}

func (d *desk) opCount() int {
	d.T.Helper()
	return len(all(d.harness, store.Reader.Operations, model.Filter{}))
}

// The provider's echo of uma's own state change settles its operation as
// evidence: a lost answer is settled from it with no second send, the
// state is applied once, and a repeated echo changes, emits and queues
// nothing.
func TestStateEchoSettlesOperation(t *testing.T) {
	d, c := newStateDesk(t)
	d.f.Respond(enginetest.LostAnswer)
	a := d.archive(c, "op-arch")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("archive = %s, want outcome_unknown", op.State)
	}

	rcpt := d.MustAccept(stateReport("s-echo", d.issue("7"), t0.Add(time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = true }))
	if rcpt.Operation == nil || *rcpt.Operation != a.Operation.Ref {
		t.Fatalf("receipt operation %+v, want %v", rcpt.Operation, a.Operation.Ref)
	}
	if st, ok := stateOf(t, d.Conversations(d.inbox.Ref)[0], d.Channel.Ref); !ok || !st.Archived {
		t.Fatalf("state %+v, want the echo applied", st)
	}
	if n := d.workPending(); n != 1 {
		t.Fatalf("pending work = %d, want the operation's worker queued", n)
	}

	// A repeated echo queues no second worker.
	d.MustAccept(stateReport("s-echo2", d.issue("7"), t0.Add(2*time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = true }))
	if n := d.workPending(); n != 1 {
		t.Fatalf("pending work = %d after a repeated echo, want 1", n)
	}

	d.deliver()
	op := d.op(a.Operation.Ref)
	if op.State != model.ProviderAccepted || op.Failure != nil {
		t.Fatalf("archive = %s (%v), want provider_accepted from the echo", op.State, op.Failure)
	}
	if n := d.stateCalls(); n != 1 {
		t.Fatalf("provider state calls = %d, want 1: the echo is never re-sent", n)
	}
	cs := stateChanges(t, d.harness)
	if len(cs) != 1 || cs[0].Trigger != model.TriggerProvider {
		t.Fatalf("provider_state events = %+v, want the echo's one provider event", cs)
	}
	if got, _ := stateOf(t, d.Conversations(d.inbox.Ref)[0], d.Channel.Ref); !got.Archived {
		t.Fatalf("state %+v", got)
	}
}

// A report that does not show what an unsettled operation asked is not its
// echo: the state applies as the provider's, and the operation stays
// outcome_unknown with no worker queued.
func TestStateReportNotMatchingOperationLeavesItPending(t *testing.T) {
	d, c := newStateDesk(t)
	d.f.Respond(enginetest.LostAnswer)
	a := d.archive(c, "op-arch")
	d.deliver()
	d.MustAccept(stateReport("s-1", d.issue("7"), t0.Add(time.Minute), true, func(s *model.ProviderConversationState) {
		s.Archived, s.Read = false, boolp(true)
	}))
	if op := d.op(a.Operation.Ref); op.State != model.Unknown {
		t.Fatalf("archive = %s, want outcome_unknown: the report is not its echo", op.State)
	}
	if n := d.workPending(); n != 0 {
		t.Fatalf("pending work = %d, want none", n)
	}
	st, _ := stateOf(t, d.Conversations(d.inbox.Ref)[0], d.Channel.Ref)
	if st.Archived || st.Read == nil || !*st.Read {
		t.Fatalf("state %+v, want the report applied", st)
	}
}

// An echo of a move to a destination named by role is matched by the folder
// role the provider reports.
func TestStateEchoOfMoveByRole(t *testing.T) {
	d, c := newStateDesk(t)
	d.f.Respond(enginetest.LostAnswer)
	a, err := d.Client.Conversations().Move(d.Ctx, model.MoveConversation{
		Command:      model.Command{Call: model.Call{Correlation: "corr"}, Operation: "op-move"},
		Conversation: c.Ref, Destination: model.MoveDestination{Role: model.RoleArchive},
	})
	if err != nil {
		t.Fatal(err)
	}
	d.deliver()
	d.MustAccept(stateReport("s-1", d.issue("7"), t0.Add(time.Minute), true, func(s *model.ProviderConversationState) {
		s.Folder = &model.Folder{ID: "f-9", Name: "Archive", Role: model.RoleArchive}
	}))
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("move = %s, want provider_accepted from the echo", op.State)
	}
	if n := d.stateCalls(); n != 1 {
		t.Fatalf("provider state calls = %d, want 1", n)
	}
}

// A report observed before the operation settled is stale: the settle wrote
// the state at its own time, and an older view never undoes it.
func TestStateReportBeforeSettleIgnored(t *testing.T) {
	d, c := newStateDesk(t)
	a := d.archive(c, "op-arch")
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("archive = %s", op.State)
	}
	before := d.Conversations(d.inbox.Ref)[0]
	d.MustAccept(stateReport("s-old", d.issue("7"), t0.Add(-time.Minute), true, func(s *model.ProviderConversationState) { s.Archived = false }))
	got := d.Conversations(d.inbox.Ref)[0]
	if st, _ := stateOf(t, got, d.Channel.Ref); !st.Archived || got.Revision != before.Revision {
		t.Fatalf("state %+v: a report observed before the settle undid it", st)
	}
}

// Provider read state and the per-actor read marker never feed each other:
// an inbound read state leaves the marker alone, and its echo settles the
// provider read operation without queueing another operation.
func TestStateReadEchoNeverTouchesReadMarkerOrRequeues(t *testing.T) {
	d, c := newStateDesk(t)
	read := func(s *model.ProviderConversationState) { s.Read = boolp(true) }

	// A provider read state alone.
	d.MustAccept(stateReport("s-1", d.issue("7"), t0.Add(time.Minute), true, read))
	ops := d.opCount()
	readMarkerAbsent(t, d.harness, c)
	if n := d.workPending(); n != 0 {
		t.Fatalf("pending work = %d after an inbound read state", n)
	}

	// The echo of a provider read operation.
	d.MustAccept(stateReport("s-2", d.issue("7"), t0.Add(2*time.Minute), true, func(s *model.ProviderConversationState) { s.Read = boolp(false) }))
	d.f.Respond(enginetest.LostAnswer)
	a := d.setRead(c, "op-read", true)
	ops++
	d.deliver()
	d.MustAccept(stateReport("s-3", d.issue("7"), t0.Add(3*time.Minute), true, read))
	d.deliver()
	if op := d.op(a.Operation.Ref); op.State != model.ProviderAccepted {
		t.Fatalf("read = %s, want provider_accepted from the echo", op.State)
	}
	if n := d.opCount(); n != ops {
		t.Fatalf("operations = %d, want %d: an echo never queues an operation", n, ops)
	}
	if n := d.stateCalls(); n != 1 {
		t.Fatalf("provider state calls = %d, want 1", n)
	}
	readMarkerAbsent(t, d.harness, c)
}
