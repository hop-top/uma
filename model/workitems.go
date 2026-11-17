package model

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"time"
)

// Work item statuses (WorkItem.Status): the normalized lifecycle an adapter
// maps a provider's raw status onto through its declared status mapping.
// The raw status is always kept beside it; an empty Status is a raw status
// the mapping does not cover. A closed status uma owns (a local status) is
// terminal for WorkItems.Act; a provider-owned closed status may move back
// to open, by a provider action, when the channel adapter declares that
// transition (adapter.Descriptor.WorkItemActs).
const (
	WorkItemOpen     = "open"
	WorkItemPending  = "pending"
	WorkItemResolved = "resolved"
	WorkItemClosed   = "closed"
)

// IsWorkItemStatus reports whether s is a declared work item status.
func IsWorkItemStatus(s string) bool {
	switch s {
	case WorkItemOpen, WorkItemPending, WorkItemResolved, WorkItemClosed:
		return true
	}
	return false
}

// Work item binding states (WorkItem.Binding): what the provider did with
// the issue or ticket the work item is bound to. Every state other than
// active is set by the provider, never by a uma command; merged and moved
// name their successor. The binding itself (WorkItem.External) never
// changes, so the conversation keeps its history.
const (
	WorkItemBindingActive       = "active"
	WorkItemBindingMerged       = "merged"
	WorkItemBindingMoved        = "moved"
	WorkItemBindingDeleted      = "deleted"
	WorkItemBindingInaccessible = "inaccessible"
)

// IsWorkItemBinding reports whether s is a declared binding state.
func IsWorkItemBinding(s string) bool {
	switch s {
	case WorkItemBindingActive, WorkItemBindingMerged, WorkItemBindingMoved,
		WorkItemBindingDeleted, WorkItemBindingInaccessible:
		return true
	}
	return false
}

// WorkItemLocalFields lists the work item fields whose source of truth may
// be uma instead of the provider (WorkItemState.LocalFields). External,
// URL, requester and binding are always the provider's.
func WorkItemLocalFields() []string {
	return []string{WorkItemFieldStatus, WorkItemFieldSubject, WorkItemFieldAssignees, WorkItemFieldFields}
}

// Work item text limits.
const (
	maxWorkItemTextBytes = 8 << 10
	maxWorkItemRefs      = 256
)

// WorkItemState is a provider's view of one issue or ticket, reported by a
// ticketing adapter on Envelope.WorkItem. It is a snapshot: every field is
// the provider's current value, absent meaning empty. Status is the
// adapter's declared mapping of RawStatus, or empty when the mapping does
// not cover it. Binding defaults to active; merged and moved require
// Successor. LocalFields declares the fields uma owns for this item, from
// the installation's synchronization settings: the provider's values for
// them seed a new work item and are ignored afterwards, and WorkItems.Act
// may change only them. Every other field is the provider's: the work item
// is ingest-only for it.
type WorkItemState struct {
	Subject     string        `json:"subject"`
	URL         string        `json:"url"`
	Status      string        `json:"status"`
	RawStatus   string        `json:"raw_status"`
	Requester   ExternalRef   `json:"requester"`
	Assignees   []ExternalRef `json:"assignees,omitempty"`
	Fields      []Extension   `json:"fields,omitempty"`
	Binding     string        `json:"binding"`
	Successor   *ExternalRef  `json:"successor,omitempty"`
	LocalFields []string      `json:"local_fields,omitempty"`
}

// Validate checks the state's shape. Channel ownership of its references is
// checked by the engine against the bound channel.
func (s WorkItemState) Validate() error { return s.validate("work_item") }

func (s WorkItemState) validate(path string) error {
	var successor error
	switch s.Binding {
	case "", WorkItemBindingActive, WorkItemBindingDeleted, WorkItemBindingInaccessible:
		if s.Successor != nil {
			successor = invalidField(fieldPath(path, "successor"), "only a merged or moved binding has a successor")
		}
	case WorkItemBindingMerged, WorkItemBindingMoved:
		if s.Successor == nil {
			successor = invalidField(fieldPath(path, "successor"), "required for a merged or moved binding")
		} else if s.Successor.Kind != RefKindWorkItem {
			successor = invalidField(fieldPath(path, "successor.kind"), "must be work_item")
		} else {
			successor = s.Successor.validate(fieldPath(path, "successor"))
		}
	default:
		successor = invalidField(fieldPath(path, "binding"), "must be a declared binding state")
	}
	var status error
	if s.Status != "" && !IsWorkItemStatus(s.Status) {
		status = invalidField(fieldPath(path, "status"), "must be a declared work item status")
	}
	var requester error
	if !s.Requester.IsZero() {
		requester = s.Requester.validate(fieldPath(path, "requester"))
	}
	return firstError(
		optionalText(fieldPath(path, "subject"), s.Subject, maxWorkItemTextBytes),
		optionalText(fieldPath(path, "url"), s.URL, maxWorkItemTextBytes),
		status,
		optionalText(fieldPath(path, "raw_status"), s.RawStatus, maxTokenBytes),
		requester,
		checkRefs(fieldPath(path, "assignees"), s.Assignees),
		checkFields(fieldPath(path, "fields"), s.Fields),
		successor,
		checkLocalFields(fieldPath(path, "local_fields"), s.LocalFields),
	)
}

