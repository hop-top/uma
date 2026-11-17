package model

import (
	"bytes"
	"maps"
	"slices"
	"time"
)

// Held and Expired are the operation states of a proposal that waits for a
// human decision. They are declared with the other states in model.go.

// PluginActorPrefix starts the key of an actor that is a plugin. Only such
// an actor's operation can be held: a person's own write never is.
const PluginActorPrefix = "plugin:"

// IsPlugin reports whether a is a plugin actor (PluginActorPrefix).
func (a ActorID) IsPlugin() bool {
	return len(a) > len(PluginActorPrefix) && string(a[:len(PluginActorPrefix)]) == PluginActorPrefix
}

// HeldDecision is how a held operation was decided.
type HeldDecision string

const (
	// HeldApproved: the proposal was approved as it stood.
	HeldApproved HeldDecision = "approved"
	// HeldRejected: the proposal was refused; nothing was applied.
	HeldRejected HeldDecision = "rejected"
	// HeldEdited: the proposal was edited, then approved.
	HeldEdited HeldDecision = "edited"
	// HeldExpired: the deadline passed with no decision.
	HeldExpired HeldDecision = "expired"
)

// IsHeldDecision reports whether d is a decision of a held operation.
func IsHeldDecision(d HeldDecision) bool {
	switch d {
	case HeldApproved, HeldRejected, HeldEdited, HeldExpired:
		return true
	}
	return false
}

// HeldBlock is what an operation proposed for a human decision carries
// beside its ordinary fields (Operation.Held). It is present from the
// proposal on, also after the decision, and absent from every operation
// that was never held.
//
// Preview is the dry-run of what the operation would do, as the proposer
// supplied it. Intent is required: the intent the operation stored when it was held,
// the arguments an approval applies unless edited: a host builds the
// arguments of an edit (EditHeld.Args) from it, whose validators compare
// target, audience, base and wait_until against it. It is the draft's
// content, so it is shown and scrubbed under the same rules as Preview.
// TrustKey is the opaque key the host counts decisions under
// and TrustLabels its optional listing labels; uma neither derives nor
// interprets them. ExpiresAt is the decision deadline.
//
// Decision, DecidedBy and DecidedAt are empty until the operation is
// decided. EditedArgs holds the arguments of the latest edit and Edits each
// edit with who made it and when. ProposedFingerprint is the fingerprint of
// the proposal as made: a replay of it is answered with the held
// operation, whatever it was edited to since.
//
// Result, ResultMessage and ResultStream of a held Operation, when set,
// name the resource approval will produce, which does not exist before it.
// A start held before its conversation exists names none: its approval
// selects or creates the conversation and sets them.
type HeldBlock struct {
	Proposer            ActorID           `json:"proposer"`
	OnBehalfOf          ActorID           `json:"on_behalf_of"`
	Reason              string            `json:"reason"`
	Preview             *Extension        `json:"preview,omitempty"`
	Intent              *Extension        `json:"intent,omitempty"`
	TrustKey            string            `json:"trust_key"`
	TrustLabels         map[string]string `json:"trust_labels,omitempty"`
	ExpiresAt           *time.Time        `json:"expires_at,omitempty"`
	Decision            HeldDecision      `json:"decision"`
	DecidedBy           ActorID           `json:"decided_by"`
	DecidedAt           *time.Time        `json:"decided_at,omitempty"`
	EditedArgs          *Extension        `json:"edited_args,omitempty"`
	Edits               []HeldEdit        `json:"edits,omitempty"`
	ProposedFingerprint []byte            `json:"proposed_fingerprint,omitempty"`
	// Applies and Local record, at hold time, what approving the operation
	// writes: Applies when it writes a local mutation, so its intent needs
	// an applier at decision time and an approval without one is refused
	// rather than committing nothing; Local when it needs no provider work
	// and so commits, rather than queues, in the unit of its approval. Both
	// are false for a provider-work intent whose approval only queues it.
	Applies bool `json:"applies"`
	Local   bool `json:"local"`
	// TargetRevision is the revision of the operation's target when it was
	// held, for a kind of target that has one (a conversation). A decision
	// finds the proposal stale when the target has advanced since, unless
	// the decider forces it. Empty for any other target.
	TargetRevision Revision `json:"target_revision"`
	// DecisionNote is the note the decider recorded with the decision.
	DecisionNote string `json:"decision_note"`
	// CarriedOverAt is when an expiry sweep first found the operation still
	// held a full sweep window after it was held: a front end shows it as
	// carried over. Set at most once, only while the operation is held; it
	// never changes the state, and the duplicate guard still applies.
	CarriedOverAt *time.Time `json:"carried_over_at,omitempty"`
	// Scope narrows the duplicate guard beyond the operation's kind and
	// target: a proposal is a duplicate of a held operation only when their
	// scopes are equal. Empty for a kind whose duplicates are decided by
	// kind and target alone.
	Scope string `json:"scope"`
}

