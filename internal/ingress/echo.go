package ingress

import (
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/internal/operations"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Echo reconciliation. A provider that shows uma's own outbound message
// back inbound (a ticket comment uma posted, a chat message uma sent) must
// not produce a second message, a message.received event or a reply loop.
// A new inbound message is an echo when, and only when:
//
//   - it carries model.EchoRef naming an attempt of an operation whose
//     message lives in this inbox: the echo is recorded as provider
//     evidence for that attempt, its provider message ID joins the
//     outbound message, and an operation left outcome_unknown is queued so
//     its worker settles it from the evidence instead of guessing; or
//   - its provider message ID is held by a uma-authored message of the
//     inbox (the ID the provider reported for the send), and the adapter
//     declares provider-assigned message IDs.
//
// A sender-chosen or undeclared ID, such as an RFC 5322 Message-ID, is
// excluded from the second rule: anyone can repeat it, so it proves
// nothing. Email header references never make an echo either. Body and
// sender similarity never make an echo. An echo reference that names no
// such operation is ignored and the message is recorded as usual.
//
// An echo without an echo reference that arrives before the send's answer
// is recorded as an inbound message: nothing yet shows it is uma's. When
// the answer later reports the same provider message ID, the settling
// unit links the copy to the outbound message (Message.EchoOf) and emits
// message.updated with change echo; the copy is never deleted (see
// operations.RecordMessageReferences).

// reconcileEcho reports whether the envelope is the echo of a uma-authored
// message and, when it is, records it as such.
func (u *ingressUnit) reconcileEcho() (bool, error) {
	if echo, err := u.markedEcho(); echo || err != nil {
		return echo, err
	}
	if u.env.MessageID == "" || !engine.ProviderAssigned(u.s.desc) {
		return false, nil
	}
	msgs, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID), matchLimit)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(msgs, func(m model.Message) bool { return m.Origin != nil }), nil
}

// mutationEcho handles an update envelope carrying an echo reference to
// an edit, delete or react operation of this inbox that the update
// confirms: the echo is provider evidence for that operation and nothing
// else. Ingress applies no content and emits no event; the operation's
// worker settles from the evidence and applies the change once, under the
// newer-inbound-wins guard. It reports whether the envelope was such an
// echo; any other echo reference, such as one naming a send, leaves the
// update to apply as usual.
func (u *ingressUnit) mutationEcho() (bool, error) {
	if !model.IsMessageUpdateKind(u.env.Kind) {
		return false, nil
	}
	ea, ok, err := u.echoedAttempt()
	if err != nil || !ok || !confirms(ea.op.Key.Kind, u.env.Kind) {
		return false, err
	}
	return true, u.evidence(ea, model.ProviderAccepted, nil, nil)
}

// confirms reports whether an update envelope of kind can be the echo of
// an operation of opKind.
func confirms(opKind, envKind string) bool {
	switch opKind {
	case model.ActionMessageEdit:
		return envKind == model.EnvelopeMessageEdited
	case model.ActionMessageDelete:
		return envKind == model.EnvelopeMessageDeleted
	case model.ActionMessageReact:
		return envKind == model.EnvelopeReactionAdded || envKind == model.EnvelopeReactionRemoved
	}
	return false
}

// isMutation reports whether opKind is a message mutation: its provider
// message is the target's, already held.
func isMutation(opKind string) bool {
	return confirms(opKind, model.EnvelopeMessageEdited) || confirms(opKind, model.EnvelopeMessageDeleted) ||
		confirms(opKind, model.EnvelopeReactionAdded)
}

// markedEcho handles an envelope carrying an echo reference.
func (u *ingressUnit) markedEcho() (bool, error) {
	ea, ok, err := u.echoedAttempt()
	if err != nil || !ok {
		return false, err
	}
	return true, u.evidence(ea, model.ProviderAccepted, nil, nil)
}

// echoedAttempt is the outbound attempt an envelope's echo reference
// names, with its operation and outbound message.
type echoedAttempt struct {
	op      store.OperationRecord
	attempt model.ID
	fence   store.Fence
	msg     model.Message
}

