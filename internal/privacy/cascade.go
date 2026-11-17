package privacy

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/internal/runtime"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// batch bounds the messages one cascade step redacts in its unit.
const batch = 100

// intentCascade names the stored intent of a cascade redaction.
const intentCascade = "uma.intent.privacy.cascade"

// cascadeIntent is what every step of a cascade reads: the target, the
// identities whose messages it owns (fixed at submission, so relinking
// never changes the set mid-way), and the revision every tombstone it
// writes takes, which marks the messages already done.
type cascadeIntent struct {
	Target      model.Ref           `json:"target"`
	Authors     []model.IdentityRef `json:"authors,omitempty"`
	Revision    model.Revision      `json:"revision"`
	Correlation model.CorrelationID `json:"correlation"`
}

// cascade queues the redaction of every message and interaction a
// contact, identity or conversation owns, with the messages' attachments:
// those of a conversation, or those the identity or the contact's
// redactable identities authored or performed, in any inbox. Submission authorizes
// model.ActionPrivacyRedactCascade on the target, and for a contact
// model.ActionPrivacyRedact on each of its identities under the
// threshold policy (see redactable), and compares the target's revision.
// Its unit tombstones the link records of the identity, or of each of the
// contact's redactable identities, through store.Tx.RedactLinks, and
// deletes their recipient logs, such as a web widget visitor's event log,
// through store.Tx.EraseRecipientLog; no
// message is redacted before a worker runs. The operation's result is the
// target; its progress counts the messages and interactions redacted and is Partial when
// the contact cascade skipped a denied identity. See cascadeEffector for
// the steps.
func (s Service) cascade(ctx context.Context, req model.Redact) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	switch req.Target.Kind {
	case model.KindContact, model.KindIdentity, model.KindConversation:
	default:
		return model.Acceptance{}, model.NewError(model.Validation, "cascade: only allowed for a contact, identity or conversation target")
	}
	if err := view(ctx, c, func(r store.Reader) error {
		_, err := targetRevision(ctx, r, req.Target)
		return err
	}); err != nil {
		return model.Acceptance{}, err
	}
	scope, chain, err := cascadeScope(ctx, c, req)
	if err != nil {
		return model.Acceptance{}, err
	}
	identities, err := authorsOf(ctx, c, scope.Target)
	if err != nil {
		return model.Acceptance{}, err
	}
	identities, partial, err := redactable(ctx, c, scope, identities)
	if err != nil {
		return model.Acceptance{}, err
	}
	var progress *model.OperationProgress
	if partial {
		progress = &model.OperationProgress{Partial: true}
	}
	authors := make([]model.IdentityRef, len(identities))
	for i, id := range identities {
		authors[i] = id.Ref
	}
	rev, err := operations.NewRevision(ctx, e)
	if err != nil {
		return model.Acceptance{}, err
	}
	data, err := json.Marshal(cascadeIntent{Target: req.Target, Authors: authors, Revision: rev, Correlation: req.Correlation})
	if err != nil {
		return model.Acceptance{}, model.NewError(model.Permanent, "operation intent cannot be stored")
	}
	target := req.Target
	return operations.Submit(ctx, c, operations.Submission{
		Kind: model.ActionPrivacyRedactCascade, Target: target, Owner: target, Command: req.Command,
		Fingerprint: fingerprint(req), Intent: model.Extension{Name: intentCascade, Version: 1, Data: data},
		Result: &target, Progress: progress,
		Precheck: func(ctx context.Context, tx store.Tx) error {
			cur, err := targetRevision(ctx, tx, target)
			if err != nil {
				return err
			}
			if cur != req.ExpectedRevision {
				return model.NewError(model.Conflict, "target revision changed")
			}
			return nil
		},
		Apply: func(ctx context.Context, tx store.Tx, _ model.OperationRef, _ model.Revision) error {
			for _, a := range authors {
				if _, err := tx.RedactLinks(ctx, a); err != nil {
					return err
				}
				if err := eraseLogs(ctx, tx, a); err != nil {
					return err
				}
			}
			for _, ref := range chain {
				if err := scrubContact(ctx, tx, ref, rev); err != nil {
					return err
				}
			}
			return nil
		},
	})
}