// HoldProposal asks that a write command be stored as held (Held) for a
// human decision instead of being applied or queued. It rides on
// Command.Hold, so every write request can carry it.
//
// The proposer is the principal's actor and must be a plugin actor
// (ActorID.IsPlugin); a hold on any other actor's command is Validation and
// claims nothing. The proposal is not part of the command's fingerprint:
// the same command with and without a Hold claims the same operation key,
// and a replay is answered with the stored operation either way.
//
// Only a command whose approval can be applied can be held: the message
// mutations, replies and notes, conversation label changes and state verbs,
// and starts. A label configure, label delete or conversation transition
// can be held only when the engine opts it in (Config.Held). Any other
// command with a Hold is Unsupported and claims nothing. A Hold on a method
// that claims no operation key (Operations.Replay, the decisions) is
// ignored.
//
// OnBehalfOf is the actor the proposer acts for, Reason says why it was
// held and Preview is the dry-run of what the command would do. TrustKey
// is the host's opaque trust key (at most MaxTrustKeyBytes) and TrustLabels
// its optional listing labels; uma stores them and counts decisions under
// them. ExpiresAt is the decision deadline.
type HoldProposal struct {
	OnBehalfOf  ActorID           `json:"on_behalf_of"`
	Reason      string            `json:"reason"`
	Preview     *Extension        `json:"preview,omitempty"`
	TrustKey    string            `json:"trust_key"`
	TrustLabels map[string]string `json:"trust_labels,omitempty"`
	ExpiresAt   *time.Time        `json:"expires_at,omitempty"`
}

// MaxTrustKeyBytes bounds a trust key.
const MaxTrustKeyBytes = 256

// Validate checks the trust key bound and the preview.
func (h HoldProposal) Validate() error {
	if len(h.TrustKey) > MaxTrustKeyBytes {
		return invalidField("hold.trust_key", "too long")
	}
	if h.Preview != nil {
		return h.Preview.validate("hold.preview", DefaultContentLimits().MaxStructuredBytes)
	}
	return nil
}

