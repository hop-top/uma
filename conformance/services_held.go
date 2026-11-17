package conformance

import (
	"encoding/json"
	"testing"

	"hop.top/uma"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
)

// Held-operation cases through the public services: a plugin actor's
// command carries a Hold, a reviewer decides it, and the ledger, the trust
// counters and the event log agree with each decision on the store under
// test. They cover what the durable suite cannot say about the services:
// authorization, visibility by the target's read right and the held writes
// that need an engine opt-in.
var heldServiceCases = []struct {
	name string
	run  func(*testing.T, *mutationEnv)
}{
	{"LifecycleApproveRejectEdit", heldSvcLifecycle},
	{"KeyClaimAndReplay", heldSvcKeyReplay},
	{"VisibilityFollowsTargetRead", heldSvcVisibility},
	{"DecisionAuthorization", heldSvcAuthorization},
	{"EventsAndTrustFollowDecisions", heldSvcEventsAndTrust},
	{"ApprovalBatch", heldSvcApprovalBatch},
	{"ImportTrust", heldSvcImportTrust},
}

const (
	heldSvcPlugin model.ActorID = "plugin:triage"
	heldSvcBlind  model.ActorID = "blind-reviewer"
)

func (env *mutationEnv) as(actor model.ActorID) uma.Client {
	env.t.Helper()
	c, err := env.engine.For(model.Principal{Tenant: serviceTenant, Actor: actor})
	if err != nil {
		env.t.Fatal(err)
	}
	return c
}

// heldReply proposes a reply to the env's conversation as the plugin.
func (env *mutationEnv) heldReply(text, trust string) model.Operation {
	env.t.Helper()
	cmd := env.cmd("")
	cmd.Hold = &model.HoldProposal{Reason: "review", TrustKey: trust, TrustLabels: map[string]string{"plugin": "triage"}}
	a, err := env.as(heldSvcPlugin).Messages().Reply(env.ctx, model.Reply{
		Command: cmd, Conversation: env.target.Ref.Conversation,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	})
	if err != nil {
		env.t.Fatalf("held Reply: %v", err)
	}
	if a.Operation.State != model.Held || a.Operation.Held == nil {
		env.t.Fatalf("proposal = %s %+v, want held", a.Operation.State, a.Operation.Held)
	}
	return a.Operation
}

func heldSvcDecide(op model.Operation) model.DecideHeld {
	return model.DecideHeld{Call: model.Call{Correlation: "decide"}, Operation: op.Ref, ExpectedRevision: op.Revision, Note: "n"}
}

// heldSvcReplyArgs are the stored reply intent of op with its text changed,
// as a host builds the arguments of an edit from what the proposal stored.
func heldSvcReplyArgs(env *mutationEnv, op model.Operation, text string) model.Extension {
	env.t.Helper()
	in, err := operations.DecodeReplyIntent(*op.Held.Intent)
	if err != nil {
		env.t.Fatal(err)
	}
	in.Content = model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}}
	data, _ := json.Marshal(in)
	return model.Extension{Name: operations.IntentReply, Version: 1, Data: data}
}

func (env *mutationEnv) heldKinds(c uma.Client, kind string) []model.Event {
	env.t.Helper()
	p, err := c.Events().List(env.ctx, model.ListEvents{
		Call: model.Call{Correlation: "events"}, Start: model.StartBeginning, Limit: uma.MaxEventListLimit,
		Filter: model.Filter{Kinds: []string{kind}},
	})
	if err != nil {
		env.t.Fatalf("Events.List(%s): %v", kind, err)
	}
	return p.Items
}

