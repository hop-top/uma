package fakeprovider_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/conformance"
	"hop.top/uma/examples/fakeprovider"
	"hop.top/uma/model"
)

// A deliberately broken full fake. Each mode is one misconduct; the
// conformance case named for it must fail against it, which proves the case
// detects that misconduct rather than passing by construction.
const (
	// brokenSharedAccount serves every channel from one provider account.
	brokenSharedAccount = "shared_account"
	// brokenAckOnError acknowledges an event the sink rejected.
	brokenAckOnError = "ack_on_error"
	// brokenWidenAudience makes every inbound message public.
	brokenWidenAudience = "widen_audience"
	// brokenReroute delivers effects to a route of its own choosing.
	brokenReroute = "reroute"
	// brokenTrustClaims takes the author from unauthenticated payload claims.
	brokenTrustClaims = "trust_claims"
	// brokenDropFields decodes extension actions into the fields it knows,
	// dropping the rest.
	brokenDropFields = "drop_fields"
	// brokenWidenEffect executes extension actions publicly.
	brokenWidenEffect = "widen_effect"
	// brokenAcceptRevoked reports every capability available and answers a
	// mutation its revoked permission refuses as applied.
	brokenAcceptRevoked = "accept_revoked"
	// brokenDropEchoRef reads an echo of the engine's own mutation without
	// its echo reference.
	brokenDropEchoRef = "drop_echo_ref"
	// brokenRetarget mutates another provider message than the one named.
	brokenRetarget = "retarget"
	// brokenRefusalTransient reports a provider refusal as a transient
	// failure, so the effect is attempted again.
	brokenRefusalTransient = "refusal_transient"
	// brokenReconcileForgets answers every reconcile that the provider has
	// no record of the effect, so an applied effect is attempted again.
	brokenReconcileForgets = "reconcile_forgets"
	// brokenReceiptAnyKind sends a receipt kind the provider has no
	// receipt for instead of refusing it.
	brokenReceiptAnyKind = "receipt_any_kind"
	// brokenReceiptTwice sends every receipt twice.
	brokenReceiptTwice = "receipt_twice"
	// brokenMembershipInverted removes a participant it was asked to add,
	// and adds one it was asked to remove.
	brokenMembershipInverted = "membership_inverted"
	// brokenHistoryUnbounded ignores the page limit of a history read.
	brokenHistoryUnbounded = "history_unbounded"
	// brokenHistoryNewestFirst serves each history page newest first.
	brokenHistoryNewestFirst = "history_newest_first"
	// brokenHistoryNoSince ignores the start of the history window.
	brokenHistoryNoSince = "history_no_since"
	// brokenHistoryOverlap starts each history page on the last message of
	// the page before.
	brokenHistoryOverlap = "history_overlap"
	// brokenHistoryNoRoute reads the channel's history when asked for one
	// conversation's.
	brokenHistoryNoRoute = "history_no_route"
	// brokenHistoryAnyCursor reads from the start on a cursor it never
	// issued.
	brokenHistoryAnyCursor = "history_any_cursor"
	// brokenHistoryForeignIDs reports past messages under other provider
	// message IDs than ingress does.
	brokenHistoryForeignIDs = "history_foreign_ids"
)

const brokenEnv = "FAKEPROVIDER_BROKEN_MODE"

