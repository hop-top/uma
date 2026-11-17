package privacy

import (
	"context"
	"encoding/json"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/ports"
	"hop.top/uma/store"
)

// target is a redaction target as read before the unit: its generic
// reference, audience and the blob keys it binds. Its revision is compared
// in the unit, so a retry of a committed redaction still replays.
type target struct {
	ref     model.Ref
	message *model.MessageRef
	// interaction is set for an interaction target.
	interaction *model.InteractionRef
	attach      *model.AttachmentRef
	audience    model.Audience
	blobs       []boundBlob
}

// boundBlob is a blob key with the attachment record binding it.
type boundBlob struct {
	attachment model.AttachmentRef
	key        ports.BlobKey
}

// Redact replaces a message, interaction or attachment, and every
// retained copy of its content, with a tombstone, as a committed
// operation; store.Tx.Redact holds the tombstone rules. ExpectedRevision
// must equal the target's revision. The principal must be allowed
// model.ActionPrivacyRedact on the target under the target's own
// audience, checked before anything changes. A message redaction emits
// message.updated with change redacted, an interaction redaction
// interaction.updated with change redacted; an attachment has no event.
// An interaction's submitted values, arguments and response token are
// cleared, as are those of the raw ingress record that created it. Bytes the tombstoned attachment
// records held are deleted from the blob store after commit; the unit
// records a staging intent for each, so a failed delete leaves them to the
// retention sweep. A message redaction never cascades; a contact, identity
// or conversation target requires Cascade, see cascade.
// PropagateToProvider is model.Unsupported.
// Retrying with the same operation ID and payload returns the stored
// operation; a different payload conflicts.
func (s Service) Redact(ctx context.Context, req model.Redact) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.PropagateToProvider {
		return model.Acceptance{}, model.NewError(model.Unsupported, "propagate_to_provider: provider redaction not supported")
	}
	if req.Target.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if req.Cascade {
		return s.cascade(ctx, req)
	}
	tg, err := load(ctx, c, req)
	if err != nil {
		return model.Acceptance{}, err
	}
	if err := c.Authorize(ctx, policy.Authorization{
		Action: model.ActionPrivacyRedact, Resource: tg.ref, Audience: tg.audience, Revision: req.ExpectedRevision,
	}, tg.ref); err != nil {
		return model.Acceptance{}, err
	}
	ids, err := allocate(ctx, e, 4)
	if err != nil {
		return model.Acceptance{}, err
	}
	rev := model.Revision(ids[0])
	var unbound []ports.BlobKey
	a, created, err := commit(ctx, c, command{
		kind: model.ActionPrivacyRedact, target: tg.ref, resultMessage: tg.message, command: req.Command,
		fingerprint: fingerprint(req),
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			res, err := tx.Redact(ctx, store.Redaction{
				Message: tg.message, Interaction: tg.interaction, Attachment: tg.attach,
				Expected: req.ExpectedRevision, Revision: rev,
			})
			if err != nil {
				return err
			}
			unbound = res.Blobs
			if tg.message != nil {
				if err := operations.ScrubHeld(ctx, tx, *tg.message, rev); err != nil {
					return err
				}
			}
			var ev model.Event
			switch {
			case tg.message != nil:
				ev, err = redactedEvent(c, ids[1], *tg.message, tg.audience, rev, req, op)
			case tg.interaction != nil:
				ev, err = interactionRedactedEvent(c, ids[1], *tg.interaction, tg.audience, rev, req, op)
			default:
				return nil
			}
			if err != nil {
				return err
			}
			return tx.AppendEvents(ctx, []model.Event{ev})
		},
	}, ids[2], model.Revision(ids[3]))
	if err == nil && created {
		deleteBlobs(ctx, c, unbound)
	} else {
		reconcile(ctx, c, tg.blobs)
	}
	return a, err
}

