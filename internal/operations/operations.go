// Package operations is the operation ledger: command submission with its
// operation key and fingerprint, the reply operation, attempt evidence and
// the Operations service.
package operations

import (
	"context"
	"maps"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// submission is one outbound command entering the operation ledger.
//
// The operation key is (tenant, caller, kind, target, operation ID): the
// caller is the client's principal, never a value from the request. Kind is
// also the policy action. The fingerprint covers the command's effectful
// fields; intent is everything a worker needs to execute it later.
type submission struct {
	kind        string
	target      model.Ref
	owner       model.Ref
	command     model.Command
	audience    model.Audience
	destination *model.Ref
	fingerprint []byte
	intent      model.Extension
	workKind    string
	// precheck validates mutable preconditions of a new execution inside the
	// claiming unit. It never runs for a replay: a completed command does
	// not fail because its own expected revision has since advanced.
	precheck func(context.Context, store.Tx) error
	// build, apply, local, result and resultStream are Submission's Build,
	// Apply, Local, Result and ResultStream.
	build        func(context.Context, store.Tx) (model.Extension, error)
	apply        func(context.Context, store.Tx, model.OperationRef, model.Revision) error
	local        bool
	result       *model.Ref
	resultStream *model.StreamRef
	// resultMessage is the message apply records, when the operation
	// produces one; result then names it too.
	resultMessage *model.MessageRef
	// progress is Submission's Progress.
	progress *model.OperationProgress
	// hold is Submission's Hold.
	hold *Hold
	// holdScope is Submission's HoldScope.
	holdScope string
	// open, when set, runs in the claiming unit of a new operation before
	// its claim, never for a replay: it writes the records the operation's
	// result lies in, such as the conversation a start selects or
	// creates, and returns the stored intent and the result message.
	open func(context.Context, store.Tx, model.OperationRef) (opened, error)
}

// opened is what a submission's open hook decided: the stored intent and
// the message the operation produces, none when an approval decides it.
type opened struct {
	intent  model.Extension
	message model.MessageRef
}

// submit authorizes a command, then claims its operation key and fingerprint
// and persists its intent and pending work in one unit. The same key with
// the same fingerprint returns the stored operation with Replayed set; the
// same key with a different fingerprint is model.Conflict. Authorization
// runs before any stored result is revealed.
func submit(ctx context.Context, c *engine.Client, s submission) (model.Acceptance, error) {
	err := c.Authorize(ctx, policy.Authorization{
		Action: s.kind, Resource: s.target, Audience: s.audience,
		Destination: s.destination, Revision: s.command.ExpectedRevision,
	}, s.owner)
	if err != nil {
		return model.Acceptance{}, err
	}
	if s.hold == nil && s.command.Hold != nil {
		if err := s.command.Hold.Validate(); err != nil {
			return model.Acceptance{}, err
		}
		s.hold = holdOf(*s.command.Hold)
	}
	if s.hold != nil && !holdable(s) {
		return model.Acceptance{}, model.NewError(model.Unsupported, "operation cannot be held")
	}
	if s.hold != nil && !c.Principal().Actor.IsPlugin() {
		return model.Acceptance{}, model.NewError(model.Validation, "only a plugin actor's operation can be held")
	}
	if s.hold != nil {
		if s.hold.TrustKey != "" {
			if err := model.ValidateTrustKey(s.hold.TrustKey); err != nil {
				return model.Acceptance{}, err
			}
		}
		if err := model.ValidateTrustLabels(s.hold.TrustLabels); err != nil {
			return model.Acceptance{}, err
		}
	}
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	opID, err := NewID(ctx, c.Engine())
	if err != nil {
		return model.Acceptance{}, err
	}
	workID, err := NewID(ctx, c.Engine())
	if err != nil {
		return model.Acceptance{}, err
	}
	rev, err := NewRevision(ctx, c.Engine())
	if err != nil {
		return model.Acceptance{}, err
	}
	var heldEvID model.ID
	if s.hold != nil {
		if heldEvID, err = NewID(ctx, c.Engine()); err != nil {
			return model.Acceptance{}, err
		}
	}
	var applyRev model.Revision
	if s.apply != nil {
		if applyRev, err = NewRevision(ctx, c.Engine()); err != nil {
			return model.Acceptance{}, err
		}
	}
	state := model.Queued
	if s.local {
		state = model.Committed
	}
	var held *model.HeldBlock
	if s.hold != nil {
		state, held = model.Held, s.hold.block(c.Principal().Actor, s.fingerprint)
		held.Applies, held.Local = s.writesLocally(), s.local
		held.Scope = s.holdScope
	}
	tenant := c.Principal().Tenant
	opRef := model.OperationRef{Tenant: tenant, ID: opID}
	rec := store.OperationRecord{
		Operation: model.Operation{
			Ref: opRef, State: state, Result: s.result, ResultStream: s.resultStream, ResultMessage: s.resultMessage,
			Progress: s.progress, Revision: rev, Held: held,
		},
		Key: store.OperationKey{
			Tenant: tenant, Caller: c.Principal().Actor, Kind: s.kind, Target: s.target, ID: s.command.Operation,
		},
		FingerprintVersion: FingerprintVersion1,
		Fingerprint:        s.fingerprint,
		Command:            s.intent,
	}
	var got store.OperationRecord
	var created, payloadConflict, already bool
	err = Transact(ctx, c.Engine(), tenant, func(tx store.Tx) error {
		var err error
		created, payloadConflict, already = false, false, false
		if s.hold != nil {
			dup, found, err := heldDuplicate(ctx, tx, rec.Key, s.holdScope)
			if err != nil || found {
				got, already = dup, found
				return err
			}
			if rec.Operation.Held.TargetRevision, err = targetRevision(ctx, tx, rec.Key.Target); err != nil {
				return err
			}
		}
		if s.build != nil {
			if rec.Command, err = s.build(ctx, tx); err != nil {
				return err
			}
		}
		if rec.Operation.Held != nil {
			in := rec.Command.Clone()
			rec.Operation.Held.Intent = &in
			if err := rec.Operation.Held.Validate(); err != nil {
				return err
			}
		}
		fresh, err := openNew(ctx, tx, s, &rec)
		if err != nil {
			return err
		}
		got, created, err = tx.ClaimOperation(ctx, rec)
		payloadConflict = model.IsCode(err, model.Conflict)
		if err == nil && fresh && !created {
			// A concurrent unit claimed the key after this one found it
			// free: rerun, so the replay never keeps this unit's writes.
			return model.NewError(model.Transient, "operation claimed by a concurrent unit")
		}
		if err != nil || !created {
			return err
		}
		if s.precheck != nil {
			if err := s.precheck(ctx, tx); err != nil {
				return err
			}
		}
		if s.hold != nil {
			ev, err := heldEvent(heldEvID, model.EventOperationHeld, rec.Key, rec.Operation,
				rec.Operation.Held.Proposer, s.command.Correlation, c.Engine().Clock().Now().UTC())
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, []model.Event{ev})
		}
		if s.apply != nil {
			if err := s.apply(ctx, tx, opRef, applyRev); err != nil {
				return err
			}
		}
		if s.local {
			return nil
		}
		return tx.EnqueueWork(ctx, store.Work{ID: workID, Kind: s.workKind, Resource: opRef.Ref()})
	})
	if err != nil {
		if payloadConflict {
			return model.Acceptance{}, model.NewError(model.Conflict,
				"operation ID already used with a different payload")
		}
		return model.Acceptance{}, err
	}
	if already {
		if err := authorizeDuplicate(ctx, c, got); err != nil {
			return model.Acceptance{}, err
		}
	}
	return model.Acceptance{
		Operation: Shown(got),
		Durable:   c.Engine().Store().Properties().Durable,
		Replayed:  !created, AlreadyHeld: already,
	}, nil
}

