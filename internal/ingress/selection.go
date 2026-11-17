package ingress

import (
	"context"
	"errors"
	"slices"
	"strings"

	"hop.top/uma/model"
	"hop.top/uma/policy"
)

// Built-in conversation selectors. Each is a pure function of its
// SelectionInput: no I/O, no clock, no state. Core validates every result
// with checkSelection before acting on it.

// contactSelector implements rule set model.RuleSetContactLifecycleV1 for
// per-contact inboxes. With mail set it applies the email threading policy
// first: a header match decides by the matched conversation's lifecycle, and
// without one List-Id traffic and flag=false inboxes start a new thread. An
// outbound start (policy.EventOutboundStart) follows the automated-message
// rules, never reopening; in a flag=false mail inbox every start opens its
// own thread, keyed on its operation.
type contactSelector struct{ mail bool }

func (s contactSelector) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	if in.Sender == nil {
		return policy.SelectionResult{}, model.NewError(model.Validation, "per-contact selection requires a sender")
	}
	if in.Inbox.Selection.LockToSingle == nil {
		return policy.SelectionResult{}, model.NewError(model.Validation, "per-contact selection requires lock_to_single")
	}
	if s.mail {
		return s.selectMail(in)
	}
	return canonical(in)
}

// contactEvent reports whether e is the contact's own activity, which may
// reopen a resolved conversation: a message of the contact, or an
// interaction by it.
func contactEvent(e policy.EventClass) bool {
	return e == policy.EventContactMessage || e == policy.EventInteraction
}

// canonical applies the single- and multi-candidate lifecycle rules to the
// sender's own conversations.
func canonical(in policy.SelectionInput) (policy.SelectionResult, error) {
	lock := *in.Inbox.Selection.LockToSingle
	owned := ownedBy(in.Candidates, in.Sender.Ref)
	var eligible, active []model.Conversation
	for _, c := range owned {
		switch c.Status {
		case model.ConversationOpen, model.ConversationPending:
			eligible = append(eligible, c)
			active = append(active, c)
		case model.ConversationResolved:
			if lock {
				eligible = append(eligible, c)
			}
		}
	}
	pool := eligible
	if lock && len(active) > 0 {
		// Reuse beats reopen.
		pool = active
	}
	if len(pool) == 0 {
		return policy.SelectionResult{CreateKey: successorKey(owned)}, nil
	}
	pick, err := newest(pool)
	if err != nil {
		return policy.SelectionResult{}, err
	}
	return reuse(in, pick, pick.Status == model.ConversationResolved && contactEvent(in.Event)), nil
}

func (s contactSelector) selectMail(in policy.SelectionInput) (policy.SelectionResult, error) {
	if len(in.Matches) > 0 {
		winner := in.Matches[0].MsgID
		var refs []model.ConversationRef
		for _, m := range in.Matches {
			if m.MsgID == winner && !slices.Contains(refs, m.Conversation) {
				refs = append(refs, m.Conversation)
			}
		}
		if len(refs) > 1 {
			return policy.SelectionResult{}, model.NewError(model.Conflict, "message ID stored in more than one conversation")
		}
		c, ok := findConversation(in.Candidates, refs[0])
		if !ok {
			return policy.SelectionResult{}, model.NewError(model.Validation, "matched conversation missing from the snapshot")
		}
		switch c.Status {
		case model.ConversationOpen, model.ConversationPending:
			return reuse(in, c, false), nil
		case model.ConversationResolved:
			return reuse(in, c, contactEvent(in.Event)), nil
		}
		// A closed match gets a successor conversation.
		return policy.SelectionResult{CreateKey: threadKey(in.References)}, nil
	}
	if in.Event == policy.EventOutboundStart && !*in.Inbox.Selection.LockToSingle {
		// A new email starts its own thread.
		return policy.SelectionResult{CreateKey: "start:" + string(in.Operation.ID)}, nil
	}
	if hasRef(in.References, model.RefKindEmailListID) || !*in.Inbox.Selection.LockToSingle {
		return policy.SelectionResult{CreateKey: threadKey(in.References)}, nil
	}
	return canonical(in)
}

// scopedSelector serves per-room, per-thread and per-work-item inboxes:
// the candidate group is one scoped room, thread or work item, independent
// of the sender. It reuses the newest conversation that is not closed,
// never reopens, and adds a new sender as participant.
type scopedSelector struct{}

func (scopedSelector) Select(_ context.Context, in policy.SelectionInput) (policy.SelectionResult, error) {
	var live []model.Conversation
	for _, c := range in.Candidates {
		if c.Status != model.ConversationClosed {
			live = append(live, c)
		}
	}
	if len(live) == 0 {
		return policy.SelectionResult{CreateKey: successorKey(in.Candidates)}, nil
	}
	pick, err := newest(live)
	if err != nil {
		return policy.SelectionResult{}, err
	}
	return reuse(in, pick, false), nil
}