// load reads the target in the principal's tenant. A record under another
// reference is not found; a kind this version does not redact is
// model.Unsupported.
func load(ctx context.Context, c *engine.Client, req model.Redact) (target, error) {
	tg := target{ref: req.Target}
	switch req.Target.Kind {
	case model.KindMessage:
		ref := *req.Message
		tg.message = &ref
	case model.KindInteraction:
		ref := *req.Interaction
		tg.interaction = &ref
	case model.KindAttachment:
		ref, err := req.Target.AsAttachment()
		if err != nil {
			return target{}, err
		}
		tg.attach = &ref
	default:
		return target{}, model.NewError(model.Unsupported, "target.kind: redaction of "+string(req.Target.Kind)+" not supported")
	}
	err := view(ctx, c, func(r store.Reader) error {
		if tg.attach != nil {
			rec, err := attachment(ctx, r, *tg.attach)
			if err != nil {
				return err
			}
			tg.audience = engine.ReadAudience(rec.Attachment.Metadata.Audience)
			tg.blobs = bound(rec)
			return nil
		}
		if tg.interaction != nil {
			x, err := r.Interaction(ctx, *tg.interaction)
			if err != nil {
				return err
			}
			if x.Ref != *tg.interaction {
				return engine.ErrNotFound()
			}
			tg.audience = engine.ReadAudience(x.Audience)
			return nil
		}
		m, err := r.GetMessage(ctx, *tg.message)
		if err != nil {
			return err
		}
		if m.Ref != *tg.message {
			return engine.ErrNotFound()
		}
		tg.audience = engine.ReadAudience(m.Audience)
		for _, p := range m.Content.Parts {
			if p.Attachment == nil || p.Attachment.Scope() != c.Principal().Tenant {
				continue
			}
			rec, err := attachment(ctx, r, *p.Attachment)
			if model.IsCode(err, model.NotFound) {
				continue
			}
			if err != nil {
				return err
			}
			tg.blobs = append(tg.blobs, bound(rec)...)
		}
		return nil
	})
	return tg, err
}

func attachment(ctx context.Context, r store.Reader, ref model.AttachmentRef) (store.AttachmentRecord, error) {
	rec, err := r.Attachment(ctx, ref)
	if err != nil {
		return store.AttachmentRecord{}, err
	}
	if rec.Attachment.Ref != ref {
		return store.AttachmentRecord{}, engine.ErrNotFound()
	}
	return rec, nil
}

func bound(rec store.AttachmentRecord) []boundBlob {
	if rec.Blob == nil {
		return nil
	}
	return []boundBlob{{attachment: rec.Attachment.Ref, key: *rec.Blob}}
}

// redactedEvent is the message.updated event of a redaction.
func redactedEvent(c *engine.Client, id model.ID, ref model.MessageRef, audience model.Audience, rev model.Revision,
	req model.Redact, op model.OperationRef,
) (model.Event, error) {
	data, err := json.Marshal(model.MessageUpdatedPayload{
		Message: ref, Change: model.MessageChangeRedacted, Trigger: model.TriggerCommand,
		PreviousRevision: req.ExpectedRevision, Operation: &op,
	})
	if err != nil {
		return model.Event{}, model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := c.Engine().Clock().Now().UTC()
	res := ref.Ref()
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventMessageUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		Actor: c.Principal().Actor, OccurredAt: now, RecordedAt: now,
		Correlation: req.Correlation, Audience: audience, Revision: rev,
		Payload: model.Extension{Name: model.SchemaMessageUpdated, Version: model.PayloadVersion1, Data: data},
	}, nil
}

