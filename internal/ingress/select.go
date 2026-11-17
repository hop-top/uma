package ingress

import (
	"slices"
	"strconv"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/policy"
	"hop.top/uma/store"
)

// conversationScope is the candidate group and routing of one selection.
type conversationScope struct {
	group store.SelectionKey
	// external holds the scoping references a new conversation records.
	external []model.ExternalRef
	route    model.ExternalRef
	owner    *model.IdentityRef
}

// scope derives the candidate group from the inbox mode: the sender's
// identity in per-contact mode, otherwise the scoped room, thread or work
// item reference, independent of the sender.
func (u *ingressUnit) scope(sender *model.Identity) (conversationScope, error) {
	mode := u.inbox.Selection.Mode
	sc := conversationScope{group: store.SelectionKey{Inbox: u.inbox.Ref, Mode: mode}}
	if mode == model.ModePerContact {
		if sender == nil {
			return sc, model.NewError(model.Validation, "per-contact selection requires a sender")
		}
		owner := sender.Ref
		sc.group = contactGroup(owner)
		sc.route, sc.owner = u.env.Sender, &owner
		return sc, nil
	}
	if mode == model.ModePerAgentContext {
		return u.agentContextScope(sc, sender)
	}
	kind := scopedKind(mode)
	i := slices.IndexFunc(u.env.References, func(r model.ExternalRef) bool { return r.Kind == kind })
	if kind == "" || i < 0 {
		return sc, model.NewError(model.Validation, "envelope.references: "+mode+" selection requires a "+kind+" reference")
	}
	r := u.env.References[i]
	sc.group.Key = kind + ":" + lengthPrefixed(r.Namespace, r.ParentID, r.ID)
	sc.external, sc.route = []model.ExternalRef{r}, r
	return sc, nil
}

// contactGroup is the per-contact candidate group of one identity: the
// inbox plus its ContactInbox.
func contactGroup(id model.IdentityRef) store.SelectionKey {
	return store.SelectionKey{Inbox: id.Inbox, Mode: model.ModePerContact, Key: "identity:" + string(id.ID)}
}

func lengthPrefixed(parts ...string) string {
	var out []byte
	for _, p := range parts {
		out = strconv.AppendInt(out, int64(len(p)), 10)
		out = append(out, ':')
		out = append(out, p...)
	}
	return string(out)
}

func (u *ingressUnit) eventClass() policy.EventClass {
	if u.start != nil {
		return policy.EventOutboundStart
	}
	if u.env.Kind == model.EnvelopeInteraction {
		return policy.EventInteraction
	}
	if u.env.Kind == model.EnvelopeAutomatedMessage || u.env.Kind == model.EnvelopeWorkItem {
		return policy.EventAutoMessage
	}
	return policy.EventContactMessage
}

// selectConversation selects or creates the conversation of a new message
// and applies the selection: reopen, participant addition or creation. It
// reports whether the conversation was created.
func (u *ingressUnit) selectConversation(sender *model.Identity) (model.Conversation, bool, error) {
	sc, err := u.scope(sender)
	if err != nil {
		return model.Conversation{}, false, err
	}
	cands, err := u.candidates(sc)
	if err != nil {
		return model.Conversation{}, false, err
	}
	u.cands = cands
	in := policy.SelectionInput{
		Inbox: u.inbox.Clone(), Sender: sender, References: slices.Clone(u.env.References),
		Candidates: cands, Event: u.eventClass(),
	}
	if u.start != nil {
		in.Operation = *u.start
	}
	if u.mailThreading() {
		if in.Matches, in.Candidates, err = u.headerMatches(cands); err != nil {
			return model.Conversation{}, false, err
		}
	}
	sel, err := u.selector()
	if err != nil {
		return model.Conversation{}, false, err
	}
	res, err := sel.Select(u.ctx, in)
	if err != nil {
		return model.Conversation{}, false, selectorError(u.ctx, err)
	}
	existing, err := checkSelection(res, in)
	if err != nil {
		return model.Conversation{}, false, err
	}
	if existing != nil {
		c, err := u.update(*existing, sender, res.Reopen, res.AddParticipant)
		return c, false, err
	}
	return u.claim(sc, res.CreateKey, sender)
}