func reuse(in policy.SelectionInput, c model.Conversation, reopen bool) policy.SelectionResult {
	ref := c.Ref
	add := in.Sender != nil && !isParticipant(c, in.Sender.Ref)
	return policy.SelectionResult{Existing: &ref, Reopen: reopen, AddParticipant: add}
}

// newest returns the candidate with the greatest CreatedAt, then the
// greatest ID by byte-wise comparison. A candidate with a zero CreatedAt
// cannot be ordered safely: model.Conflict, never an arbitrary pick.
func newest(cs []model.Conversation) (model.Conversation, error) {
	var best model.Conversation
	for i, c := range cs {
		if c.CreatedAt.IsZero() {
			return model.Conversation{}, model.NewError(model.Conflict, "candidate conversations cannot be ordered")
		}
		if i == 0 || later(c, best) {
			best = c
		}
	}
	return best, nil
}

func later(a, b model.Conversation) bool {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c > 0
	}
	return a.Ref.ID > b.Ref.ID
}

// successorKey is the snapshot-derived create key of a candidate group: the
// latest conversation by CreatedAt then ID, or "none". Equal snapshots give
// equal keys, so concurrent first messages claim one conversation.
func successorKey(cs []model.Conversation) string {
	if len(cs) == 0 {
		return "after:none"
	}
	latest := cs[0]
	for _, c := range cs[1:] {
		if later(c, latest) {
			latest = c
		}
	}
	return "after:" + string(latest.Ref.ID)
}

// threadKey is the create key of a new email thread: its root, the first
// References entry, else the In-Reply-To msg-id, else the own Message-ID.
// Sibling replies under one unknown root converge on one claim.
func threadKey(refs []model.ExternalRef) string {
	for _, kind := range []string{model.RefKindEmailReferences, model.RefKindEmailInReplyTo, model.RefKindEmailMessageID} {
		for _, r := range refs {
			if r.Namespace == model.EmailHeaderNamespace && r.Kind == kind {
				return "thread:" + r.ID
			}
		}
	}
	return "thread:none"
}

func hasRef(refs []model.ExternalRef, kind string) bool {
	return slices.ContainsFunc(refs, func(r model.ExternalRef) bool {
		return r.Namespace == model.EmailHeaderNamespace && r.Kind == kind
	})
}

func ownedBy(cs []model.Conversation, owner model.IdentityRef) []model.Conversation {
	var out []model.Conversation
	for _, c := range cs {
		if c.Owner != nil && *c.Owner == owner {
			out = append(out, c)
		}
	}
	return out
}

func findConversation(cs []model.Conversation, ref model.ConversationRef) (model.Conversation, bool) {
	for _, c := range cs {
		if c.Ref == ref {
			return c, true
		}
	}
	return model.Conversation{}, false
}

func isParticipant(c model.Conversation, id model.IdentityRef) bool {
	return slices.ContainsFunc(c.Participants, func(p model.Participant) bool { return p.Identity == id })
}

// maxCreateKeyBytes bounds a selector's create key.
const maxCreateKeyBytes = 1024

// checkSelection validates a selector result against its snapshot and
// returns the chosen existing conversation, if any. It rejects, with
// model.Validation: both or neither of Existing and CreateKey; an Existing
// outside the snapshot; a closed Existing; Reopen of anything not resolved;
// Reopen or AddParticipant with a create key; and an oversized create key.
func checkSelection(res policy.SelectionResult, in policy.SelectionInput) (*model.Conversation, error) {
	switch {
	case res.Existing == nil && res.CreateKey == "":
		return nil, model.NewError(model.Validation, "selector returned neither an existing conversation nor a create key")
	case res.Existing != nil && res.CreateKey != "":
		return nil, model.NewError(model.Validation, "selector returned both an existing conversation and a create key")
	case res.CreateKey != "":
		switch {
		case res.Reopen || res.AddParticipant:
			return nil, model.NewError(model.Validation, "create key cannot reopen or add participants")
		case len(res.CreateKey) > maxCreateKeyBytes || strings.ContainsRune(res.CreateKey, 0):
			return nil, model.NewError(model.Validation, "create key invalid")
		}
		return nil, nil
	}
	c, ok := findConversation(in.Candidates, *res.Existing)
	switch {
	case !ok:
		return nil, model.NewError(model.Validation, "selected conversation is not a candidate")
	case c.Status == model.ConversationClosed:
		return nil, model.NewError(model.Validation, "closed conversation selected for a new message")
	case res.Reopen && c.Status != model.ConversationResolved:
		return nil, model.NewError(model.Validation, "reopen of a conversation that is not resolved")
	}
	return &c, nil
}

// selectorError maps a selector failure: context errors stay standard, a
// typed error with a declared code passes through, anything else becomes
// model.Unavailable without the selector's text.
func selectorError(ctx context.Context, err error) error {
	if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return err
	}
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return me
	}
	return model.NewError(model.Unavailable, "conversation selection unavailable")
}