func heldSvcLifecycle(t *testing.T, env *mutationEnv) {
	reviewer := env.client.Operations()
	approved := env.heldReply("one", "triage/reply")
	got, err := reviewer.Approve(env.ctx, heldSvcDecide(approved))
	if err != nil || got.Operation.State != model.Queued || got.Decision != model.HeldApproved || got.Correction || got.Replayed {
		t.Fatalf("Approve = %+v, %v", got, err)
	}
	if h := got.Operation.Held; h.DecidedBy != mutationActor || h.DecidedAt == nil || h.Proposer != heldSvcPlugin {
		t.Fatalf("held block after approve = %+v", h)
	}
	// The next proposal is a new one: the first is decided.
	rejected := env.heldReply("two", "triage/reply")
	rej, err := reviewer.Reject(env.ctx, heldSvcDecide(rejected))
	if err != nil || rej.Operation.State != model.Rejected || rej.Decision != model.HeldRejected || !rej.Correction {
		t.Fatalf("Reject = %+v, %v", rej, err)
	}
	edited := env.heldReply("three", "triage/reply")
	ed, err := reviewer.Edit(env.ctx, model.EditHeld{DecideHeld: heldSvcDecide(edited), Args: heldSvcReplyArgs(env, edited, "three, edited")})
	if err != nil || ed.Operation.State != model.Queued || ed.Decision != model.HeldEdited || !ed.Correction ||
		len(ed.Operation.Held.Edits) != 1 || ed.Operation.Held.Edits[0].By != mutationActor {
		t.Fatalf("Edit = %+v, %v", ed, err)
	}
	for _, c := range []struct {
		op    model.Operation
		state model.DeliveryState
	}{{approved, model.Queued}, {rejected, model.Rejected}, {edited, model.Queued}} {
		if st := env.get(c.op.Ref).State; st != c.state {
			t.Fatalf("Get(%s) = %s, want %s", c.op.Ref.ID, st, c.state)
		}
	}
	// Stale refusals tell their reasons apart and leave the proposal held.
	stale := env.heldReply("four", "triage/reply")
	bad := heldSvcDecide(stale)
	bad.ExpectedRevision = "not-the-revision"
	if _, err := reviewer.Approve(env.ctx, bad); !model.IsCode(err, model.Conflict) || !model.IsReason(err, model.StaleRevision) {
		t.Fatalf("Approve at a stale revision = %v, want Conflict/%s", err, model.StaleRevision)
	}
	if env.get(stale.Ref).State != model.Held {
		t.Fatal("a refused decision changed the operation")
	}
}

func heldSvcKeyReplay(t *testing.T, env *mutationEnv) {
	cmd := env.cmd("")
	cmd.Hold = &model.HoldProposal{Reason: "review", TrustKey: "triage/reply"}
	req := model.Reply{
		Command: cmd, Conversation: env.target.Ref.Conversation,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	}
	plugin := env.as(heldSvcPlugin)
	first, err := plugin.Messages().Reply(env.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := plugin.Messages().Reply(env.ctx, req)
	if err != nil || !again.Replayed || again.AlreadyHeld || again.Operation.Ref != first.Operation.Ref || again.Operation.State != model.Held {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	// Another key for the same kind and target, while one is held, is the held operation.
	dup := req
	dup.Command = env.cmd("")
	dup.Hold = req.Hold
	dup.Content.Parts = []model.Part{{Kind: model.PartText, Text: "hello, again"}}
	second, err := plugin.Messages().Reply(env.ctx, dup)
	if err != nil || !second.AlreadyHeld || second.Operation.Ref != first.Operation.Ref {
		t.Fatalf("duplicate = %+v, %v", second, err)
	}
	// Approval keeps the claimed key, so the proposal still replays to it.
	got, err := env.client.Operations().Approve(env.ctx, heldSvcDecide(first.Operation))
	if err != nil || got.Operation.Ref != first.Operation.Ref || *got.Operation.Key != *first.Operation.Key {
		t.Fatalf("Approve = %+v, %v", got, err)
	}
	after, err := plugin.Messages().Reply(env.ctx, req)
	if err != nil || !after.Replayed || after.Operation.Ref != first.Operation.Ref || after.Operation.State != model.Queued {
		t.Fatalf("replay after approve = %+v, %v", after, err)
	}
	// A person's own write is never held.
	own := env.cmd("")
	own.Hold = req.Hold
	if _, err := env.client.Messages().Reply(env.ctx, model.Reply{Command: own, Conversation: req.Conversation, Content: req.Content}); !model.IsCode(err, model.Validation) {
		t.Fatalf("a person's held Reply = %v, want Validation", err)
	}
}

// A held operation is the draft content of its target: a principal that may
// read the operation but not its target gets not found, never a stub.
func heldSvcVisibility(t *testing.T, env *mutationEnv) {
	env.policy.deny(heldSvcBlind, model.ActionConversationRead)
	held := env.heldReply("draft", "triage/reply")
	blind := env.as(heldSvcBlind)
	if _, err := blind.Operations().Get(env.ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "g"}, Ref: held.Ref}); !model.IsCode(err, model.NotFound) {
		t.Fatalf("Get without the target's read right = %v, want NotFound", err)
	}
	list := func(c uma.Client) []model.Operation {
		p, err := c.Operations().List(env.ctx, model.ListRequest{
			Call: model.Call{Correlation: "l"}, Filter: model.Filter{Statuses: []string{string(model.Held)}}, Page: model.PageRequest{Limit: 20},
		})
		if err != nil {
			t.Fatal(err)
		}
		return p.Items
	}
	if got := list(blind); len(got) != 0 {
		t.Fatalf("List without the target's read right shows %d held operations", len(got))
	}
	if got := list(env.client); len(got) != 1 || got[0].Ref != held.Ref || got[0].Held == nil {
		t.Fatalf("List for a reader of the target = %+v", got)
	}
	if _, err := blind.Operations().Approve(env.ctx, heldSvcDecide(held)); !model.IsCode(err, model.NotFound) {
		t.Fatalf("Approve without the target's read right = %v, want NotFound", err)
	}
	if env.get(held.Ref).State != model.Held {
		t.Fatal("a refused decision changed the operation")
	}
	if evs := env.heldKinds(blind, model.EventOperationHeld); len(evs) != 0 {
		t.Fatalf("operation.held visible without the target's read right: %d events", len(evs))
	}
	if evs := env.heldKinds(env.client, model.EventOperationHeld); len(evs) != 1 {
		t.Fatalf("operation.held events for a reader = %d, want 1", len(evs))
	}
}