// authorizeDuplicate admits the held operation the duplicate guard found
// to the proposer it would be shown to, under the check every read of a
// held operation makes (AuthorizeRead). A proposer that may not read the
// target is refused with model.Conflict and no record: the held block is
// the draft content of that target, which the refused proposer cannot see
// at its source. The refusal carries nothing of the held operation, and
// the proposal stored nothing. That a proposal is pending is all it says;
// the guard cannot answer at all without that. A replay of the proposer's
// own key is no duplicate and never comes here.
func authorizeDuplicate(ctx context.Context, c *engine.Client, held store.OperationRecord) error {
	err := AuthorizeRead(ctx, c, held)
	if model.IsCode(err, model.NotFound) {
		return model.NewError(model.Conflict, "operation already held")
	}
	return err
}

// openNew runs s.open when the operation key is not claimed yet and
// stores what it decided in rec, reporting whether it ran. A claimed key
// leaves rec as it is: the claim returns the stored operation.
func openNew(ctx context.Context, tx store.Tx, s submission, rec *store.OperationRecord) (bool, error) {
	if s.open == nil {
		return false, nil
	}
	switch _, err := tx.OperationByKey(ctx, rec.Key); {
	case err == nil:
		return false, nil
	case !model.IsCode(err, model.NotFound):
		return false, err
	}
	o, err := s.open(ctx, tx, rec.Operation.Ref)
	if err != nil {
		return false, err
	}
	rec.Command = o.intent
	if o.message != (model.MessageRef{}) {
		result, msg := o.message.Ref(), o.message
		rec.Operation.Result, rec.Operation.ResultMessage = &result, &msg
	}
	return true, nil
}

