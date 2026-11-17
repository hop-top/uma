package operations

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"hop.top/uma/internal/engine"
	"hop.top/uma/model"
	"hop.top/uma/store"
)

// Message mutations: Edit, Delete and React change an existing message at
// the provider through the operation ledger. Submission claims the
// operation key (kind = the policy action, target = the message ref),
// stores the intent with the target's base snapshot and queues the work;
// it never writes the message. The message changes only once the provider
// applied the effect or an authoritative inbound copy confirmed it; until
// then Operations.Get shows the change as model.Operation.Pending.

// Mutation intent names.
const (
	// IntentEdit names the stored intent of an edit operation.
	IntentEdit = "uma.intent.edit"
	// IntentDelete names the stored intent of a delete operation.
	IntentDelete = "uma.intent.delete"
	// IntentReact names the stored intent of a react operation.
	IntentReact = "uma.intent.react"
)

// MutationBase is the target's edit state as the claiming unit read it:
// the base a mutation was accepted against. Settling compares LastEdit,
// Deleted and Redacted with the then-current message; Revision is
// recorded (and shown as model.PendingMutation.BaseRevision) but not
// compared, since reactions, provider-ID joins and audience narrowing bump
// it without moving content. It is never part of the fingerprint.
type MutationBase struct {
	Revision model.Revision `json:"revision"`
	LastEdit *model.Edit    `json:"last_edit,omitempty"`
	Deleted  bool           `json:"deleted"`
	Redacted bool           `json:"redacted"`
}

// baseOf snapshots m's edit state.
func baseOf(m model.Message) MutationBase {
	b := MutationBase{Revision: m.Revision, Deleted: m.Deleted, Redacted: m.Redacted}
	if m.LastEdit != nil {
		e := m.LastEdit.Clone()
		b.LastEdit = &e
	}
	return b
}

// MutationIntent is what every mutation intent holds: the target message,
// its audience (the audience of the operation's events and effect), the
// base snapshot and WaitUntil. While the target has no provider message
// reference on its route channel, execution waits for it only as long as
// the target's origin operation is queued, retry_pending or
// outcome_unknown and WaitUntil has not passed; then the operation fails
// permanently.
type MutationIntent struct {
	Message   model.MessageRef `json:"message"`
	Audience  model.Audience   `json:"audience"`
	Base      MutationBase     `json:"base"`
	WaitUntil time.Time        `json:"wait_until"`
}

// ProviderMessageRef returns m's provider message reference on channel, or
// nil when m holds none there: the provider resource a mutation of m
// targets, and the message a reply to m threads under.
func ProviderMessageRef(m model.Message, channel model.ChannelRef) *model.ExternalRef {
	for _, x := range m.External {
		if x.Channel == channel && x.Namespace == model.ProviderMessageNamespace && x.Kind == model.RefKindMessage {
			t := x
			return &t
		}
	}
	return nil
}

// mutation is one mutation command ready for the ledger: its policy
// action, the capability its channel must declare, the command, the
// fingerprint, whether ExpectedRevision is compared, and how the intent
// is encoded from the shared MutationIntent part.
type mutation struct {
	kind        string
	capability  model.Capability
	message     model.MessageRef
	command     model.Command
	compareRev  bool
	fingerprint func(audience model.Audience) []byte
	encode      func(MutationIntent) (model.Extension, error)
	// eligible, when set, refuses a target the command does not apply
	// to, checked in the claiming unit of a new operation.
	eligible func(model.Message) error
}