// candidates reads the selection's candidate group: the conversations
// claimed under sc.group that are not closed, plus the newest closed one.
// It never returns nil, so a selector sees the same snapshot, an empty
// non-nil list for an empty group, from every store.
//
// The read is skipped when sc.group is the contact group of the identity
// this unit created and no selection has used yet, because the group is
// empty by construction. A contact group is keyed by one identity's
// reference, and a conversation joins it only through ClaimConversation
// under a sender whose identity is already stored. The unit claimed the
// identity under a reference it had just allocated: the ID generator never
// repeats a reference and the store rejects one it holds, so no committed
// unit can have claimed under the group, and this unit has not yet, since
// the claim follows the first selection. Every other group reads: an
// identity found stored, a room, thread, work item or agent context group,
// which any sender's conversations share, and any later selection of the
// unit. A first contact's read would be empty, yet at SERIALIZABLE it takes
// a predicate lock on the claims index page every concurrent first contact
// writes to, and the units then conflict with one another.
func (u *ingressUnit) candidates(sc conversationScope) ([]model.Conversation, error) {
	fresh := u.fresh
	u.fresh = nil
	if fresh != nil && sc.group == contactGroup(*fresh) {
		return []model.Conversation{}, nil
	}
	cands, err := u.tx.Candidates(u.ctx, sc.group, candidateLimit)
	if err != nil {
		return nil, err
	}
	if cands == nil {
		cands = []model.Conversation{}
	}
	return cands, nil
}

// selector returns the inbox's registered selection policy, or the built-in
// selector of its mode.
func (u *ingressUnit) selector() (policy.ConversationSelector, error) {
	if p := u.inbox.Selection.Policy; p != "" {
		return u.s.e.ConversationSelector(p)
	}
	if u.inbox.Selection.Mode == model.ModePerContact {
		return contactSelector{mail: engine.EmailThreading(u.s.desc)}, nil
	}
	return scopedSelector{}, nil
}

// mailThreading reports whether the email threading policy applies: a
// per-contact inbox bound to a channel whose adapter declares it.
func (u *ingressUnit) mailThreading() bool {
	return u.inbox.Selection.Mode == model.ModePerContact && engine.EmailThreading(u.s.desc)
}

