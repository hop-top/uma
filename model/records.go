package model

import "time"

// Conversation statuses (closed set, rule set contact-lifecycle/v1).
const (
	// ConversationOpen is active; awaiting or receiving handling.
	ConversationOpen = "open"
	// ConversationPending is active but parked; inbound does not change it.
	ConversationPending = "pending"
	// ConversationResolved is handled and may reopen.
	ConversationResolved = "resolved"
	// ConversationClosed is terminal: never selected for a new message, never
	// reopened. Its messages stay addressable.
	ConversationClosed = "closed"
)

// IsConversationStatus reports whether s is a declared conversation status.
func IsConversationStatus(s string) bool {
	switch s {
	case ConversationOpen, ConversationPending, ConversationResolved, ConversationClosed:
		return true
	}
	return false
}

// Conversation selection modes (Selection.Mode, Conversation.Mode).
const (
	ModePerContact      = "per_contact"
	ModePerRoom         = "per_room"
	ModePerThread       = "per_thread"
	ModePerWorkItem     = "per_work_item"
	ModePerAgentContext = "per_agent_context"
)

// IsSelectionMode reports whether s is a declared selection mode.
func IsSelectionMode(s string) bool {
	switch s {
	case ModePerContact, ModePerRoom, ModePerThread, ModePerWorkItem, ModePerAgentContext:
		return true
	}
	return false
}

// Contact kinds (Contact.Kind, Identity.Kind). A bot or agent identity is never
// linked to a person contact.
const (
	ContactKindPerson = "person"
	ContactKindBot    = "bot"
	ContactKindAgent  = "agent"
)

// AuthorKindSystem is the Author.Kind of a message from a system actor
// (Envelope.SystemActor): no identity and no contact. It is never a contact
// kind, so never an Envelope.SenderKind. Author.Kind is otherwise the
// sender identity's contact kind.
const AuthorKindSystem = "system"

// IsContactKind reports whether s is a declared contact kind.
func IsContactKind(s string) bool {
	switch s {
	case ContactKindPerson, ContactKindBot, ContactKindAgent:
		return true
	}
	return false
}

// LinkAssurance is how strongly an identity is shown to belong to a contact.
// It is distinct from source assurance (Identity.Assurance). Levels are
// ordered low to high.
type LinkAssurance string

const (
	// LinkAssuranceNone: provisional or unlinked; no claim of equality.
	LinkAssuranceNone LinkAssurance = "none"
	// LinkAssuranceAsserted: an authorized actor attests; no external proof.
	LinkAssuranceAsserted LinkAssurance = "asserted"
	// LinkAssuranceImported: a trusted system-of-record import names the link.
	LinkAssuranceImported LinkAssurance = "imported"
	// LinkAssuranceAttributeMatch: both sides carry the same provider-verified
	// normalized attribute.
	LinkAssuranceAttributeMatch LinkAssurance = "attribute_match"
	// LinkAssuranceHostAuthenticated: one host-authenticated session binds both
	// identities.
	LinkAssuranceHostAuthenticated LinkAssurance = "host_authenticated"
	// LinkAssuranceProven: a challenge sent through one identity was answered
	// through the other.
	LinkAssuranceProven LinkAssurance = "proven"
)

var linkAssuranceOrder = [...]LinkAssurance{
	LinkAssuranceNone, LinkAssuranceAsserted, LinkAssuranceImported,
	LinkAssuranceAttributeMatch, LinkAssuranceHostAuthenticated, LinkAssuranceProven,
}

// Rank returns the position of a in the assurance order, or -1 when unknown.
func (a LinkAssurance) Rank() int {
	for i, v := range linkAssuranceOrder {
		if v == a {
			return i
		}
	}
	return -1
}

// Below reports whether a is a known level strictly lower than known level b.
func (a LinkAssurance) Below(b LinkAssurance) bool {
	ra, rb := a.Rank(), b.Rank()
	return ra >= 0 && rb >= 0 && ra < rb
}

// Validate requires a declared level.
func (a LinkAssurance) Validate() error { return a.validate("assurance") }

func (a LinkAssurance) validate(field string) error {
	if a.Rank() < 0 {
		return invalidField(field, "must be a declared link assurance")
	}
	return nil
}

// LinkClass classifies a link history record.
type LinkClass string

