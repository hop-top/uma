package runtime

import (
	"context"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Effector executes the provider effect of the operations whose stored
// intent it names. The reply effector is built in; New takes the others.
type Effector interface {
	// Intent is the stored intent name the effector executes.
	Intent() string
	// Decode reads one stored intent of that name. It performs no I/O.
	Decode(model.Extension) (Job, error)
}

// Job is one decoded operation intent, run by the current owner of its
// work item. Load runs before any other method except Audience.
type Job interface {
	// Audience is the audience of the effect and of the operation's
	// delivery events.
	Audience() model.Audience
	// Load reads the stored state the effect needs and returns the channel
	// whose adapter executes it. A failure settles the operation.
	Load(ctx context.Context, e *engine.Engine, tenant model.TenantID) (model.Channel, error)
	// Route is where the provider applies the effect and, for an effect on
	// an existing provider resource, that resource.
	Route() (route model.ExternalRef, target *model.ExternalRef)
	// Recheck revalidates the stored state and authorizes the originating
	// actor immediately before each attempt.
	Recheck(ctx context.Context, actor *engine.Client, rec store.OperationRecord) error
	// Capabilities are the capabilities the channel's adapter must declare
	// and report available for an attempt.
	Capabilities() []model.Capability
	// Bind returns the provider call of an attempt on inst, or false when
	// inst cannot perform the effect.
	Bind(inst adapter.Instance) (ProviderCall, bool)
}

// Gated is an optional Job interface for effects that run in order or
// may need no provider call. Both are asked after Load succeeds.
type Gated interface {
	// Wait reports that a predecessor has no provider answer yet: the item
	// is deferred by the base retry delay without an attempt, evidence or
	// state change.
	Wait() bool
	// Local reports that the effect needs no provider call: the operation
	// settles as model.Committed without opening the adapter.
	Local() bool
}

// Carrier is an optional Job interface for effects that send content: the
// content an attempt carries, handed to Reconcile with the attempt.
type Carrier interface {
	SentContent() model.Content
}

// InteractionCarrier is an optional Job interface for effects that answer
// an interaction: the context, response token included, the job loaded for
// this attempt. The executor hands a copy of it to the adapter in
// adapter.Effect.Interaction, for the attempt and for its reconciliation;
// it never stores or formats the token.
type InteractionCarrier interface {
	Interaction() *adapter.InteractionContext
}

// Reacting is an optional Job interface for reaction effects: the emoji an
// attempt adds or removes, handed to Reconcile with the attempt.
type Reacting interface {
	Reaction() (emoji string, remove bool)
}

// Receipting is an optional Job interface for receipts: the receipt kind an
// attempt sends, handed to Reconcile with the attempt.
type Receipting interface {
	ReceiptKind() string
}

// Membership is an optional Job interface for membership changes: the
// participant an attempt adds or removes, handed to Reconcile with the
// attempt.
type Membership interface {
	MembershipChange() (participant model.ExternalRef, remove bool)
}

// StateChange is the request fields of a provider state effect, handed to
// Reconcile with the attempt.
type StateChange struct {
	Items        []model.ExternalRef
	Undo         bool
	Read         bool
	FlagKind     string
	Destination  model.MoveDestination
	AddLabels    []string
	RemoveLabels []string
}

// StateChanging is an optional Job interface for provider state effects.
type StateChanging interface {
	StateChange() StateChange
}

// Acting is an optional Job interface for work item acts: the status and
// status reason an attempt requests, handed to Reconcile with the attempt.
type Acting interface {
	RequestedAct() (status, reason string)
}

// Executing is an optional Job interface for extension effects: the action
// an attempt asked the provider to apply, handed to Reconcile with the
// attempt.
type Executing interface {
	RequestedAction() model.Extension
}

// reconcileRequest is the reconciliation of job's attempt eff, recorded
// at attemptedAt: the capability it exercised (the job's first), the
// content it carried, the reaction it added or removed, the work item
// act it requested and the extension action it carried.
func reconcileRequest(job Job, eff adapter.Effect, attemptedAt time.Time) adapter.ReconcileRequest {
	req := adapter.ReconcileRequest{Effect: eff, AttemptedAt: attemptedAt}
	if caps := job.Capabilities(); len(caps) > 0 {
		req.Capability = caps[0]
	}
	if s, ok := job.(Carrier); ok {
		req.Content = s.SentContent()
	}
	if r, ok := job.(Reacting); ok {
		req.Emoji, req.Remove = r.Reaction()
	}
	if a, ok := job.(Acting); ok {
		req.Status, req.Reason = a.RequestedAct()
	}
	if r, ok := job.(Receipting); ok {
		req.Receipt = r.ReceiptKind()
	}
	if m, ok := job.(Membership); ok {
		req.Participant, req.Remove = m.MembershipChange()
	}
	if st, ok := job.(StateChanging); ok {
		c := st.StateChange()
		req.Items, req.Undo, req.Read, req.FlagKind = c.Items, c.Undo, c.Read, c.FlagKind
		req.Destination, req.AddLabels, req.RemoveLabels = c.Destination, c.AddLabels, c.RemoveLabels
	}
	if x, ok := job.(Executing); ok {
		req.Action = x.RequestedAction()
	}
	return req
}

// Settler is an optional Job interface. Settle runs inside the unit that
// settles the operation, with its final state, also when applied evidence
// settles it without Load. It writes only within the unit and may run more
// than once.
type Settler interface {
	Settle(ctx context.Context, tx store.Tx, op model.Operation) error
}

// referenceChanger is an optional Job interface: the job is handed the
// reference changes the provider reported with the answer that applied its
// effect, before Settle runs in the unit that settles it.
type referenceChanger interface {
	changeReferences([]adapter.ReferenceChange)
}

// Stepper is an optional Effector interface for local operations that run
// in bounded steps rather than as a provider effect. Step runs one step of
// rec's operation for the current owner of w: inside its own units it
// writes the step's records and, under w's fence, updates the operation,
// completes w and, while the operation stays queued, enqueues its next
// item. A step whose unit fails changes nothing, so the next owner repeats
// it. Retries, quarantine and replay apply to its items as to any other.
type Stepper interface {
	Step(ctx context.Context, e *engine.Engine, w store.Work, rec store.OperationRecord) error
}

// ProviderCall issues the provider effect of one attempt.
type ProviderCall func(context.Context, adapter.Effect) (adapter.Outcome, error)

// effectors maps intent names to their effectors.
type effectors map[string]Effector

// newEffectors returns the built-in reply, start, note, refetch, work item
// act, edit, delete, react, receipt, membership and provider state effectors plus extra. A name already present
// keeps its first effector.
func newEffectors(e *engine.Engine, extra []Effector) effectors {
	out := effectors{}
	builtin := []Effector{
		replyEffector{e}, startEffector{e}, noteEffector{e}, refetchEffector{}, workItemActEffector{e},
		editEffector{e}, deleteEffector{e}, reactEffector{e}, receiptEffector{e}, membershipEffector{e},
	}
	for _, c := range stateCapabilities {
		builtin = append(builtin, stateEffector{e, c})
	}
	for _, fx := range append(builtin, extra...) {
		if fx == nil {
			continue
		}
		if _, dup := out[fx.Intent()]; !dup {
			out[fx.Intent()] = fx
		}
	}
	return out
}

// decode reads a stored intent with the effector that names it. An intent
// no effector names is model.SchemaMismatch and never retried.
func (fx effectors) decode(intent model.Extension) (Job, error) {
	if e, ok := fx[intent.Name]; ok {
		return e.Decode(intent)
	}
	return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
}

// CapabilityReady checks that the descriptor declares c and the open
// instance reports it available. A capability withdrawn for lack of
// permission is model.Unauthorized and never retried.
func CapabilityReady(ctx context.Context, d adapter.Descriptor, inst adapter.Instance, c model.Capability) *model.Error {
	return capabilityReady(ctx, d, inst, c)
}
