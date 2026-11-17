package model

import "strings"

// ResourceKind names the kind of resource a generic Ref points at: a built-in
// kind (channel, inbox, contact, identity, conversation, message, operation,
// task, work_item, attachment, subscription, stream, export, label, event,
// interaction) or a namespaced extension kind. A kind is not permission to
// access arbitrary records.
type ResourceKind string

// Built-in kinds, used by the typed refs below.
const (
	resourceChannel      ResourceKind = "channel"
	resourceInbox        ResourceKind = "inbox"
	resourceContact      ResourceKind = "contact"
	resourceIdentity     ResourceKind = "identity"
	resourceConversation ResourceKind = "conversation"
	resourceMessage      ResourceKind = "message"
	resourceOperation    ResourceKind = "operation"
	resourceTask         ResourceKind = "task"
	resourceWorkItem     ResourceKind = "work_item"
	resourceAttachment   ResourceKind = "attachment"
	resourceSubscription ResourceKind = "subscription"
	resourceStream       ResourceKind = "stream"
	resourceExport       ResourceKind = "export"
	resourceLabel        ResourceKind = "label"
	resourceEvent        ResourceKind = "event"
	resourceInteraction  ResourceKind = "interaction"
)

type refScope uint8

const (
	scopeUnknown refScope = iota
	scopeTenant
	scopeInbox
	scopeConversation
	scopeExtension
)

func (k ResourceKind) scope() refScope {
	switch k {
	case resourceChannel, resourceInbox, resourceContact, resourceOperation, resourceSubscription, resourceExport,
		resourceLabel, resourceEvent:
		return scopeTenant
	case resourceIdentity, resourceConversation, resourceAttachment:
		return scopeInbox
	case resourceMessage, resourceTask, resourceWorkItem, resourceStream, resourceInteraction:
		return scopeConversation
	}
	if isRegistryKey(string(k), 3) {
		return scopeExtension
	}
	return scopeUnknown
}

// Validate accepts built-in kinds and namespaced extension kinds: three or more
// lowercase dot-separated segments.
func (k ResourceKind) Validate() error { return k.validate("kind") }

func (k ResourceKind) validate(field string) error {
	if k == "" {
		return invalidField(field, "required")
	}
	if k.scope() == scopeUnknown {
		return invalidField(field, "must be a built-in or namespaced extension resource kind")
	}
	return nil
}

// isRegistryKey reports whether s is lowercase dot-separated segments of
// [a-z0-9_-], each starting with [a-z0-9], with at least minSegments segments.
func isRegistryKey(s string, minSegments int) bool {
	if s == "" || len(s) > maxKeyBytes {
		return false
	}
	segments := strings.Split(s, ".")
	if len(segments) < minSegments {
		return false
	}
	for _, seg := range segments {
		if seg == "" || !isLowerAlnum(seg[0]) {
			return false
		}
		for i := 1; i < len(seg); i++ {
			if c := seg[i]; !isLowerAlnum(c) && c != '_' && c != '-' {
				return false
			}
		}
	}
	return true
}

