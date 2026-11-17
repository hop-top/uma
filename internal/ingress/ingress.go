// Package ingress applies verified provider ingress: the engine-bound sink,
// the ingress unit (identity claim, conversation selection, message and
// event writes), the built-in conversation selectors and their selection
// rules.
package ingress

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"hop.top/uma/adapter"
	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Ingress limits.
const (
	// maxIngressAttempts bounds retries of one ingress unit after
	// model.Transient serialization failures. Every attempt is a complete,
	// side-effect-free unit.
	maxIngressAttempts = 16
	// candidateLimit bounds a selection candidate group; a larger group is
	// model.Conflict, never a truncated snapshot.
	candidateLimit = 256
	// matchLimit bounds the stored messages one provider message ID may name.
	matchLimit = 16
	// maxSuccessors bounds the create-key successor chain past claimed
	// closed conversations.
	maxSuccessors = 8
)

// Sink is the engine-bound adapter.Sink of one configured channel instance
// and one trusted service principal. The adapter verifies transport and
// payloads before Accept; the engine still checks that every reference
// belongs to the bound channel and that the sender namespace is one the
// adapter declares. Nothing in an envelope widens the binding.
type Sink struct {
	e         *engine.Engine
	principal model.Principal
	channel   model.Channel
	desc      adapter.Descriptor
}

// Bind returns the sink for channel ref under service principal p. The
// channel must exist in p's tenant, its type must be registered and p must
// be allowed model.ActionChannelReceive on it. Binding reads the channel
// once; it opens no adapter instance.
func Bind(ctx context.Context, e *engine.Engine, p model.Principal, ref model.ChannelRef) (*Sink, error) {
	done, err := e.Begin()
	if err != nil {
		return nil, err
	}
	defer done()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if ref.Tenant != p.Tenant {
		return nil, engine.ErrNotFound()
	}
	var ch model.Channel
	if err := e.Store().View(ctx, p.Tenant, func(r store.Reader) error {
		var err error
		ch, err = r.GetChannel(ctx, ref)
		return err
	}); err != nil {
		return nil, err
	}
	_, desc, ok := e.Adapter(ch.Type)
	if !ok {
		return nil, model.NewError(model.Unsupported, "adapter type not registered")
	}
	s := &Sink{e: e, principal: p, channel: ch, desc: engine.CloneDescriptor(desc)}
	if err := authorizeBind(ctx, s); err != nil {
		return nil, err
	}
	return s, nil
}

// Principal returns the service principal the sink is bound to.
func (s *Sink) Principal() model.Principal { return s.principal }

// Channel returns the bound channel as read when the sink was bound.
func (s *Sink) Channel() model.Channel { return s.channel }

// Descriptor returns the descriptor of the bound channel's adapter. It is
// the sink's own copy; callers must not modify it.
func (s *Sink) Descriptor() adapter.Descriptor { return s.desc }

// Accept applies one verified ingress in a single store unit: destination to
// exactly one enabled inbox, event-identity claim, sender identity claim,
// conversation selection and mutation, message write and committed-event
// append. The receipt reports Committed once that unit commits, Durable when
// the store is durable, and Duplicate when the event identity was already
// claimed, returning the original receipt's ID. The ingress's receiver
// cursor, if any, commits in the same unit. Each attempt first authorizes
// model.ActionChannelReceive for the sink's principal into the resolved
// inbox, a duplicate included; a denial is model.Unauthorized. A rejected
// ingress leaves no trace, so the adapter keeps it for redelivery.
//
// In a per-agent-context inbox the envelope names its agent context by a
// model.RefKindContext reference, which selects the conversation whatever
// the sender; the sender who creates it owns it. Without a context
// reference the envelope must continue a task (Envelope.Task), which takes
// the task's conversation; otherwise it is model.Validation. An opening
// task binding allocates the task and binds the message in the message's
// own unit; a continuation names an open task of the inbox, else
// model.NotFound or, for a terminal task or another context,
// model.Conflict. A committed receipt of such an inbox carries the
// conversation, context, message and task references, and a duplicate
// returns the first acceptance's.
//
// An interaction envelope (model.EnvelopeInteraction) records one
// interaction instead of a message: see checkInteractionEnvelope and
// applyInteraction.
//
// A conversation.state envelope (model.EnvelopeConversationState) reports
// the provider's state of one conversation instead: see applyConversationState.
func (s *Sink) Accept(ctx context.Context, in model.Ingress) (r model.IngressReceipt, err error) {
	if err := reservedEventID(in); err != nil {
		return model.IngressReceipt{}, err
	}
	return s.accept(ctx, in, acceptMode{})
}

