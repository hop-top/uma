package enginetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/privacy"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/ports/portstest"
	"hop.top/uma/store"
	"hop.top/uma/store/memory"
)

const (
	OutKey      model.TypeKey = "com.example.chat.messages"
	OutPurpose                = "com.example.chat.api"
	ChanOne     model.ID      = "00000002-0000-7000-8000-000000000001"
	ChanTwo     model.ID      = "00000002-0000-7000-8000-000000000002"
	CredRef                   = "cred-1"
	TestEpochS                = 1_700_000_000
	RouteNSpace               = "com.example.chat"
)

// SendFunc scripts one provider answer. applied reports whether the
// provider carried out the effect, whatever the caller is told.
type SendFunc func(ctx context.Context, r adapter.SendRequest) (out adapter.Outcome, err error, applied bool)

// OutFactory is a scripted outbound adapter. Its instances resolve a
// credential for every send when Resolve is set, and record each request
// that reached the provider.
type OutFactory struct {
	Reconciler bool
	Resolve    bool
	// Notes declares the internal_note capability; notes record in
	// NotesSent.
	Notes bool
	// Start declares the start_conversation capability and the route
	// namespace as an identity namespace.
	Start bool
	// Recipients declares the reply_recipients capability.
	Recipients bool
	// Mutations declares the edit, delete and react capabilities; mutation
	// requests record in MutationCalls.
	Mutations bool
	// WorkItemActs declares the work_item_act capability with IssueActs;
	// requests record in Acts.
	WorkItemActs bool
	// Refetches declares the refetch capability.
	Refetches bool
	// SingleReaction declares single-reaction cardinality.
	SingleReaction bool
	// ReconcileSettle is the descriptor's reconcile settle window.
	ReconcileSettle time.Duration
	// Receipts declares the receipt capability; receipts record in
	// MutationCalls.
	Receipts bool
	// Membership declares the membership capability; changes record in
	// MutationCalls.
	Membership bool
	// State declares the provider state capabilities of StateCaps with the
	// limits of StateLimits; calls record in MutationCalls.
	State       bool
	StateCaps   []model.Capability
	StateLimits stateLimitsFunc
	// RawRead declares the raw_read capability; reads record in RawCalls
	// and are answered by RawFn.
	RawRead  bool
	RawFn    func(adapter.RawRequest) (adapter.RawMessage, error)
	rawCalls []adapter.RawRequest

	mu          sync.Mutex
	script      []SendFunc
	sends       []adapter.SendRequest
	applied     []adapter.SendRequest
	reconciles  []adapter.ReconcileRequest
	Opens       int
	Closes      int
	withdrawn   map[model.Capability]model.ErrorCode
	ReconcileFn func(adapter.ReconcileRequest) (adapter.Outcome, error)
	seq         int
	CredReqs    []ports.CredentialRequest
	log         []string
	openErr     error
	notes       []adapter.NoteRequest
	actScript   []ActFunc
	acts        []adapter.WorkItemActRequest
	mutations   []MutationCall
}

// FailOpen makes every later Open fail with err.
func (f *OutFactory) FailOpen(err error) {
	f.mu.Lock()
	f.openErr = err
	f.mu.Unlock()
}

func (f *OutFactory) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func (f *OutFactory) Describe() adapter.Descriptor {
	caps := []model.Capability{model.CapabilitySend}
	if f.Reconciler {
		caps = append(caps, model.CapabilityReconcile)
	}
	if f.Notes {
		caps = append(caps, model.CapabilityInternalNote)
	}
	if f.Mutations {
		caps = append(caps, model.CapabilityEdit, model.CapabilityDelete, model.CapabilityReact)
	}
	var acts []adapter.WorkItemTransition
	if f.WorkItemActs {
		caps, acts = append(caps, model.CapabilityWorkItemAct), IssueActs()
	}
	if f.Refetches {
		caps = append(caps, model.CapabilityRefetch)
	}
	if f.Receipts {
		caps = append(caps, model.CapabilityReceipt)
	}
	if f.Membership {
		caps = append(caps, model.CapabilityMembership)
	}
	if f.State {
		caps = append(caps, f.StateCaps...)
	}
	if f.RawRead {
		caps = append(caps, model.CapabilityRawRead)
	}
	exchange := ChatExchange
	if f.SingleReaction {
		exchange.Reactions = adapter.ReactionsSingle
	}
	var namespaces []string
	if f.Start {
		caps = append(caps, model.CapabilityStartConversation)
		namespaces = []string{RouteNSpace}
	}
	if f.Recipients {
		caps = append(caps, model.CapabilityReplyRecipients)
	}
	return adapter.Descriptor{
		Type: OutKey, ContractMajor: adapter.ContractMajor,
		ConfigurationSchema: model.Extension{Name: string(OutKey) + ".config", Version: 1},
		Capabilities:        caps, Events: []string{model.EventDeliveryUpdated},
		IdentityNamespaces: namespaces, Exchange: exchange, WorkItemActs: acts, ReconcileSettle: f.ReconcileSettle,
	}
}

