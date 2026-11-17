package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/attachments"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// StageAttachment stages inbound bytes through the Attachments service as
// the sink's service principal, once it is allowed
// model.ActionChannelReceive into the inbox the destination resolves to:
// Prepare in that inbox, then Upload, each under an operation ID derived from the
// channel and the request key, so a redelivery replays both.
func (s *Sink) StageAttachment(ctx context.Context, req adapter.StageRequest, body io.Reader) (model.AttachmentRef, error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.AttachmentRef{}, err
	}
	defer done()
	if err := model.OperationID(req.Key).Validate(); err != nil {
		return model.AttachmentRef{}, model.NewError(model.Validation, "stage.key: required, at most 255 bytes")
	}
	if req.Destination == "" {
		return model.AttachmentRef{}, model.NewError(model.Validation, "stage.destination: required")
	}
	inbox, err := s.authorizeIngress(ctx, req.Destination)
	if err != nil {
		return model.AttachmentRef{}, err
	}
	key := s.stageKey(req.Key)
	call := model.Call{Correlation: model.CorrelationID("ingress-stage-" + key[:16])}
	svc := attachments.New(s.e.Bind(s.principal))
	a, err := svc.Prepare(ctx, model.PrepareAttachment{
		Command: model.Command{Call: call, Operation: model.OperationID("ingress.stage." + key)},
		Inbox:   inbox.Ref, Metadata: req.Metadata,
	})
	if err != nil {
		return model.AttachmentRef{}, err
	}
	if a.Operation.Result == nil {
		return model.AttachmentRef{}, model.NewError(model.Permanent, "staged attachment has no reference")
	}
	ref, err := a.Operation.Result.AsAttachment()
	if err != nil {
		return model.AttachmentRef{}, err
	}
	if _, err := svc.Upload(ctx, model.Lookup[model.AttachmentRef]{Call: call, Ref: ref},
		model.Command{Call: call, Operation: model.OperationID("ingress.upload." + key)}, body); err != nil {
		return model.AttachmentRef{}, err
	}
	return ref, nil
}

// stagingInbox resolves a destination of the bound channel to its enabled
// inbox.
func (s *Sink) stagingInbox(ctx context.Context, destination string) (model.Inbox, error) {
	var in model.Inbox
	err := s.e.Store().View(ctx, s.channel.Ref.Tenant, func(r store.Reader) error {
		var err error
		in, err = r.ResolveDestination(ctx, s.channel.Ref, destination)
		return err
	})
	switch {
	case model.IsCode(err, model.NotFound):
		return model.Inbox{}, model.NewError(model.NotFound, "destination is not bound to an inbox")
	case model.IsCode(err, model.Conflict):
		return model.Inbox{}, model.NewError(model.Conflict, "destination resolves to more than one inbox")
	case err != nil:
		return model.Inbox{}, err
	case in.Channel != s.channel.Ref:
		return model.Inbox{}, model.NewError(model.NotFound, "destination is not bound to an inbox")
	case in.Disabled:
		return model.Inbox{}, model.NewError(model.Disabled, "inbox disabled")
	}
	return in, nil
}

// stageKey is the hex digest naming one staged key of the bound channel.
func (s *Sink) stageKey(key string) string {
	h := sha256.New()
	for _, p := range []string{string(s.channel.Ref.Tenant), string(s.channel.Ref.ID), key} {
		h.Write([]byte(strconv.Itoa(len(p))))
		h.Write([]byte{':'})
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil)[:20])
}

// checkAttachments requires every attachment part of c to name a staged
// attachment of the unit's inbox whose audience is audience.
func (u *ingressUnit) checkAttachments(c model.Content, audience model.Audience) error {
	for _, p := range c.Parts {
		if p.Attachment == nil {
			continue
		}
		ref := *p.Attachment
		if !ref.In(u.inbox.Ref) {
			return model.NewError(model.Validation, "envelope.content: attachment must belong to the destination inbox")
		}
		rec, err := u.tx.Attachment(u.ctx, ref)
		switch {
		case model.IsCode(err, model.NotFound):
			return model.NewError(model.Validation, "envelope.content: attachment not staged")
		case err != nil:
			return err
		case rec.Attachment.Ref != ref || rec.State != attachments.StateStored:
			return model.NewError(model.Validation, "envelope.content: attachment not staged")
		case rec.Attachment.Metadata.Audience != audience:
			return model.NewError(model.Validation, "envelope.content: attachment audience differs from the message's")
		}
	}
	return nil
}

var _ adapter.AttachmentSink = (*Sink)(nil)