// AcceptImported is Accept for one envelope Runtime.Import read from the
// provider's history: a message it records carries Message.ImportedBy set
// to by, the actor the import runs for, so it never wakes a snooze nor
// counts as unread. Everything else is as Accept: the same checks,
// deduplication, authorization and events.
func (s *Sink) AcceptImported(ctx context.Context, in model.Ingress, by model.ActorID) (model.IngressReceipt, error) {
	if err := reservedEventID(in); err != nil {
		return model.IngressReceipt{}, err
	}
	return s.accept(ctx, in, acceptMode{importedBy: by})
}

// reservedEventID refuses an envelope whose event ID claims the prefix
// refetches use.
func reservedEventID(in model.Ingress) error {
	if strings.HasPrefix(in.Envelope.EventID, refetchEventPrefix) {
		return model.NewError(model.Validation, "envelope.event_id: the "+refetchEventPrefix+" prefix is reserved")
	}
	return nil
}

// acceptMode is how accept applies one ingress: authoritative applies a
// refetched copy whatever its source version (AcceptRefetched), and
// importedBy marks a new message as imported from the provider's history
// by that actor (AcceptImported).
type acceptMode struct {
	authoritative bool
	importedBy    model.ActorID
}

// accept is Accept, applying the ingress as mode says.
func (s *Sink) accept(ctx context.Context, in model.Ingress, mode acceptMode) (r model.IngressReceipt, err error) {
	defer s.observeIngest(ctx, in.Correlation, s.e.Clock().Now(), &r, &err)
	done, err := s.e.Begin()
	if err != nil {
		return model.IngressReceipt{}, err
	}
	defer done()
	in.Envelope = in.Envelope.Clone()
	if err := s.check(in); err != nil {
		return model.IngressReceipt{}, err
	}
	key := dedupKey(in.Envelope)
	for attempt := 1; ; attempt++ {
		inbox, err := s.authorizeIngress(ctx, in.Envelope.Destination)
		if err != nil {
			return model.IngressReceipt{}, err
		}
		r, err := s.unit(ctx, in, key, inbox, mode)
		if !model.IsCode(err, model.Transient) || attempt >= maxIngressAttempts || ctx.Err() != nil {
			return r, err
		}
	}
}

// EvaluateInteraction asks the configured interaction validator for field
// errors or dynamic options of one inbound submission, as the sink's
// service principal, bounded by the interaction's deadline. It is
// read-only: it writes nothing, so its answer never proves the submission
// was accepted; an accepted submission still passes through durable ingress
// before the adapter acknowledges it. A missing validator denies; a passed
// deadline, before or during evaluation, is model.DeadlineExpired.
func (s *Sink) EvaluateInteraction(ctx context.Context, in model.InteractionInput) (model.InteractionDecision, error) {
	done, err := s.e.Begin()
	if err != nil {
		return model.InteractionDecision{}, err
	}
	defer done()
	if err := s.checkInteraction(in); err != nil {
		return model.InteractionDecision{}, err
	}
	v, err := s.e.InteractionValidator()
	if err != nil {
		return model.InteractionDecision{}, err
	}
	remaining := in.Deadline.Sub(s.e.Clock().Now())
	if remaining <= 0 {
		return model.InteractionDecision{}, interactionExpired()
	}
	evalCtx, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	d, err := v.Evaluate(evalCtx, s.principal, in.Clone())
	switch {
	case err == nil:
		return d.Clone(), nil
	case ctx.Err() != nil:
		return model.InteractionDecision{}, ctx.Err()
	case errors.Is(evalCtx.Err(), context.DeadlineExceeded):
		return model.InteractionDecision{}, interactionExpired()
	}
	var me *model.Error
	if errors.As(err, &me) && me.Code.Valid() {
		return model.InteractionDecision{}, me
	}
	return model.InteractionDecision{}, model.NewError(model.Unavailable, "interaction validator unavailable")
}