// scrubContact erases the display name and attributes of a contact target
// in the cascade's unit, under the cascade's revision, and keeps the
// contact, its kind, its redirect and its identities. Other targets, and a contact with
// neither field, are left as they are.
func scrubContact(ctx context.Context, tx store.Tx, target model.Ref, rev model.Revision) error {
	if target.Kind != model.KindContact {
		return nil
	}
	ref, err := target.AsContact()
	if err != nil {
		return err
	}
	ct, err := tx.GetContact(ctx, ref)
	if err != nil {
		return err
	}
	if ct.DisplayName == "" && len(ct.Attributes) == 0 {
		return nil
	}
	return tx.PutContact(ctx, model.Contact{Ref: ct.Ref, Kind: ct.Kind, Revision: rev, MergedInto: ct.MergedInto}, ct.Revision)
}

// cascadeScope is req aimed at what the cascade owns, and the targets whose
// name and attributes it scrubs. A contact merged into another owns nothing
// itself: its cascade is that of the contact its identities went to (see
// redirects), authorized and judged by that contact's identities and
// revision, and it scrubs every contact on the way. The operation, its key
// and its revision check stay the requested target's.
func cascadeScope(ctx context.Context, c *engine.Client, req model.Redact) (model.Redact, []model.Ref, error) {
	if req.Target.Kind != model.KindContact {
		return req, []model.Ref{req.Target}, nil
	}
	ref, err := req.Target.AsContact()
	if err != nil {
		return req, nil, err
	}
	var chain []model.Contact
	if err := view(ctx, c, func(r store.Reader) (err error) {
		chain, err = redirects(ctx, r, ref)
		return err
	}); err != nil {
		return req, nil, err
	}
	scrub := make([]model.Ref, len(chain))
	for i, ct := range chain {
		scrub[i] = ct.Ref.Ref()
	}
	if live := chain[len(chain)-1]; live.Ref != ref {
		req.Target, req.ExpectedRevision = live.Ref.Ref(), live.Revision
	}
	return req, scrub, nil
}

// CascadeEffector returns the runtime effector of cascade redactions.
func CascadeEffector() runtime.Effector { return cascadeEffector{} }

// cascadeEffector runs cascade redactions in steps, one per claimed item.
// A step reads the owned messages in recording order, skips those already
// carrying the cascade's tombstone revision and takes up to batch of the
// rest; it authorizes model.ActionPrivacyRedact on each as the
// originating actor, then, in one unit under the item's fence, redacts
// them, emits their message.updated events, advances the operation's
// progress and either enqueues the next step or settles the operation.
// A crashed step committed nothing, or committed everything including its
// successor, so a cascade resumes without redoing or skipping a message.
//
// The cascade settles as model.Committed once no owned message is left,
// and as model.Failed with code model.Unauthorized at the first message
// the actor may not redact: the messages before it in this step are still
// redacted, and progress counts exactly the redacted ones. A message
// whose own reply is still at work fails the step's unit; the item is
// retried once its lease expires.
type cascadeEffector struct{}

func (cascadeEffector) Intent() string { return intentCascade }

// Decode refuses: cascade operations run through Step, never as a
// provider effect.
func (cascadeEffector) Decode(model.Extension) (runtime.Job, error) {
	return nil, &model.Error{Code: model.SchemaMismatch, SafeMessage: "cascade runs in steps", Retry: model.NoRetry}
}

var errFull = errors.New("batch full")

