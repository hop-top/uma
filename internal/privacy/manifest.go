package privacy

import (
	"context"
	"slices"

	"hop.top/uma/internal/attachments"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// errSuperseded refuses an export holding a record redacted since it was
// built: its bytes would serve the erased content.
func errSuperseded() error {
	return &model.Error{Code: model.Conflict, SafeMessage: "export superseded by a redaction; export again", Retry: model.NoRetry}
}

// errUnreadable refuses an export holding a record its opener may not
// read now.
func errUnreadable() error {
	return model.NewError(model.Unauthorized, "export holds records the caller may not read")
}

// reauthorize admits the opener only if every record m lists exists and
// is readable to it now, under the read the export applied when writing
// it: contacts, identities, conversations and labels by their read action,
// messages and interactions under their current audience, attachments
// through their current holders. Records are read a page per unit and
// authorized outside it. A missing manifest refuses the export. Once every
// record is readable, a message, interaction or attachment redacted since
// the export was built, one not listed among its tombstones, refuses it
// with errSuperseded.
func reauthorize(ctx context.Context, c *engine.Client, m *store.ExportManifest) error {
	if m == nil {
		return errUnreadable()
	}
	superseded := false
	redacted := func(ref model.Ref, now bool) {
		if now && !slices.Contains(m.Tombstones, ref) {
			superseded = true
		}
	}
	read := func(action string, res model.Ref, rev model.Revision) error {
		return c.Authorize(ctx, policy.Authorization{Action: action, Resource: res, Revision: rev}, res)
	}
	for _, check := range []func() error{
		func() error {
			return recheck(ctx, c, m.Contacts,
				func(r store.Reader, ref model.ContactRef) (model.Contact, error) { return r.GetContact(ctx, ref) },
				func(v model.Contact) (model.ContactRef, error) {
					return v.Ref, read(model.ActionContactRead, v.Ref.Ref(), v.Revision)
				})
		},
		func() error {
			return recheck(ctx, c, m.Identities,
				func(r store.Reader, ref model.IdentityRef) (model.Identity, error) { return r.GetIdentity(ctx, ref) },
				func(v model.Identity) (model.IdentityRef, error) {
					return v.Ref, read(model.ActionIdentityRead, v.Ref.Ref(), v.Revision)
				})
		},
		func() error {
			return recheck(ctx, c, m.Conversations,
				func(r store.Reader, ref model.ConversationRef) (model.Conversation, error) {
					return r.GetConversation(ctx, ref)
				},
				func(v model.Conversation) (model.ConversationRef, error) {
					return v.Ref, read(model.ActionConversationRead, v.Ref.Ref(), v.Revision)
				})
		},
		func() error {
			return recheck(ctx, c, m.Labels,
				func(r store.Reader, ref model.LabelRef) (model.Label, error) { return r.GetLabel(ctx, ref) },
				func(v model.Label) (model.LabelRef, error) {
					return v.Ref, read(model.ActionLabelRead, v.Ref.Ref(), v.Revision)
				})
		},
		func() error {
			return recheck(ctx, c, m.Messages,
				func(r store.Reader, ref model.MessageRef) (model.Message, error) { return r.GetMessage(ctx, ref) },
				func(v model.Message) (model.MessageRef, error) {
					redacted(v.Ref.Ref(), v.Redacted)
					return v.Ref, c.Authorize(ctx, engine.MessageRead(v), v.Ref.Ref())
				})
		},
		func() error {
			return recheck(ctx, c, m.Interactions,
				func(r store.Reader, ref model.InteractionRef) (model.Interaction, error) {
					return r.Interaction(ctx, ref)
				},
				func(v model.Interaction) (model.InteractionRef, error) {
					redacted(v.Ref.Ref(), v.Redacted)
					return v.Ref, c.Authorize(ctx, engine.InteractionRead(v), v.Ref.Ref())
				})
		},
		func() error {
			return recheck(ctx, c, m.Attachments,
				func(r store.Reader, ref model.AttachmentRef) (store.AttachmentRecord, error) {
					return attachment(ctx, r, ref)
				},
				func(v store.AttachmentRecord) (model.AttachmentRef, error) {
					redacted(v.Attachment.Ref.Ref(), v.State == store.AttachmentRedacted)
					return v.Attachment.Ref, attachments.Readable(ctx, c, v)
				})
		},
	} {
		if err := check(); err != nil {
			return err
		}
	}
	if superseded {
		return errSuperseded()
	}
	return nil
}

// recheck reads refs a page per unit through get and authorizes each
// record outside the unit through auth, which also returns the reference
// the record is stored under. A missing record, one stored under another
// reference, and a denial are errUnreadable.
func recheck[R comparable, V any](ctx context.Context, c *engine.Client, refs []R,
	get func(store.Reader, R) (V, error), auth func(V) (R, error),
) error {
	for start := 0; start < len(refs); start += page {
		chunk := refs[start:min(start+page, len(refs))]
		vals := make([]V, 0, len(chunk))
		if err := view(ctx, c, func(r store.Reader) error {
			vals = vals[:0]
			for _, ref := range chunk {
				v, err := get(r, ref)
				if err != nil {
					return err
				}
				vals = append(vals, v)
			}
			return nil
		}); err != nil {
			return unreadable(err)
		}
		for i, v := range vals {
			got, err := auth(v)
			if err == nil && got != chunk[i] {
				err = engine.ErrNotFound()
			}
			if err != nil {
				return unreadable(err)
			}
		}
	}
	return nil
}

// unreadable maps a denial or a missing record to errUnreadable and keeps
// any other failure.
func unreadable(err error) error {
	if model.IsCode(err, model.Unauthorized) || model.IsCode(err, model.NotFound) {
		return errUnreadable()
	}
	return err
}