// Service is the Operations service of one client.
type Service struct{ c *engine.Client }

// New returns the Operations service bound to c.
func New(c *engine.Client) Service { return Service{c} }

// Get returns an operation's current state after authorizing the client's
// principal to read it. A foreign or unknown operation is not found. An
// unsettled mutation carries its Pending change when the principal may
// read the target message (see withPending).
func (s Service) Get(ctx context.Context, req model.Lookup[model.OperationRef]) (model.Operation, error) {
	if s.c.Engine().IsClosed() {
		return model.Operation{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Operation{}, err
	}
	if req.Ref.Tenant != s.c.Principal().Tenant {
		return model.Operation{}, engine.ErrNotFound()
	}
	var rec store.OperationRecord
	err := s.c.Engine().Store().View(ctx, req.Ref.Tenant, func(r store.Reader) error {
		var err error
		rec, err = r.Operation(ctx, req.Ref)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Operation{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Operation{}, err
	}
	return s.readable(ctx, req.Ref.Ref(), rec)
}

// ReadAuthorization is the check that exposes a loaded operation to the
// client's principal: model.ActionOperationRead on the operation at its
// revision, owned by the operation itself. Get, Lookup and the operation
// list make exactly this check.
func ReadAuthorization(rec store.OperationRecord) (policy.Authorization, model.Ref) {
	ref := rec.Operation.Ref.Ref()
	return policy.Authorization{
		Action: model.ActionOperationRead, Resource: ref, Revision: rec.Operation.Revision,
	}, ref
}

// readable authorizes the client's principal to read a loaded operation
// (ReadAuthorization) and returns a copy. A record whose ref differs from
// resource is not found. It is the one path from a ledger record to a
// caller: Get and Lookup expose the same operation under the same check.
func (s Service) readable(ctx context.Context, resource model.Ref, rec store.OperationRecord) (model.Operation, error) {
	if rec.Operation.Ref.Ref() != resource {
		return model.Operation{}, engine.ErrNotFound()
	}
	if err := AuthorizeRead(ctx, s.c, rec); err != nil {
		return model.Operation{}, err
	}
	return withPending(ctx, s.c, rec)
}

// AuthorizeRead exposes a loaded operation to the client's principal: the
// ReadAuthorization check and, for an operation that carries a held block,
// the read right of its target as well, because the held arguments are the
// draft content of that target. Without it the operation is not found,
// exactly as an unknown one, never a redacted stub. Get, Lookup, Reconcile
// and the operation list all admit through it.
func AuthorizeRead(ctx context.Context, c *engine.Client, rec store.OperationRecord) error {
	auth, owner := ReadAuthorization(rec)
	if err := c.Authorize(ctx, auth, owner); err != nil {
		return err
	}
	if rec.Operation.Held == nil {
		return nil
	}
	t := rec.Key.Target
	action, ok := model.ReadAction(t.Kind)
	if !ok {
		return engine.ErrNotFound()
	}
	d, err := c.Decide(ctx, policy.Authorization{Action: action, Resource: t}, t)
	switch {
	case err != nil:
		return err
	case !d.Allow:
		return engine.ErrNotFound()
	}
	return nil
}

// Reruns of a unit lost to concurrent units. A serializable store aborts
// all but one of the units writing one row at once (first updater wins);
// each loser must rerun on the winner's state. With k writers in step the
// last needs k runs, and reruns started at once collide again, so reruns
// are spaced apart: full jitter over unitPauseBase doubled per consecutive
// loss, at most unitPauseMax. A unit that keeps losing after
// maxUnitAttempts runs reports model.Transient to its caller; the pauses
// add at most about 0.6s before it does. The bounds were set when every
// unit appending an event wrote the tenant's event head row, so callers of
// one tenant contended even on different resources: against PostgreSQL, 8
// callers in step lost calls to Transient with 16 immediate reruns or with
// 8 spaced ones, and none with these bounds. Units writing one row still
// contend that way, such as label changes of one conversation.
const (
	maxUnitAttempts = 16
	unitPauseBase   = time.Millisecond
	unitPauseMax    = 64 * time.Millisecond
)

// unitJitter draws a pause in [0, max]; tests replace it.
var unitJitter = engine.UniformJitter

// Transact runs fn in a unit of work, rerunning the whole unit when the
// store reports a transient serialization failure. fn must be free of side
// effects outside the unit.
func Transact(ctx context.Context, e *engine.Engine, tenant model.TenantID, fn func(store.Tx) error) error {
	var err error
	for n := range maxUnitAttempts {
		if n > 0 {
			if werr := pauseUnit(ctx, n); werr != nil {
				return werr
			}
		}
		err = e.Store().Transact(ctx, tenant, fn)
		if !model.IsCode(err, model.Transient) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// pauseUnit waits before the rerun after n consecutive losses, through a
// stdlib timer: it spaces reruns apart and is not an idle wait.
func pauseUnit(ctx context.Context, n int) error {
	d := unitJitter(min(unitPauseBase<<min(n-1, 20), unitPauseMax))
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewID allocates an internal ID. A generator failure is transient and
// malformed output permanent; either aborts before any write.
func NewID(ctx context.Context, e *engine.Engine) (model.ID, error) {
	id, err := e.IDs().New(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", model.NewError(model.Transient, "ID allocation failed")
	}
	if err := id.Validate(); err != nil {
		return "", &model.Error{Code: model.Permanent, SafeMessage: "ID generator returned a malformed ID", Retry: model.NoRetry}
	}
	return id, nil
}

// NewRevision allocates a fresh record revision.
func NewRevision(ctx context.Context, e *engine.Engine) (model.Revision, error) {
	id, err := NewID(ctx, e)
	return model.Revision(id), err
}

// holdable reports whether a command carried by Command.Hold can be held:
// approval writes no local mutation (no apply, local write or open hook),
// or an Applier is registered for its intent.
func holdable(s submission) bool {
	if !s.writesLocally() {
		return true
	}
	return hasApplier(s.intent.Name)
}

// writesLocally reports whether approving s writes a local mutation: it has
// an apply step, commits locally or opens its result's records. Such an
// intent needs an Applier to be held and to be approved.
func (s submission) writesLocally() bool { return s.apply != nil || s.local || s.open != nil }

// RefuseHold refuses a command that carries a Hold on a write that commits
// in its claiming unit and so has no approval step: a hold on it is
// model.Unsupported, claims nothing and applies nothing. A hold that is
// itself malformed is model.Validation, as on a held submission. Every
// local ledger path calls it before it claims its operation key; a write
// that asks to be held is either held or refused, never applied.
func RefuseHold(c model.Command) error {
	if c.Hold == nil {
		return nil
	}
	if err := c.Hold.Validate(); err != nil {
		return err
	}
	return model.NewError(model.Unsupported, "operation cannot be held")
}

// holdOf is the internal Hold of a public proposal.
func holdOf(p model.HoldProposal) *Hold {
	h := &Hold{
		Reason: p.Reason, OnBehalfOf: p.OnBehalfOf, TrustKey: p.TrustKey,
		TrustLabels: maps.Clone(p.TrustLabels), ExpiresAt: p.ExpiresAt,
	}
	if p.Preview != nil {
		pv := p.Preview.Clone()
		h.Preview = &pv
	}
	return h
}