func (cascadeEffector) Step(ctx context.Context, e *engine.Engine, w store.Work, rec store.OperationRecord) error {
	var in cascadeIntent
	if rec.Command.Version != 1 || json.Unmarshal(rec.Command.Data, &in) != nil {
		return failStep(ctx, e, w, rec, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown cascade intent", Retry: model.NoRetry})
	}
	tenant := rec.Operation.Ref.Tenant
	actor := e.Bind(model.Principal{Tenant: tenant, Actor: rec.Key.Caller})
	var pending []cascadeItem
	more := false
	err := owned(ctx, actor, in, func(m cascadeItem) error {
		if m.revision == in.Revision {
			return nil
		}
		if len(pending) == batch {
			more = true
			return errFull
		}
		pending = append(pending, m)
		return nil
	})
	if err != nil && !errors.Is(err, errFull) {
		return err
	}
	var failure *model.Error
	for i, m := range pending {
		err := actor.Authorize(ctx, policy.Authorization{
			Action: model.ActionPrivacyRedact, Resource: m.resource, Audience: m.audience, Revision: m.revision,
		}, m.resource)
		if model.IsCode(err, model.Unauthorized) || model.IsCode(err, model.NotFound) {
			failure = &model.Error{
				Code: model.Unauthorized, Retry: model.NoRetry,
				SafeMessage: "cascade stopped at a record the actor may not redact; progress counts the records redacted",
			}
			pending = pending[:i]
			break
		}
		if err != nil {
			// Policy unavailable: the item is retried once its lease
			// expires.
			return model.NewError(model.Transient, "cascade authorization unavailable")
		}
	}
	state := model.Queued
	switch {
	case failure != nil:
		state = model.Failed
	case !more:
		state = model.Committed
	}
	ids := make([]model.ID, 0, len(pending)+2)
	for range len(pending) + 2 {
		id, err := operations.NewID(ctx, e)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	var unbound []ports.BlobKey
	err = operations.Transact(ctx, e, tenant, func(tx store.Tx) error {
		cur, err := tx.Operation(ctx, rec.Operation.Ref)
		if err != nil {
			return err
		}
		if cur.Operation.Revision != rec.Operation.Revision {
			return model.NewError(model.Conflict, "cascade advanced by another owner")
		}
		unbound = unbound[:0]
		events := make([]model.Event, 0, len(pending))
		for i, m := range pending {
			res, err := tx.Redact(ctx, store.Redaction{
				Message: m.message, Interaction: m.interaction, Expected: m.revision, Revision: in.Revision,
			})
			if err != nil {
				return err
			}
			unbound = append(unbound, res.Blobs...)
			if m.message != nil {
				if err := operations.ScrubHeld(ctx, tx, *m.message, in.Revision); err != nil {
					return err
				}
			}
			req := model.Redact{Command: model.Command{Call: model.Call{Correlation: in.Correlation}, ExpectedRevision: m.revision}}
			var ev model.Event
			if m.message != nil {
				ev, err = redactedEvent(actor, ids[2+i], *m.message, m.audience, in.Revision, req, rec.Operation.Ref)
			} else {
				ev, err = interactionRedactedEvent(actor, ids[2+i], *m.interaction, m.audience, in.Revision, req, rec.Operation.Ref)
			}
			if err != nil {
				return err
			}
			events = append(events, ev)
		}
		if len(events) > 0 {
			if err := tx.AppendEvents(ctx, events); err != nil {
				return err
			}
		}
		next := cur.Operation.Clone()
		var progress model.OperationProgress
		if next.Progress != nil {
			progress = *next.Progress
		}
		progress.Done += int64(len(pending))
		next.Progress = &progress
		next.State, next.Failure, next.Revision = state, failure, model.Revision(ids[0])
		if err := tx.UpdateOperation(ctx, next, cur.Operation.Revision, w.Fence); err != nil {
			return err
		}
		if err := tx.CompleteWork(ctx, w); err != nil {
			return err
		}
		if state != model.Queued {
			return nil
		}
		return tx.EnqueueWork(ctx, store.Work{ID: ids[1], Kind: w.Kind, Resource: w.Resource})
	})
	if err != nil {
		return err
	}
	deleteBlobs(ctx, actor, unbound)
	return nil
}

// failStep fails the operation of an item that cannot run, under its fence,
// and completes the item.
func failStep(ctx context.Context, e *engine.Engine, w store.Work, rec store.OperationRecord, failure *model.Error) error {
	rev, err := operations.NewRevision(ctx, e)
	if err != nil {
		return err
	}
	return operations.Transact(ctx, e, rec.Operation.Ref.Tenant, func(tx store.Tx) error {
		next := rec.Operation.Clone()
		next.State, next.Failure, next.Revision = model.Failed, failure, rev
		if err := tx.UpdateOperation(ctx, next, rec.Operation.Revision, w.Fence); err != nil {
			return err
		}
		return tx.CompleteWork(ctx, w)
	})
}

// cascadeItem is one record a cascade redacts: a message or an
// interaction, with what authorizing and tombstoning it needs.
type cascadeItem struct {
	resource    model.Ref
	audience    model.Audience
	revision    model.Revision
	message     *model.MessageRef
	interaction *model.InteractionRef
}

func messageItem(m model.Message) cascadeItem {
	ref := m.Ref
	return cascadeItem{resource: ref.Ref(), audience: engine.ReadAudience(m.Audience), revision: m.Revision, message: &ref}
}

func interactionItem(i model.Interaction) cascadeItem {
	ref := i.Ref
	return cascadeItem{resource: ref.Ref(), audience: engine.ReadAudience(i.Audience), revision: i.Revision, interaction: &ref}
}

// owned calls each for every record the cascade owns, the messages then the
// interactions, each in recording order, a page per read unit.
func owned(ctx context.Context, c *engine.Client, in cascadeIntent, each func(cascadeItem) error) error {
	filter := model.Filter{}
	if in.Target.Kind == model.KindConversation {
		conv, err := in.Target.AsConversation()
		if err != nil {
			return err
		}
		filter.Conversation = &conv
	} else {
		for _, a := range in.Authors {
			if !slices.Contains(filter.Inboxes, a.Inbox) {
				filter.Inboxes = append(filter.Inboxes, a.Inbox)
			}
		}
		if len(filter.Inboxes) == 0 {
			return nil
		}
	}
	mine := func(id model.IdentityRef) bool {
		return in.Target.Kind == model.KindConversation || slices.Contains(in.Authors, id)
	}
	err := pages(ctx, c, store.Reader.Messages, filter, func(m model.Message) error {
		if !mine(m.Author.Identity) {
			return nil
		}
		return each(messageItem(m))
	})
	if err != nil {
		return err
	}
	return pages(ctx, c, store.Reader.Interactions, filter, func(i model.Interaction) error {
		if !mine(i.Actor.Identity) {
			return nil
		}
		return each(interactionItem(i))
	})
}

// authorsOf returns the identities whose messages and link records a
// contact or identity cascade owns: the identity itself, with only its
// reference set, or every identity linked to the contact. A conversation
// cascade owns its messages whoever wrote them, and no link record.
func authorsOf(ctx context.Context, c *engine.Client, target model.Ref) ([]model.Identity, error) {
	switch target.Kind {
	case model.KindConversation:
		return nil, nil
	case model.KindIdentity:
		ref, err := target.AsIdentity()
		if err != nil {
			return nil, err
		}
		return []model.Identity{{Ref: ref}}, nil
	}
	contact, err := target.AsContact()
	if err != nil {
		return nil, err
	}
	var out []model.Identity
	err = pages(ctx, c, store.Reader.Identities, model.Filter{Contact: &contact}, func(id model.Identity) error {
		if id.Contact == contact {
			out = append(out, id)
		}
		return nil
	})
	return out, err
}

// targetRevision reads a cascade target's current revision; a record
// under another reference is not found.
func targetRevision(ctx context.Context, r store.Reader, target model.Ref) (model.Revision, error) {
	switch target.Kind {
	case model.KindConversation:
		ref, err := target.AsConversation()
		if err != nil {
			return "", err
		}
		v, err := r.GetConversation(ctx, ref)
		if err == nil && v.Ref != ref {
			err = engine.ErrNotFound()
		}
		return v.Revision, err
	case model.KindIdentity:
		ref, err := target.AsIdentity()
		if err != nil {
			return "", err
		}
		v, err := r.GetIdentity(ctx, ref)
		if err == nil && v.Ref != ref {
			err = engine.ErrNotFound()
		}
		return v.Revision, err
	}
	ref, err := target.AsContact()
	if err != nil {
		return "", err
	}
	v, err := r.GetContact(ctx, ref)
	if err == nil && v.Ref != ref {
		err = engine.ErrNotFound()
	}
	return v.Revision, err
}
