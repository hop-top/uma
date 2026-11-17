package privacy

import (
	"context"
	"encoding/json"
	"slices"

	"hop.top/uma/internal/attachments"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// exporter writes one export's lines and lists every record it writes in
// its manifest. Records are read a page per unit and authorized outside
// it, each under its own audience; a record the principal may not read is
// skipped without trace.
type exporter struct {
	ctx context.Context
	c   *engine.Client
	enc *json.Encoder

	conversations []model.ConversationRef
	// labels are the labels the written conversations hold, in order of
	// first reference.
	labels      []model.LabelRef
	attachments []model.AttachmentRef
	manifest    store.ExportManifest
}

func (x *exporter) run(header model.Export) error {
	f := header.Filter
	if err := x.line(model.ExportLineExport, header); err != nil {
		return err
	}
	switch {
	case f.Contact != nil:
		authors, inboxes, err := x.contact(*f.Contact, f.Inboxes)
		if err != nil {
			return err
		}
		if len(inboxes) > 0 {
			err = x.messages(model.Filter{Inboxes: inboxes}, func(m model.Message) bool {
				return slices.Contains(authors, m.Author.Identity)
			})
			if err != nil {
				return err
			}
			err = x.interactions(model.Filter{Inboxes: inboxes}, func(i model.Interaction) bool {
				return slices.Contains(authors, i.Actor.Identity)
			})
			if err != nil {
				return err
			}
		}
	case f.Conversation != nil:
		x.conversations = append(x.conversations, *f.Conversation)
		if err := x.messages(model.Filter{Conversation: f.Conversation}, nil); err != nil {
			return err
		}
		if err := x.interactions(model.Filter{Conversation: f.Conversation}, nil); err != nil {
			return err
		}
	default:
		if err := pages(x.ctx, x.c, store.Reader.Conversations, model.Filter{Inboxes: f.Inboxes}, func(cv model.Conversation) error {
			x.addConversation(cv.Ref)
			return nil
		}); err != nil {
			return err
		}
		if err := x.messages(model.Filter{Inboxes: f.Inboxes}, nil); err != nil {
			return err
		}
		if err := x.interactions(model.Filter{Inboxes: f.Inboxes}, nil); err != nil {
			return err
		}
	}
	if err := x.writeConversations(); err != nil {
		return err
	}
	if err := x.writeLabels(); err != nil {
		return err
	}
	return x.writeAttachments()
}

func (x *exporter) line(typ string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return model.NewError(model.Permanent, "export record encoding failed")
	}
	return x.enc.Encode(model.ExportLine{Type: typ, Data: data})
}

// readable authorizes one record read; a denial skips the record.
func (x *exporter) readable(a policy.Authorization) (bool, error) {
	return allowed(x.c.Authorize(x.ctx, a, a.Resource))
}

// allowed reports an authorization's outcome: true when it allows, false
// when it denies or the record is not found, and any other failure.
func allowed(err error) (bool, error) {
	switch {
	case err == nil:
		return true, nil
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		return false, nil
	}
	return false, err
}

// contact writes the contact and its identities, limited to inboxes when
// set, and returns every identity's reference and inbox. A contact merged
// into another is exported as the contact its identities went to.
func (x *exporter) contact(ref model.ContactRef, inboxes []model.InboxRef) ([]model.IdentityRef, []model.InboxRef, error) {
	var contact model.Contact
	if err := view(x.ctx, x.c, func(r store.Reader) (err error) {
		contact, err = surviving(x.ctx, r, ref)
		return err
	}); err != nil {
		return nil, nil, err
	}
	ref = contact.Ref
	ok, err := x.readable(policy.Authorization{Action: model.ActionContactRead, Resource: ref.Ref(), Revision: contact.Revision})
	if err != nil {
		return nil, nil, err
	}
	if ok {
		if err := x.line(model.ExportLineContact, contact); err != nil {
			return nil, nil, err
		}
		x.manifest.Contacts = append(x.manifest.Contacts, ref)
	}
	var authors []model.IdentityRef
	var in []model.InboxRef
	err = pages(x.ctx, x.c, store.Reader.Identities, model.Filter{Contact: &ref, Inboxes: inboxes}, func(id model.Identity) error {
		if id.Contact != ref {
			return nil
		}
		authors = append(authors, id.Ref)
		if !slices.Contains(in, id.Ref.Inbox) {
			in = append(in, id.Ref.Inbox)
		}
		ok, err := x.readable(policy.Authorization{Action: model.ActionIdentityRead, Resource: id.Ref.Ref(), Revision: id.Revision})
		if err != nil || !ok {
			return err
		}
		x.manifest.Identities = append(x.manifest.Identities, id.Ref)
		return x.line(model.ExportLineIdentity, id)
	})
	return authors, in, err
}