// submitMutation runs the shared submission of Edit, Delete and React:
// scope, the target view and the capability check, then submit with the
// base snapshot taken in build and the target prechecked after a new
// claim. The result is the target.
func submitMutation(ctx context.Context, c *engine.Client, mu mutation) (model.Acceptance, error) {
	if mu.message.Conversation.Inbox.Tenant != c.Principal().Tenant {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	var target model.Message
	err := c.Engine().Store().View(ctx, c.Principal().Tenant, func(r store.Reader) error {
		var err error
		if target, err = r.GetMessage(ctx, mu.message); err != nil {
			return err
		}
		if target.Ref != mu.message {
			return engine.ErrNotFound()
		}
		conv, err := r.GetConversation(ctx, mu.message.Conversation)
		if err != nil {
			return err
		}
		inbox, err := r.GetInbox(ctx, conv.Ref.Inbox)
		if err != nil {
			return err
		}
		ch, err := r.GetChannel(ctx, inbox.Channel)
		if err != nil {
			return err
		}
		return outboundCapability(c.Engine(), ch.Type, mu.capability)
	})
	if model.IsCode(err, model.NotFound) {
		return model.Acceptance{}, engine.ErrNotFound()
	}
	if err != nil {
		return model.Acceptance{}, err
	}
	audience := engine.ReadAudience(target.Audience)
	waitUntil := c.Engine().Clock().Now().UTC().Add(model.DefaultStreamDeadline)
	ref := mu.message
	result := ref.Ref()
	return submit(ctx, c, submission{
		kind:        mu.kind,
		target:      result,
		owner:       result,
		command:     mu.command,
		audience:    audience,
		fingerprint: mu.fingerprint(audience),
		workKind:    WorkKindOperation,
		build: func(ctx context.Context, tx store.Tx) (model.Extension, error) {
			in := MutationIntent{Message: ref, Audience: audience, WaitUntil: waitUntil}
			switch m, err := tx.GetMessage(ctx, ref); {
			case err == nil && m.Ref == ref:
				in.Base = baseOf(m)
			case err != nil && !model.IsCode(err, model.NotFound):
				return model.Extension{}, err
			}
			// A target gone since the view keeps a zero base: a replay
			// returns the stored intent, and a new claim fails precheck.
			return mu.encode(in)
		},
		precheck:      mutationPrecheck(ref, mu.command.ExpectedRevision, mu.compareRev, mu.eligible),
		result:        &result,
		resultMessage: &ref,
	})
}

// mutationPrecheck returns the claiming unit's check of a new mutation:
// the target passes CheckMutationTarget, then eligible when set and, when
// compareRev is set, is at ExpectedRevision (else model.Conflict).
func mutationPrecheck(ref model.MessageRef, expected model.Revision, compareRev bool, eligible func(model.Message) error) func(context.Context, store.Tx) error {
	return func(ctx context.Context, tx store.Tx) error {
		m, err := targetState(ctx, tx, ref)
		if err != nil {
			return err
		}
		if eligible != nil {
			if err := eligible(m); err != nil {
				return err
			}
		}
		if compareRev && m.Revision != expected {
			return model.NewError(model.Conflict, "message revision changed")
		}
		return checkNotLiveStream(ctx, tx, m)
	}
}

// CheckMutationTarget reads the target of a mutation and requires it
// present under its exact reference (else not found), neither deleted
// nor redacted, and not a live stream's provider message (each
// model.Conflict).
func CheckMutationTarget(ctx context.Context, r store.Reader, ref model.MessageRef) (model.Message, error) {
	m, err := targetState(ctx, r, ref)
	if err != nil {
		return model.Message{}, err
	}
	return m, checkNotLiveStream(ctx, r, m)
}

// targetState reads a mutation target, present under its exact
// reference, neither deleted nor redacted.
func targetState(ctx context.Context, r store.Reader, ref model.MessageRef) (model.Message, error) {
	m, err := r.GetMessage(ctx, ref)
	switch {
	case model.IsCode(err, model.NotFound) || (err == nil && m.Ref != ref):
		return model.Message{}, engine.ErrNotFound()
	case err != nil:
		return model.Message{}, err
	case m.Redacted:
		return model.Message{}, model.NewError(model.Conflict, "message redacted")
	case m.Deleted:
		return model.Message{}, model.NewError(model.Conflict, "message deleted")
	}
	return m, nil
}

// checkNotLiveStream fails with model.Conflict when m's provider message
// on its conversation's route channel is a provider reference of a
// stream of the conversation that is not terminal (opening, open or
// ending): a live stream's message changes only through the stream.
func checkNotLiveStream(ctx context.Context, r store.Reader, m model.Message) error {
	conv, err := r.GetConversation(ctx, m.Ref.Conversation)
	if err != nil {
		return err
	}
	target := ProviderMessageRef(m, conv.ReplyRoute.Channel)
	if target == nil {
		return nil
	}
	streams, err := r.StreamsOf(ctx, m.Ref.Conversation)
	if err != nil {
		return err
	}
	for _, st := range streams {
		if slices.Contains(st.ProviderReferences, *target) {
			return model.NewError(model.Conflict, "message is a live stream's")
		}
	}
	return nil
}

// encodeIntent stores v as intent name, version 1.
func encodeIntent(name string, v any) (model.Extension, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return model.Extension{}, model.NewError(model.Validation, "mutation intent cannot be stored")
	}
	return model.Extension{Name: name, Version: 1, Data: data}, nil
}

// decodeIntent reads a stored version 1 intent named name into T.
func decodeIntent[T any](e model.Extension, name string) (T, error) {
	var in T
	if e.Name != name || e.Version != 1 {
		return in, &model.Error{Code: model.SchemaMismatch, SafeMessage: "unknown operation intent", Retry: model.NoRetry}
	}
	if err := json.Unmarshal(e.Data, &in); err != nil {
		return in, &model.Error{Code: model.Permanent, SafeMessage: "stored operation intent unreadable", Retry: model.NoRetry}
	}
	return in, nil
}