// brokenCases names, per mode, each suite group and case that must fail
// against it.
var brokenCases = map[string][][2]string{
	brokenSharedAccount: {{"PolicyBypass", "BoundInboxOnly"}},
	brokenAckOnError:    {{"PolicyBypass", "SelectorStaysInsideCandidates"}},
	brokenWidenAudience: {{"PolicyBypass", "AudienceNeverWidened"}},
	brokenReroute:       {{"PolicyBypass", "StoredRouteOnly"}, {"Membership", "MembershipAddThenRemove"}},
	brokenTrustClaims:   {{"PolicyBypass", "NoIdentityFromMetadata"}},
	brokenDropFields:    {{"Extensions", "TypedRoundTripIncompatibleRejectedCorrelatedReplay"}},
	brokenWidenEffect:   {{"Extensions", "TypedRoundTripIncompatibleRejectedCorrelatedReplay"}},

	brokenAcceptRevoked: {
		{"Mutations", "MutationRevokedPermissionMakesNoCall"},
		{"Receipts", "ReceiptRevokedPermissionMakesNoCall"},
		{"Membership", "MembershipRevokedPermissionMakesNoCall"},
	},
	brokenDropEchoRef: {{"Mutations", "MutationEchoSettlesUnknown"}},
	brokenRetarget:    {{"Mutations", "EditTargetsStoredProviderMessage"}, {"Receipts", "ReceiptTargetsReceivedProviderMessage"}},

	brokenRefusalTransient: {{"Receipts", "ReceiptRejectedIsFinal"}, {"Membership", "MembershipRejectedLeavesConversation"}},
	brokenReconcileForgets: {{"Receipts", "ReceiptAnswerLostIsNeverResent"}, {"Membership", "MembershipAnswerLostIsNeverResent"}},
	brokenReceiptAnyKind:   {{"Receipts", "ReceiptKindWithoutProviderReceiptRefused"}},
	brokenReceiptTwice:     {{"Receipts", "ReceiptRetriedSubmitSentOnce"}},

	brokenMembershipInverted: {{"Membership", "MembershipAddThenRemove"}},

	brokenHistoryUnbounded:   {{"History", "HistoryWindowOldestFirstInBoundedPages"}},
	brokenHistoryNewestFirst: {{"History", "HistoryWindowOldestFirstInBoundedPages"}},
	brokenHistoryNoSince:     {{"History", "HistoryWindowOldestFirstInBoundedPages"}},
	brokenHistoryOverlap:     {{"History", "HistoryWindowOldestFirstInBoundedPages"}},
	brokenHistoryNoRoute:     {{"History", "HistoryConversationOrChannel"}},
	brokenHistoryAnyCursor:   {{"History", "HistoryUnknownCursorIsValidation"}},
	brokenHistoryForeignIDs:  {{"History", "HistoryImportSkipsInboundCopy"}},
}

// TestBrokenFakeFailsBypassCases runs each case against the broken mode it
// targets in a child test process and requires it to fail. A case the
// child skips, for example while the engine cannot receive, proves nothing
// and skips here too.
func TestBrokenFakeFailsBypassCases(t *testing.T) {
	if os.Getenv(brokenEnv) != "" {
		t.Skip("child process")
	}
	for mode, cs := range brokenCases {
		for _, c := range cs {
			t.Run(mode+"/"+c[1], func(t *testing.T) { brokenCaseFails(t, mode, c[0], c[1]) })
		}
	}
}

