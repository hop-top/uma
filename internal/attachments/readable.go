package attachments

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// holdersPage bounds the holding messages one read unit returns.
const holdersPage = 100

// Readable authorizes c's principal to read the attachment rec records,
// under the current audience of the messages that hold it rather than
// only the audience it was staged under:
//
//   - model.ActionAttachmentRead must allow it under its own audience, the
//     floor set when it was staged;
//   - and, while any message's parts name it, one such message must be
//     readable to the principal: model.ActionMessageRead on the message,
//     and model.ActionAttachmentRead on the attachment, both under the
//     message's audience. An attachment several messages share stays
//     readable while one of them is, so narrowing a message narrows its
//     attachments without rewriting them;
//   - an attachment no message holds, staged and not yet sent or left by
//     an edit, deletion or redaction, is readable by its stager only.
//
// A denial is model.Unauthorized; a policy or storage failure is returned.
func Readable(ctx context.Context, c *engine.Client, rec store.AttachmentRecord) error {
	ref := rec.Attachment.Ref
	if err := c.Authorize(ctx, read(rec, rec.Attachment.Metadata.Audience), ref.Ref()); err != nil {
		return err
	}
	held := false
	page := model.PageRequest{Limit: holdersPage}
	for {
		var p model.Page[model.Message]
		if err := view(ctx, c, func(r store.Reader) (err error) {
			p, err = r.MessagesByAttachment(ctx, ref, page)
			return err
		}); err != nil {
			return err
		}
		for _, m := range p.Items {
			held = true
			ok, err := holderReadable(ctx, c, rec, m)
			if err != nil || ok {
				return err
			}
		}
		if p.Complete {
			break
		}
		page.After = p.Next
	}
	if !held && rec.Stager != "" && rec.Stager == c.Principal().Actor {
		return nil
	}
	return model.NewError(model.Unauthorized, "denied by policy")
}

// ReadableThrough reports whether c's principal may read the attachment
// rec records through m, one message whose parts name it: Readable's floor
// (model.ActionAttachmentRead under the attachment's own audience) and
// its holder rule for m. A true result means Readable allows too. A
// denial is false; a policy failure is returned.
func ReadableThrough(ctx context.Context, c *engine.Client, rec store.AttachmentRecord, m model.Message) (bool, error) {
	floor := read(rec, rec.Attachment.Metadata.Audience)
	switch err := c.Authorize(ctx, floor, floor.Resource); {
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		return false, nil
	case err != nil:
		return false, err
	}
	return holderReadable(ctx, c, rec, m)
}

// holderReadable reports whether the principal may read m and, under m's
// audience, the attachment it holds. A denial is false; a policy failure
// is returned.
func holderReadable(ctx context.Context, c *engine.Client, rec store.AttachmentRecord, m model.Message) (bool, error) {
	for _, a := range []policy.Authorization{engine.MessageRead(m), read(rec, m.Audience)} {
		err := c.Authorize(ctx, a, a.Resource)
		switch {
		case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
			return false, nil
		case err != nil:
			return false, err
		}
	}
	return true, nil
}

// read is model.ActionAttachmentRead on rec under audience.
func read(rec store.AttachmentRecord, audience model.Audience) policy.Authorization {
	return policy.Authorization{
		Action: model.ActionAttachmentRead, Resource: rec.Attachment.Ref.Ref(),
		Audience: engine.ReadAudience(audience), Revision: rec.Attachment.Revision,
	}
}