func isLowerAlnum(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') }

// Scoped is implemented by every reference that belongs to one tenant.
type Scoped interface{ Scope() TenantID }

// CheckScope returns a not_found error when any ref belongs to a tenant other
// than t. It never confirms that a foreign resource exists. The engine calls it
// with the principal's tenant on every request ref.
func CheckScope(t TenantID, refs ...Scoped) error {
	for _, r := range refs {
		if r.Scope() != t {
			return &Error{Code: NotFound, SafeMessage: "resource not found", Retry: NoRetry}
		}
	}
	return nil
}

// SameTenant reports whether a and b belong to the same, non-empty tenant.
func SameTenant(a, b Scoped) bool { return a.Scope() != "" && a.Scope() == b.Scope() }

// newValid returns r when it validates and the zero value otherwise.
func newValid[T interface{ Validate() error }](r T) (T, error) {
	if err := r.Validate(); err != nil {
		var zero T
		return zero, err
	}
	return r, nil
}

func checkTenant(field string, t TenantID) error { return checkToken(field, string(t)) }

func checkLeaf(field string, id ID, optional bool) error {
	if optional && id == "" {
		return nil
	}
	return checkID(field, id)
}

func checkTenantScoped(path string, t TenantID, id ID, leafOptional bool) error {
	return firstError(
		checkTenant(fieldPath(path, "tenant"), t),
		checkLeaf(fieldPath(path, "id"), id, leafOptional),
	)
}

// Tenant-scoped refs.

// NewChannelRef returns a validated channel ref.
func NewChannelRef(t TenantID, id ID) (ChannelRef, error) {
	return newValid(ChannelRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r ChannelRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r ChannelRef) ValidateScope() error { return r.validate("", true) }

func (r ChannelRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r ChannelRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r ChannelRef) IsZero() bool { return r == ChannelRef{} }

// Ref returns the generic form.
func (r ChannelRef) Ref() Ref { return Ref{Tenant: r.Tenant, Kind: resourceChannel, ID: r.ID} }

// NewInboxRef returns a validated inbox ref.
func NewInboxRef(t TenantID, id ID) (InboxRef, error) {
	return newValid(InboxRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r InboxRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r InboxRef) ValidateScope() error { return r.validate("", true) }

func (r InboxRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r InboxRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r InboxRef) IsZero() bool { return r == InboxRef{} }

// Ref returns the generic form.
func (r InboxRef) Ref() Ref { return Ref{Tenant: r.Tenant, Kind: resourceInbox, ID: r.ID} }

// NewContactRef returns a validated contact ref.
func NewContactRef(t TenantID, id ID) (ContactRef, error) {
	return newValid(ContactRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r ContactRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r ContactRef) ValidateScope() error { return r.validate("", true) }

func (r ContactRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r ContactRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r ContactRef) IsZero() bool { return r == ContactRef{} }

// Ref returns the generic form.
func (r ContactRef) Ref() Ref { return Ref{Tenant: r.Tenant, Kind: resourceContact, ID: r.ID} }

// NewOperationRef returns a validated operation ref.
func NewOperationRef(t TenantID, id ID) (OperationRef, error) {
	return newValid(OperationRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r OperationRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r OperationRef) ValidateScope() error { return r.validate("", true) }

func (r OperationRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r OperationRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r OperationRef) IsZero() bool { return r == OperationRef{} }

// Ref returns the generic form.
func (r OperationRef) Ref() Ref { return Ref{Tenant: r.Tenant, Kind: resourceOperation, ID: r.ID} }

// NewSubscriptionRef returns a validated subscription ref.
func NewSubscriptionRef(t TenantID, id ID) (SubscriptionRef, error) {
	return newValid(SubscriptionRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r SubscriptionRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r SubscriptionRef) ValidateScope() error { return r.validate("", true) }

func (r SubscriptionRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r SubscriptionRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r SubscriptionRef) IsZero() bool { return r == SubscriptionRef{} }

// Ref returns the generic form.
func (r SubscriptionRef) Ref() Ref {
	return Ref{Tenant: r.Tenant, Kind: resourceSubscription, ID: r.ID}
}

// NewExportRef returns a validated export ref.
func NewExportRef(t TenantID, id ID) (ExportRef, error) {
	return newValid(ExportRef{Tenant: t, ID: id})
}

// Validate checks tenant and ID.
func (r ExportRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the tenant; an empty ID (creation) is allowed.
func (r ExportRef) ValidateScope() error { return r.validate("", true) }

func (r ExportRef) validate(path string, leafOptional bool) error {
	return checkTenantScoped(path, r.Tenant, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r ExportRef) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r ExportRef) IsZero() bool { return r == ExportRef{} }

// Ref returns the generic form.
func (r ExportRef) Ref() Ref { return Ref{Tenant: r.Tenant, Kind: resourceExport, ID: r.ID} }

// Inbox-scoped refs.

func checkInboxScoped(path string, in InboxRef, id ID, leafOptional bool) error {
	return firstError(
		in.validate(fieldPath(path, "inbox"), false),
		checkLeaf(fieldPath(path, "id"), id, leafOptional),
	)
}

func inboxRef(kind ResourceKind, in InboxRef, id ID) Ref {
	return Ref{Tenant: in.Tenant, Kind: kind, Inbox: in.ID, ID: id}
}

// NewIdentityRef returns a validated identity ref under inbox in.
func NewIdentityRef(in InboxRef, id ID) (IdentityRef, error) {
	return newValid(IdentityRef{Inbox: in, ID: id})
}

// Validate checks the inbox chain and ID.
func (r IdentityRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the inbox chain; an empty ID (creation) is allowed.
func (r IdentityRef) ValidateScope() error { return r.validate("", true) }

func (r IdentityRef) validate(path string, leafOptional bool) error {
	return checkInboxScoped(path, r.Inbox, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r IdentityRef) Scope() TenantID { return r.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r IdentityRef) IsZero() bool { return r == IdentityRef{} }

// In reports whether r belongs to inbox in.
func (r IdentityRef) In(in InboxRef) bool { return r.Inbox == in }

// Ref returns the generic form.
func (r IdentityRef) Ref() Ref { return inboxRef(resourceIdentity, r.Inbox, r.ID) }

// NewConversationRef returns a validated conversation ref under inbox in.
func NewConversationRef(in InboxRef, id ID) (ConversationRef, error) {
	return newValid(ConversationRef{Inbox: in, ID: id})
}

// Validate checks the inbox chain and ID.
func (r ConversationRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the inbox chain; an empty ID (creation) is allowed.
func (r ConversationRef) ValidateScope() error { return r.validate("", true) }

func (r ConversationRef) validate(path string, leafOptional bool) error {
	return checkInboxScoped(path, r.Inbox, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r ConversationRef) Scope() TenantID { return r.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r ConversationRef) IsZero() bool { return r == ConversationRef{} }

// In reports whether r belongs to inbox in.
func (r ConversationRef) In(in InboxRef) bool { return r.Inbox == in }

// Ref returns the generic form.
func (r ConversationRef) Ref() Ref { return inboxRef(resourceConversation, r.Inbox, r.ID) }

// NewAttachmentRef returns a validated attachment ref under inbox in.
func NewAttachmentRef(in InboxRef, id ID) (AttachmentRef, error) {
	return newValid(AttachmentRef{Inbox: in, ID: id})
}

// Validate checks the inbox chain and ID.
func (r AttachmentRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the inbox chain; an empty ID (creation) is allowed.
func (r AttachmentRef) ValidateScope() error { return r.validate("", true) }

func (r AttachmentRef) validate(path string, leafOptional bool) error {
	return checkInboxScoped(path, r.Inbox, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r AttachmentRef) Scope() TenantID { return r.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r AttachmentRef) IsZero() bool { return r == AttachmentRef{} }

// In reports whether r belongs to inbox in.
func (r AttachmentRef) In(in InboxRef) bool { return r.Inbox == in }

// Ref returns the generic form.
func (r AttachmentRef) Ref() Ref { return inboxRef(resourceAttachment, r.Inbox, r.ID) }

// Conversation-scoped refs. Their generic form drops the conversation.

func checkConversationScoped(path string, c ConversationRef, id ID, leafOptional bool) error {
	return firstError(
		c.validate(fieldPath(path, "conversation"), false),
		checkLeaf(fieldPath(path, "id"), id, leafOptional),
	)
}

// NewMessageRef returns a validated message ref under conversation c.
func NewMessageRef(c ConversationRef, id ID) (MessageRef, error) {
	return newValid(MessageRef{Conversation: c, ID: id})
}

// Validate checks the conversation chain and ID.
func (r MessageRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the conversation chain; an empty ID is allowed.
func (r MessageRef) ValidateScope() error { return r.validate("", true) }

func (r MessageRef) validate(path string, leafOptional bool) error {
	return checkConversationScoped(path, r.Conversation, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r MessageRef) Scope() TenantID { return r.Conversation.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r MessageRef) IsZero() bool { return r == MessageRef{} }

// In reports whether r belongs to conversation c.
func (r MessageRef) In(c ConversationRef) bool { return r.Conversation == c }

// Ref returns the generic form; the conversation is dropped.
func (r MessageRef) Ref() Ref { return inboxRef(resourceMessage, r.Conversation.Inbox, r.ID) }

// NewTaskRef returns a validated task ref under conversation c.
func NewTaskRef(c ConversationRef, id ID) (TaskRef, error) {
	return newValid(TaskRef{Conversation: c, ID: id})
}

// Validate checks the conversation chain and ID.
func (r TaskRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the conversation chain; an empty ID is allowed.
func (r TaskRef) ValidateScope() error { return r.validate("", true) }

func (r TaskRef) validate(path string, leafOptional bool) error {
	return checkConversationScoped(path, r.Conversation, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r TaskRef) Scope() TenantID { return r.Conversation.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r TaskRef) IsZero() bool { return r == TaskRef{} }

// In reports whether r belongs to conversation c.
func (r TaskRef) In(c ConversationRef) bool { return r.Conversation == c }

// Ref returns the generic form; the conversation is dropped.
func (r TaskRef) Ref() Ref { return inboxRef(resourceTask, r.Conversation.Inbox, r.ID) }

// NewWorkItemRef returns a validated work item ref under conversation c.
func NewWorkItemRef(c ConversationRef, id ID) (WorkItemRef, error) {
	return newValid(WorkItemRef{Conversation: c, ID: id})
}

// Validate checks the conversation chain and ID.
func (r WorkItemRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the conversation chain; an empty ID is allowed.
func (r WorkItemRef) ValidateScope() error { return r.validate("", true) }

func (r WorkItemRef) validate(path string, leafOptional bool) error {
	return checkConversationScoped(path, r.Conversation, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r WorkItemRef) Scope() TenantID { return r.Conversation.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r WorkItemRef) IsZero() bool { return r == WorkItemRef{} }

// In reports whether r belongs to conversation c.
func (r WorkItemRef) In(c ConversationRef) bool { return r.Conversation == c }

// Ref returns the generic form; the conversation is dropped.
func (r WorkItemRef) Ref() Ref { return inboxRef(resourceWorkItem, r.Conversation.Inbox, r.ID) }

// NewStreamRef returns a validated stream ref under conversation c.
func NewStreamRef(c ConversationRef, id ID) (StreamRef, error) {
	return newValid(StreamRef{Conversation: c, ID: id})
}

// Validate checks the conversation chain and ID.
func (r StreamRef) Validate() error { return r.validate("", false) }

// ValidateScope checks the conversation chain; an empty ID is allowed.
func (r StreamRef) ValidateScope() error { return r.validate("", true) }

func (r StreamRef) validate(path string, leafOptional bool) error {
	return checkConversationScoped(path, r.Conversation, r.ID, leafOptional)
}

// Scope returns the owning tenant.
func (r StreamRef) Scope() TenantID { return r.Conversation.Inbox.Tenant }

// IsZero reports whether r is unset.
func (r StreamRef) IsZero() bool { return r == StreamRef{} }

// In reports whether r belongs to conversation c.
func (r StreamRef) In(c ConversationRef) bool { return r.Conversation == c }

// Ref returns the generic form; the conversation is dropped.
func (r StreamRef) Ref() Ref { return inboxRef(resourceStream, r.Conversation.Inbox, r.ID) }

// Generic Ref.

// Validate checks tenant, kind, the inbox scope the kind requires or forbids,
// and ID.
func (r Ref) Validate() error { return r.validate("", false) }

// ValidateScope applies Validate except that an empty ID is allowed.
func (r Ref) ValidateScope() error { return r.validate("", true) }

func (r Ref) validate(path string, leafOptional bool) error {
	if err := firstError(
		checkTenant(fieldPath(path, "tenant"), r.Tenant),
		r.Kind.validate(fieldPath(path, "kind")),
	); err != nil {
		return err
	}
	inbox := fieldPath(path, "inbox")
	var err error
	switch r.Kind.scope() {
	case scopeTenant:
		if r.Inbox != "" {
			err = invalidField(inbox, "must be empty for a tenant-scoped kind")
		}
	case scopeInbox, scopeConversation:
		err = checkID(inbox, r.Inbox)
	default:
		err = checkLeaf(inbox, r.Inbox, true)
	}
	return firstError(err, checkLeaf(fieldPath(path, "id"), r.ID, leafOptional))
}

// Scope returns the owning tenant.
func (r Ref) Scope() TenantID { return r.Tenant }

// IsZero reports whether r is unset.
func (r Ref) IsZero() bool { return r == Ref{} }

func (r Ref) expect(kind ResourceKind) error {
	if r.Kind != kind {
		return invalidField("kind", "does not match the requested reference type")
	}
	return r.Validate()
}

// AsChannel converts a channel Ref after checking kind, scope and ID.
func (r Ref) AsChannel() (ChannelRef, error) {
	if err := r.expect(resourceChannel); err != nil {
		return ChannelRef{}, err
	}
	return ChannelRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// AsInbox converts an inbox Ref after checking kind, scope and ID.
func (r Ref) AsInbox() (InboxRef, error) {
	if err := r.expect(resourceInbox); err != nil {
		return InboxRef{}, err
	}
	return InboxRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// AsContact converts a contact Ref after checking kind, scope and ID.
func (r Ref) AsContact() (ContactRef, error) {
	if err := r.expect(resourceContact); err != nil {
		return ContactRef{}, err
	}
	return ContactRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// AsOperation converts an operation Ref after checking kind, scope and ID.
func (r Ref) AsOperation() (OperationRef, error) {
	if err := r.expect(resourceOperation); err != nil {
		return OperationRef{}, err
	}
	return OperationRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// AsSubscription converts a subscription Ref after checking kind, scope and ID.
func (r Ref) AsSubscription() (SubscriptionRef, error) {
	if err := r.expect(resourceSubscription); err != nil {
		return SubscriptionRef{}, err
	}
	return SubscriptionRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// AsExport converts an export Ref after checking kind, scope and ID.
func (r Ref) AsExport() (ExportRef, error) {
	if err := r.expect(resourceExport); err != nil {
		return ExportRef{}, err
	}
	return ExportRef{Tenant: r.Tenant, ID: r.ID}, nil
}

// AsIdentity converts an identity Ref after checking kind, scope and ID.
func (r Ref) AsIdentity() (IdentityRef, error) {
	if err := r.expect(resourceIdentity); err != nil {
		return IdentityRef{}, err
	}
	return IdentityRef{Inbox: InboxRef{Tenant: r.Tenant, ID: r.Inbox}, ID: r.ID}, nil
}

// AsConversation converts a conversation Ref after checking kind, scope and ID.
func (r Ref) AsConversation() (ConversationRef, error) {
	if err := r.expect(resourceConversation); err != nil {
		return ConversationRef{}, err
	}
	return ConversationRef{Inbox: InboxRef{Tenant: r.Tenant, ID: r.Inbox}, ID: r.ID}, nil
}

// AsAttachment converts an attachment Ref after checking kind, scope and ID.
func (r Ref) AsAttachment() (AttachmentRef, error) {
	if err := r.expect(resourceAttachment); err != nil {
		return AttachmentRef{}, err
	}
	return AttachmentRef{Inbox: InboxRef{Tenant: r.Tenant, ID: r.Inbox}, ID: r.ID}, nil
}