func (f *OutFactory) Validate(model.Extension) []model.ValidationIssue { return nil }

func (f *OutFactory) Open(_ context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	f.mu.Lock()
	f.Opens++
	openErr := f.openErr
	f.mu.Unlock()
	if openErr != nil {
		return nil, openErr
	}
	base := &OutInstance{F: f, channel: ch, creds: deps.Credentials}
	if f.Reconciler {
		return &OutReconciler{base}, nil
	}
	return base, nil
}

// Respond queues scripted answers, consumed in call order. Without one, a
// send is applied and accepted.
func (f *OutFactory) Respond(fns ...SendFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script = append(f.script, fns...)
}

func (f *OutFactory) Withdraw(c model.Capability, reason model.ErrorCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.withdrawn == nil {
		f.withdrawn = map[model.Capability]model.ErrorCode{}
	}
	f.withdrawn[c] = reason
}

// Restore reports c available again after Withdraw.
func (f *OutFactory) Restore(c model.Capability) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.withdrawn, c)
}

func (f *OutFactory) ProviderSends() []adapter.SendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sends)
}

func (f *OutFactory) Reconciled() []adapter.ReconcileRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reconciles)
}

func (f *OutFactory) Ref(ch model.ChannelRef) model.ExternalRef {
	f.seq++
	return model.ExternalRef{Channel: ch, Namespace: RouteNSpace, Kind: "message", ID: fmt.Sprintf("pm%d", f.seq)}
}

type OutInstance struct {
	F       *OutFactory
	channel model.Channel
	creds   ports.Credentials
}

func (i *OutInstance) Capabilities(context.Context) ([]model.CapabilityStatus, error) {
	i.F.mu.Lock()
	defer i.F.mu.Unlock()
	var out []model.CapabilityStatus
	for _, c := range i.F.Describe().Capabilities {
		st := model.CapabilityStatus{Name: c, Available: true}
		if i.F.StateLimits != nil && slices.Contains(i.F.StateCaps, c) {
			st.Limits = i.F.StateLimits(c)
		}
		if r, ok := i.F.withdrawn[c]; ok {
			st.Available, st.Reason = false, r
		}
		out = append(out, st)
	}
	return out, nil
}

func (i *OutInstance) Close(context.Context) error {
	i.F.mu.Lock()
	i.F.Closes++
	i.F.mu.Unlock()
	return nil
}

func (i *OutInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	if i.F.Resolve {
		req := ports.CredentialRequest{Purpose: OutPurpose, Accept: []ports.CredentialKind{ports.KindSecret}}
		i.F.mu.Lock()
		i.F.CredReqs = append(i.F.CredReqs, req)
		i.F.mu.Unlock()
		if _, err := i.creds.Resolve(ctx, req); err != nil {
			return adapter.Outcome{}, err
		}
	}
	i.F.mu.Lock()
	var fn SendFunc
	if len(i.F.script) > 0 {
		fn, i.F.script = i.F.script[0], i.F.script[1:]
	}
	i.F.sends = append(i.F.sends, r)
	i.F.log = append(i.F.log, "send")
	i.F.mu.Unlock()
	if fn == nil {
		fn = Accepted
	}
	out, err, applied := fn(ctx, r)
	if applied {
		i.F.mu.Lock()
		i.F.applied = append(i.F.applied, r)
		if len(out.References) == 0 {
			out.References = []model.ExternalRef{i.F.Ref(r.Route.Channel)}
		}
		i.F.mu.Unlock()
		if err != nil || out.State == model.Unknown {
			out.References = nil
		}
	}
	return out, err
}