// echoedAttempt resolves the envelope's echo reference to a recorded
// attempt of an operation whose message lives in this inbox. ok is false
// when the envelope names none.
func (u *ingressUnit) echoedAttempt() (echoedAttempt, bool, error) {
	i := slices.IndexFunc(u.env.References, isEchoRef)
	if i < 0 {
		return echoedAttempt{}, false, nil
	}
	ref := u.env.References[i]
	opID, err1 := model.ParseID(ref.ParentID)
	attempt, err2 := model.ParseID(ref.ID)
	if err1 != nil || err2 != nil {
		return echoedAttempt{}, false, nil
	}
	opRef := model.OperationRef{Tenant: u.inbox.Ref.Tenant, ID: opID}
	rec, err := u.tx.Operation(u.ctx, opRef)
	if model.IsCode(err, model.NotFound) {
		return echoedAttempt{}, false, nil
	}
	if err != nil {
		return echoedAttempt{}, false, err
	}
	out := rec.Operation.ResultMessage
	if out == nil || out.Conversation.Inbox != u.inbox.Ref {
		return echoedAttempt{}, false, nil
	}
	fence, ok, err := u.attemptFence(opRef, attempt)
	if err != nil || !ok {
		return echoedAttempt{}, false, err
	}
	msg, err := u.tx.GetMessage(u.ctx, *out)
	if model.IsCode(err, model.NotFound) {
		return echoedAttempt{}, false, nil
	}
	if err != nil {
		return echoedAttempt{}, false, err
	}
	return echoedAttempt{op: rec, attempt: attempt, fence: fence, msg: msg}, true, nil
}

// evidence records provider evidence in state for an echoed attempt,
// joins an applied attempt's provider message ID to its message, and
// queues the operation's worker when the evidence has work for it: an
// operation left outcome_unknown, which the worker settles from the
// evidence instead of guessing, or a settled reply the evidence outranks
// (operations.Advances), which the worker moves on. Evidence the operation
// already outranks queues nothing. readAt marks delivered evidence the
// provider also reported read, and when.
func (u *ingressUnit) evidence(ea echoedAttempt, state model.DeliveryState, failure *model.Error, readAt *time.Time) error {
	var refs []model.ExternalRef
	// A reaction's references name the reaction, which an echo does not
	// carry: the message ID it reports is the target's.
	if u.env.MessageID != "" && ea.op.Key.Kind != model.ActionMessageReact {
		pref := model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID)
		refs = append(refs, pref)
		// A mutation's provider message is the target, which holds it.
		if !isMutation(ea.op.Key.Kind) && operations.AppliedState(state) {
			if err := u.recordProviderID(ea.msg, pref); err != nil {
				return err
			}
		}
	}
	opRef := ea.op.Operation.Ref
	ev := store.AttemptEvidence{
		Operation: opRef, Attempt: ea.attempt, Fence: ea.fence, State: state,
		ProviderReferences: refs, ObservedAt: u.now, Failure: failure, ReadAt: readAt,
	}
	if err := u.tx.AppendAttemptEvidence(u.ctx, ev); err != nil {
		return err
	}
	u.evidenced = &opRef
	if ea.op.Operation.State != model.Unknown && !operations.Advances(ea.op.Key.Kind, ea.op.Operation, ev) {
		return nil
	}
	id, err := u.newID()
	if err != nil {
		return err
	}
	return u.tx.EnqueueWork(u.ctx, store.Work{ID: id, Kind: operations.WorkKindOperation, Resource: opRef.Ref()})
}

// applyStatus applies the delivery report of a status envelope to the
// attempt its echo reference names. A report naming no attempt, or an
// attempt of another inbox's operation, settles nothing: delivery
// evidence never guesses an attempt. The report is evidence: the
// operation's worker weighs it with the rest (operations.Outcome), so
// ingress itself never changes an operation and the order reports arrive
// in does not matter.
func (u *ingressUnit) applyStatus() error {
	d := u.env.Delivery
	if d == nil {
		return nil
	}
	ea, ok, err := u.echoedAttempt()
	if err != nil || !ok {
		return err
	}
	var failure *model.Error
	if !operations.AppliedState(d.State) {
		failure = &model.Error{Code: model.Permanent, SafeMessage: "provider reported the delivery " + string(d.State), Retry: model.NoRetry}
		if f := d.Failure; f != nil {
			failure.Code, failure.SafeMessage = f.Code, f.SafeMessage
		}
	}
	return u.evidence(ea, d.State, failure, d.ReadAt)
}