func heldSvcAuthorization(t *testing.T, env *mutationEnv) {
	held := env.heldReply("draft", "triage/reply")
	// Proposing grants no decision: the plugin needs the action like anyone.
	env.policy.deny(heldSvcPlugin, model.ActionOperationApprove)
	if _, err := env.as(heldSvcPlugin).Operations().Approve(env.ctx, heldSvcDecide(held)); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("self-approval = %v, want Unauthorized", err)
	}
	for action, call := range map[string]func(uma.Operations) error{
		model.ActionOperationApprove: func(o uma.Operations) error { _, err := o.Approve(env.ctx, heldSvcDecide(held)); return err },
		model.ActionOperationReject:  func(o uma.Operations) error { _, err := o.Reject(env.ctx, heldSvcDecide(held)); return err },
		model.ActionOperationEdit: func(o uma.Operations) error {
			_, err := o.Edit(env.ctx, model.EditHeld{DecideHeld: heldSvcDecide(held), Args: heldSvcReplyArgs(env, held, "x")})
			return err
		},
	} {
		actor := model.ActorID("no-" + action)
		env.policy.deny(actor, action)
		if err := call(env.as(actor).Operations()); !model.IsCode(err, model.Unauthorized) {
			t.Fatalf("%s denied = %v, want Unauthorized", action, err)
		}
	}
	if env.get(held.Ref).State != model.Held {
		t.Fatal("a refused decision changed the operation")
	}
}

