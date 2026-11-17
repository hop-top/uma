package model

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// A provider mailbox is a container the provider keeps mail in or marks it
// with: a Gmail label, an Exchange folder or category, an IMAP mailbox. It
// is not a tenant Label (Labels) and ProviderLabel names one only as a
// conversation holds it. A mail adapter lists the mailboxes of its account
// and creates one through two extensions (see MailboxListName and
// MailboxCreateName): a read, Extensions.Query, and an effect,
// Extensions.Execute, which is a write like any other and can be held for a
// plugin actor.

// Provider mailbox kinds (ProviderMailbox.Kind).
const (
	// MailboxLabel is a tag a message carries beside others (Gmail).
	MailboxLabel = "label"
	// MailboxFolder is a place a message sits in, one at a time (Exchange
	// mail folders, IMAP mailboxes).
	MailboxFolder = "folder"
	// MailboxCategory is a named, colored tag of an Exchange mailbox,
	// referenced by its display name.
	MailboxCategory = "category"
)

// IsMailboxKind reports whether k is a declared mailbox kind.
func IsMailboxKind(k string) bool {
	return k == MailboxLabel || k == MailboxFolder || k == MailboxCategory
}

// Mailbox roles (ProviderMailbox.Role) beyond the destination roles
// (RoleInbox, RoleArchive, RoleTrash, RoleJunk).
const (
	MailboxRoleSent   = "sent"
	MailboxRoleDrafts = "drafts"
)

// IsMailboxRole reports whether r is a declared mailbox role, or empty.
func IsMailboxRole(r string) bool {
	switch r {
	case "", RoleInbox, RoleArchive, RoleTrash, RoleJunk, MailboxRoleSent, MailboxRoleDrafts:
		return true
	}
	return false
}

// MailboxSeparator is the hierarchy separator of ProviderMailbox.Name and
// of the name a create takes, whatever the provider uses itself: an adapter
// translates it (an IMAP server's delimiter) and creates the missing
// parents of a path where the provider has them as objects.
const MailboxSeparator = "/"

// MaxMailboxNameBytes bounds a mailbox name, path included.
const MaxMailboxNameBytes = 225

// Extension name suffixes of the mailbox read and effect of an adapter
// whose type is T: T+MailboxListSuffix and T+MailboxCreateSuffix. Each
// is also the capability the adapter declares for it, so a host reads per
// channel, from Configuration().Capabilities, whether it can list or create
// mailboxes; the read also needs extension_query and the effect
// extension_execute.
const (
	MailboxListSuffix   = ".mailboxes"
	MailboxCreateSuffix = ".mailbox_create"
	// MailboxExtensionVersion is the version of both extensions.
	MailboxExtensionVersion = 1
)

// MailboxListName is the name of the mailbox list read, and the capability
// that gates it, of adapter type t.
func MailboxListName(t TypeKey) string { return string(t) + MailboxListSuffix }

// MailboxCreateName is the name of the mailbox create effect, and the
// capability that gates it, of adapter type t.
func MailboxCreateName(t TypeKey) string { return string(t) + MailboxCreateSuffix }

// ProviderMailboxNamespace and RefKindMailbox name the provider reference
// of a mailbox, as a create reports it in Operation.ProviderReferences.
const (
	ProviderMailboxNamespace = CoreNamespace + ".provider"
	RefKindMailbox           = "mailbox"
)

// ProviderMailboxRef is the provider reference of mailbox id on channel ch.
func ProviderMailboxRef(ch ChannelRef, id string) ExternalRef {
	return ExternalRef{Channel: ch, Namespace: ProviderMailboxNamespace, Kind: RefKindMailbox, ID: id}
}

// ProviderMailbox is one mailbox of a provider account. ID is the
// provider's own: a Gmail label ID, an Exchange folder ID, an IMAP mailbox
// name as the server spells it; a category is referenced by its display
// name, which is its ID here. Name is the display name with
// MailboxSeparator as hierarchy separator. Role is the special use the
// provider gives it, empty for most.
type ProviderMailbox struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

// Validate checks m's fields.
func (m ProviderMailbox) Validate() error {
	switch {
	case m.ID == "":
		return invalidField("id", "required")
	case !IsMailboxKind(m.Kind):
		return invalidField("kind", "must be label, folder or category")
	case !IsMailboxRole(m.Role):
		return invalidField("role", "not a mailbox role")
	}
	return ValidateMailboxName(m.Name)
}

// ProviderMailboxes is the result of the mailbox list read: the result
// schema of MailboxListName, version MailboxExtensionVersion. Decode a
// result with DecodeProviderMailboxes.
type ProviderMailboxes struct {
	Mailboxes []ProviderMailbox `json:"mailboxes,omitempty"`
}

// NewProviderMailboxesResult returns the read's result extension for the
// definition name.
func NewProviderMailboxesResult(name string, boxes []ProviderMailbox) (Extension, error) {
	if boxes == nil {
		boxes = []ProviderMailbox{}
	}
	data, err := json.Marshal(ProviderMailboxes{Mailboxes: boxes})
	if err != nil {
		return Extension{}, err
	}
	return Extension{Name: name, Version: MailboxExtensionVersion, Data: data}, nil
}

// DecodeProviderMailboxes reads the mailboxes of a list read's result.
func DecodeProviderMailboxes(e Extension) ([]ProviderMailbox, error) {
	var r ProviderMailboxes
	if err := json.Unmarshal(e.Data, &r); err != nil {
		return nil, invalidField("data", "not a mailbox list")
	}
	for _, m := range r.Mailboxes {
		if err := m.Validate(); err != nil {
			return nil, err
		}
	}
	return r.Mailboxes, nil
}

// CreateProviderMailbox is the data of the mailbox create effect. Kind is
// optional: the adapter's default kind (label for Gmail, folder for
// Exchange and IMAP); an adapter refuses a kind it has none of. Create is
// idempotent by name: a mailbox of that name and kind that exists is the
// result, never an error and never a second one. Its provider reference
// (ProviderMailboxNamespace, RefKindMailbox) is in
// Operation.ProviderReferences once the effect settled.
type CreateProviderMailbox struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Extension returns the create effect's action for adapter type t.
func (c CreateProviderMailbox) Extension(t TypeKey) (Extension, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return Extension{}, err
	}
	return Extension{Name: MailboxCreateName(t), Version: MailboxExtensionVersion, Data: data}, nil
}

// Validate checks c without any provider.
func (c CreateProviderMailbox) Validate() error {
	if c.Kind != "" && !IsMailboxKind(c.Kind) {
		return invalidField("kind", "must be label, folder or category")
	}
	return ValidateMailboxName(c.Name)
}

// ValidateMailboxName checks a mailbox name: UTF-8 without control
// characters, at most MaxMailboxNameBytes, and no empty path segment (no
// leading, trailing or doubled MailboxSeparator).
func ValidateMailboxName(name string) error {
	switch {
	case name == "":
		return invalidField("name", "required")
	case len(name) > MaxMailboxNameBytes:
		return invalidField("name", "too long")
	case !utf8.ValidString(name):
		return invalidField("name", "must be UTF-8")
	case strings.TrimSpace(name) != name:
		return invalidField("name", "must not start or end with space")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return invalidField("name", "must not contain control characters")
		}
	}
	for _, seg := range strings.Split(name, MailboxSeparator) {
		if strings.TrimSpace(seg) == "" {
			return invalidField("name", "must not have an empty path segment")
		}
	}
	return nil
}