// checkInteraction validates an interaction's shape
// (model.InteractionInput.Validate) and requires its actor, bot, source
// message and context on the bound channel.
func (s *Sink) checkInteraction(in model.InteractionInput) error {
	if err := in.Validate(); err != nil {
		return err
	}
	if err := s.ownRef("interaction.actor", in.Actor); err != nil {
		return err
	}
	for field, r := range map[string]*model.ExternalRef{"interaction.bot": in.Bot, "interaction.message": in.Message} {
		if r == nil {
			continue
		}
		if err := s.ownRef(field, *r); err != nil {
			return err
		}
	}
	for _, r := range in.Context {
		if err := s.ownRef("interaction.context", r); err != nil {
			return err
		}
	}
	return nil
}

func interactionExpired() error {
	return &model.Error{Code: model.DeadlineExpired, SafeMessage: "interaction deadline passed", Retry: model.NoRetry}
}

// check validates an ingress against the binding before any storage access.
func (s *Sink) check(in model.Ingress) error {
	env := in.Envelope
	if err := checkCorrelation(in.Correlation); err != nil {
		return err
	}
	if err := checkCursor(in.ReceiverCursor); err != nil {
		return err
	}
	switch {
	case !model.IsEnvelopeKind(env.Kind):
		return model.NewError(model.Validation, "envelope.kind: must be a declared envelope kind")
	case env.Kind == model.EnvelopeConversationState:
		return s.checkConversationState(env)
	case env.Destination == "":
		return model.NewError(model.Validation, "envelope.destination: required")
	case env.EventID == "" && env.MessageID == "":
		return model.NewError(model.Validation, "envelope: event_id or message_id required for deduplication")
	case env.Task != nil && !carriesTask(env.Kind):
		return model.NewError(model.Validation, "envelope.task: only a new message carries a task binding")
	case env.InReplyTo != "" && !carriesTask(env.Kind):
		return model.NewError(model.Validation, "envelope.in_reply_to: only a new message carries a reply parent")
	case env.Kind == model.EnvelopeExtensionEvent:
		return s.checkExtensionEvent(env)
	case env.Event != nil:
		return model.NewError(model.Validation, "envelope.event: only an extension.event carries an event")
	case env.Kind == model.EnvelopeInteraction:
		return s.checkInteractionEnvelope(env)
	case env.Interaction != nil:
		return model.NewError(model.Validation, "envelope.interaction: only an interaction carries one")
	case env.State != nil:
		return model.NewError(model.Validation, "envelope.state: only a conversation.state carries one")
	}
	for _, r := range env.References {
		if err := s.ownRef("envelope.references", r); err != nil {
			return err
		}
	}
	if err := s.checkTaskBinding(env); err != nil {
		return err
	}
	if err := s.checkSender(env); err != nil {
		return err
	}
	if env.Payload.Name != "" {
		if err := env.Payload.Validate(); err != nil {
			return err
		}
	}
	if err := s.checkWorkItem(env); err != nil {
		return err
	}
	if err := checkDelivery(env); err != nil {
		return err
	}
	switch env.Kind {
	case model.EnvelopeMessage, model.EnvelopeAutomatedMessage:
		return engine.FirstErr(env.Content.Validate(), env.Audience.Validate())
	case model.EnvelopeMessageEdited:
		if len(env.Content.Parts) == 0 {
			// An audience-only edit: it can only narrow.
			return engine.FirstErr(requireMessageID(env), env.Audience.Validate())
		}
		return engine.FirstErr(requireMessageID(env), env.Content.Validate())
	case model.EnvelopeMessageDeleted:
		return requireMessageID(env)
	case model.EnvelopeReactionAdded, model.EnvelopeReactionRemoved:
		if err := checkReaction(env); err != nil {
			return err
		}
		return requireMessageID(env)
	}
	return nil
}

