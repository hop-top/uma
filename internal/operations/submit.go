package operations

import (
	"context"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Submission is one outbound command a service outside this package hands
// to the operation ledger. Kind is the policy action and the operation
// key's kind; Target is the key's target and the authorized resource, and
// Owner its scope as storage records it. Intent is what a worker executes;
// an effector registered with the runtime must name it. Precheck, when set,
// validates mutable preconditions of a new execution inside the claiming
// unit and never runs for a replay.
type Submission struct {
	Kind        string
	Target      model.Ref
	Owner       model.Ref
	Command     model.Command
	Audience    model.Audience
	Fingerprint []byte
	Intent      model.Extension
	Precheck    func(context.Context, store.Tx) error
	// Build, when set, replaces Intent: it builds the stored intent inside
	// the claiming unit, before the claim, from state it only reads. It
	// never checks mutable preconditions, so a replay never fails on it.
	Build func(context.Context, store.Tx) (model.Extension, error)
	// Apply, when set, writes the command's local mutation inside the
	// claiming unit after a new claim, never for a replay; rev is a fresh
	// revision for the records it writes.
	Apply func(ctx context.Context, tx store.Tx, op model.OperationRef, rev model.Revision) error
	// Local commits the operation as model.Committed with no work: the
	// command needs no provider effect.
	Local bool
	// Result, ResultMessage and ResultStream, when set, are the operation's
	// result; ResultMessage is the message the operation produces, whose
	// provider references settlement records.
	Result        *model.Ref
	ResultMessage *model.MessageRef
	ResultStream  *model.StreamRef
	// Progress, when set, is the new operation's initial progress.
	Progress *model.OperationProgress
	// Hold, when set, stores the operation as model.Held for a human
	// decision instead of queueing it: see Hold.
	Hold *Hold
	// HoldScope narrows the duplicate guard of a held operation below its
	// kind and target: only a held operation of the same scope answers it.
	// A start scopes to its recipient and a label create to its name, as
	// kind and target alone do not tell distinct proposals apart. Empty
	// scopes by kind and target alone.
	HoldScope string
}

// Hold asks Submit to store a plugin actor's operation as held. The
// operation claims its key, as any other, and records the proposal
// (model.HeldBlock) with the intent a later approval applies; no work is
// queued, Apply does not run and no provider is called. Precheck still
// runs, so a proposal that could never apply is refused up front.
//
// Only a plugin actor (model.ActorID.IsPlugin) can hold: a Hold on a
// person's submission is model.Validation and claims nothing. The host
// decides when to hold; uma does not.
//
// While an operation of the same kind, target and scope
// (Submission.HoldScope) is held, another proposal for them, under any key, stores nothing and is answered with
// that held operation (model.Acceptance.AlreadyHeld).
type Hold struct {
	// Reason says why the host held the operation.
	Reason string
	// Preview is the dry-run of what the operation would do.
	Preview *model.Extension
	// OnBehalfOf is the actor the proposer acts for, when it does.
	OnBehalfOf model.ActorID
	// TrustKey and TrustLabels are the host's opaque trust key and its
	// listing labels; uma only stores them.
	TrustKey    string
	TrustLabels map[string]string
	// ExpiresAt is the decision deadline, when there is one.
	ExpiresAt *time.Time
}

// Submit queues any service's outbound command for Runtime.Run, which
// executes it with the Effector whose Intent names s.Intent, or commits a
// Local one in its claiming unit.
//
// It authorizes s, then claims its operation key and fingerprint and
// persists its intent and pending operation work in one unit, as Reply
// does. The same key with the same fingerprint returns the stored
// operation with Replayed set; with a different fingerprint it is
// model.Conflict.
func Submit(ctx context.Context, c *engine.Client, s Submission) (model.Acceptance, error) {
	return submit(ctx, c, submission{
		kind: s.Kind, target: s.Target, owner: s.Owner, command: s.Command, audience: s.Audience,
		fingerprint: s.Fingerprint, intent: s.Intent, workKind: WorkKindOperation, precheck: s.Precheck,
		build: s.Build, apply: s.Apply, local: s.Local, result: s.Result, resultMessage: s.ResultMessage,
		resultStream: s.ResultStream, progress: s.Progress, hold: s.Hold, holdScope: s.HoldScope,
	})
}
