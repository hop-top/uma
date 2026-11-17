package services

import (
	"context"
	"slices"
	"strconv"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// Link links the conversation to a far end as one committed operation; see
// linkConversation.
func (s Conversations) Link(ctx context.Context, req model.ConversationLinkChange) (model.Acceptance, error) {
	return linkConversation(ctx, s.c, req, true)
}

// Unlink removes a link as one committed operation; see linkConversation.
func (s Conversations) Unlink(ctx context.Context, req model.ConversationLinkChange) (model.Acceptance, error) {
	return linkConversation(ctx, s.c, req, false)
}

// linkConversation adds (add) or removes the link between req.Conversation
// and req.Link, authorized on both ends: model.ActionConversationLink on the
// conversation at its revision, and on a far conversation at its revision
// too, or model.ActionWorkItemRead on a far work item. A link between two
// conversations is held by both, written in one unit; a link to a work
// item by the conversation alone. In the claiming unit a set
// ExpectedRevision must be the conversation's, the far end must be stored,
// and a link already held (add) or absent (remove) changes nothing: the
// operation commits with no event and no revision moves. Otherwise each
// conversation end is written once at a new revision with one
// conversation.linked or conversation.unlinked event naming its far end.
// Growing either end past model.MaxConversationLinks is model.Conflict.
func linkConversation(ctx context.Context, c *engine.Client, req model.ConversationLinkChange, add bool) (model.Acceptance, error) {
	e := c.Engine()
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
	var conv, far model.Conversation
	var item model.WorkItem
	if err := view(ctx, c, func(r store.Reader) error {
		if err := loadConversation(ctx, r, req.Conversation, &conv); err != nil {
			return err
		}
		if req.Link.Conversation != nil {
			return loadConversation(ctx, r, *req.Link.Conversation, &far)
		}
		return loadWorkItem(ctx, r, *req.Link.WorkItem, &item)
	}); err != nil {
		return model.Acceptance{}, err
	}
	// Two conversation revisions, two operation IDs, two event IDs.
	ids, err := allocate(ctx, e, 6)
	if err != nil {
		return model.Acceptance{}, err
	}
	res := req.Conversation.Ref()
	kind, schema := model.EventConversationLinked, model.SchemaConversationLinked
	if !add {
		kind, schema = model.EventConversationUnlinked, model.SchemaConversationUnlinked
	}
	back := model.ConversationLink{Conversation: &conv.Ref}
	return commit(ctx, c, localCommand{
		kind: model.ActionConversationLink, target: res, resource: res, owner: res,
		command: req.Command, fingerprint: conversationLinkFingerprint(req, add), result: res,
		authorize: func(ctx context.Context) error {
			if err := c.Authorize(ctx, policy.Authorization{Action: model.ActionConversationLink, Resource: res, Revision: conv.Revision}, res); err != nil {
				return err
			}
			if req.Link.Conversation != nil {
				fr := far.Ref.Ref()
				return c.Authorize(ctx, policy.Authorization{Action: model.ActionConversationLink, Resource: fr, Revision: far.Revision}, fr)
			}
			ir := item.Ref.Ref()
			return c.Authorize(ctx, policy.Authorization{Action: model.ActionWorkItemRead, Resource: ir, Revision: item.Revision}, ir)
		},
		apply: func(ctx context.Context, tx store.Tx, op model.OperationRef) error {
			var cur model.Conversation
			if err := loadConversation(ctx, tx, req.Conversation, &cur); err != nil {
				return err
			}
			if req.ExpectedRevision != "" && cur.Revision != req.ExpectedRevision {
				return model.NewError(model.Conflict, "conversation revision changed")
			}
			ends := []struct {
				conv model.Conversation
				link model.ConversationLink
			}{{cur, req.Link}}
			if req.Link.Conversation != nil {
				var other model.Conversation
				if err := loadConversation(ctx, tx, *req.Link.Conversation, &other); err != nil {
					return err
				}
				ends = append(ends, struct {
					conv model.Conversation
					link model.ConversationLink
				}{other, back})
			} else if err := loadWorkItem(ctx, tx, *req.Link.WorkItem, nil); err != nil {
				return err
			}
			if slices.ContainsFunc(cur.Links, sameLink(req.Link)) == add {
				return nil
			}
			var evs []model.Event
			for i, end := range ends {
				links, changed := applyLink(end.conv.Links, end.link, add)
				if !changed {
					continue
				}
				if len(links) > model.MaxConversationLinks {
					return model.NewError(model.Conflict,
						"links: a conversation holds at most "+strconv.Itoa(model.MaxConversationLinks)+" links")
				}
				next := end.conv.Clone()
				next.Links = links
				next.Revision = model.Revision(ids[i])
				if err := tx.PutConversation(ctx, next, end.conv.Revision); err != nil {
					return err
				}
				ev, err := commandEvent(c, ids[4+i], kind, schema, next.Ref.Ref(), next.Revision,
					model.Audience{Visibility: model.Internal}, req.Correlation,
					model.ConversationLinkPayload{
						Conversation: next.Ref, Link: end.link.Clone(), Trigger: model.TriggerCommand,
						PreviousRevision: end.conv.Revision, Operation: &op,
					})
				if err != nil {
					return err
				}
				evs = append(evs, ev)
			}
			return tx.AppendEvents(ctx, evs)
		},
	}, ids[2], model.Revision(ids[3]))
}

// sameLink matches a link equal to l, end by end.
func sameLink(l model.ConversationLink) func(model.ConversationLink) bool {
	return func(x model.ConversationLink) bool { return model.CompareConversationLinks(x, l) == 0 }
}

// applyLink returns links with l added (add) or removed, sorted, and
// whether it changed.
func applyLink(links []model.ConversationLink, l model.ConversationLink, add bool) ([]model.ConversationLink, bool) {
	i := slices.IndexFunc(links, sameLink(l))
	switch {
	case add && i < 0:
		next := append(slices.Clone(links), l.Clone())
		slices.SortFunc(next, model.CompareConversationLinks)
		return next, true
	case !add && i >= 0:
		return slices.Delete(slices.Clone(links), i, i+1), true
	}
	return links, false
}

// conversationLinkFingerprint returns the version 1 fingerprint of a Link or Unlink
// call: kind, target, direction, far end and expected revision.
func conversationLinkFingerprint(req model.ConversationLinkChange, add bool) []byte {
	f := operations.NewFingerprint()
	f.Set("kind", operations.Field().Str(model.ActionConversationLink))
	f.Set("target", operations.Field().Ref(req.Conversation.Ref()))
	f.Set("add", operations.Field().Str(strconv.FormatBool(add)))
	f.Set("link", operations.Field().Ref(req.Link.Ref()))
	f.Set("expected_revision", operations.Field().Str(string(req.ExpectedRevision)))
	return f.Sum()
}

// readableLinks returns the links whose far end the principal may read
// (model.ActionConversationRead on a conversation, model.ActionWorkItemRead
// on a work item); the others are left out without trace.
func readableLinks(ctx context.Context, c *engine.Client, links []model.ConversationLink) ([]model.ConversationLink, error) {
	var out []model.ConversationLink
	for _, l := range links {
		ok, err := farEndReadable(ctx, c, l)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, l)
		}
	}
	return out, nil
}

// farEndReadable reports whether the principal may read link's far end.
func farEndReadable(ctx context.Context, c *engine.Client, l model.ConversationLink) (bool, error) {
	action := model.ActionConversationRead
	if l.WorkItem != nil {
		action = model.ActionWorkItemRead
	}
	ref := l.Ref()
	err := c.Authorize(ctx, policy.Authorization{Action: action, Resource: ref}, ref)
	switch {
	case err == nil:
		return true, nil
	case model.IsCode(err, model.Unauthorized), model.IsCode(err, model.NotFound):
		return false, nil
	}
	return false, err
}