// messages writes the readable messages f selects that keep accepts, and
// collects their conversations and attachments.
func (x *exporter) messages(f model.Filter, keep func(model.Message) bool) error {
	return pages(x.ctx, x.c, store.Reader.Messages, f, func(m model.Message) error {
		if keep != nil && !keep(m) {
			return nil
		}
		ok, err := x.readable(engine.MessageRead(m))
		if err != nil || !ok {
			return err
		}
		if err := x.line(model.ExportLineMessage, m); err != nil {
			return err
		}
		x.manifest.Messages = append(x.manifest.Messages, m.Ref)
		if m.Redacted {
			x.manifest.Tombstones = append(x.manifest.Tombstones, m.Ref.Ref())
		}
		x.addConversation(m.Ref.Conversation)
		for _, p := range m.Content.Parts {
			if p.Attachment != nil && !slices.Contains(x.attachments, *p.Attachment) {
				x.attachments = append(x.attachments, *p.Attachment)
			}
		}
		return nil
	})
}

// interactions writes the readable interactions f selects that keep
// accepts, and collects their conversations. The lines never hold a
// response token: no read returns one.
func (x *exporter) interactions(f model.Filter, keep func(model.Interaction) bool) error {
	return pages(x.ctx, x.c, store.Reader.Interactions, f, func(i model.Interaction) error {
		if keep != nil && !keep(i) {
			return nil
		}
		ok, err := x.readable(engine.InteractionRead(i))
		if err != nil || !ok {
			return err
		}
		if err := x.line(model.ExportLineInteraction, i); err != nil {
			return err
		}
		x.manifest.Interactions = append(x.manifest.Interactions, i.Ref)
		if i.Redacted {
			x.manifest.Tombstones = append(x.manifest.Tombstones, i.Ref.Ref())
		}
		x.addConversation(i.Ref.Conversation)
		return nil
	})
}

func (x *exporter) addConversation(ref model.ConversationRef) {
	if !slices.Contains(x.conversations, ref) {
		x.conversations = append(x.conversations, ref)
	}
}

func (x *exporter) writeConversations() error {
	for _, ref := range x.conversations {
		var cv model.Conversation
		err := view(x.ctx, x.c, func(r store.Reader) (err error) {
			cv, err = r.GetConversation(x.ctx, ref)
			return err
		})
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return err
		}
		ok, err := x.readable(policy.Authorization{Action: model.ActionConversationRead, Resource: ref.Ref(), Revision: cv.Revision})
		if err != nil {
			return err
		}
		if ok {
			// Workflow state is about desk actors, not the subject: never
			// exported.
			if err := x.line(model.ExportLineConversation, cv.WithoutWorkflow()); err != nil {
				return err
			}
			x.manifest.Conversations = append(x.manifest.Conversations, ref)
			for _, l := range cv.Labels {
				if !slices.Contains(x.labels, l) {
					x.labels = append(x.labels, l)
				}
			}
		}
	}
	return nil
}

// writeLabels writes the definition of every label a written conversation
// holds, when the principal may read it (model.ActionLabelRead): a
// conversation's label refs alone are unintelligible to its subject.
func (x *exporter) writeLabels() error {
	for _, ref := range x.labels {
		var l model.Label
		err := view(x.ctx, x.c, func(r store.Reader) (err error) {
			l, err = r.GetLabel(x.ctx, ref)
			return err
		})
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return err
		}
		ok, err := x.readable(policy.Authorization{Action: model.ActionLabelRead, Resource: ref.Ref(), Revision: l.Revision})
		if err != nil {
			return err
		}
		if ok {
			if err := x.line(model.ExportLineLabel, l); err != nil {
				return err
			}
			x.manifest.Labels = append(x.manifest.Labels, ref)
		}
	}
	return nil
}

func (x *exporter) writeAttachments() error {
	for _, ref := range x.attachments {
		if ref.Scope() != x.c.Principal().Tenant {
			continue
		}
		var rec store.AttachmentRecord
		err := view(x.ctx, x.c, func(r store.Reader) (err error) {
			rec, err = attachment(x.ctx, r, ref)
			return err
		})
		if model.IsCode(err, model.NotFound) {
			continue
		}
		if err != nil {
			return err
		}
		ok, err := allowed(attachments.Readable(x.ctx, x.c, rec))
		if err != nil {
			return err
		}
		if ok {
			if err := x.line(model.ExportLineAttachment, rec.Attachment); err != nil {
				return err
			}
			x.manifest.Attachments = append(x.manifest.Attachments, ref)
			if rec.State == store.AttachmentRedacted {
				x.manifest.Tombstones = append(x.manifest.Tombstones, ref.Ref())
			}
		}
	}
	return nil
}

// pages calls each for every record list selects under f, a page per
// read unit, outside the unit.
func pages[T any](ctx context.Context, c *engine.Client, list func(store.Reader, context.Context, model.ListRequest) (model.Page[T], error),
	f model.Filter, each func(T) error,
) error {
	req := model.ListRequest{Filter: f, Page: model.PageRequest{Limit: page}}
	for {
		var p model.Page[T]
		if err := view(ctx, c, func(r store.Reader) (err error) {
			p, err = list(r, ctx, req)
			return err
		}); err != nil {
			return err
		}
		for _, v := range p.Items {
			if err := each(v); err != nil {
				return err
			}
		}
		if p.Complete {
			return nil
		}
		req.Page.After = p.Next
	}
}