func (s *Sink) checkSender(env model.Envelope) error {
	if err := s.checkSenderKind(env); err != nil {
		return err
	}
	if err := checkSenderAttributes(env); err != nil {
		return err
	}
	if env.SystemActor {
		if !env.Sender.IsZero() {
			return model.NewError(model.Validation, "envelope.sender: a system actor carries no sender")
		}
		return nil
	}
	if (env.Kind == model.EnvelopeStatus || env.Kind == model.EnvelopeWorkItem) && env.Sender.IsZero() {
		return nil
	}
	if err := s.ownRef("envelope.sender", env.Sender); err != nil {
		return err
	}
	if !slices.Contains(s.desc.IdentityNamespaces, env.Sender.Namespace) {
		return model.NewError(model.Validation, "envelope.sender: namespace not declared by the adapter")
	}
	return nil
}

// ownRef validates r and requires it to belong to the bound channel.
func (s *Sink) ownRef(field string, r model.ExternalRef) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Channel != s.channel.Ref {
		return model.NewError(model.Validation, field+": must belong to the bound channel")
	}
	return nil
}

// checkDelivery accepts a delivery report on a status envelope only, with
// a reportable state, read only on a delivered one, and a valid failure
// code.
func checkDelivery(env model.Envelope) error {
	d := env.Delivery
	if d == nil {
		return nil
	}
	if env.Kind != model.EnvelopeStatus {
		return model.NewError(model.Validation, "envelope.delivery: only a status carries a delivery report")
	}
	switch d.State {
	case model.ProviderAccepted, model.Delivered, model.Failed, model.Rejected:
	default:
		return model.NewError(model.Validation, "envelope.delivery.state: must be provider_accepted, delivered, failed or rejected")
	}
	if d.ReadAt != nil && (d.State != model.Delivered || d.ReadAt.IsZero()) {
		return model.NewError(model.Validation, "envelope.delivery.read_at: only a delivered report carries a read time, and it is not zero")
	}
	if f := d.Failure; f != nil && (!f.Code.Valid() || f.SafeMessage == "") {
		return model.NewError(model.Validation, "envelope.delivery.failure: needs a valid code and a safe message")
	}
	return nil
}

func checkCorrelation(c model.CorrelationID) error {
	if c == "" {
		return nil
	}
	return c.Validate()
}

func requireMessageID(env model.Envelope) error {
	if env.MessageID == "" {
		return model.NewError(model.Validation, "envelope.message_id: required to target a message")
	}
	return nil
}

// dedupKey is the event identity: kind, provider event ID, provider message
// ID and source revision, length-prefixed. Distinct revisions of one message
// never share a key; a redelivered event always does.
func dedupKey(env model.Envelope) string {
	var b strings.Builder
	for _, p := range []string{env.Kind, env.EventID, env.MessageID, env.SourceRevision} {
		b.WriteString(strconv.Itoa(len(p)))
		b.WriteByte(':')
		b.WriteString(p)
	}
	return b.String()
}

// unit runs one attempt of the ingress transaction.
func (s *Sink) unit(ctx context.Context, in model.Ingress, key string, authorized model.Inbox, mode acceptMode) (model.IngressReceipt, error) {
	var receipt model.IngressReceipt
	start := s.e.Clock().Now()
	now := start.UTC()
	err := s.e.Store().Transact(ctx, s.channel.Ref.Tenant, func(tx store.Tx) error {
		u := &ingressUnit{s: s, ctx: ctx, tx: tx, in: in, env: in.Envelope, now: now, authorized: authorized, acceptMode: mode}
		var err error
		receipt, err = u.run(key)
		return err
	})
	s.observeCommit(ctx, in.Correlation, start, err)
	if err != nil {
		return model.IngressReceipt{}, err
	}
	return receipt, nil
}