// SendNote records an internal note and answers it like a send.
func (i *OutInstance) SendNote(ctx context.Context, r adapter.NoteRequest) (adapter.Outcome, error) {
	i.F.mu.Lock()
	i.F.notes = append(i.F.notes, r)
	i.F.log = append(i.F.log, "note")
	i.F.mu.Unlock()
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{
		model.ProviderMessageRef(r.Route.Channel, "note-"+string(r.Attempt)),
	}}, nil
}

// NotesSent returns the internal notes that reached the provider.
func (f *OutFactory) NotesSent() []adapter.NoteRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.notes)
}

type OutReconciler struct{ *OutInstance }

// Reconcile reports the provider's record of the effect's operation, unless
// the test overrides it.
func (i *OutReconciler) Reconcile(_ context.Context, e adapter.ReconcileRequest) (adapter.Outcome, error) {
	i.F.mu.Lock()
	i.F.reconciles = append(i.F.reconciles, e)
	i.F.log = append(i.F.log, "reconcile")
	override := i.F.ReconcileFn
	var found *adapter.SendRequest
	for k := range slices.Backward(i.F.applied) {
		if i.F.applied[k].Operation == e.Operation {
			found = &i.F.applied[k]
			break
		}
	}
	i.F.mu.Unlock()
	if override != nil {
		return override(e)
	}
	if found == nil {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record")}, nil
	}
	return adapter.Outcome{State: model.ProviderAccepted, References: []model.ExternalRef{{
		Channel: found.Route.Channel, Namespace: RouteNSpace, Kind: "message", ID: "reconciled",
	}}}, nil
}

func Accepted(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.ProviderAccepted}, nil, true
}

func LostAnswer(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{State: model.Unknown, Failure: model.NewError(model.OutcomeUnknown, "answer lost")}, nil, true
}

func TransientFailure(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
	return adapter.Outcome{}, model.NewError(model.Transient, "unreachable"), false
}

func RateLimited(after time.Duration) SendFunc {
	return func(context.Context, adapter.SendRequest) (adapter.Outcome, error, bool) {
		e := model.NewError(model.RateLimited, "slow down")
		e.RetryAfter = after
		return adapter.Outcome{}, e, false
	}
}

// HostCreds is a host credential port with revocation.
type HostCreds struct {
	mu      sync.Mutex
	Revoked bool
	reqs    []ports.CredentialRequest
}

func (h *HostCreds) Resolve(_ context.Context, r ports.CredentialRequest) (ports.Credential, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reqs = append(h.reqs, r)
	if h.Revoked {
		return nil, model.NewError(model.Unauthorized, "consent revoked")
	}
	return ports.Secret{Bytes: []byte("token")}, nil
}

func (h *HostCreds) Requests() []ports.CredentialRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reqs)
}

// Rig is an engine over a memory store with one channel, inbox and
// conversation whose stored route is on that channel.
type Rig struct {
	T      *testing.T
	Store  *SwapStore
	Ctx    context.Context
	E      *engine.Engine
	RT     *runtime.Runtime
	Clock  *portstest.FixedClock
	F      *OutFactory
	Creds  *HostCreds
	Policy *SwitchPolicy
	Conv   model.Conversation
	Ch     model.Channel
	C      Client
}

// SwitchPolicy allows until denied.
type SwitchPolicy struct {
	mu     sync.Mutex
	deny   bool
	down   bool
	checks []policy.Authorization
}

func (p *SwitchPolicy) Authorize(_ context.Context, a policy.Authorization) (policy.Decision, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks = append(p.checks, a)
	if p.down {
		return policy.Decision{}, errors.New("policy service unreachable")
	}
	return policy.Decision{Allow: !p.deny}, nil
}

func (p *SwitchPolicy) Set(deny bool) {
	p.mu.Lock()
	p.deny = deny
	p.mu.Unlock()
}

// Outage makes the policy fail with an untyped error, which the engine reads
// as unavailable.
func (p *SwitchPolicy) Outage(down bool) {
	p.mu.Lock()
	p.down = down
	p.mu.Unlock()
}

// Checks returns the authorizations asked so far, in order.
func (p *SwitchPolicy) Checks() []policy.Authorization {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.checks)
}

func (p *SwitchPolicy) Actions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, a := range p.checks {
		out = append(out, a.Action)
	}
	return out
}

