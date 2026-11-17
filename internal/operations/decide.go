package operations

import (
	"bytes"
	"context"
	"slices"
	"sync"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Deciding a held operation. Approve, Reject and Edit are transitions of
// the one ledger entry the proposal claimed: the same operation, key and
// reference, so an approval applies under the proposal's original
// idempotency key and a second decision finds the first.
//
//   - Approve: held to queued, or to committed for a local write. The
//     operation's local mutation is written by its intent's Applier in the
//     same unit as the transition and its work is enqueued. An operation
//     held with a local mutation (model.HeldBlock.Applies) whose intent has
//     no Applier is refused, never approved with nothing written.
//   - Edit: the same, with the edited arguments in place of the proposed
//     ones. The edit is recorded (before and after, who and when) and
//     counts as a correction.
//   - Reject: held to rejected. Nothing is applied and no work is queued.
//     Counts as a correction.
//
// Every decision is authorized before anything is revealed: the read check
// of a held operation (AuthorizeRead: operation.read and the target's read
// right, else not found) and then its own action (operation.approve,
// operation.reject, operation.edit), as the principal. Approve and edit,
// which apply the operation, are authorized for the claimed action on the
// claimed target as well (the key's kind on its target, such as
// message.reply on the conversation): a decider cannot make a write the
// proposer's own principal could not be shown to be allowed. Reject applies
// nothing and needs only its own action. The decider is the principal's
// actor, never a request field.
//
// An edit is validated before it is applied: the intent's registered
// Validator (RegisterValidator) checks the edited arguments against the
// claimed key, so an edit never changes the target. An intent with no
// Validator cannot be edited.
//
// Every decision is compare-and-swap on the operation's revision
// (DecideHeld.ExpectedRevision) and refused as stale when the target
// advanced since the hold unless DecideHeld.Force is set. A decision that
// was already made returns the operation as it stands with Replayed set and
// writes nothing: a second approve, a second reject, or an edit repeated
// with the same arguments. Any other decision of a decided operation is
// model.Conflict.

// Decision is what a decision wrote, handed to the in-unit decision hook
// and shown to the caller as model.Decided.
type Decision struct {
	Operation   model.OperationRef
	Key         store.OperationKey
	Decision    model.HeldDecision
	By          model.ActorID
	At          time.Time
	TrustKey    string
	TrustLabels map[string]string
	Correction  bool
	// CorrectedKeys are the trust keys an edit changed, as the editor named
	// them (model.EditHeld.CorrectedKeys); nil for the operation's own.
	CorrectedKeys []string
	// Batch is the approval's batch (model.DecideHeld.Batch); empty means
	// the operation is its own batch.
	Batch string
}

// Applier writes the local mutation of an approved operation inside the
// approval's unit, as Submission.Apply does for an operation that is not
// held. op is the operation with the Result, ResultMessage, ResultStream
// and ResultTask named when it was held: the applier writes under exactly
// those references (an OpeningApplier is the one exception). key is the claimed key, whose Caller is the proposer;
// intent is the stored intent, or the edited arguments of an edit; rev is a
// fresh revision for the records it writes.
type Applier func(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) error

var appliers sync.Map

// RegisterApplier registers the Applier of operations whose stored intent
// is named intent. An operation is held with a local mutation only when its
// intent has one (a hold on it is otherwise model.Unsupported), and its
// approval is refused when the Applier is gone. An operation held with no
// local mutation, a provider-work intent, needs none: its approval only
// queues its work. Whether an approval commits locally or queues is
// recorded when the operation is held (model.HeldBlock.Local), not read
// from this registry. Registering a name twice replaces it.
func RegisterApplier(intent string, a Applier) { appliers.Store(intent, a) }

// OpeningApplier is an Applier of an intent whose approval decides the
// message the operation produces, because the record it lands in is chosen
// then: a held start that selects or creates its conversation on approval.
// It returns that message, which becomes the operation's ResultMessage and
// Result. It is otherwise an Applier: it writes inside the approval's unit
// and is the only exception to writing under exactly the references named
// when held, since an operation it applies names none.
type OpeningApplier func(ctx context.Context, e *engine.Engine, tx store.Tx, op model.Operation, key store.OperationKey, intent model.Extension, rev model.Revision) (model.MessageRef, error)

var openingAppliers sync.Map

// RegisterOpeningApplier registers the OpeningApplier of operations whose
// stored intent is named intent, in place of an Applier. Registering a name
// twice replaces it.
func RegisterOpeningApplier(intent string, a OpeningApplier) { openingAppliers.Store(intent, a) }

func openingApplierOf(intent string) OpeningApplier {
	if a, ok := openingAppliers.Load(intent); ok {
		ap, _ := a.(OpeningApplier)
		return ap
	}
	return nil
}

// hasApplier reports whether approving an operation of intent can write its
// local mutation.
func hasApplier(intent string) bool {
	return applierOf(intent) != nil || openingApplierOf(intent) != nil
}

func applierOf(intent string) Applier {
	if a, ok := appliers.Load(intent); ok {
		ap, _ := a.(Applier)
		return ap
	}
	return builtinAppliers[intent]
}

// Validator checks the edited arguments of a held operation against the
// operation as it was claimed: key is the claimed key, whose Target the
// edited intent must still address in every target-bearing field, stored the
// intent as proposed and edited the replacement (same name and version).
// It runs inside the unit of the edit, reads through r, and returns an error
// that is model.Validation when the arguments are refused. It is the
// submit-time check of the intent's command applied to the edit: an edit
// cannot do what the proposer's own command could not.
type Validator func(ctx context.Context, r store.Reader, key store.OperationKey, stored, edited model.Extension) error

var validators sync.Map

// RegisterValidator registers the Validator of operations whose stored
// intent is named intent, beside its Applier. An intent with none cannot be
// edited: its arguments cannot be checked against the claimed target, so
// Edit refuses it with model.Validation; approval and rejection are
// unaffected. Registering a name twice replaces it.
func RegisterValidator(intent string, v Validator) { validators.Store(intent, v) }

func validatorOf(intent string) Validator {
	if v, ok := validators.Load(intent); ok {
		vv, _ := v.(Validator)
		return vv
	}
	if v, ok := builtinValidators[intent]; ok {
		return v
	}
	return stateEditValidators()[intent]
}

// Effective returns rec with its intent as it is to be executed: the edited
// arguments of an edited operation, else the stored intent. A worker reads
// the intent of an operation through it.
func Effective(rec store.OperationRecord) store.OperationRecord {
	if h := rec.Operation.Held; h != nil && h.EditedArgs != nil {
		rec.Command = h.EditedArgs.Clone()
	}
	if rec.Operation.Held != nil {
		rec.Command = withResolvedConversation(rec.Command, rec.Operation.ResultMessage)
	}
	return rec
}

// Approve approves a held operation as proposed. See Deciding a held
// operation above.
func (s Service) Approve(ctx context.Context, req model.DecideHeld) (model.Decided, error) {
	return s.decide(ctx, req, nil, nil, model.ActionOperationApprove, model.HeldApproved)
}

// Reject rejects a held operation: nothing is applied and its note is
// recorded. See Deciding a held operation above.
func (s Service) Reject(ctx context.Context, req model.DecideHeld) (model.Decided, error) {
	return s.decide(ctx, req, nil, nil, model.ActionOperationReject, model.HeldRejected)
}

// Edit replaces the arguments of a held operation and approves it as
// edited. The arguments keep the intent's extension name and version. See
// Deciding a held operation above.
func (s Service) Edit(ctx context.Context, req model.EditHeld) (model.Decided, error) {
	if s.c.Engine().IsClosed() {
		return model.Decided{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Decided{}, err
	}
	return s.decide(ctx, req.DecideHeld, &req.Args, req.CorrectedKeys, model.ActionOperationEdit, model.HeldEdited)
}

func (s Service) decide(ctx context.Context, req model.DecideHeld, args *model.Extension, corrected []string, action string, kind model.HeldDecision) (model.Decided, error) {
	c, e := s.c, s.c.Engine()
	if e.IsClosed() {
		return model.Decided{}, engine.ErrClosed()
	}
	if err := req.Validate(); err != nil {
		return model.Decided{}, err
	}
	if req.Operation.Tenant != c.Principal().Tenant {
		return model.Decided{}, engine.ErrNotFound()
	}
	var rec store.OperationRecord
	err := e.Store().View(ctx, req.Operation.Tenant, func(r store.Reader) error {
		var err error
		rec, err = r.Operation(ctx, req.Operation)
		return err
	})
	if model.IsCode(err, model.NotFound) {
		return model.Decided{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Decided{}, err
	}
	if rec.Operation.Ref != req.Operation || rec.Operation.Held == nil {
		return model.Decided{}, engine.ErrNotFound()
	}
	if err := AuthorizeRead(ctx, c, rec); err != nil {
		return model.Decided{}, err
	}
	res := req.Operation.Ref()
	if err := c.Authorize(ctx, policy.Authorization{
		Action: action, Resource: res, Revision: rec.Operation.Revision,
	}, res); err != nil {
		return model.Decided{}, err
	}
	if kind != model.HeldRejected {
		if err := c.Authorize(ctx, policy.Authorization{
			Action: rec.Key.Kind, Resource: rec.Key.Target, Revision: rec.Operation.Revision,
		}, rec.Key.Target); err != nil {
			return model.Decided{}, err
		}
	}
	done, err := e.Begin()
	if err != nil {
		return model.Decided{}, err
	}
	defer done()
	ids := make([]model.ID, 4)
	for i := range ids {
		if ids[i], err = NewID(ctx, e); err != nil {
			return model.Decided{}, err
		}
	}
	d := decider{
		c: c, req: req, args: args, corrected: corrected, kind: kind,
		rev: model.Revision(ids[0]), applyRev: model.Revision(ids[1]), workID: ids[2], eventID: ids[3],
	}
	var out store.OperationRecord
	var replayed bool
	err = Transact(ctx, e, req.Operation.Tenant, func(tx store.Tx) error {
		var err error
		out, replayed, _, err = d.run(ctx, tx)
		return err
	})
	if err != nil {
		return model.Decided{}, err
	}
	h := out.Operation.Held
	return model.Decided{
		Operation: Shown(out), Durable: e.Store().Properties().Durable, Replayed: replayed,
		Decision: h.Decision, TrustKey: h.TrustKey, TrustLabels: cloneLabels(h.TrustLabels),
		Correction: corrects(h.Decision),
	}, nil
}

// corrects reports whether a decision counts as a correction: an edit or a
// reject. An approval as proposed and an expiry do not.
func corrects(d model.HeldDecision) bool { return d == model.HeldEdited || d == model.HeldRejected }

func cloneLabels(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// decider is one decision's unit. Its IDs and the decider's principal are
// fixed before the unit, so a rerun of the unit writes the same values.
type decider struct {
	c         *engine.Client
	req       model.DecideHeld
	args      *model.Extension
	corrected []string
	kind      model.HeldDecision
	rev       model.Revision
	applyRev  model.Revision
	workID    model.ID
	eventID   model.ID
}

func (d decider) run(ctx context.Context, tx store.Tx) (store.OperationRecord, bool, Decision, error) {
	cur, err := tx.Operation(ctx, d.req.Operation)
	switch {
	case model.IsCode(err, model.NotFound):
		return cur, false, Decision{}, engine.ErrNotFound()
	case err != nil:
		return cur, false, Decision{}, err
	}
	h := cur.Operation.Held
	if h == nil {
		return cur, false, Decision{}, engine.ErrNotFound()
	}
	if cur.Operation.State != model.Held {
		if d.repeats(cur) {
			return cur, true, Decision{}, nil
		}
		return cur, false, Decision{}, model.NewError(model.Conflict, "operation already decided")
	}
	if cur.Operation.Revision != d.req.ExpectedRevision {
		return cur, false, Decision{}, model.NewReasonError(model.Conflict, model.StaleRevision, "operation revision changed")
	}
	now := d.c.Engine().Clock().Now().UTC()
	if d.kind != model.HeldRejected && h.ExpiresAt != nil && !now.Before(*h.ExpiresAt) {
		return cur, false, Decision{}, model.NewError(model.Conflict, "held operation expired")
	}
	if !d.req.Force {
		if err := d.checkFresh(ctx, tx, cur); err != nil {
			return cur, false, Decision{}, err
		}
	}
	next := cur.Operation.Clone()
	nh := next.Held
	nh.Decision, nh.DecidedBy, nh.DecidedAt, nh.DecisionNote = d.kind, d.c.Principal().Actor, &now, d.req.Note
	next.Revision = d.rev
	intent := cur.Command
	switch d.kind {
	case model.HeldRejected:
		next.State = model.Rejected
	default:
		next.State = model.Queued
		if h.Local {
			next.State = model.Committed
		}
		ap, open := applierOf(cur.Command.Name), openingApplierOf(cur.Command.Name)
		if ap == nil && open == nil && h.Applies {
			return cur, false, Decision{}, model.NewError(model.Unsupported, "held operation cannot be approved: its intent has no applier")
		}
		if d.args != nil {
			if d.args.Name != cur.Command.Name || d.args.Version != cur.Command.Version {
				return cur, false, Decision{}, model.NewError(model.Validation, "args: must keep the intent's name and version")
			}
			validate := validatorOf(cur.Command.Name)
			if validate == nil {
				return cur, false, Decision{}, model.NewError(model.Validation, "args: this intent cannot be edited")
			}
			if err := validate(ctx, tx, cur.Key, cur.Command.Clone(), d.args.Clone()); err != nil {
				return cur, false, Decision{}, err
			}
			before := Effective(cur).Command
			after := d.args.Clone()
			nh.EditedArgs = &after
			nh.Edits = append(nh.Edits, model.HeldEdit{By: nh.DecidedBy, At: now, Before: before.Clone(), After: d.args.Clone()})
			intent = after
		}
		switch {
		case open != nil:
			msg, err := open(ctx, d.c.Engine(), tx, cur.Operation, cur.Key, intent, d.applyRev)
			if err != nil {
				return cur, false, Decision{}, err
			}
			res := msg.Ref()
			next.Result, next.ResultMessage = &res, &msg
		case ap != nil:
			if err := ap(ctx, d.c.Engine(), tx, cur.Operation, cur.Key, intent, d.applyRev); err != nil {
				return cur, false, Decision{}, err
			}
		}
	}
	if err := tx.UpdateOperation(ctx, next, cur.Operation.Revision, store.Fence{}); err != nil {
		return cur, false, Decision{}, err
	}
	if next.State == model.Queued {
		if err := tx.EnqueueWork(ctx, store.Work{ID: d.workID, Kind: WorkKindOperation, Resource: next.Ref.Ref()}); err != nil {
			return cur, false, Decision{}, err
		}
	}
	dec := Decision{
		Operation: next.Ref, Key: cur.Key, Decision: d.kind, By: nh.DecidedBy, At: now,
		TrustKey: nh.TrustKey, TrustLabels: cloneLabels(nh.TrustLabels), Correction: corrects(d.kind),
		CorrectedKeys: slices.Clone(d.corrected), Batch: d.req.Batch,
	}
	if err := recordDecision(ctx, tx, dec); err != nil {
		return cur, false, Decision{}, err
	}
	ev, err := heldEvent(d.eventID, model.EventOperationDecided, cur.Key, next, nh.DecidedBy, d.req.Correlation, now)
	if err != nil {
		return cur, false, Decision{}, err
	}
	if err := tx.AppendEvents(ctx, []model.Event{ev}); err != nil {
		return cur, false, Decision{}, err
	}
	cur.Operation = next
	return cur, false, dec, nil
}

// repeats reports whether the decision of a decided operation is this one
// made again.
func (d decider) repeats(cur store.OperationRecord) bool {
	h := cur.Operation.Held
	switch d.kind {
	case model.HeldApproved:
		return h.Decision == model.HeldApproved || h.Decision == model.HeldEdited
	case model.HeldRejected:
		return h.Decision == model.HeldRejected
	case model.HeldEdited:
		return h.Decision == model.HeldEdited && h.EditedArgs != nil &&
			h.EditedArgs.Name == d.args.Name && h.EditedArgs.Version == d.args.Version &&
			bytes.Equal(h.EditedArgs.Data, d.args.Data)
	}
	return false
}

// checkFresh refuses a decision on a proposal whose target advanced since
// it was held. Only a target with a revision is compared; a target that is
// gone is stale as well.
func (d decider) checkFresh(ctx context.Context, tx store.Tx, cur store.OperationRecord) error {
	held := cur.Operation.Held.TargetRevision
	if held == "" {
		return nil
	}
	now, err := targetRevision(ctx, tx, cur.Key.Target)
	if err != nil {
		return err
	}
	if now != held {
		return model.NewReasonError(model.Conflict, model.StaleTarget, "target changed since the operation was held")
	}
	return nil
}

// targetRevision reads the revision of a target that has one: a
// conversation. Any other target, and a conversation not found, has none.
func targetRevision(ctx context.Context, tx store.Tx, target model.Ref) (model.Revision, error) {
	if target.Kind != model.KindConversation {
		return "", nil
	}
	ref, err := target.AsConversation()
	if err != nil {
		return "", nil
	}
	conv, err := tx.GetConversation(ctx, ref)
	if model.IsCode(err, model.NotFound) {
		return "", nil
	}
	return conv.Revision, err
}