// brokenCaseFails runs case group/name against the broken mode in a child
// test process and requires it to fail.
func brokenCaseFails(t *testing.T, mode, group, name string) {
	path := "TestBrokenFakeHelper/" + group + "/" + name
	// Fixed arguments: the test binary and a pattern built from constants.
	cmd := exec.Command(os.Args[0], "-test.run", "^TestBrokenFakeHelper$/^"+group+"$/^"+name+"$", "-test.count=1", "-test.v") // #nosec G204 G702 -- fixed binary and pattern
	cmd.Env = append(scrubbedEnv(), brokenEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	switch {
	case strings.Contains(string(out), "--- SKIP: "+path+" "):
		t.Skipf("%s skipped against the broken fake; no evidence:\n%s", name, out)
	case err == nil || !strings.Contains(string(out), "--- FAIL: "+path+" "):
		t.Fatalf("%s passed against the %s fake (err %v):\n%s", name, mode, err, out)
	}
}

// TestBrokenFakeHelper runs the suite against the broken fake; only the
// child process of TestBrokenFakeFailsBypassCases runs it.
func TestBrokenFakeHelper(t *testing.T) {
	mode := os.Getenv(brokenEnv)
	if mode == "" {
		t.Skip("run by TestBrokenFakeFailsBypassCases")
	}
	h := conformance.AdapterHarness{
		New: func(*testing.T) (adapter.Factory, conformance.Provider) {
			f := fakeprovider.NewFull()
			p := &provider{f: f}
			if mode == brokenSharedAccount {
				p.account = func(ch model.ChannelRef) model.ChannelRef { return sharedAccount(ch.Tenant) }
			}
			return &brokenFactory{inner: f, mode: mode}, p
		},
		Capabilities: fakeprovider.NewFull().Describe().Capabilities,
		Extensions:   fakeprovider.NewFull().Extensions(),
		Action:       pinAction(),
	}
	if mode == brokenReceiptAnyKind {
		// The provider has no delivered receipt, which the adapter ignores.
		h.UnsupportedReceiptKinds = []string{model.ReceiptDelivered}
	}
	conformance.RunAdapterSuite(t, h)
}

func scrubbedEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, brokenEnv+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// sharedAccount is the one channel whose script serves every channel in
// shared_account mode.
func sharedAccount(t model.TenantID) model.ChannelRef {
	return model.ChannelRef{Tenant: t, ID: "00000000-0000-7000-8000-00000000ffff"}
}

type brokenFactory struct {
	inner *fakeprovider.Factory
	mode  string
}

func (b *brokenFactory) Describe() adapter.Descriptor { return b.inner.Describe() }

func (b *brokenFactory) Validate(c model.Extension) []model.ValidationIssue {
	return b.inner.Validate(c)
}

func (b *brokenFactory) Open(ctx context.Context, ch model.Channel, deps adapter.Dependencies) (adapter.Instance, error) {
	x := &brokenInstance{mode: b.mode, actual: ch.Ref, account: ch.Ref}
	if b.mode == brokenSharedAccount {
		x.account = sharedAccount(ch.Ref.Tenant)
		ch.Ref = x.account
	}
	inst, err := b.inner.Open(ctx, ch, deps)
	if err != nil {
		return nil, err
	}
	x.full = inst.(*fakeprovider.Full)
	return x, nil
}

// brokenInstance wraps a full fake instance opened on account, while the
// engine bound it to actual.
type brokenInstance struct {
	full    *fakeprovider.Full
	mode    string
	actual  model.ChannelRef
	account model.ChannelRef
}

func (x *brokenInstance) Capabilities(ctx context.Context) ([]model.CapabilityStatus, error) {
	st, err := x.full.Capabilities(ctx)
	if x.mode == brokenAcceptRevoked {
		for i := range st {
			st[i].Available, st[i].Reason = true, ""
		}
	}
	return st, err
}

// mutated answers a mutation: in accept_revoked mode a refusal for a
// revoked permission is reported as applied.
func (x *brokenInstance) mutated(out adapter.Outcome, err error) (adapter.Outcome, error) {
	if x.mode == brokenAcceptRevoked && model.CodeOf(err) == model.Unauthorized {
		return adapter.Outcome{State: model.ProviderAccepted}, nil
	}
	if x.mode == brokenRefusalTransient && err == nil && out.State == model.Rejected {
		return adapter.Outcome{}, model.NewError(model.Transient, "refusal reported as transient")
	}
	return x.back(out), err
}
func (x *brokenInstance) Close(ctx context.Context) error { return x.full.Close(ctx) }
func (x *brokenInstance) Run(ctx context.Context, sink adapter.Sink) error {
	return x.full.Run(ctx, brokenSink{Sink: sink, x: x})
}

func (x *brokenInstance) Send(ctx context.Context, r adapter.SendRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	out, err := x.full.Send(ctx, r)
	return x.back(out), err
}

func (x *brokenInstance) Edit(ctx context.Context, r adapter.EditRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	out, err := x.full.Edit(ctx, r)
	return x.mutated(out, err)
}

func (x *brokenInstance) Delete(ctx context.Context, r adapter.DeleteRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	out, err := x.full.Delete(ctx, r)
	return x.mutated(out, err)
}

func (x *brokenInstance) React(ctx context.Context, r adapter.ReactRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	out, err := x.full.React(ctx, r)
	return x.mutated(out, err)
}

func (x *brokenInstance) Execute(ctx context.Context, r adapter.ExecuteRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	switch x.mode {
	case brokenDropFields:
		var known struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(r.Action.Data, &known); err != nil {
			return adapter.Outcome{}, err
		}
		data, err := json.Marshal(known)
		if err != nil {
			return adapter.Outcome{}, err
		}
		r.Action.Data = data
	case brokenWidenEffect:
		r.Audience = model.Audience{Visibility: model.Public}
	}
	out, err := x.full.Execute(ctx, r)
	return x.back(out), err
}

func (x *brokenInstance) Reconcile(ctx context.Context, r adapter.ReconcileRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	out, err := x.full.Reconcile(ctx, r)
	if x.mode == brokenReconcileForgets && err == nil {
		return adapter.Outcome{State: model.Failed, Failure: model.NewError(model.NotFound, "no record of the effect")}, nil
	}
	return x.back(out), err
}

func (x *brokenInstance) SendReceipt(ctx context.Context, r adapter.ReceiptRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	out, err := x.full.SendReceipt(ctx, r)
	if x.mode == brokenReceiptTwice && err == nil {
		out, err = x.full.SendReceipt(ctx, r)
	}
	return x.mutated(out, err)
}

func (x *brokenInstance) ChangeMembership(ctx context.Context, r adapter.MembershipRequest) (adapter.Outcome, error) {
	r.Effect = x.outbound(r.Effect)
	r.Participant = x.onto(r.Participant, x.actual, x.account)
	if x.mode == brokenMembershipInverted {
		r.Remove = !r.Remove
	}
	out, err := x.full.ChangeMembership(ctx, r)
	return x.mutated(out, err)
}

func (x *brokenInstance) History(ctx context.Context, r adapter.HistoryRequest) (adapter.HistoryPage, error) {
	if r.Route != nil {
		rt := x.onto(*r.Route, x.actual, x.account)
		r.Route = &rt
	}
	switch x.mode {
	case brokenHistoryUnbounded:
		r.Limit = 1000
	case brokenHistoryNoSince:
		r.Since = time.Time{}
	case brokenHistoryNoRoute:
		r.Route = nil
	case brokenHistoryAnyCursor:
		if _, err := strconv.Atoi(r.Cursor); err != nil {
			r.Cursor = ""
		}
	}
	p, err := x.full.History(ctx, r)
	if err != nil {
		return p, err
	}
	for i := range p.Envelopes {
		env := &p.Envelopes[i]
		env.Sender = x.onto(env.Sender, x.account, x.actual)
		if x.mode == brokenHistoryForeignIDs {
			env.MessageID = "foreign-" + env.MessageID
		}
	}
	switch x.mode {
	case brokenHistoryNewestFirst:
		slices.Reverse(p.Envelopes)
	case brokenHistoryOverlap:
		if n, err := strconv.Atoi(p.Next); err == nil && n > 1 {
			p.Next = strconv.Itoa(n - 1)
		}
	}
	return p, nil
}

func (x *brokenInstance) Probe(ctx context.Context) ([]model.CapabilityStatus, error) {
	return x.full.Probe(ctx)
}

// outbound moves an effect onto the serving account and, in reroute mode,
// away from the route the engine chose.
func (x *brokenInstance) outbound(e adapter.Effect) adapter.Effect {
	e.Route = x.onto(e.Route, x.actual, x.account)
	if e.Target != nil {
		t := x.onto(*e.Target, x.actual, x.account)
		e.Target = &t
	}
	if x.mode == brokenReroute {
		e.Route.ID = "rerouted-" + e.Route.ID
	}
	if x.mode == brokenRetarget && e.Target != nil {
		t := *e.Target
		t.ID = "other-" + t.ID
		e.Target = &t
	}
	return e
}

func (x *brokenInstance) back(out adapter.Outcome) adapter.Outcome {
	for i, r := range out.References {
		out.References[i] = x.onto(r, x.account, x.actual)
	}
	return out
}

func (x *brokenInstance) onto(r model.ExternalRef, from, to model.ChannelRef) model.ExternalRef {
	if r.Channel == from {
		r.Channel = to
	}
	return r
}

// brokenSink applies the inbound misconduct before the engine's sink.
type brokenSink struct {
	adapter.Sink
	x *brokenInstance
}

func (s brokenSink) Accept(ctx context.Context, in model.Ingress) (model.IngressReceipt, error) {
	x, env := s.x, &in.Envelope
	env.Sender = x.onto(env.Sender, x.account, x.actual)
	for i, r := range env.References {
		env.References[i] = x.onto(r, x.account, x.actual)
	}
	switch x.mode {
	case brokenDropEchoRef:
		env.References = slices.DeleteFunc(env.References, func(r model.ExternalRef) bool { return r.Namespace == model.EchoNamespace })
	case brokenWidenAudience:
		env.Audience.Visibility = model.Public
	case brokenTrustClaims:
		var p struct {
			Claimed []string `json:"claimed_senders"`
		}
		if env.Payload.Name == eventPayload && json.Unmarshal(env.Payload.Data, &p) == nil && len(p.Claimed) > 0 {
			env.Sender.ID = p.Claimed[0]
		}
	}
	r, err := s.Sink.Accept(ctx, in)
	if err != nil && x.mode == brokenAckOnError {
		return model.IngressReceipt{}, nil
	}
	return r, err
}

var (
	_ adapter.Receiver          = (*brokenInstance)(nil)
	_ adapter.Sender            = (*brokenInstance)(nil)
	_ adapter.Editor            = (*brokenInstance)(nil)
	_ adapter.Deleter           = (*brokenInstance)(nil)
	_ adapter.Reactor           = (*brokenInstance)(nil)
	_ adapter.Reconciler        = (*brokenInstance)(nil)
	_ adapter.ExtensionExecutor = (*brokenInstance)(nil)
	_ adapter.PermissionProber  = (*brokenInstance)(nil)
	_ adapter.ReceiptSender     = (*brokenInstance)(nil)
	_ adapter.MembershipChanger = (*brokenInstance)(nil)
	_ adapter.HistoryReader     = (*brokenInstance)(nil)
)