// RigOption adjusts a rig's composition and adapter before the engine is
// built.
type RigOption func(*engine.Config, *OutFactory)

func NewRig(t *testing.T, opts ...RigOption) *Rig {
	t.Helper()
	clock := portstest.NewFixedClock(time.Unix(TestEpochS, 0))
	f := &OutFactory{}
	r := &Rig{T: t, Ctx: t.Context(), Clock: clock, F: f, Creds: &HostCreds{}, Policy: &SwitchPolicy{}}
	r.Store = &SwapStore{inner: memory.New(memory.Options{Clock: clock})}
	cfg := engine.Config{
		Store:       r.Store,
		Authorizer:  r.Policy,
		Adapters:    []adapter.Factory{f},
		Credentials: r.Creds,
		Clock:       clock,
		IDs:         portstest.NewSequenceIDs(7),
		Waiter:      NewSignalWaiter(),
	}
	for _, o := range opts {
		o(&cfg, f)
	}
	r.E = MustNew(t, cfg)
	r.RT = runtime.New(r.E, privacy.CascadeEffector())
	r.RT.SetJitter(func(time.Duration) time.Duration { return 0 })
	t.Cleanup(func() { _ = r.E.Close(context.Background()) })
	r.Ch = model.Channel{
		Ref: model.ChannelRef{Tenant: TenantA, ID: ChanOne}, Type: OutKey,
		Configuration: model.Extension{Name: string(OutKey) + ".config", Version: 1},
		CredentialRef: CredRef, Revision: "ch1",
	}
	inbox := model.Inbox{Ref: model.InboxRef{Tenant: TenantA, ID: InboxOne}, Channel: r.Ch.Ref, Name: "support", Revision: "in1"}
	r.Conv = model.Conversation{
		Ref: model.ConversationRef{Inbox: inbox.Ref, ID: ConvOne}, Mode: model.ModePerRoom, Status: model.ConversationOpen,
		ReplyRoute: model.ExternalRef{Channel: r.Ch.Ref, Namespace: RouteNSpace, Kind: "room", ID: "room-1"},
		CreatedAt:  clock.Now(), Revision: "cv1",
	}
	other := r.Ch
	other.Ref.ID, other.Revision = ChanTwo, "ch2"
	err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error {
		for _, ch := range []model.Channel{r.Ch, other} {
			if err := tx.PutChannel(r.Ctx, ch, ""); err != nil {
				return err
			}
		}
		if err := tx.PutInbox(r.Ctx, inbox, ""); err != nil {
			return err
		}
		return tx.PutConversation(r.Ctx, r.Conv, "")
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if r.C, err = For(r.E, PrincipalA()); err != nil {
		t.Fatal(err)
	}
	return r
}

func WithReconciler(_ *engine.Config, f *OutFactory) { f.Reconciler = true }

// WithNotes declares the internal_note capability.
func WithNotes(_ *engine.Config, f *OutFactory) { f.Notes = true }

// WithStart declares the start_conversation capability.
func WithStart(_ *engine.Config, f *OutFactory) { f.Start = true }

// WithReplyRecipients declares the reply_recipients capability.
func WithReplyRecipients(_ *engine.Config, f *OutFactory) { f.Recipients = true }

// WithMutations declares the edit, delete and react capabilities.
func WithMutations(_ *engine.Config, f *OutFactory) { f.Mutations = true }

func WithCredentialResolution(_ *engine.Config, f *OutFactory) { f.Resolve = true }

func WithRetry(p engine.RetryPolicy) RigOption {
	return func(c *engine.Config, _ *OutFactory) { c.Retry = p }
}

func (r *Rig) Reply(opID model.OperationID, text string) model.Acceptance {
	r.T.Helper()
	a, err := r.C.Messages().Reply(r.Ctx, r.ReplyReq(opID, text))
	if err != nil {
		r.T.Fatalf("Reply(%s): %v", opID, err)
	}
	return a
}

func (r *Rig) ReplyReq(opID model.OperationID, text string) model.Reply {
	return model.Reply{
		Command:      model.Command{Call: model.Call{Correlation: "corr-1"}, Operation: opID},
		Conversation: r.Conv.Ref,
		Content:      model.Content{Parts: []model.Part{{Kind: model.PartText, Text: text}}},
	}
}

// Claim leases claimable operation work for owner.
func (r *Rig) Claim(owner string) []store.Work {
	r.T.Helper()
	works, err := r.RT.ClaimWork(r.Ctx, TenantA, runtime.WorkerOptions{Owner: owner, Limit: 10, Kinds: []string{operations.WorkKindOperation}, Lease: time.Minute})
	if err != nil {
		r.T.Fatalf("claim: %v", err)
	}
	return works
}

// Step claims and executes every claimable item once, as owner.
func (r *Rig) Step(owner string) int {
	r.T.Helper()
	works := r.Claim(owner)
	for _, w := range works {
		if err := r.RT.ExecuteClaimed(r.Ctx, TenantA, w); err != nil {
			r.T.Fatalf("execute %s: %v", w.ID, err)
		}
	}
	return len(works)
}

func (r *Rig) Op(ref model.OperationRef) model.Operation {
	r.T.Helper()
	op, err := r.C.Operations().Get(r.Ctx, model.Lookup[model.OperationRef]{Call: model.Call{Correlation: "get"}, Ref: ref})
	if err != nil {
		r.T.Fatalf("Operations.Get: %v", err)
	}
	return op
}

func (r *Rig) Evidence(ref model.OperationRef) []store.AttemptEvidence {
	r.T.Helper()
	var out []store.AttemptEvidence
	err := r.E.Store().View(r.Ctx, TenantA, func(rd store.Reader) error {
		p, err := rd.Attempts(r.Ctx, ref, model.PageRequest{Limit: 100})
		out = p.Items
		return err
	})
	if err != nil {
		r.T.Fatal(err)
	}
	return out
}

func (r *Rig) Events() []model.Event {
	r.T.Helper()
	var out []model.Event
	err := r.E.Store().View(r.Ctx, TenantA, func(rd store.Reader) error {
		p, err := rd.Events(r.Ctx, model.Filter{}, "", 100)
		out = p.Items
		return err
	})
	if err != nil {
		r.T.Fatal(err)
	}
	return out
}

func (r *Rig) SetRoute(route model.ExternalRef) {
	r.T.Helper()
	next := r.Conv
	next.ReplyRoute, next.Revision = route, r.Conv.Revision+"x"
	err := r.E.Store().Transact(r.Ctx, TenantA, func(tx store.Tx) error {
		return tx.PutConversation(r.Ctx, next, r.Conv.Revision)
	})
	if err != nil {
		r.T.Fatal(err)
	}
	r.Conv = next
}

func RequireState(t testing.TB, op model.Operation, want model.DeliveryState, code model.ErrorCode) {
	t.Helper()
	if op.State != want {
		t.Fatalf("state = %s (failure %v), want %s", op.State, op.Failure, want)
	}
	switch {
	case code == "" && op.Failure != nil:
		t.Fatalf("failure = %v, want none", op.Failure)
	case code != "" && (op.Failure == nil || op.Failure.Code != code):
		t.Fatalf("failure = %v, want code %s", op.Failure, code)
	}
}

// SwapStore is a store whose backend a test replaces mid-run, so a rig's
// engine sees the replacement on its next unit of work.
type SwapStore struct {
	mu    sync.Mutex
	inner store.Store
}

// Set replaces the backend.
func (s *SwapStore) Set(st store.Store) {
	s.mu.Lock()
	s.inner = st
	s.mu.Unlock()
}

// Inner returns the current backend.
func (s *SwapStore) Inner() store.Store {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inner
}

func (s *SwapStore) Properties() store.Properties { return s.Inner().Properties() }

func (s *SwapStore) View(ctx context.Context, t model.TenantID, fn func(store.Reader) error) error {
	return s.Inner().View(ctx, t, fn)
}

func (s *SwapStore) Transact(ctx context.Context, t model.TenantID, fn func(store.Tx) error) error {
	return s.Inner().Transact(ctx, t, fn)
}

// SignalWaiter reports each idle wait and blocks until resumed or
// cancelled.
type SignalWaiter struct {
	Idle   chan struct{}
	Resume chan struct{}
}

func NewSignalWaiter() *SignalWaiter {
	return &SignalWaiter{Idle: make(chan struct{}, 16), Resume: make(chan struct{})}
}

func (w *SignalWaiter) Wait(ctx context.Context, _ time.Duration) error {
	select {
	case w.Idle <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-w.Resume:
		return nil
	}
}
