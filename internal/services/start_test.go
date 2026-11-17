package services_test

import (
	"context"
	"sync"
	"testing"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/enginetest"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// startInbox is the per-contact inbox start tests seed on the rig channel.
const startInbox model.ID = "00000004-0000-7000-8000-000000000001"

// recordingSelector records the inputs it decides on and creates a
// conversation per start.
type recordingSelector struct {
	mu  sync.Mutex
	got []policy.SelectionInput
}

func (s *recordingSelector) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	s.mu.Lock()
	s.got = append(s.got, in)
	s.mu.Unlock()
	return policy.SelectionResult{CreateKey: "start:" + string(in.Operation.ID)}, nil
}

// newStartRig returns a rig whose adapter starts conversations unless
// noStart, with a per-contact inbox under selection sel.
func newStartRig(t *testing.T, sel model.Selection, opts ...enginetest.RigOption) (*rig, model.InboxRef) {
	t.Helper()
	r := newRig(t, opts...)
	in := model.Inbox{Ref: model.InboxRef{Tenant: tenantA, ID: startInbox}, Channel: r.Ch.Ref, Name: "outreach", Selection: sel, Revision: "in1"}
	if err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error { return tx.PutInbox(r.Ctx, in, "") }); err != nil {
		t.Fatal(err)
	}
	return r, in.Ref
}

func recipient(r *rig, id string) model.ExternalRef {
	return model.ExternalRef{Channel: r.Ch.Ref, Namespace: routeNSpace, Kind: "user", ID: id}
}

func startReq(op model.OperationID, in model.InboxRef, to model.ExternalRef) model.StartConversation {
	return model.StartConversation{
		Command: model.Command{Call: model.Call{Correlation: "corr-1"}, Operation: op},
		Inbox:   in, Recipient: to,
		Content: model.Content{Parts: []model.Part{{Kind: model.PartText, Text: "hello"}}},
	}
}

// A registered selector decides a start on the recipient's identity, the
// outbound_start event class and the start's operation, with no header
// matches.
func TestStartSelectorInput(t *testing.T) {
	sel := &recordingSelector{}
	r, in := newStartRig(t,
		model.Selection{Mode: model.ModePerContact, Policy: "outreach", LockToSingle: enginetest.Flag(true), LifecycleRules: model.RuleSetContactLifecycleV1},
		enginetest.WithStart, func(c *engine.Config, _ *outFactory) {
			c.Selectors = map[string]policy.ConversationSelector{"outreach": sel}
		})
	to := recipient(r, "u-1")
	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, to))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(sel.got) != 1 {
		t.Fatalf("selector asked %d times, want once", len(sel.got))
	}
	got := sel.got[0]
	if got.Event != policy.EventOutboundStart || got.Operation != acc.Operation.Ref || got.Sender == nil ||
		got.Sender.SourceID != to.ID || got.Sender.Namespace != to.Namespace || len(got.Matches) != 0 || got.Inbox.Ref != in {
		t.Fatalf("selection input %+v, want outbound_start for the recipient under the start's operation", got)
	}
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, to)); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(sel.got) != 1 {
		t.Fatalf("a replayed start selected again (%d selections)", len(sel.got))
	}
	if _, err := r.C.Conversations().Start(r.Ctx, startReq("op-2", in, to)); err != nil {
		t.Fatalf("second start: %v", err)
	}
	var convs []model.Conversation
	_ = r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		p, err := rd.Conversations(r.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 10}})
		convs = p.Items
		return err
	})
	if len(convs) != 2 {
		t.Fatalf("conversations = %d, want one per start under the selector's per-operation key", len(convs))
	}
}

// Without the start_conversation capability a start is unsupported and
// writes nothing, whatever the send capability says.
func TestStartNeedsDeclaredCapability(t *testing.T) {
	r, in := newStartRig(t, perContact(true))
	_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "u-1")))
	requireCode(t, err, model.Unsupported, "start_conversation")
	if n := len(r.Claim("w")); n != 0 {
		t.Fatalf("an unsupported start queued %d work items", n)
	}
	if acts := r.Policy.Actions(); len(acts) != 0 {
		t.Fatalf("decisions %v asked before the capability check", acts)
	}
}

// A capability the instance withdraws fails the start's attempt without a
// provider call.
func TestStartWithdrawnCapabilityFailsWithoutCall(t *testing.T) {
	r, in := newStartRig(t, perContact(true), enginetest.WithStart)
	r.F.Withdraw(model.CapabilityStartConversation, model.Unauthorized)
	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "u-1")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Step("w")
	enginetest.RequireState(t, r.Op(acc.Operation.Ref), model.Failed, model.Unauthorized)
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
}

// A denial at execution, after a policy change, fails the attempt without
// a provider call.
func TestStartReauthorizedAtExecution(t *testing.T) {
	var mu sync.Mutex
	deny := false
	var asked []policy.Authorization
	r, in := newStartRig(t, perContact(true), enginetest.WithStart, func(c *engine.Config, _ *outFactory) {
		c.Authorizer = policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
			mu.Lock()
			defer mu.Unlock()
			if a.Action == model.ActionConversationStart {
				asked = append(asked, a)
			}
			return policy.Decision{Allow: !deny || a.Action != model.ActionConversationStart}, nil
		})
	})
	acc, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "u-1")))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	mu.Lock()
	deny = true
	mu.Unlock()
	r.Step("w")
	enginetest.RequireState(t, r.Op(acc.Operation.Ref), model.Failed, model.Unauthorized)
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 2 || asked[1].Resource != in.Ref() || asked[1].Principal != principalA() {
		t.Fatalf("conversation.start decisions %+v, want submission and execution on the inbox as the caller", asked)
	}
	if n := len(r.F.ProviderSends()); n != 0 {
		t.Fatalf("provider sends = %d, want none", n)
	}
}