func heldSvcEventsAndTrust(t *testing.T, env *mutationEnv) {
	reviewer := env.client.Operations()
	chRef := env.provRef.Channel
	a := env.heldReply("one", "triage/reply")
	env.heldReply("dup", "triage/reply") // a duplicate-guard hit: no event
	if evs := env.heldKinds(env.client, model.EventOperationHeld); len(evs) != 1 {
		t.Fatalf("operation.held events = %d, want 1", len(evs))
	}
	if _, err := reviewer.Approve(env.ctx, heldSvcDecide(a)); err != nil {
		t.Fatal(err)
	}
	if rep, err := reviewer.Approve(env.ctx, heldSvcDecide(a)); err != nil || !rep.Replayed {
		t.Fatalf("second Approve = %+v, %v", rep, err)
	}
	evs := env.heldKinds(env.client, model.EventOperationDecided)
	if len(evs) != 1 {
		t.Fatalf("operation.decided events = %d, want 1: none for a replay", len(evs))
	}
	var p model.HeldOperationPayload
	if err := json.Unmarshal(evs[0].Payload.Data, &p); err != nil || p.Decision != model.HeldApproved || p.DecidedBy != mutationActor || p.Operation != a.Ref {
		t.Fatalf("decided payload = %+v, %v", p, err)
	}
	call := model.Call{Correlation: "trust"}
	tr, err := reviewer.Trust(env.ctx, model.TrustLookup{Call: call, Channel: chRef, Key: "triage/reply"})
	if err != nil || tr.Clean != 1 || tr.Approvals != 1 || tr.Labels["plugin"] != "triage" {
		t.Fatalf("Trust = %+v, %v", tr, err)
	}
	b := env.heldReply("two", "triage/reply")
	if _, err := reviewer.Reject(env.ctx, heldSvcDecide(b)); err != nil {
		t.Fatal(err)
	}
	tr, err = reviewer.Trust(env.ctx, model.TrustLookup{Call: call, Channel: chRef, Key: "triage/reply"})
	if err != nil || tr.Clean != 0 || tr.Approvals != 1 || tr.Corrections != 1 {
		t.Fatalf("Trust after a reject = %+v, %v", tr, err)
	}
	if rs, err := reviewer.ResetTrust(env.ctx, model.ResetTrust{Call: call, Channel: chRef, Key: "triage/reply"}); err != nil || rs.Clean != 0 || rs.Approvals != 1 {
		t.Fatalf("ResetTrust = %+v, %v", rs, err)
	}
	page, err := reviewer.Trusts(env.ctx, model.TrustFilter{Call: call, Channel: chRef, Page: model.PageRequest{Limit: 10}})
	if err != nil || len(page.Items) != 1 || page.Items[0].Key != "triage/reply" || !page.Complete {
		t.Fatalf("Trusts = %+v, %v", page, err)
	}
	other := env.heldReply("three", "triage/other")
	if _, err := reviewer.Approve(env.ctx, heldSvcDecide(other)); err != nil {
		t.Fatal(err)
	}
	set, err := reviewer.ResetTrustSet(env.ctx, model.ResetTrustSet{Call: call, Channel: chRef, Keys: []string{"triage/other", "never"}})
	if err != nil || len(set) != 1 || set[0].Key != "triage/other" || set[0].Clean != 0 || set[0].Approvals != 1 {
		t.Fatalf("ResetTrustSet = %+v, %v", set, err)
	}
	env.policy.deny(heldSvcBlind, model.ActionTrustRead)
	env.policy.deny(heldSvcBlind, model.ActionTrustReset)
	blind := env.as(heldSvcBlind).Operations()
	if _, err := blind.Trust(env.ctx, model.TrustLookup{Call: call, Channel: chRef, Key: "triage/reply"}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("Trust without trust.read = %v", err)
	}
	if _, err := blind.ResetTrust(env.ctx, model.ResetTrust{Call: call, Channel: chRef, Key: "triage/reply"}); !model.IsCode(err, model.Unauthorized) {
		t.Fatalf("ResetTrust without trust.reset = %v", err)
	}
}