// ingressUnit is the state of one ingress transaction attempt.
type ingressUnit struct {
	s      *Sink
	ctx    context.Context
	tx     store.Tx
	in     model.Ingress
	env    model.Envelope
	now    time.Time
	inbox  model.Inbox
	id     model.ID
	events []model.Event
	// cands is the candidate snapshot of the latest selection.
	cands []model.Conversation
	// fresh is the identity this unit created and no selection has used
	// yet; see candidates.
	fresh *model.IdentityRef
	// acceptMode: authoritative applies a refetched copy whatever its
	// source version; importedBy marks a new message as imported.
	acceptMode
	// authorized is the inbox this attempt authorized ingress into.
	authorized model.Inbox
	// start is the operation of an outbound-first selection
	// (OpenConversation); nil for ingress.
	start *model.OperationRef
	// claimAs, set only by a start with a legacy recipient, is the
	// reference the sender's identity is claimed under instead of
	// env.Sender's; see Start.Legacy.
	claimAs *model.ExternalRef
	// correlation is what a per-agent-context message produced; see
	// correlate.
	correlation *receiptRefs
	// evidenced is the operation this unit recorded attempt evidence for,
	// if any; the receipt names it.
	evidenced *model.OperationRef
}

func (u *ingressUnit) run(key string) (model.IngressReceipt, error) {
	if err := u.resolveInbox(); err != nil {
		return model.IngressReceipt{}, err
	}
	if err := u.checkAuthorized(); err != nil {
		return model.IngressReceipt{}, err
	}
	id, err := u.newID()
	if err != nil {
		return model.IngressReceipt{}, err
	}
	rec, created, err := u.tx.ClaimIngress(u.ctx, store.IngressRecord{
		ID: id, Channel: u.s.channel.Ref, DedupKey: key, Input: u.rawInput(),
		VerifierVersion: string(u.s.desc.Type) + "/" + strconv.FormatUint(uint64(u.s.desc.ContractMajor), 10),
	})
	if err != nil {
		return model.IngressReceipt{}, err
	}
	if err := u.saveCursor(); err != nil {
		return model.IngressReceipt{}, err
	}
	durable := u.s.e.Store().Properties().Durable
	if !created {
		r := rec.Receipt
		if r.ID == "" {
			// Claimed but not yet processed: retained for later processing.
			r = model.IngressReceipt{ID: rec.ID, Durable: durable}
		}
		r.Duplicate = true
		return r, nil
	}
	u.id = id
	switch k := u.env.Kind; {
	case k == model.EnvelopeStatus:
		// A status claims its identity and applies a delivery report to
		// the attempt it names, if any.
		err = u.applyStatus()
	case model.IsMessageUpdateKind(k):
		err = u.applyUpdate()
	case k == model.EnvelopeWorkItem:
		err = u.applyWorkItem()
	case k == model.EnvelopeExtensionEvent:
		err = u.applyExtensionEvent()
	case k == model.EnvelopeInteraction:
		err = u.applyInteraction()
	case k == model.EnvelopeConversationState:
		err = u.applyConversationState()
	default:
		err = u.applyMessage()
	}
	if err != nil {
		return model.IngressReceipt{}, err
	}
	if len(u.events) > 0 {
		if err := u.tx.AppendEvents(u.ctx, u.events); err != nil {
			return model.IngressReceipt{}, err
		}
	}
	r := model.IngressReceipt{ID: id, Committed: true, Durable: durable}
	u.correlation.fill(&r)
	r.Operation = u.evidenced
	if err := u.tx.CompleteIngress(u.ctx, id, r); err != nil {
		return model.IngressReceipt{}, err
	}
	return r, nil
}

// resolveInbox resolves the destination to exactly one enabled inbox of the
// bound channel with a valid, supported selection configuration.
func (u *ingressUnit) resolveInbox() error {
	in, err := u.tx.ResolveDestination(u.ctx, u.s.channel.Ref, u.env.Destination)
	switch {
	case model.IsCode(err, model.NotFound):
		return model.NewError(model.NotFound, "destination is not bound to an inbox")
	case model.IsCode(err, model.Conflict):
		return model.NewError(model.Conflict, "destination resolves to more than one inbox")
	case err != nil:
		return err
	case in.Channel != u.s.channel.Ref:
		return model.NewError(model.NotFound, "destination is not bound to an inbox")
	case in.Disabled:
		return model.NewError(model.Disabled, "inbox disabled")
	}
	if err := in.Selection.Validate(); err != nil {
		return err
	}
	u.inbox = in
	return nil
}