// Clone returns a deep copy.
func (s WorkItemState) Clone() WorkItemState {
	s.Assignees = slices.Clone(s.Assignees)
	s.Fields = cloneEach(s.Fields, Extension.Clone)
	s.Successor = clonePtr(s.Successor, same)
	s.LocalFields = slices.Clone(s.LocalFields)
	return s
}

// WorkItemBindingChange records one binding transition of a work item.
type WorkItemBindingChange struct {
	From           string       `json:"from"`
	To             string       `json:"to"`
	Successor      *ExternalRef `json:"successor,omitempty"`
	OccurredAt     time.Time    `json:"occurred_at"`
	SourceRevision string       `json:"source_revision"`
}

// WorkItemSource is the provider version of the latest state applied to a
// work item: its source revision and source time. A later state with an
// earlier or equal source time is stale and never overwrites newer values.
type WorkItemSource struct {
	Revision   string    `json:"revision"`
	OccurredAt time.Time `json:"occurred_at"`
}

// SchemaWorkItemUpdate names the builtin WorkItems.Act action schema
// (WorkItemAction.Action.Name, version 1): a WorkItemUpdate.
const SchemaWorkItemUpdate = CoreNamespace + ".work_item.update"

// WorkItemUpdate is the version 1 WorkItems.Act action: the fields to set.
// An absent field is unchanged; at least one is required. Assignees and
// Fields replace the whole set; empty clears it.
//
// Field ownership decides where an update goes. Fields the work item
// declares local (WorkItem.LocalFields) change in uma. A status the
// provider owns changes at the provider, through the channel adapter's
// work item transitions (model.CapabilityWorkItemAct), and only alone: a
// provider-owned status with any other field is model.Validation ("send
// provider-owned and uma-owned fields in separate actions"). Every other
// provider-owned field is ingest-only: setting it is model.Unsupported.
type WorkItemUpdate struct {
	Status *string `json:"status,omitempty"`
	// StatusReason qualifies a provider-owned Status with the reason the
	// provider records for the transition, one of the reasons the channel
	// adapter declares for it (adapter.WorkItemTransition.Reasons); absent
	// lets the adapter choose its default. GitHub, for example, takes
	// completed or not_planned when closing an issue and reopened when
	// reopening it. It is valid only with Status, as one lowercase
	// snake_case segment of at most MaxStatusReasonBytes, and only for a
	// status the provider owns: with a local status it is model.Validation.
	StatusReason *string        `json:"status_reason,omitempty"`
	Subject      *string        `json:"subject,omitempty"`
	Assignees    *[]ExternalRef `json:"assignees,omitempty"`
	Fields       *[]Extension   `json:"fields,omitempty"`
}

// MaxStatusReasonBytes bounds a work item status reason.
const MaxStatusReasonBytes = 64

// ValidateStatusReason checks a work item status reason's shape: one
// lowercase snake_case segment ([a-z][a-z0-9_]*) of at most
// MaxStatusReasonBytes. Which reasons a transition takes is the channel
// adapter's declaration.
func ValidateStatusReason(reason string) error { return checkStatusReason("status_reason", reason) }

func checkStatusReason(field, reason string) error {
	switch {
	case reason == "":
		return invalidField(field, "required; omit it for the adapter's default")
	case len(reason) > MaxStatusReasonBytes:
		return invalidField(field, "exceeds 64 bytes")
	case !builtinTokenRE.MatchString(reason):
		return invalidField(field, "must be one lowercase snake_case segment")
	}
	return nil
}