// Request shape is checked before anything is read or decided.
func TestStartValidation(t *testing.T) {
	r, in := newStartRig(t, perContact(true), enginetest.WithStart)
	to := recipient(r, "u-1")
	noContent := startReq("op-1", in, to)
	noContent.Content = model.Content{}
	withRevision := startReq("op-2", in, to)
	withRevision.ExpectedRevision = "r1"
	otherTenant := to
	otherTenant.Channel.Tenant = tenantB
	for name, c := range map[string]struct {
		req  model.StartConversation
		want string
	}{
		"no content":          {noContent, "content.parts"},
		"expected revision":   {withRevision, "expected_revision"},
		"no recipient":        {startReq("op-3", in, model.ExternalRef{}), "recipient"},
		"recipient elsewhere": {startReq("op-4", in, otherTenant), "recipient.channel.tenant"},
		"no inbox":            {startReq("op-5", model.InboxRef{}, to), "inbox"},
		"no operation":        {startReq("", in, to), "operation"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.C.Conversations().Start(r.Ctx, c.req)
			requireCode(t, err, model.Validation, c.want)
		})
	}
	if acts := r.Policy.Actions(); len(acts) != 0 {
		t.Fatalf("decisions %v asked for invalid requests", acts)
	}
}

// The claiming unit checks the inbox again: one disabled after the first
// check, here while the start is being authorized, refuses the start with
// nothing written.
func TestStartRechecksInboxInUnit(t *testing.T) {
	var r *rig
	var in model.InboxRef
	disable := func() {
		err := r.E.Store().Transact(r.Ctx, tenantA, func(tx store.Tx) error {
			cur, err := tx.GetInbox(r.Ctx, in)
			if err != nil {
				return err
			}
			next := cur
			next.Disabled, next.Revision = true, cur.Revision+"x"
			return tx.PutInbox(r.Ctx, next, cur.Revision)
		})
		if err != nil {
			t.Errorf("disable: %v", err)
		}
	}
	r, in = newStartRig(t, perContact(true), enginetest.WithStart, func(c *engine.Config, _ *outFactory) {
		c.Authorizer = policy.AuthorizeFunc(func(_ context.Context, a policy.Authorization) (policy.Decision, error) {
			if a.Action == model.ActionConversationStart {
				disable()
			}
			return policy.Decision{Allow: true}, nil
		})
	})
	_, err := r.C.Conversations().Start(r.Ctx, startReq("op-1", in, recipient(r, "u-1")))
	requireCode(t, err, model.Disabled, "")
	if n := len(r.Claim("w")); n != 0 {
		t.Fatalf("a refused start queued %d work items", n)
	}
	_ = r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		p, err := rd.Identities(r.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 10}})
		if err == nil && len(p.Items) != 0 {
			t.Errorf("a refused start left %d identities", len(p.Items))
		}
		return err
	})
}

// staleKeyStore hides claimed operation keys from OperationByKey, as a
// store whose units do not serialize that read with a concurrent claim
// would.
type staleKeyStore struct{ store.Store }

func (s staleKeyStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Store.Transact(ctx, t, func(tx store.Tx) error { return fn(staleKeyTx{tx}) })
}

type staleKeyTx struct{ store.Tx }

func (staleKeyTx) OperationByKey(context.Context, store.OperationKey) (store.OperationRecord, error) {
	return store.OperationRecord{}, model.NewError(model.NotFound, "stale read")
}

// A start that selected for a key it then finds claimed keeps nothing it
// wrote: the claim's replay never commits a second selection.
func TestStartLostClaimKeepsNoSelection(t *testing.T) {
	sel := &recordingSelector{}
	r, in := newStartRig(t,
		model.Selection{Mode: model.ModePerContact, Policy: "outreach", LockToSingle: enginetest.Flag(false), LifecycleRules: model.RuleSetContactLifecycleV1},
		enginetest.WithStart, func(c *engine.Config, _ *outFactory) {
			c.Selectors = map[string]policy.ConversationSelector{"outreach": sel}
		})
	req := startReq("op-1", in, recipient(r, "u-1"))
	if _, err := r.C.Conversations().Start(r.Ctx, req); err != nil {
		t.Fatalf("Start: %v", err)
	}
	r.Store.Set(staleKeyStore{r.Store.Inner()})
	_, err := r.C.Conversations().Start(r.Ctx, req)
	requireCode(t, err, model.Transient, "")
	var n int
	_ = r.E.Store().View(r.Ctx, tenantA, func(rd store.Reader) error {
		p, err := rd.Conversations(r.Ctx, model.ListRequest{Filter: model.Filter{Inboxes: []model.InboxRef{in}}, Page: model.PageRequest{Limit: 10}})
		n = len(p.Items)
		return err
	})
	if n != 1 {
		t.Fatalf("conversations = %d, want the first start's only", n)
	}
}