func (u *ingressUnit) newID() (model.ID, error) { return newID(u.ctx, u.s.e) }

func (u *ingressUnit) newRevision() (model.Revision, error) {
	id, err := u.newID()
	return model.Revision(id), err
}

// applyMessage records a new inbound message, unless it is the echo of a
// uma-authored one or a message the inbox already holds, and binds its
// work item in a per-work-item inbox.
func (u *ingressUnit) applyMessage() error {
	if err := u.requireWorkItemMode(); err != nil {
		return err
	}
	if err := u.requireAgentContextMode(); err != nil {
		return err
	}
	if echo, err := u.reconcileEcho(); echo || err != nil {
		return err
	}
	if known, err := u.knownMessage(); known || err != nil {
		return err
	}
	if err := u.checkAttachments(u.env.Content, u.env.Audience); err != nil {
		return err
	}
	sender, err := u.resolveSender()
	if err != nil {
		return err
	}
	cont, err := u.continuedTask()
	if err != nil {
		return err
	}
	conv, created, err := u.targetConversation(sender, cont)
	if err != nil {
		return err
	}
	msgID, err := u.newID()
	if err != nil {
		return err
	}
	rev, err := u.newRevision()
	if err != nil {
		return err
	}
	var external []model.ExternalRef
	if u.env.MessageID != "" {
		external = append(external, model.ProviderMessageRef(u.s.channel.Ref, u.env.MessageID))
	}
	external = append(external, withoutEchoRefs(u.env.References)...)
	msg := model.Message{
		Ref:        model.MessageRef{Conversation: conv.Ref, ID: msgID},
		Author:     u.author(sender),
		Content:    u.env.Content,
		Audience:   u.env.Audience,
		External:   external,
		OccurredAt: u.env.OccurredAt,
		RecordedAt: u.now,
		Revision:   rev,
		ImportedBy: u.importedBy,
		Source:     u.s.channel.Type,
		Metadata:   u.metadata(nil),
	}
	if msg.InReplyTo, err = u.replyParent(conv.Ref); err != nil {
		return err
	}
	task, err := u.bindTask(conv, &msg, cont)
	if err != nil {
		return err
	}
	if err := u.putReceived(msg); err != nil {
		return err
	}
	if err := u.messageReceived(msg, sender, created); err != nil {
		return err
	}
	if err := u.taskOpened(task, msg.Ref); err != nil {
		return err
	}
	u.correlate(conv, msg, task)
	return u.bindWorkItem(conv, created)
}

// replyParent resolves the envelope's reply parent to the message of conv
// it names, by provider message ID or, for mail, Message-ID. It is nil when
// the envelope names none, the inbox holds no such message in conv, or the
// ID is ambiguous there: a reply link is never guessed.
func (u *ingressUnit) replyParent(conv model.ConversationRef) (*model.MessageRef, error) {
	if u.env.InReplyTo == "" {
		return nil, nil
	}
	msgs, err := u.messagesByMsgID(u.env.InReplyTo)
	if err != nil {
		return nil, err
	}
	var parent *model.MessageRef
	for _, m := range msgs {
		if !m.Ref.In(conv) {
			continue
		}
		if parent != nil {
			return nil, nil
		}
		ref := m.Ref
		parent = &ref
	}
	return parent, nil
}

// metadata returns prior metadata with the provider payload and the source
// version of the applied envelope replaced.
func (u *ingressUnit) metadata(prior []model.Extension) []model.Extension {
	out := slices.DeleteFunc(slices.Clone(prior), func(x model.Extension) bool {
		return x.Name == sourceMetadataName || (u.env.Payload.Name != "" && x.Name == u.env.Payload.Name)
	})
	if u.env.Payload.Name != "" {
		out = append(out, u.env.Payload.Clone())
	}
	return append(out, sourceMetadata(u.env))
}