// messagesByMsgID returns the stored messages of the inbox holding msg-id
// id, as their provider message ID or as an email message_id reference,
// each once.
func (u *ingressUnit) messagesByMsgID(id string) ([]model.Message, error) {
	var out []model.Message
	for _, ref := range []model.ExternalRef{
		model.ProviderMessageRef(u.s.channel.Ref, id),
		{Channel: u.s.channel.Ref, Namespace: model.EmailHeaderNamespace, Kind: model.RefKindEmailMessageID, ID: id},
	} {
		msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, ref, matchLimit)
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			if !slices.ContainsFunc(out, func(x model.Message) bool { return x.Ref == m.Ref }) {
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// headerMatches looks up the envelope's threading msg-ids among stored
// messages of the inbox, inbound and outbound, in precedence order: the
// In-Reply-To msg-id, then References from last to first. It stops at the
// first msg-id stored anywhere in the inbox and returns one match per
// conversation storing it, plus the candidate snapshot extended with those
// conversations.
func (u *ingressUnit) headerMatches(cands []model.Conversation) ([]policy.ReferenceMatch, []model.Conversation, error) {
	type ask struct{ header, id string }
	var asks []ask
	var refs []string
	for _, r := range u.env.References {
		if r.Namespace != model.EmailHeaderNamespace {
			continue
		}
		switch r.Kind {
		case model.RefKindEmailInReplyTo:
			if len(asks) == 0 {
				asks = append(asks, ask{policy.HeaderInReplyTo, r.ID})
			}
		case model.RefKindEmailReferences:
			refs = append(refs, r.ID)
		}
	}
	for _, id := range slices.Backward(refs) {
		asks = append(asks, ask{policy.HeaderReferences, id})
	}
	seen := map[string]bool{}
	for _, a := range asks {
		if seen[a.id] {
			continue
		}
		seen[a.id] = true
		msgs, err := u.messagesByMsgID(a.id)
		if err != nil {
			return nil, nil, err
		}
		if len(msgs) == 0 {
			continue
		}
		var matches []policy.ReferenceMatch
		out := slices.Clone(cands)
		for _, m := range msgs {
			conv := m.Ref.Conversation
			if slices.ContainsFunc(matches, func(x policy.ReferenceMatch) bool { return x.Conversation == conv }) {
				continue
			}
			matches = append(matches, policy.ReferenceMatch{Header: a.header, MsgID: a.id, Conversation: conv})
			if _, ok := findConversation(out, conv); ok {
				continue
			}
			c, err := u.tx.GetConversation(u.ctx, conv)
			if err != nil {
				return nil, nil, err
			}
			out = append(out, c)
		}
		return matches, out, nil
	}
	return nil, cands, nil
}

// update applies a reuse decision: reopen of a resolved conversation and
// addition of the sender as participant.
func (u *ingressUnit) update(c model.Conversation, sender *model.Identity, reopen, addParticipant bool) (model.Conversation, error) {
	var changes []string
	var status *statusChange
	if reopen {
		status = &statusChange{From: c.Status, To: model.ConversationOpen}
		c.Status = model.ConversationOpen
		changes = append(changes, changeStatus)
	}
	if addParticipant && sender != nil && !isParticipant(c, sender.Ref) {
		c.Participants = append(slices.Clone(c.Participants), model.Participant{Identity: sender.Ref, Role: roleParticipant})
		changes = append(changes, changeParticipants)
	}
	if len(changes) == 0 {
		return c, nil
	}
	prev := c.Revision
	rev, err := u.newRevision()
	if err != nil {
		return model.Conversation{}, err
	}
	c.Revision = rev
	if err := u.tx.PutConversation(u.ctx, c, prev); err != nil {
		return model.Conversation{}, err
	}
	return c, u.conversationChanged(c, changes, status, prev)
}

// Participant roles.
const (
	roleContact     = "contact"
	roleParticipant = "participant"
)

// claim creates the conversation under the selection's create key. An
// existing claim under the key is reused, so concurrent decisions over
// equal snapshots converge on one conversation; a claimed conversation that
// has since closed gets a successor under a key derived from it.
func (u *ingressUnit) claim(sc conversationScope, key string, sender *model.Identity) (model.Conversation, bool, error) {
	id, err := u.newID()
	if err != nil {
		return model.Conversation{}, false, err
	}
	rev, err := u.newRevision()
	if err != nil {
		return model.Conversation{}, false, err
	}
	conv := model.Conversation{
		Ref:  model.ConversationRef{Inbox: u.inbox.Ref, ID: id},
		Mode: u.inbox.Selection.Mode, Status: model.ConversationOpen, Owner: sc.owner,
		External: sc.external, ReplyRoute: sc.route, CreatedAt: u.now, Revision: rev,
	}
	if sender != nil {
		role := roleParticipant
		if sc.owner != nil {
			role = roleContact
		}
		conv.Participants = []model.Participant{{Identity: sender.Ref, Role: role}}
	}
	for range maxSuccessors {
		got, created, err := u.tx.ClaimConversation(u.ctx, store.ConversationClaim{Selection: sc.group, CreateKey: key, Conversation: conv})
		if err != nil {
			return model.Conversation{}, false, err
		}
		if created {
			return got, true, u.conversationChanged(got, []string{changeCreated}, nil, "")
		}
		if got.Status != model.ConversationClosed {
			c, err := u.update(got, sender, false, true)
			return c, false, err
		}
		key += "|after:" + string(got.Ref.ID)
	}
	return model.Conversation{}, false, model.NewError(model.Conflict, "conversation create key successor chain exhausted")
}