// Validate checks that at least one field is set and each set field's
// value. A status reason needs a status.
func (u WorkItemUpdate) Validate() error {
	if u.Status == nil && u.StatusReason == nil && u.Subject == nil && u.Assignees == nil && u.Fields == nil {
		return invalidField("action.data", "sets no field")
	}
	var status, reason, subject, assignees, fields error
	if u.Status != nil && !IsWorkItemStatus(*u.Status) {
		status = invalidField("action.data.status", "must be a declared work item status")
	}
	switch {
	case u.StatusReason == nil:
	case u.Status == nil:
		reason = invalidField("action.data.status_reason", "only with status")
	default:
		reason = checkStatusReason("action.data.status_reason", *u.StatusReason)
	}
	if u.Subject != nil {
		subject = optionalText("action.data.subject", *u.Subject, maxWorkItemTextBytes)
	}
	if u.Assignees != nil {
		assignees = checkRefs("action.data.assignees", *u.Assignees)
	}
	if u.Fields != nil {
		fields = checkFields("action.data.fields", *u.Fields)
	}
	return firstError(status, reason, subject, assignees, fields)
}

// Changed returns the work item fields u sets, in WorkItemLocalFields order.
// A status reason belongs to the status field.
func (u WorkItemUpdate) Changed() []string {
	var out []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{WorkItemFieldStatus, u.Status != nil},
		{WorkItemFieldSubject, u.Subject != nil},
		{WorkItemFieldAssignees, u.Assignees != nil},
		{WorkItemFieldFields, u.Fields != nil},
	} {
		if f.set {
			out = append(out, f.name)
		}
	}
	return out
}

// DecodeWorkItemUpdate reads a WorkItems.Act action. A name other than
// SchemaWorkItemUpdate is model.Unsupported, another version
// model.SchemaMismatch, and unknown or malformed data model.Validation.
func DecodeWorkItemUpdate(a Extension) (WorkItemUpdate, error) {
	var u WorkItemUpdate
	switch {
	case a.Name != SchemaWorkItemUpdate:
		return u, NewError(Unsupported, "action.name: unknown work item action")
	case a.Version != 1:
		return u, &Error{Code: SchemaMismatch, SafeMessage: "action.version: unsupported work item action version", Retry: NoRetry}
	}
	dec := json.NewDecoder(bytes.NewReader(a.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&u); err != nil || dec.More() {
		return WorkItemUpdate{}, invalidField("action.data", "must be a work item update")
	}
	if err := u.Validate(); err != nil {
		return WorkItemUpdate{}, err
	}
	return u, nil
}

// Echo correlation.
//
// An outbound effect a provider echoes back inbound must not come back as a
// new message. Core correlates an echo with its operation by either
// reference:
//
//   - the provider message ID: an adapter reports the ID of a sent message
//     as ProviderMessageRef(route channel, id) in its Outcome, and an inbound
//     message with that ID is the echo of the uma-authored message holding
//     it;
//   - an echo reference: a provider that may echo before answering the send
//     call needs the adapter to embed the effect's operation and attempt in
//     the outbound payload, where the provider round-trips it, and to report
//     EchoRef on the echo's envelope. The echo then counts as provider
//     evidence for that attempt.
//
// Body or sender similarity never correlates anything.

// EchoNamespace is the namespace of EchoRef.
const EchoNamespace = CoreNamespace + ".echo"

// RefKindAttempt is the kind of EchoRef.
const RefKindAttempt = "attempt"

// EchoRef returns the envelope reference naming the outbound attempt of op
// that an inbound echo reports, on channel ch.
func EchoRef(ch ChannelRef, op OperationRef, attempt ID) ExternalRef {
	return ExternalRef{Channel: ch, Namespace: EchoNamespace, Kind: RefKindAttempt, ID: string(attempt), ParentID: string(op.ID)}
}

func optionalText(field, s string, max int) error {
	if s == "" {
		return nil
	}
	return checkRaw(field, s, max)
}

func checkRefs(field string, refs []ExternalRef) error {
	if len(refs) > maxWorkItemRefs {
		return invalidField(field, "too many references")
	}
	for i, r := range refs {
		if err := r.validate(indexPath(field, i)); err != nil {
			return err
		}
	}
	return nil
}

func checkFields(field string, xs []Extension) error {
	if len(xs) > maxWorkItemRefs {
		return invalidField(field, "too many fields")
	}
	for i, x := range xs {
		if err := x.validate(indexPath(field, i), DefaultContentLimits().MaxStructuredBytes); err != nil {
			return err
		}
	}
	return nil
}

func checkLocalFields(field string, names []string) error {
	allowed := WorkItemLocalFields()
	for i, n := range names {
		if !slices.Contains(allowed, n) {
			return invalidField(indexPath(field, i), "must name a field uma may own")
		}
		if slices.Contains(names[:i], n) {
			return invalidField(indexPath(field, i), "duplicate field")
		}
	}
	return nil
}

func indexPath(field string, i int) string {
	return field + "[" + strconv.Itoa(i) + "]"
}