// Held starts and held label creates scope the duplicate guard below kind
// and target: one start per recipient, one label create per name.
func heldSvcStartScope(t *testing.T, factory StoreFactory) {
	env := newStartEnvHeld(t, factory, uma.HeldPolicy{})
	in := env.inbox(env.chat, "chat-held", startPerContact(true))
	plugin, err := env.engine.For(model.Principal{Tenant: serviceTenant, Actor: heldSvcPlugin})
	if err != nil {
		t.Fatal(err)
	}
	propose := func(op model.OperationID, to string) model.Acceptance {
		t.Helper()
		req := env.startReq(op, in, env.recipient(env.chat, to), "hello "+to)
		req.Hold = &model.HoldProposal{Reason: "review", TrustKey: "triage/start"}
		a, err := plugin.Conversations().Start(env.ctx, req)
		if err != nil {
			t.Fatalf("held Start to %s: %v", to, err)
		}
		return a
	}
	alice, bob := propose("st-a", "alice"), propose("st-b", "bob")
	if alice.Operation.State != model.Held || bob.Operation.State != model.Held || alice.AlreadyHeld || bob.AlreadyHeld ||
		alice.Operation.Ref == bob.Operation.Ref {
		t.Fatalf("starts to two recipients = %+v, %+v, want two held operations", alice, bob)
	}
	if again := propose("st-c", "alice"); !again.AlreadyHeld || again.Operation.Ref != alice.Operation.Ref {
		t.Fatalf("second start to alice = %+v, want her held operation", again)
	}
	if again := propose("st-a", "alice"); again.AlreadyHeld || !again.Replayed || again.Operation.Ref != alice.Operation.Ref {
		t.Fatalf("replay = %+v", again)
	}
	// The default holds a start without creating its conversation: nothing
	// until it is approved, which then names the message.
	if alice.Operation.ResultMessage != nil {
		t.Fatalf("held start names a message: %+v", alice.Operation)
	}
	got, err := env.client.Operations().Approve(env.ctx, model.DecideHeld{
		Call: model.Call{Correlation: "d"}, Operation: alice.Operation.Ref, ExpectedRevision: alice.Operation.Revision,
	})
	if err != nil || got.Operation.State != model.Queued || got.Operation.ResultMessage == nil {
		t.Fatalf("Approve = %+v, %v", got, err)
	}
	// Decided, her guard is open again.
	if next := propose("st-d", "alice"); next.AlreadyHeld || next.Operation.State != model.Held {
		t.Fatalf("start after the decision = %+v", next)
	}
}

func heldSvcLabelScope(t *testing.T, factory StoreFactory) {
	env := newLabelsEnvHeld(t, factory, uma.HeldPolicy{LabelConfigure: true})
	plugin, err := env.eng.For(model.Principal{Tenant: serviceTenant, Actor: heldSvcPlugin})
	if err != nil {
		t.Fatal(err)
	}
	propose := func(name string) model.Acceptance {
		t.Helper()
		cmd := env.cmd("")
		cmd.Hold = &model.HoldProposal{Reason: "review", TrustKey: "triage/label"}
		a, err := plugin.Labels().Configure(env.ctx, model.ConfigureLabel{Command: cmd, Label: model.Label{Ref: model.LabelRef{Tenant: serviceTenant}, Name: name}})
		if err != nil {
			t.Fatalf("held Configure(%s): %v", name, err)
		}
		return a
	}
	red, blue := propose("red"), propose("blue")
	if red.Operation.State != model.Held || blue.Operation.State != model.Held || red.AlreadyHeld || blue.AlreadyHeld ||
		red.Operation.Ref == blue.Operation.Ref {
		t.Fatalf("creates of two names = %+v, %+v, want two held operations", red, blue)
	}
	if again := propose("red"); !again.AlreadyHeld || again.Operation.Ref != red.Operation.Ref {
		t.Fatalf("second create of red = %+v, want the held one", again)
	}
	labels := func() int {
		p, err := env.client.Labels().List(env.ctx, model.ListRequest{Call: env.call(), Page: model.PageRequest{Limit: 20}})
		if err != nil {
			t.Fatal(err)
		}
		return len(p.Items)
	}
	if n := labels(); n != 0 {
		t.Fatalf("a held create wrote %d labels", n)
	}
	reviewer := env.client.Operations()
	if _, err := reviewer.Approve(env.ctx, model.DecideHeld{Call: env.call(), Operation: red.Operation.Ref, ExpectedRevision: red.Operation.Revision}); err != nil {
		t.Fatal(err)
	}
	if n := labels(); n != 1 {
		t.Fatalf("labels after one approval = %d, want 1", n)
	}
	if _, err := reviewer.Reject(env.ctx, model.DecideHeld{Call: env.call(), Operation: blue.Operation.Ref, ExpectedRevision: blue.Operation.Revision}); err != nil {
		t.Fatal(err)
	}
	if n := labels(); n != 1 {
		t.Fatalf("labels after a reject = %d, want 1: a rejected create leaves nothing", n)
	}
}