const (
	// LinkClassLink moves an identity off its provisional contact.
	LinkClassLink LinkClass = "link"
	// LinkClassRelink moves an identity between existing contacts.
	LinkClassRelink LinkClass = "relink"
	// LinkClassUnlink detaches an identity to a fresh contact.
	LinkClassUnlink LinkClass = "unlink"
	// LinkClassMerge moves an identity with the rest of its contact's
	// identities into another contact, in one Contacts.Merge.
	LinkClassMerge LinkClass = "merge"
)

// Validate requires a declared class.
func (c LinkClass) Validate() error { return c.validate("class") }

func (c LinkClass) validate(field string) error {
	switch c {
	case LinkClassLink, LinkClassRelink, LinkClassUnlink, LinkClassMerge:
		return nil
	}
	return invalidField(field, "must be link, relink, unlink or merge")
}

// IdentityLink is an append-only link history record, one per identity link
// change, written with the pointer change and its domain event. It preserves
// the identity's original key and never rewrites conversations or routes.
//
// Assurance is the resulting link assurance: the verified level for link
// and relink, LinkAssuranceNone for unlink; Identity.LinkAssurance mirrors
// the latest. Evidence.Assurance is the level the verifier returned, which
// for an unlink may be above none. Reason is the actor's audit reason code,
// set for unlink and empty for link and relink; PolicyVersion and
// ReasonCode are the provenance of the policy decision that allowed the
// change. Redaction of the identity, or of its contact, tombstones the
// record: Facts are cleared and Redacted set, while Verifier, Reference,
// Reason, Assurance, references, revisions and provenance stay.
type IdentityLink struct {
	ID       ID          `json:"id"`
	Tenant   TenantID    `json:"tenant"`
	Identity IdentityRef `json:"identity"`
	// IdentityKey is a snapshot of the original reference.
	IdentityKey IdentityKey `json:"identity_key"`
	Class       LinkClass   `json:"class"`
	From        ContactRef  `json:"from"`
	To          ContactRef  `json:"to"`
	// Supersedes is the previous record for this identity; empty for the first.
	Supersedes ID `json:"supersedes"`
	// Evidence is the verifier-returned evidence, never the caller's.
	Evidence        Evidence      `json:"evidence"`
	VerifierVersion string        `json:"verifier_version"`
	Assurance       LinkAssurance `json:"assurance"`
	// Anchor is the identity already on To, when the evidence kind needs one.
	Anchor         *IdentityRef  `json:"anchor,omitempty"`
	Actor          ActorID       `json:"actor"`
	Operation      OperationRef  `json:"operation"`
	Correlation    CorrelationID `json:"correlation"`
	PolicyVersion  string        `json:"policy_version"`
	ReasonCode     string        `json:"reason_code"`
	RevisionBefore Revision      `json:"revision_before"`
	RevisionAfter  Revision      `json:"revision_after"`
	RecordedAt     time.Time     `json:"recorded_at"`
	Reason         string        `json:"reason"`
	Redacted       bool          `json:"redacted"`
}

// Validate checks envelope IDs, that Tenant and Inbox equal the Resource scope,
// and the audience and payload shapes. The one exception is Inbox: an event
// whose Resource is an operation, which belongs to no inbox, may leave it
// empty or set it to the inbox of the resource the operation acts on.
func (e Event) Validate() error {
	var tenant, inbox, causation error
	if e.Tenant != e.Resource.Tenant {
		tenant = invalidField("tenant", "must equal the resource tenant")
	}
	switch {
	case e.Resource.Inbox == "" && e.Resource.Kind == KindOperation && e.Inbox != "":
		// An operation is tenant-scoped: its event may name the inbox of
		// the resource the operation acts on.
		inbox = checkID("inbox", e.Inbox)
	case e.Inbox != e.Resource.Inbox:
		inbox = invalidField("inbox", "must equal the resource inbox")
	}
	if e.Causation != "" {
		causation = checkID("causation", e.Causation)
	}
	return firstError(
		checkID("id", e.ID),
		checkToken("kind", e.Kind),
		e.Resource.validate("resource", false),
		tenant,
		inbox,
		causation,
		checkOptionalToken("correlation", string(e.Correlation)),
		e.Audience.validate("audience"),
		e.Payload.validateOptional("payload"),
	)
}