// interactionRedactedEvent is the interaction.updated event of a
// redaction.
func interactionRedactedEvent(c *engine.Client, id model.ID, ref model.InteractionRef, audience model.Audience, rev model.Revision,
	req model.Redact, op model.OperationRef,
) (model.Event, error) {
	data, err := json.Marshal(model.InteractionUpdatedPayload{
		Interaction: ref, Change: model.InteractionChangeRedacted, Trigger: model.TriggerCommand,
		PreviousRevision: req.ExpectedRevision, Operation: &op,
	})
	if err != nil {
		return model.Event{}, model.NewError(model.Permanent, "event payload encoding failed")
	}
	now := c.Engine().Clock().Now().UTC()
	res := ref.Ref()
	return model.Event{
		Version: model.EventEnvelopeVersion, ID: id, Kind: model.EventInteractionUpdated,
		Tenant: res.Tenant, Inbox: res.Inbox, Resource: res,
		Actor: c.Principal().Actor, OccurredAt: now, RecordedAt: now,
		Correlation: req.Correlation, Audience: audience, Revision: rev,
		Payload: model.Extension{Name: model.SchemaInteractionUpdated, Version: model.PayloadVersion1, Data: data},
	}, nil
}

// fingerprint returns the version 1 fingerprint of a redaction: kind,
// target, message, interaction, cascade, reason, provider propagation and expected
// revision.
func fingerprint(req model.Redact) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionPrivacyRedact))
	f.Set("target", operations.Field().Ref(req.Target))
	msg := operations.Field().Present(req.Message != nil)
	if req.Message != nil {
		msg.Str(string(req.Message.Conversation.ID))
	}
	f.Set("message", msg)
	if req.Interaction != nil {
		f.Set("interaction", operations.Field().Str(string(req.Interaction.Conversation.ID)))
	}
	f.Set("cascade", operations.Field().Present(req.Cascade))
	f.Set("reason", operations.Field().Str(req.Reason))
	f.Set("propagate_to_provider", operations.Field().Present(req.PropagateToProvider))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// deleteBlobs removes the bytes a committed redaction unbound and clears
// their staging intents. Cleanup outlives the caller's cancellation; a
// failure leaves the intent for the retention sweep and never fails the
// committed operation.
func deleteBlobs(ctx context.Context, c *engine.Client, keys []ports.BlobKey) {
	blobs := c.Engine().Blobs()
	if blobs == nil || len(keys) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var deleted []ports.BlobKey
	for _, k := range keys {
		if blobs.Delete(ctx, k) == nil {
			deleted = append(deleted, k)
		}
	}
	if len(deleted) == 0 {
		return
	}
	_ = operations.Transact(ctx, c.Engine(), c.Principal().Tenant, func(tx store.Tx) error {
		for _, k := range deleted {
			if err := tx.ClearStaging(ctx, k); err != nil {
				return err
			}
		}
		return nil
	})
}

// reconcile settles the blobs a redaction may have unbound after a unit
// that failed, whose outcome was unknown or that replayed an operation an
// earlier attempt committed: each key the target bound before is deleted
// when the redaction's staging intent for it is recorded, which happens
// only if the redaction committed. When the intent cannot be read the
// bytes are left to the retention sweep.
func reconcile(ctx context.Context, c *engine.Client, bound []boundBlob) {
	if c.Engine().Blobs() == nil || len(bound) == 0 {
		return
	}
	ctx = context.WithoutCancel(ctx)
	var unbound []ports.BlobKey
	err := view(ctx, c, func(r store.Reader) error {
		for _, b := range bound {
			in, err := r.Staging(ctx, b.key)
			switch {
			case model.IsCode(err, model.NotFound):
				continue
			case err != nil:
				return err
			}
			if in.Attachment == b.attachment && !in.Abandoned {
				unbound = append(unbound, b.key)
			}
		}
		return nil
	})
	if err == nil {
		deleteBlobs(ctx, c, unbound)
	}
}

func view(ctx context.Context, c *engine.Client, fn func(store.Reader) error) error {
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, fn)
	if model.IsCode(err, model.NotFound) {
		return engine.ErrNotFound()
	}
	return err
}

// allocate returns n fresh IDs, allocated before any unit of work.
func allocate(ctx context.Context, e *engine.Engine, n int) ([]model.ID, error) {
	out := make([]model.ID, n)
	for i := range out {
		id, err := operations.NewID(ctx, e)
		if err != nil {
			return nil, err
		}
		out[i] = id
	}
	return out, nil
}
