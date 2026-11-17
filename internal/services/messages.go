package services

import (
	"context"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Messages is the Messages service of one client.
type Messages struct{ c *engine.Client }

// NewMessages returns the Messages service bound to c.
func NewMessages(c *engine.Client) Messages { return Messages{c} }

// Get returns a retained message the principal may read under the
// message's audience. A message outside the conversation, inbox or tenant
// the reference names is not found.
func (s Messages) Get(ctx context.Context, req model.Lookup[model.MessageRef]) (model.Message, error) {
	c := s.c
	done, err := c.Engine().Begin()
	if err != nil {
		return model.Message{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Message{}, err
	}
	if req.Ref.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Message{}, engine.ErrNotFound()
	}
	var m model.Message
	if err := view(ctx, c, func(r store.Reader) (err error) {
		m, err = r.GetMessage(ctx, req.Ref)
		return err
	}); err != nil {
		return model.Message{}, err
	}
	if m.Ref != req.Ref {
		return model.Message{}, engine.ErrNotFound()
	}
	if err := c.Authorize(ctx, engine.MessageRead(m), req.Ref.Ref()); err != nil {
		return model.Message{}, err
	}
	return m.Clone(), nil
}

// List pages the retained messages the principal may read, each under its
// own audience, in recording order, optionally limited to Filter.Inboxes,
// Filter.Conversation and Filter.Task (the task's message history,
// Message.Task) and narrowed by the message search fields Text,
// Since, Before and Attachments (model's search documentation) and by
// Filter.Labels, the labels the message's conversation holds now, which
// also needs conversation.read on that conversation: a message whose
// conversation the principal may not read is skipped, so a label-filtered
// list never reveals a conversation's labels. Messages the principal may
// not read, internal notes included, are skipped without trace, whatever
// they match. With MediaTypes set, a message is
// listed only through a matching attachment the principal may also read
// (mediaReadable). A search page that completes the query carries no
// Next, so where the scan stopped never reveals a hidden match: rerun the
// search for messages recorded since. It reads domain history only, never
// remote provider history.
func (s Messages) List(ctx context.Context, req model.ListRequest) (model.Page[model.Message], error) {
	media, labels := req.Filter.Attachments, req.Filter.Labels
	return list(ctx, s.c, listQuery[model.Message]{
		name:   "messages",
		fields: filterFields{inboxes: true, conversation: true, labels: true, task: true, text: true, since: true, before: true, attachments: true},
		read:   store.Reader.Messages,
		within: func(f model.Filter, m model.Message) bool {
			return inInboxes(f, m.Ref.Conversation.Inbox) && inConversation(f, m.Ref.Conversation) && inTask(f, m.Task) && searchWithin(f, m)
		},
		admit: func(ctx context.Context, c *engine.Client, m model.Message) error {
			if err := c.Authorize(ctx, engine.MessageRead(m), m.Ref.Ref()); err != nil {
				return err
			}
			if err := mediaReadable(ctx, c, media, m); err != nil {
				return err
			}
			return labelsHeld(ctx, c, labels, m)
		},
		clone:  model.Message.Clone,
		sealed: searching(req.Filter),
	}, req)
}

// Reply queues a public reply on the conversation's stored route as an
// outbound operation; operations.Reply holds its rules.
func (s Messages) Reply(ctx context.Context, req model.Reply) (model.Acceptance, error) {
	return operations.Reply(ctx, s.c, req)
}

// Edit queues a provider edit of a message's content as an outbound
// operation; operations.Edit holds its rules.
func (s Messages) Edit(ctx context.Context, req model.EditMessage) (model.Acceptance, error) {
	return operations.Edit(ctx, s.c, req)
}

// Delete queues a provider deletion of a message as an outbound operation;
// operations.Delete holds its rules.
func (s Messages) Delete(ctx context.Context, req model.DeleteMessage) (model.Acceptance, error) {
	return operations.Delete(ctx, s.c, req)
}

// React queues adding or removing the caller's reaction on a message as an
// outbound operation; operations.React holds its rules.
func (s Messages) React(ctx context.Context, req model.Reaction) (model.Acceptance, error) {
	return operations.React(ctx, s.c, req)
}

// Receipt queues a delivery or read receipt for a received message as an
// outbound operation; operations.Receipt holds its rules.
func (s Messages) Receipt(ctx context.Context, req model.Receipt) (model.Acceptance, error) {
	return operations.Receipt(ctx, s.c, req)
}

// AddInternalNote records an internal note in a conversation, as a
// committed operation, and a message.created event with origin note. The
// note's audience is internal under the request's policy reference; it
// never becomes a public reply. It stays in uma unless its inbox opted in
// (model.Inbox.DeliverNotes): then the note is queued as an outbound
// operation and delivered as a private provider note by an adapter
// declaring model.CapabilityInternalNote (see operations.Note). A set
// ExpectedRevision must equal the conversation's revision. Retrying with
// the same operation ID and payload returns the stored operation; a
// different payload conflicts.
func (s Messages) AddInternalNote(ctx context.Context, req model.InternalNote) (model.Acceptance, error) {
	c, e := s.c, s.c.Engine()
	done, err := e.Begin()
	if err != nil {
		return model.Acceptance{}, err
	}
	defer done()
	if err := req.Validate(); err != nil {
		return model.Acceptance{}, err
	}
	if req.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var deliver bool
	var source model.TypeKey
	if err := view(ctx, c, func(r store.Reader) error {
		if err := loadConversation(ctx, r, req.Conversation, nil); err != nil {
			return err
		}
		inbox, err := r.GetInbox(ctx, req.Conversation.Inbox)
		if err != nil || !inbox.DeliverNotes {
			return err
		}
		ch, err := r.GetChannel(ctx, inbox.Channel)
		deliver, source = true, ch.Type
		return err
	}); err != nil {
		return model.Acceptance{}, err
	}
	audience := noteAudience(req.PolicyRef)
	if deliver {
		return operations.Note(ctx, c, operations.DeliveredNote{
			Request: req, Source: source, Audience: audience, Fingerprint: noteFingerprint(req, audience),
		})
	}
	ids, err := allocate(ctx, e, 5)
	if err != nil {
		return model.Acceptance{}, err
	}
	conv := req.Conversation.Ref()
	msgRef := model.MessageRef{Conversation: req.Conversation, ID: ids[0]}
	content := req.Content.Clone()
	if req.Hold != nil {
		return holdLocalNote(ctx, c, req, audience, msgRef)
	}
	return commit(ctx, c, localCommand{
		kind: model.ActionMessageNote, target: conv, resource: conv, owner: conv, audience: audience,
		command: req.Command, fingerprint: noteFingerprint(req, audience),
		result: msgRef.Ref(), resultMessage: &msgRef,
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			return writeLocalNote(ctx, tx, e, c.Principal().Actor, op, localNote{
				conv: req.Conversation, msg: msgRef, content: content, audience: audience,
				expected: req.ExpectedRevision, correlation: req.Correlation, rev: model.Revision(ids[1]), event: ids[2],
			})
		},
	}, ids[3], model.Revision(ids[4]))
}

// noteAudience is the audience of every internal note.
func noteAudience(policyRef string) model.Audience {
	return model.Audience{Visibility: model.Internal, PolicyRef: policyRef}
}

// noteFingerprint returns the version 1 fingerprint of an internal note:
// kind, target conversation, content, audience and expected revision.
func noteFingerprint(req model.InternalNote, audience model.Audience) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionMessageNote))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("content", operations.Field().Content(req.Content))
	f.Set("audience", operations.Field().Audience(audience))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}