// TrustEntry is the counters kept under one trust key. Clean is the count
// of consecutive approvals as proposed since the last correction or reset;
// Approvals and Corrections are totals. uma counts; whether a count
// justifies skipping review is the host's policy.
type TrustEntry struct {
	Key         string            `json:"key"`
	Labels      map[string]string `json:"labels,omitempty"`
	Clean       int64             `json:"clean"`
	Approvals   int64             `json:"approvals"`
	Corrections int64             `json:"corrections"`
	// LastCorrectionAt is when the latest correction was counted; nil when
	// none was.
	LastCorrectionAt *time.Time `json:"last_correction_at,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

// Clone returns a deep copy.
func (e TrustEntry) Clone() TrustEntry {
	e.Labels = maps.Clone(e.Labels)
	if e.LastCorrectionAt != nil {
		t := *e.LastCorrectionAt
		e.LastCorrectionAt = &t
	}
	return e
}

// TrustLookup reads the entry of one trust key.
type TrustLookup struct {
	Call
	// Channel is the channel the counters are scoped to: the channel of
	// the decided operations' target inbox.
	Channel ChannelRef `json:"channel"`
	Key     string     `json:"key"`
}

// TrustFilter lists entries. Labels, when set, keeps the entries whose
// labels include every pair.
type TrustFilter struct {
	Call
	// Channel is the channel the counters are scoped to: the channel of
	// the decided operations' target inbox.
	Channel ChannelRef        `json:"channel"`
	Labels  map[string]string `json:"labels,omitempty"`
	Page    PageRequest       `json:"page"`
}

// Matches reports whether labels include every pair of f.Labels.
func (f TrustFilter) Matches(labels map[string]string) bool {
	for k, v := range f.Labels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// ResetTrust sets the clean count of one trust key to zero; totals are kept.
type ResetTrust struct {
	Call
	// Channel is the channel the counters are scoped to: the channel of
	// the decided operations' target inbox.
	Channel ChannelRef `json:"channel"`
	Key     string     `json:"key"`
}

// ResetTrustSet resets the clean count of several trust keys, all or none.
type ResetTrustSet struct {
	Call
	// Channel is the channel the counters are scoped to: the channel of
	// the decided operations' target inbox.
	Channel ChannelRef `json:"channel"`
	Keys    []string   `json:"keys,omitempty"`
}

// ImportTrust seeds the counters of one trust key from a host's earlier
// approval history. It is idempotent: each total becomes the larger of the
// stored and the imported value, LastCorrectionAt the later one, so an
// import never lowers what live decisions accumulated and a repeat changes
// nothing. Clean, the consecutive count, is raised only while no stored
// correction is later than the imported history's last (LastCorrectionAt,
// nil for none): a later correction ended the streak the import describes.
//
// Approvals defaults to Clean when zero; Corrections defaults to 1 when zero
// and LastCorrectionAt is set. Labels, when not nil, replace the key's.
type ImportTrust struct {
	Call
	// Channel is the channel the counters are scoped to: the channel of
	// the decided operations' target inbox.
	Channel          ChannelRef        `json:"channel"`
	Key              string            `json:"key"`
	Labels           map[string]string `json:"labels,omitempty"`
	Clean            int64             `json:"clean"`
	Approvals        int64             `json:"approvals"`
	Corrections      int64             `json:"corrections"`
	LastCorrectionAt *time.Time        `json:"last_correction_at,omitempty"`
}

// Validate checks the call, the key, the labels and the counts: none
// negative, and Clean at most Approvals.
func (i ImportTrust) Validate() error {
	if err := firstError(i.Call.Validate(), i.Channel.Validate(), ValidateTrustKey(i.Key), ValidateTrustLabels(i.Labels)); err != nil {
		return err
	}
	if i.Clean < 0 || i.Approvals < 0 || i.Corrections < 0 {
		return invalidField("counts", "must not be negative")
	}
	if i.Approvals != 0 && i.Clean > i.Approvals {
		return invalidField("clean", "exceeds approvals")
	}
	return nil
}

// DecideHeld is what Approve, Reject and Edit of a held operation take.
// ExpectedRevision is required: the revision of the operation the decider
// saw, compared before anything is written, so a decision never lands on a
// proposal that changed since. Force decides although the target advanced
// since the hold (HeldBlock.TargetRevision); it never overrides the
// revision compare or an authorization. Note is recorded with the decision.
//
// Batch is optional: approvals of operations with one trust key that share a
// Batch count as one clean approval of that key, as RecordCleanApproval's
// batch does; a host approving N held operations in one action names one
// batch. Empty means the operation is its own batch. Edit and Reject count
// corrections, which no batch merges.
type DecideHeld struct {
	Call
	Operation        OperationRef `json:"operation"`
	ExpectedRevision Revision     `json:"expected_revision"`
	Force            bool         `json:"force"`
	Note             string       `json:"note"`
	Batch            string       `json:"batch"`
}

// Validate checks the call, the operation, the expected revision and the
// batch.
func (d DecideHeld) Validate() error {
	if err := firstError(d.Call.Validate(), d.Operation.Validate()); err != nil {
		return err
	}
	if d.ExpectedRevision == "" {
		return invalidField("expected_revision", "required by this operation")
	}
	if err := checkOptionalToken("expected_revision", string(d.ExpectedRevision)); err != nil {
		return err
	}
	if d.Batch != "" {
		if err := ValidateTrustBatch(d.Batch); err != nil {
			return invalidField("batch", "must be a valid trust batch")
		}
	}
	return nil
}

// EditHeld edits a held operation's arguments and approves it as edited.
// Args replaces the stored intent: same extension name and version, new
// data.
//
// CorrectedKeys names the trust keys the edit changed: each counts as a
// correction, and no other key moves. The editor knows what it changed; uma
// does not interpret the arguments. Empty means the operation's own trust
// key (HeldBlock.TrustKey).
type EditHeld struct {
	DecideHeld
	Args          Extension `json:"args"`
	CorrectedKeys []string  `json:"corrected_keys,omitempty"`
}

// Validate checks the decision and the arguments.
func (e EditHeld) Validate() error {
	if err := e.DecideHeld.Validate(); err != nil {
		return err
	}
	if err := e.Args.validate("args", DefaultContentLimits().MaxStructuredBytes); err != nil {
		return err
	}
	if len(e.Args.Data) == 0 {
		return invalidField("args.data", "required")
	}
	for _, k := range e.CorrectedKeys {
		if err := ValidateTrustKey(k); err != nil {
			return invalidField("corrected_keys", "each must be a valid trust key")
		}
	}
	return nil
}

// Decided is the result of deciding a held operation. Operation is the
// operation as it stands after the decision, Replayed is set when the
// decision had been made already and nothing was written.
//
// Decision, TrustKey, TrustLabels and Correction are what the host counts
// trust on: Correction is true for an edit or a reject, false for an
// approval as proposed (see the held-operations decision record). A
// Replayed result must not be counted again.
type Decided struct {
	Operation   Operation         `json:"operation"`
	Durable     bool              `json:"durable"`
	Replayed    bool              `json:"replayed"`
	Decision    HeldDecision      `json:"decision"`
	TrustKey    string            `json:"trust_key"`
	TrustLabels map[string]string `json:"trust_labels,omitempty"`
	Correction  bool              `json:"correction"`
}

// HeldEdit is one edit of a held operation's arguments.
type HeldEdit struct {
	By     ActorID   `json:"by"`
	At     time.Time `json:"at"`
	Before Extension `json:"before"`
	After  Extension `json:"after"`
}

// Validate checks the invariants of every held block: the intent the
// operation stored when it was held is present and named.
func (h HeldBlock) Validate() error {
	if h.Intent == nil || h.Intent.Name == "" {
		return invalidField("held.intent", "required")
	}
	return nil
}

// Clone returns a deep copy.
func (h HeldBlock) Clone() HeldBlock {
	h.Preview = clonePtr(h.Preview, Extension.Clone)
	h.Intent = clonePtr(h.Intent, Extension.Clone)
	h.TrustLabels = maps.Clone(h.TrustLabels)
	h.ExpiresAt = clonePtr(h.ExpiresAt, same)
	h.DecidedAt = clonePtr(h.DecidedAt, same)
	h.CarriedOverAt = clonePtr(h.CarriedOverAt, same)
	h.EditedArgs = clonePtr(h.EditedArgs, Extension.Clone)
	h.Edits = slices.Clone(h.Edits)
	for i, e := range h.Edits {
		h.Edits[i].Before, h.Edits[i].After = e.Before.Clone(), e.After.Clone()
	}
	h.ProposedFingerprint = bytes.Clone(h.ProposedFingerprint)
	return h
}

// HeldOperationPayload is the version 1 payload of operation.held,
// operation.decided and operation.expired: references and names only, never
// the preview, the reason or any argument, so an event reveals no draft
// content. Key is the operation's ledger key, whose Target a subscriber
// must be able to read to receive the event, and Proposer its proposer.
// TrustKey is the key the host counts decisions under. Verb and Target
// repeat Key.Kind and Key.Target as top-level fields: the operation's verb
// and the resource it acts on. The event's Inbox is Target's inbox.
//
// Decision and DecidedBy are empty in operation.held. operation.decided
// carries HeldApproved, HeldRejected or HeldEdited and the decider;
// operation.expired carries HeldExpired and no decider.
type HeldOperationPayload struct {
	Operation OperationRef `json:"operation"`
	Verb      string       `json:"verb"`
	Target    Ref          `json:"target"`
	Key       OperationKey `json:"key"`
	Proposer  ActorID      `json:"proposer"`
	TrustKey  string       `json:"trust_key"`
	Decision  HeldDecision `json:"decision"`
	DecidedBy ActorID      `json:"decided_by"`
}

// IsHeldEvent reports whether kind is one of the held operation events,
// which a subscriber receives only while it may read the operation's target.
func IsHeldEvent(kind string) bool {
	switch kind {
	case EventOperationHeld, EventOperationDecided, EventOperationExpired:
		return true
	}
	return false
}