// stateEchoes records provider evidence for every state operation of conv
// that reported, the channel's state after the report, shows the change of
// (operations.StateIntent.Echoes). Such an operation has an attempt the
// provider may have applied, and is still unsettled: queued, waiting to
// retry, or outcome_unknown. The evidence settles it without a second send;
// an operation left outcome_unknown is queued so its worker settles it from
// the evidence, once. Ingress changes no operation itself and queues no
// operation: the state applies here as the provider's, and the settle finds
// it applied and writes nothing. Evidence already recorded is not recorded,
// nor the worker queued, again.
func (u *ingressUnit) stateEchoes(conv model.Conversation, held, reported model.ProviderConversationState) error {
	after := model.Cursor("")
	for {
		p, err := u.tx.Operations(u.ctx, model.ListRequest{
			Filter: model.Filter{
				Conversation: &conv.Ref,
				Statuses:     []string{string(model.Queued), string(model.RetryPending), string(model.Unknown)},
			},
			Page: model.PageRequest{Limit: 100, After: after},
		})
		if err != nil {
			return err
		}
		for _, rec := range p.Items {
			in, ok := operations.StateIntentOf(rec)
			if !ok || in.Channel != u.s.channel.Ref || !in.Echoes(held, reported) {
				continue
			}
			if err := u.stateEvidence(rec); err != nil {
				return err
			}
		}
		if p.Complete || p.Next == "" || p.Next == after {
			return nil
		}
		after = p.Next
	}
}

// stateEvidence records the report as evidence for the latest attempt of
// the state operation rec, and queues its worker when it is outcome_unknown.
func (u *ingressUnit) stateEvidence(rec store.OperationRecord) error {
	opRef := rec.Operation.Ref
	h, err := operations.LoadHistory(u.ctx, u.tx, opRef)
	if err != nil {
		return err
	}
	if h.Latest == "" || operations.AppliedState(h.Last.State) {
		return nil
	}
	ev := store.AttemptEvidence{
		Operation: opRef, Attempt: h.Latest, Fence: h.Last.Fence, State: model.ProviderAccepted, ObservedAt: u.now,
	}
	if err := u.tx.AppendAttemptEvidence(u.ctx, ev); err != nil {
		return err
	}
	if u.evidenced == nil {
		u.evidenced = &opRef
	}
	if rec.Operation.State != model.Unknown {
		return nil
	}
	id, err := u.newID()
	if err != nil {
		return err
	}
	return u.tx.EnqueueWork(u.ctx, store.Work{ID: id, Kind: operations.WorkKindOperation, Resource: opRef.Ref()})
}

// attemptFence returns the fence attempt ran under, and whether op
// recorded that attempt at all.
func (u *ingressUnit) attemptFence(op model.OperationRef, attempt model.ID) (store.Fence, bool, error) {
	after := model.Cursor("")
	for {
		p, err := u.tx.Attempts(u.ctx, op, model.PageRequest{Limit: 100, After: after})
		if err != nil {
			return store.Fence{}, false, err
		}
		for _, ev := range p.Items {
			if ev.Attempt == attempt {
				return ev.Fence, true, nil
			}
		}
		if p.Complete || p.Next == "" || p.Next == after {
			return store.Fence{}, false, nil
		}
		after = p.Next
	}
}

// recordProviderID adds the echo's provider message ID to the outbound
// message unless it, or another message of the inbox, already holds it.
func (u *ingressUnit) recordProviderID(m model.Message, ref model.ExternalRef) error {
	if slices.Contains(m.External, ref) {
		return nil
	}
	held, err := u.tx.MessagesByReference(u.ctx, u.inbox.Ref, ref, 1)
	if model.IsCode(err, model.Conflict) || (err == nil && len(held) > 0) {
		return nil
	}
	if err != nil {
		return err
	}
	prev := m.Revision
	rev, err := u.newRevision()
	if err != nil {
		return err
	}
	m.External = append(slices.Clone(m.External), ref)
	m.Revision = rev
	return u.tx.PutMessage(u.ctx, m, prev)
}

func isEchoRef(r model.ExternalRef) bool {
	return r.Namespace == model.EchoNamespace && r.Kind == model.RefKindAttempt
}

// withoutEchoRefs drops echo references: they correlate, they are not
// references of the message.
func withoutEchoRefs(refs []model.ExternalRef) []model.ExternalRef {
	return slices.DeleteFunc(slices.Clone(refs), isEchoRef)
}
